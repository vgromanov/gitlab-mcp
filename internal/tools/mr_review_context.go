package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-retryablehttp"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	capabilityReviewContextMetaV1 = "readmeta.review_context.metadata.v1"
	reviewCeilItems               = 1000
	reviewCeilBytes               = 8 << 20
	reviewCeilRequests            = 128
	reviewCeilElapsed             = 30 * time.Second
	errCursorKeyMissingContext    = "GITLAB_MCP_CURSOR_KEY is required for get_merge_request_review_context; configure a raw secret of at least 32 bytes"
)

type reviewContextCursorIn struct {
	Section string `json:"section"`
	Cursor  string `json:"cursor"`
}

type reviewContextItemIn struct {
	ProjectID           string                  `json:"project_id"`
	MergeRequestIID     int64                   `json:"merge_request_iid"`
	ExpectedHead        *string                 `json:"expected_head,omitempty"`
	Sections            []string                `json:"sections"`
	Cursors             []reviewContextCursorIn `json:"cursors,omitempty"`
	DiscussionSelection discussionSelection     `json:"discussion_selection,omitempty" jsonschema:"semantic or all; omit for semantic"`
	discResume          *cursor.Payload         `json:"-"`
}

type getMergeRequestReviewContextIn struct {
	Items        []reviewContextItemIn `json:"items"`
	MaxItems     *int                  `json:"max_items,omitempty"`
	MaxBytes     *int64                `json:"max_bytes,omitempty"`
	MaxElapsedMS *int64                `json:"max_elapsed_ms,omitempty"`
	MaxRequests  *int                  `json:"max_requests,omitempty"`
}

type metadataDTO struct {
	ProjectID       int64  `json:"project_id"`
	IID             int64  `json:"iid"`
	MergeRequestID  int64  `json:"merge_request_id"`
	SourceProjectID int64  `json:"source_project_id"`
	TargetProjectID int64  `json:"target_project_id"`
	SourceBranch    string `json:"source_branch"`
	TargetBranch    string `json:"target_branch"`
	HeadSHA         string `json:"head_sha"`
	BaseSHA         string `json:"base_sha"`
	StartSHA        string `json:"start_sha"`
	SourceSHA       string `json:"source_sha"`
	TargetSHA       string `json:"target_sha"`
	VersionID       int64  `json:"version_id"`
}

type metadataOut struct {
	Metadata metadataDTO      `json:"metadata"`
	Digest   string           `json:"digest"`
	Section  readmeta.Section `json:"section"`
}

type reviewContextItemOut struct {
	ProjectID                int64                       `json:"project_id"`
	MergeRequestIID          int64                       `json:"merge_request_iid"`
	ReviewClean              bool                        `json:"review_clean"`
	ExpectedHeadMatch        *bool                       `json:"expected_head_match"`
	ObservationalConsistency string                      `json:"observational_consistency"`
	ContextRef               *string                     `json:"context_ref"`
	Metadata                 *metadataOut                `json:"metadata"`
	Approvals                *mrApprovalReadResult       `json:"approvals"`
	ApprovalDigest           *string                     `json:"approval_digest,omitempty"`
	Discussions              *discussionsView            `json:"discussions,omitempty"`
	DiffManifest             *diffWindowOut              `json:"diff_manifest,omitempty"`
	Sections                 map[string]readmeta.Section `json:"sections"`
	Cause                    string                      `json:"cause,omitempty"`
	diffManifestDigest       string
}

type reviewContextOut struct {
	AtomicSnapshot         bool                   `json:"atomic_snapshot"`
	SignatureAttestsReview bool                   `json:"signature_attests_review"`
	Items                  []reviewContextItemOut `json:"items"`
}

type reviewBracket struct {
	OwnerID      int64
	IID          int64
	MRID         int64
	SourceID     int64
	TargetID     int64
	SourceBranch string
	TargetBranch string
	Head         string
	Base         string
	Start        string
	SourceSHA    string
	TargetSHA    string
	VersionID    int64
}

type reviewRuntime struct {
	d            Deps
	ctx          context.Context
	budget       *igl.Budget
	actorID      int64
	instance     string
	policyFP     string
	now          time.Time
	elapsed      time.Duration
	fail         error
	discDeadline time.Time
	projects     map[int64]CanonicalProject
	allowProj    map[int64]struct{}
	allowGrp     map[int64]struct{}
}

func registerMergeRequestReviewContext(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        cursor.ToolReviewContext,
		Description: "Read up to ten merge requests and sign one review-context ref per proved merge request",
	}, getMergeRequestReviewContext)
}

func getMergeRequestReviewContext(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestReviewContextIn, d Deps) (*mcp.CallToolResult, any, error) {
	norm, elapsed, caps, err := normalizeReviewContextInput(in)
	if err != nil {
		return nil, nil, err
	}
	if d.Config == nil || len(d.Config.CursorKey) == 0 {
		return nil, nil, fmt.Errorf("%s", errCursorKeyMissingContext)
	}
	if err := rejectReviewCursors(d, norm, d.now()); err != nil {
		return nil, nil, err
	}
	ctx, budget, release := ensureReviewBudget(ctx, elapsed, caps)
	defer release()
	if err := reviewBudgetPreflight(budget); err != nil {
		return nil, nil, err
	}
	actor, err := queueResolvePrincipal(ctx, d)
	if err != nil {
		return nil, nil, reviewTransportErr(err)
	}
	if err := reviewChargeItem(ctx); err != nil {
		return nil, nil, err
	}
	instance, err := cursorInstance(d.Config)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: instance", cursor.ResyncRequired)
	}
	rt := &reviewRuntime{
		d: d, ctx: ctx, budget: budget, actorID: actor, instance: instance,
		policyFP: d.Config.PolicyFingerprint(), now: d.now(), elapsed: elapsed,
		projects: map[int64]CanonicalProject{},
	}
	if err := rt.loadAllowlist(); err != nil {
		return nil, nil, reviewTransportErr(err)
	}
	out := reviewContextOut{AtomicSnapshot: false, SignatureAttestsReview: false}
	for _, item := range norm {
		out.Items = append(out.Items, rt.one(item))
		if rt.fail != nil {
			return nil, nil, rt.fail
		}
	}
	return nil, out, nil
}

type reviewCaps struct {
	items    int
	bytes    int64
	requests int
}

func normalizeReviewContextInput(in getMergeRequestReviewContextIn) ([]reviewContextItemIn, time.Duration, reviewCaps, error) {
	if len(in.Items) < 1 || len(in.Items) > 10 {
		return nil, 0, reviewCaps{}, fmt.Errorf("items must be 1..10")
	}
	elapsed := reviewCeilElapsed
	var caps reviewCaps
	if in.MaxItems != nil {
		if *in.MaxItems <= 0 || *in.MaxItems > reviewCeilItems {
			return nil, 0, reviewCaps{}, fmt.Errorf("max_items must be positive and at most %d", reviewCeilItems)
		}
		caps.items = *in.MaxItems
	}
	if in.MaxBytes != nil {
		if *in.MaxBytes <= 0 || *in.MaxBytes > reviewCeilBytes {
			return nil, 0, reviewCaps{}, fmt.Errorf("max_bytes must be positive and at most %d", reviewCeilBytes)
		}
		caps.bytes = *in.MaxBytes
	}
	if in.MaxRequests != nil {
		if *in.MaxRequests <= 0 || *in.MaxRequests > reviewCeilRequests {
			return nil, 0, reviewCaps{}, fmt.Errorf("max_requests must be positive and at most %d", reviewCeilRequests)
		}
		caps.requests = *in.MaxRequests
	}
	if in.MaxElapsedMS != nil {
		if *in.MaxElapsedMS <= 0 || *in.MaxElapsedMS > reviewCeilElapsed.Milliseconds() {
			return nil, 0, reviewCaps{}, fmt.Errorf("max_elapsed_ms must be positive and at most %d", reviewCeilElapsed.Milliseconds())
		}
		elapsed = time.Duration(*in.MaxElapsedMS) * time.Millisecond
	}
	out := make([]reviewContextItemIn, len(in.Items))
	for i, item := range in.Items {
		if item.MergeRequestIID < 1 {
			return nil, 0, reviewCaps{}, fmt.Errorf("merge_request_iid must be >= 1")
		}
		if strings.TrimSpace(item.ProjectID) == "" {
			return nil, 0, reviewCaps{}, fmt.Errorf("project_id is required")
		}
		if item.ExpectedHead != nil {
			if _, ok := readmeta.ObservedHeadSHA(*item.ExpectedHead); !ok || *item.ExpectedHead != strings.ToLower(*item.ExpectedHead) {
				return nil, 0, reviewCaps{}, fmt.Errorf("expected_head must be canonical 40-hex")
			}
		}
		if len(item.Sections) == 0 {
			return nil, 0, reviewCaps{}, fmt.Errorf("sections are required")
		}
		seen := map[string]struct{}{}
		sections := append([]string(nil), item.Sections...)
		for _, name := range sections {
			if !reviewSectionAllowed(name) {
				return nil, 0, reviewCaps{}, fmt.Errorf("unknown section %q", name)
			}
			if _, ok := seen[name]; ok {
				return nil, 0, reviewCaps{}, fmt.Errorf("duplicate section %q", name)
			}
			seen[name] = struct{}{}
		}
		sort.Strings(sections)
		item.Sections = sections
		switch string(item.DiscussionSelection) {
		case "", "semantic", "all":
		default:
			return nil, 0, reviewCaps{}, fmt.Errorf("discussion_selection must be semantic or all")
		}
		for _, c := range item.Cursors {
			if strings.TrimSpace(c.Cursor) != c.Cursor || strings.TrimSpace(c.Section) != c.Section {
				return nil, 0, reviewCaps{}, fmt.Errorf("%s: malformed cursor", cursor.ResyncRequired)
			}
		}
		out[i] = item
	}
	return out, elapsed, caps, nil
}

func reviewSectionAllowed(name string) bool {
	switch name {
	case "metadata", "approvals", "discussions", "pipeline_graph", "diff_manifest":
		return true
	default:
		return false
	}
}

func rejectReviewCursors(d Deps, items []reviewContextItemIn, now time.Time) error {
	for i, item := range items {
		var resume *cursor.Payload
		for _, c := range item.Cursors {
			if c.Cursor == "" {
				continue
			}
			if strings.TrimSpace(c.Cursor) != c.Cursor || strings.TrimSpace(c.Section) != c.Section {
				return fmt.Errorf("%s: malformed cursor", cursor.ResyncRequired)
			}
			decoded, err := cursor.Decode(d.Config.CursorKey, c.Cursor, now)
			if err != nil {
				return fmt.Errorf("%s: cursor validation failed", cursor.ResyncRequired)
			}
			if decoded.Tool != cursor.ToolReviewContext || decoded.Section != cursor.SectionReviewDiscussions || decoded.DiscussionsCont == nil {
				return fmt.Errorf("%s: review context does not resume section cursors", cursor.ResyncRequired)
			}
			if !itemWants(item, "discussions") || c.Section != "discussions" {
				return fmt.Errorf("%s: review context does not resume section cursors", cursor.ResyncRequired)
			}
			if resume != nil {
				return fmt.Errorf("%s: review context does not resume section cursors", cursor.ResyncRequired)
			}
			if decoded.Filters.Selection != itemDiscSelection(item) {
				return fmt.Errorf("%s: discussion cursor selection", cursor.ResyncRequired)
			}
			if decoded.Scope.MergeRequestIID == nil || *decoded.Scope.MergeRequestIID != item.MergeRequestIID {
				return fmt.Errorf("%s: discussion cursor binding", cursor.ResyncRequired)
			}
			copyDecoded := decoded
			resume = &copyDecoded
		}
		item.discResume = resume
		items[i] = item
	}
	return nil
}

func ensureReviewBudget(ctx context.Context, elapsed time.Duration, caps reviewCaps) (context.Context, *igl.Budget, func()) {
	b := igl.BudgetFromContext(ctx)
	owned := false
	if b == nil {
		b = igl.DefaultBudget()
		b.MaxItems = reviewCeilItems
		b.MaxBytes = reviewCeilBytes
		b.MaxRequests = reviewCeilRequests
		b.MaxElapsed = reviewCeilElapsed
		ctx = igl.WithBudget(ctx, b)
		owned = true
	}
	b.CapLimits(reviewCeilItems, reviewCeilBytes, reviewCeilRequests)
	b.CapLimits(caps.items, caps.bytes, caps.requests)
	deadline := time.Now().Add(elapsed)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	dctx, cancel := context.WithDeadline(ctx, deadline)
	dctx = igl.WithExactReadProvenance(dctx)
	release := func() {
		cancel()
		if owned {
			b.Cancel()
		}
	}
	return dctx, b, release
}

func reviewBudgetPreflight(b *igl.Budget) error {
	if b == nil {
		return nil
	}
	reqs, nbytes, items := b.Stats()
	if b.MaxRequests > 0 && reqs >= b.MaxRequests {
		return fmt.Errorf("%s", readmeta.CodeBudgetRequests)
	}
	if b.MaxBytes > 0 && nbytes >= b.MaxBytes {
		return fmt.Errorf("%s", readmeta.CodeBudgetBytes)
	}
	if b.MaxItems > 0 && items >= b.MaxItems {
		return fmt.Errorf("%s", readmeta.CodeBudgetItems)
	}
	return nil
}

func reviewChargeItem(ctx context.Context) error {
	b := igl.BudgetFromContext(ctx)
	if b == nil {
		return nil
	}
	if err := b.AddItem(); err != nil {
		return fmt.Errorf("%s", readmeta.CodeBudgetItems)
	}
	return nil
}

type reviewPhaseHookKey struct{}

func withReviewPhaseHook(ctx context.Context, fn func(string) error) context.Context {
	return context.WithValue(ctx, reviewPhaseHookKey{}, fn)
}

func (rt *reviewRuntime) phaseErr(name string) error {
	if fn, ok := rt.ctx.Value(reviewPhaseHookKey{}).(func(string) error); ok && fn != nil {
		if err := fn(name); err != nil {
			return err
		}
	}
	return rt.ctx.Err()
}

func (rt *reviewRuntime) sealRequested(item reviewContextItemIn, out *reviewContextItemOut) {
	for _, name := range item.Sections {
		if _, ok := out.Sections[name]; ok {
			continue
		}
		var sec readmeta.Section
		if name == "approvals" {
			sec = newApprovalsSection(rt.now)
		} else {
			sec = unsupportedReviewSection(rt.now, name)
			sec.ContentComplete = readmeta.ContentCompleteUnknown
			sec.Consistency = readmeta.ConsistencyUnknown
			sec.HeadSHA = nil
			sec.NextCursor = nil
			sec.Limitations = []readmeta.Limitation{}
			if name == "metadata" {
				sec.CapabilityVersion = capabilityReviewContextMetaV1
			}
		}
		out.Sections[name] = sec
	}
	if out.Cause != "" {
		out.ReviewClean = false
		out.ContextRef = nil
		rt.dropDiffManifestClaim(out, out.Cause, "closing")
	}
}

func reviewTransportErr(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", reviewClassify(err))
}

func reviewClassify(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) {
		return readmeta.CodeBudgetElapsed
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, errQueueCancelled) {
		return readmeta.CodeCancelled
	}
	if isTypedBudget(err) {
		return reviewBudgetCode(err)
	}
	msg := err.Error()
	for _, code := range []string{
		readmeta.CodeBudgetRequests,
		readmeta.CodeBudgetBytes,
		readmeta.CodeBudgetElapsed,
		readmeta.CodeBudgetItems,
		readmeta.CodeCancelled,
		readmeta.CodeAuthzDenied,
		readmeta.CodeIdentityUnresolved,
		readmeta.CodeInaccessible,
		readmeta.CodeUnsupported,
		readmeta.CodeInconsistent,
		readmeta.CodeHTTPError,
	} {
		if strings.HasPrefix(msg, code) {
			return code
		}
	}
	return readmeta.CodeHTTPError
}

func reviewBudgetCode(err error) string {
	switch {
	case errors.Is(err, igl.ErrBudgetRequests):
		return readmeta.CodeBudgetRequests
	case errors.Is(err, igl.ErrBudgetBytes):
		return readmeta.CodeBudgetBytes
	case errors.Is(err, igl.ErrBudgetElapsed):
		return readmeta.CodeBudgetElapsed
	default:
		return readmeta.CodeBudgetItems
	}
}

func (rt *reviewRuntime) loadAllowlist() error {
	rt.allowProj = map[int64]struct{}{}
	rt.allowGrp = map[int64]struct{}{}
	if !policyActive(rt.d.Config) {
		return nil
	}
	for _, tok := range rt.d.Config.AllowedProjectIDs {
		if strings.TrimSpace(tok) == "" {
			continue
		}
		p, err := queueGetProject(rt.ctx, rt.d, tok)
		if err != nil {
			return err
		}
		if err := reviewChargeItem(rt.ctx); err != nil {
			return err
		}
		rt.allowProj[p.ID] = struct{}{}
	}
	for _, tok := range rt.d.Config.AllowedGroupIDs {
		if strings.TrimSpace(tok) == "" {
			continue
		}
		g, err := queueGetGroup(rt.ctx, rt.d, tok)
		if err != nil {
			return err
		}
		rt.allowGrp[g.ID] = struct{}{}
	}
	return nil
}

func (rt *reviewRuntime) authorizeProject(projectID string) (CanonicalProject, error) {
	def := ""
	if rt.d.Config != nil {
		def = rt.d.Config.DefaultProjectID
	}
	pid, err := ResolveProjectID(projectID, def)
	if err != nil {
		return CanonicalProject{}, err
	}
	if id, convErr := strconv.ParseInt(pid, 10, 64); convErr == nil {
		if canon, ok := rt.projects[id]; ok {
			return canon, nil
		}
	}
	p, err := queueGetProject(rt.ctx, rt.d, pid)
	if err != nil {
		return CanonicalProject{}, err
	}
	if _, ok := rt.projects[p.ID]; ok {
		return rt.projects[p.ID], rt.projectPolicy(rt.projects[p.ID])
	}
	canon := projectFromAPI(p)
	if err := reviewChargeItem(rt.ctx); err != nil {
		return CanonicalProject{}, err
	}
	if err := rt.projectPolicy(canon); err != nil {
		return CanonicalProject{}, err
	}
	rt.projects[canon.ID] = canon
	return canon, nil
}

func (rt *reviewRuntime) projectPolicy(canon CanonicalProject) error {
	if !policyActive(rt.d.Config) {
		return nil
	}
	if len(rt.d.Config.AllowedProjectIDs) > 0 {
		if _, ok := rt.allowProj[canon.ID]; !ok {
			return authzDenied("project not allowed")
		}
	}
	if len(rt.d.Config.AllowedGroupIDs) > 0 {
		if canon.NamespaceKind != "group" {
			return authzDenied("project namespace not under allowed group")
		}
		ok, err := queueGroupAncestryContains(rt.ctx, rt.d, canon.NamespaceID, rt.allowGrp)
		if err != nil {
			return err
		}
		if !ok {
			return authzDenied("project not under allowed group")
		}
	}
	return nil
}

func (rt *reviewRuntime) one(item reviewContextItemIn) reviewContextItemOut {
	out := reviewContextItemOut{
		MergeRequestIID:          item.MergeRequestIID,
		ObservationalConsistency: readmeta.ConsistencyUnknown,
		Sections:                 map[string]readmeta.Section{},
	}
	rt.discDeadline = time.Time{}
	rt.fillUnsupported(item, &out)
	defer rt.sealRequested(item, &out)
	if err := rt.phaseErr("proof"); err != nil {
		out.Cause = reviewClassify(err)
		return out
	}
	owner, err := rt.authorizeProject(item.ProjectID)
	if err != nil {
		out.Cause = causeOf(err)
		return out
	}
	out.ProjectID = owner.ID
	if err := reviewChargeItem(rt.ctx); err != nil {
		out.Cause = readmeta.CodeBudgetItems
		return out
	}
	firstDetail, cause, consistency, derr := rt.readDetail(owner, item.MergeRequestIID)
	if derr != nil {
		out.Cause = causeOf(derr)
		return out
	}
	if cause != "" {
		out.Cause = cause
		out.ObservationalConsistency = consistency
		return out
	}
	if err := rt.authorizeForks(owner, firstDetail); err != nil {
		out.Cause = causeOf(err)
		return out
	}
	first, ferr := rt.finishBracket(firstDetail, item.MergeRequestIID)
	if ferr != nil {
		out.Cause = causeOf(ferr)
		return out
	}
	if !first.proved {
		if item.discResume != nil {
			rt.fail = fmt.Errorf("%s: discussion cursor", cursor.ResyncRequired)
			out.Cause = cursor.ResyncRequired
			return out
		}
		out.Cause = first.cause
		out.ObservationalConsistency = first.consistency
		if itemWants(item, "approvals") && first.cause != readmeta.CodeInconsistent {
			rt.readApprovals(owner, item.MergeRequestIID, &out)
		}
		return out
	}
	if itemWants(item, "approvals") {
		if err := rt.phaseErr("section"); err != nil {
			out.Cause = reviewClassify(err)
			out.ContextRef = nil
			out.Metadata = nil
			return out
		}
		rt.readApprovals(owner, item.MergeRequestIID, &out)
		if out.Cause == readmeta.CodeBudgetRequests || out.Cause == readmeta.CodeBudgetBytes || out.Cause == readmeta.CodeBudgetElapsed || out.Cause == readmeta.CodeBudgetItems || out.Cause == readmeta.CodeCancelled {
			out.ContextRef = nil
			out.Metadata = nil
			return out
		}
	}
	if itemWants(item, "discussions") {
		if err := rt.readDiscussions(item, owner, first.bracket, &out); err != nil {
			if errors.Is(err, errDiscResync) {
				rt.fail = fmt.Errorf("%s: discussion cursor", cursor.ResyncRequired)
				out.Cause = cursor.ResyncRequired
			}
			rt.finalizeDiscBound(&out)
			return out
		}
		if !rt.discDeadline.IsZero() {
			parent := rt.ctx
			child, cancel := discBoundContext(parent, rt.d.now(), rt.discDeadline)
			rt.ctx = child
			defer func() {
				cancel()
				rt.ctx = parent
			}()
		}
	}
	if itemWants(item, "diff_manifest") {
		if err := rt.attachDiffManifest(owner, item.MergeRequestIID, first.bracket, &out); err != nil {
			out.Cause = reviewClassify(err)
			out.ContextRef = nil
			out.ReviewClean = false
			rt.finalizeDiscBound(&out)
			return out
		}
	}
	if err := rt.phaseErr("bracket"); err != nil {
		return rt.failClosedManifest(&out, reviewClassify(err))
	}
	secondDetail, cause2, consistency2, serr := rt.readDetail(owner, item.MergeRequestIID)
	if serr != nil {
		out.Metadata = nil
		return rt.failClosedManifest(&out, causeOf(serr))
	}
	if cause2 != "" || !sameBracketIdentity(first.bracket, secondDetail) {
		out.Cause = cause2
		if out.Cause == "" {
			out.Cause = readmeta.CodeInconsistent
		}
		out.ObservationalConsistency = consistency2
		if out.ObservationalConsistency == "" {
			out.ObservationalConsistency = readmeta.ConsistencyInconsistent
		}
		out.Metadata = nil
		out.ContextRef = nil
		rt.dropDiffManifestClaim(&out, readmeta.CodeInconsistent, "bracket")
		rt.markSection(&out, "metadata", readmeta.ContentCompleteUnknown, out.ObservationalConsistency)
		rt.finalizeDiscBound(&out)
		return out
	}
	second, serr2 := rt.finishBracket(secondDetail, item.MergeRequestIID)
	if serr2 != nil {
		out.Metadata = nil
		return rt.failClosedManifest(&out, causeOf(serr2))
	}
	if !second.proved || !bracketsEqual(first.bracket, second.bracket) {
		out.Cause = second.cause
		if out.Cause == "" {
			out.Cause = readmeta.CodeInconsistent
		}
		out.ObservationalConsistency = readmeta.ConsistencyInconsistent
		if second.cause == readmeta.CodeIdentityUnresolved || second.consistency == readmeta.ConsistencyUnknown {
			out.ObservationalConsistency = readmeta.ConsistencyUnknown
			if out.Cause == readmeta.CodeInconsistent {
				out.Cause = readmeta.CodeIdentityUnresolved
			}
		}
		out.Metadata = nil
		out.ContextRef = nil
		rt.dropDiffManifestClaim(&out, readmeta.CodeInconsistent, "bracket")
		rt.markSection(&out, "metadata", readmeta.ContentCompleteUnknown, out.ObservationalConsistency)
		rt.finalizeDiscBound(&out)
		return out
	}
	agreed := second.bracket
	rt.finalizeDiscBound(&out)
	if itemWants(item, "discussions") {
		finishDiscussionClaim(&out, agreed.Head)
	}
	match := expectedMatch(item.ExpectedHead, agreed.Head)
	out.ExpectedHeadMatch = match
	metaComplete := false
	if itemWants(item, "metadata") && first.selected && second.selected {
		meta, dig, sec := metadataEvidence(rt.now, agreed)
		out.Metadata = &metadataOut{Metadata: meta, Digest: dig, Section: sec}
		out.Sections["metadata"] = sec
		metaComplete = true
	}
	complete := []string{}
	excluded := []string{}
	if itemWants(item, "metadata") {
		if metaComplete {
			complete = append(complete, "metadata")
		} else {
			excluded = append(excluded, "metadata")
			rt.markSection(&out, "metadata", readmeta.ContentCompleteUnknown, readmeta.ConsistencyUnknown)
		}
	}
	if itemWants(item, "approvals") {
		if approvalComplete(out.Approvals) {
			complete = append(complete, "approvals")
		} else {
			excluded = append(excluded, "approvals")
		}
	}
	for _, name := range item.Sections {
		if name == "metadata" || name == "approvals" {
			continue
		}
		if name == "discussions" && out.Sections["discussions"].ContentComplete == readmeta.ContentCompleteTrue {
			complete = append(complete, "discussions")
			continue
		}
		if name == "diff_manifest" && diffManifestComplete(out) {
			complete = append(complete, "diff_manifest")
			continue
		}
		excluded = append(excluded, name)
	}
	sort.Strings(complete)
	sort.Strings(excluded)
	rt.finalizeDiscBound(&out)
	complete, excluded = dropUnprovedDiscussions(item, &out, complete, excluded)
	setReviewClean(&out, complete, excluded, match, len(item.Sections))
	if len(complete) == 0 {
		out.ContextRef = nil
		return out
	}
	rt.beforeMint()
	if err := rt.phaseErr("mint"); err != nil {
		return rt.failClosedManifest(&out, reviewClassify(err))
	}
	if err := budgetAllowsNext(rt.ctx); err != nil {
		return rt.failClosedManifest(&out, causeOf(err))
	}
	if err := reviewPreMintBudget(rt.ctx); err != nil {
		return rt.failClosedManifest(&out, causeOf(err))
	}
	rt.finalizeDiscBound(&out)
	complete, excluded = dropUnprovedDiscussions(item, &out, complete, excluded)
	setReviewClean(&out, complete, excluded, match, len(item.Sections))
	if len(complete) == 0 {
		out.ContextRef = nil
		return out
	}
	ref, err := rt.mint(owner, item, agreed, complete, excluded, metaDigestOf(out))
	if err != nil {
		return rt.failClosedManifest(&out, causeOf(err))
	}
	out.ContextRef = &ref
	return out
}

func setReviewClean(out *reviewContextItemOut, complete, excluded []string, match *bool, sections int) {
	if out == nil {
		return
	}
	out.ReviewClean = len(excluded) == 0 && len(complete) == sections && (match == nil || *match)
	if out.ReviewClean {
		out.ObservationalConsistency = readmeta.ConsistencyConsistent
	} else if out.ObservationalConsistency == "" {
		out.ObservationalConsistency = readmeta.ConsistencyUnknown
	}
}

func dropUnprovedDiscussions(item reviewContextItemIn, out *reviewContextItemOut, complete, excluded []string) ([]string, []string) {
	if out == nil || !itemWants(item, "discussions") {
		return complete, excluded
	}
	if out.Sections["discussions"].ContentComplete == readmeta.ContentCompleteTrue {
		return complete, excluded
	}
	nextC := complete[:0]
	seen := false
	for _, name := range complete {
		if name == "discussions" {
			seen = true
			continue
		}
		nextC = append(nextC, name)
	}
	complete = nextC
	for _, name := range excluded {
		if name == "discussions" {
			sort.Strings(complete)
			sort.Strings(excluded)
			return complete, excluded
		}
	}
	if seen || itemWants(item, "discussions") {
		excluded = append(excluded, "discussions")
	}
	sort.Strings(complete)
	sort.Strings(excluded)
	return complete, excluded
}

type reviewMintHookKey struct{}

// withReviewMintHook runs fn after the bracket is proved and before Encode.
// Tests use it to cancel the current item without starting another read.
func withReviewMintHook(ctx context.Context, fn func()) context.Context {
	return context.WithValue(ctx, reviewMintHookKey{}, fn)
}

func diffManifestComplete(out reviewContextItemOut) bool {
	sec, ok := out.Sections["diff_manifest"]
	return ok && out.diffManifestDigest != "" && sec.ContentComplete == readmeta.ContentCompleteTrue && sec.Consistency == readmeta.ConsistencyConsistent && sec.NextCursor == nil
}

func (rt *reviewRuntime) attachDiffManifest(owner CanonicalProject, iid int64, b reviewBracket, out *reviewContextItemOut) error {
	if b.VersionID < 1 || b.MRID < 1 {
		sec := unsupportedReviewSection(rt.now, "diff_manifest")
		sec.AddLimitation(readmeta.CodeIdentityUnresolved, "diff manifest")
		out.Sections["diff_manifest"] = sec
		return nil
	}
	got, err := readBoundedDiffManifest(rt.ctx, rt.d, diffQuery{
		OwnerID: owner.ID, IID: iid, MRID: b.MRID, SourceProjectID: b.SourceID, TargetProjectID: b.TargetID,
		Selection: diffSelection{
			ProjectID: strconv.FormatInt(owner.ID, 10), IID: iid, Mode: diffModeVersion, VersionID: b.VersionID, PerPage: 20,
			Head: b.Head, Base: b.Base, Start: b.Start,
		},
		Full: true, Now: rt.now,
	})
	if err != nil {
		if passthroughTypedProviderErr(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		sec := newDiffSection(rt.now)
		sec = stampDiffFailure(sec, readmeta.CodeHTTPError)
		out.Sections["diff_manifest"] = sec
		return nil
	}
	got.Section.NextCursor = nil
	got.Section.PaginationExhausted = got.Section.ContentComplete == readmeta.ContentCompleteTrue
	out.Sections["diff_manifest"] = got.Section
	out.DiffManifest = &got
	if got.Digest != nil && got.Section.ContentComplete == readmeta.ContentCompleteTrue && got.Section.Consistency == readmeta.ConsistencyConsistent {
		out.diffManifestDigest = *got.Digest
	}
	return nil
}

// failClosedManifest clears scalar and nested manifest claims on the value
// that return will copy. A deferred seal runs too late to change those scalars.
func (rt *reviewRuntime) failClosedManifest(out *reviewContextItemOut, cause string) reviewContextItemOut {
	out.Cause = cause
	out.ContextRef = nil
	out.ReviewClean = false
	rt.dropDiffManifestClaim(out, cause, "closing")
	rt.finalizeDiscBound(out)
	return *out
}

func reviewPreMintBudget(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			if b := igl.BudgetFromContext(ctx); b != nil && b.ElapsedExceeded() {
				return igl.ErrBudgetElapsed
			}
			return err
		}
		return err
	}
	b := igl.BudgetFromContext(ctx)
	if b == nil {
		return nil
	}
	if b.ElapsedExceeded() {
		return igl.ErrBudgetElapsed
	}
	_, _, items := b.Stats()
	if b.MaxItems > 0 && items >= b.MaxItems {
		return igl.ErrBudgetItems
	}
	return nil
}

func (rt *reviewRuntime) dropDiffManifestClaim(out *reviewContextItemOut, code, msg string) {
	if out == nil {
		return
	}
	claimed := out.diffManifestDigest != ""
	out.diffManifestDigest = ""
	sec, ok := out.Sections["diff_manifest"]
	if !ok || !claimed {
		return
	}
	sec.ContentComplete = readmeta.ContentCompleteUnknown
	sec.Consistency = readmeta.ConsistencyUnknown
	sec.ManifestCoverage = readmeta.CoverageUnknown
	sec.PaginationExhausted = false
	sec.NextCursor = nil
	sec.HeadSHA = nil
	sec.AddLimitation(code, msg)
	out.Sections["diff_manifest"] = sec
	if out.DiffManifest != nil {
		out.DiffManifest.Digest = nil
		out.DiffManifest.Section = sec
	}
	out.ReviewClean = false
}

func (rt *reviewRuntime) beforeMint() {
	fn, _ := rt.ctx.Value(reviewMintHookKey{}).(func())
	if fn != nil {
		fn()
	}
}

func (rt *reviewRuntime) fillUnsupported(item reviewContextItemIn, out *reviewContextItemOut) {
	for _, name := range item.Sections {
		if name == "metadata" || name == "approvals" {
			continue
		}
		out.Sections[name] = unsupportedReviewSection(rt.now, name)
	}
}

func unsupportedReviewSection(now time.Time, name string) readmeta.Section {
	sec := readmeta.Section{
		RetrievedAt:         now.UTC().Format(time.RFC3339),
		Source:              readmeta.SourceGitLabREST,
		Provider:            readmeta.ProviderGitLab,
		CapabilityVersion:   "readmeta.review_context." + name + ".v1",
		HeadSHA:             nil,
		PaginationExhausted: false,
		ContentComplete:     readmeta.ContentCompleteUnknown,
		Consistency:         readmeta.ConsistencyUnknown,
		Limitations:         []readmeta.Limitation{{Code: readmeta.CodeUnsupported, Message: name}},
		NextCursor:          nil,
		Counts:              readmeta.Counts{},
		ManifestCoverage:    readmeta.CoverageUnknown,
		PatchCoverage:       readmeta.CoverageUnknown,
	}
	return sec
}

func itemWants(item reviewContextItemIn, name string) bool {
	for _, s := range item.Sections {
		if s == name {
			return true
		}
	}
	return false
}

func (rt *reviewRuntime) authorizeForks(owner CanonicalProject, b reviewBracket) error {
	if b.SourceID != owner.ID {
		if _, err := rt.authorizeProject(strconv.FormatInt(b.SourceID, 10)); err != nil {
			return err
		}
	}
	if b.TargetID != owner.ID && b.TargetID != b.SourceID {
		if _, err := rt.authorizeProject(strconv.FormatInt(b.TargetID, 10)); err != nil {
			return err
		}
	}
	return nil
}

type bracketRead struct {
	bracket     reviewBracket
	proved      bool
	selected    bool
	cause       string
	consistency string
}

func (rt *reviewRuntime) readDetail(owner CanonicalProject, iid int64) (reviewBracket, string, string, error) {
	if err := rt.phaseErr("detail"); err != nil {
		return reviewBracket{}, "", "", err
	}
	body, _, err := rt.rawGET(fmt.Sprintf("projects/%s/merge_requests/%d", gitlab.PathEscape(projectAPIID(owner)), iid), nil)
	if err != nil {
		return reviewBracket{}, "", "", err
	}
	b, cause, consistency := parseDetail(owner.ID, iid, body)
	return b, cause, consistency, nil
}

func sameBracketIdentity(a, b reviewBracket) bool {
	return a.OwnerID == b.OwnerID && a.IID == b.IID && a.MRID == b.MRID &&
		a.SourceID == b.SourceID && a.TargetID == b.TargetID &&
		a.SourceBranch == b.SourceBranch && a.TargetBranch == b.TargetBranch &&
		a.Head == b.Head && a.Base == b.Base && a.Start == b.Start
}

func (rt *reviewRuntime) finishBracket(b reviewBracket, iid int64) (bracketRead, error) {
	srcSHA, err := rt.readBranchSHA(b.SourceID, b.SourceBranch)
	if err != nil {
		return bracketRead{}, err
	}
	if srcSHA == "" || srcSHA != b.Head {
		if srcSHA != "" && srcSHA != b.Head {
			return bracketRead{cause: readmeta.CodeInconsistent, consistency: readmeta.ConsistencyInconsistent}, nil
		}
		return bracketRead{cause: readmeta.CodeIdentityUnresolved, consistency: readmeta.ConsistencyUnknown}, nil
	}
	tgtSHA, err := rt.readBranchSHA(b.TargetID, b.TargetBranch)
	if err != nil {
		return bracketRead{}, err
	}
	if tgtSHA == "" {
		return bracketRead{cause: readmeta.CodeIdentityUnresolved, consistency: readmeta.ConsistencyUnknown}, nil
	}
	if err := reviewChargeItem(rt.ctx); err != nil {
		return bracketRead{}, err
	}
	if err := reviewChargeItem(rt.ctx); err != nil {
		return bracketRead{}, err
	}
	b.SourceSHA = srcSHA
	b.TargetSHA = tgtSHA
	verBody, verHdr, err := rt.rawGET(fmt.Sprintf("projects/%s/merge_requests/%d/versions", gitlab.PathEscape(strconv.FormatInt(b.OwnerID, 10)), iid), &versionsPage{PerPage: 20})
	if err != nil {
		return bracketRead{}, err
	}
	id, kind := selectReviewVersion(rt.ctx, verBody, verHdr, b)
	switch kind {
	case "budget":
		return bracketRead{}, fmt.Errorf("%s", readmeta.CodeBudgetItems)
	case "selected":
		b.VersionID = id
		return bracketRead{bracket: b, proved: true, selected: true, consistency: readmeta.ConsistencyConsistent}, nil
	case readmeta.CodeInconsistent:
		return bracketRead{cause: readmeta.CodeInconsistent, consistency: readmeta.ConsistencyInconsistent}, nil
	default:
		return bracketRead{cause: readmeta.ConsistencyUnknown, consistency: readmeta.ConsistencyUnknown}, nil
	}
}

type versionsPage struct {
	PerPage int `url:"per_page,omitempty"`
}

func (rt *reviewRuntime) readBranchSHA(projectID int64, branch string) (string, error) {
	if projectID < 1 || strings.TrimSpace(branch) == "" {
		return "", nil
	}
	body, _, err := rt.rawGET(fmt.Sprintf("projects/%s/repository/branches/%s", gitlab.PathEscape(strconv.FormatInt(projectID, 10)), gitlab.PathEscape(branch)), nil)
	if err != nil {
		return "", err
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return "", nil
	}
	name := reviewJSONString(env["name"])
	if name != branch {
		return "", nil
	}
	var commit map[string]json.RawMessage
	if err := json.Unmarshal(env["commit"], &commit); err != nil {
		return "", nil
	}
	sha, ok := readmeta.ObservedHeadSHA(reviewJSONString(commit["id"]))
	if !ok {
		return "", nil
	}
	return sha, nil
}

func (rt *reviewRuntime) rawGET(path string, opt any) ([]byte, http.Header, error) {
	if err := budgetAllowsNext(rt.ctx); err != nil {
		return nil, nil, err
	}
	var bodyCap *approvalBodyCapture
	req, err := rt.d.Client.NewRequest(http.MethodGet, path, opt, []gitlab.RequestOptionFunc{
		gitlab.WithContext(rt.ctx),
		gitlab.WithRequestRetry(reviewReadCheckRetry(&bodyCap)),
	})
	if err != nil {
		return nil, nil, reviewTransportErr(err)
	}
	var buf bytes.Buffer
	resp, err := rt.d.Client.Do(req, &buf)
	if errors.Is(rt.ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) {
		return nil, nil, fmt.Errorf("%s", readmeta.CodeBudgetElapsed)
	}
	if readErr := bodyCap.capturedReadErr(); readErr != nil {
		return nil, nil, reviewTransportErr(readErr)
	}
	if err != nil {
		if errors.Is(err, igl.ErrExactReadRedirect) {
			return nil, nil, fmt.Errorf("%s", readmeta.CodeHTTPError)
		}
		if errors.Is(err, gitlab.ErrNotFound) {
			return nil, nil, fmt.Errorf("%s", readmeta.CodeIdentityUnresolved)
		}
		return nil, nil, reviewTransportErr(err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		if code == http.StatusNotFound {
			return nil, nil, fmt.Errorf("%s", readmeta.CodeIdentityUnresolved)
		}
		return nil, nil, fmt.Errorf("%s", readmeta.CodeHTTPError)
	}
	hdr := http.Header{}
	if resp.Response != nil {
		hdr = resp.Response.Header
	}
	return append([]byte(nil), buf.Bytes()...), hdr, nil
}

func reviewReadCheckRetry(cap **approvalBodyCapture) retryablehttp.CheckRetry {
	return func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if errors.Is(err, igl.ErrExactReadRedirect) {
			return false, err
		}
		if resp != nil && resp.Body != nil {
			if _, ok := resp.Body.(*approvalBodyCapture); !ok {
				c := &approvalBodyCapture{ReadCloser: resp.Body}
				*cap = c
				resp.Body = c
			}
		}
		return igl.SafeReadCheckRetry(ctx, resp, err)
	}
}

func parseDetail(ownerID, iid int64, body []byte) (reviewBracket, string, string) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return reviewBracket{}, readmeta.CodeHTTPError, readmeta.ConsistencyUnknown
	}
	gotIID, ok := jsonInt(env["iid"])
	if !ok || gotIID != iid {
		return reviewBracket{}, readmeta.CodeIdentityUnresolved, readmeta.ConsistencyUnknown
	}
	gotOwner, ok := jsonInt(env["project_id"])
	if !ok || gotOwner != ownerID {
		return reviewBracket{}, readmeta.CodeIdentityUnresolved, readmeta.ConsistencyUnknown
	}
	mrID, ok := jsonInt(env["id"])
	if !ok || mrID < 1 {
		return reviewBracket{}, readmeta.CodeIdentityUnresolved, readmeta.ConsistencyUnknown
	}
	sourceID, sok := jsonInt(env["source_project_id"])
	targetID, tok := jsonInt(env["target_project_id"])
	if !sok || !tok || sourceID < 1 || targetID < 1 {
		return reviewBracket{}, readmeta.CodeIdentityUnresolved, readmeta.ConsistencyUnknown
	}
	sourceBranch := reviewJSONString(env["source_branch"])
	targetBranch := reviewJSONString(env["target_branch"])
	if sourceBranch == "" || targetBranch == "" {
		return reviewBracket{}, readmeta.CodeIdentityUnresolved, readmeta.ConsistencyUnknown
	}
	var refs map[string]json.RawMessage
	if err := json.Unmarshal(env["diff_refs"], &refs); err != nil {
		return reviewBracket{}, readmeta.CodeIdentityUnresolved, readmeta.ConsistencyUnknown
	}
	head, hok := readmeta.ObservedHeadSHA(reviewJSONString(refs["head_sha"]))
	base, bok := readmeta.ObservedHeadSHA(reviewJSONString(refs["base_sha"]))
	start, sok2 := readmeta.ObservedHeadSHA(reviewJSONString(refs["start_sha"]))
	sha, shaOK := readmeta.ObservedHeadSHA(reviewJSONString(env["sha"]))
	if !hok || !bok || !sok2 || !shaOK {
		return reviewBracket{}, readmeta.CodeIdentityUnresolved, readmeta.ConsistencyUnknown
	}
	if sha != head {
		return reviewBracket{}, readmeta.CodeInconsistent, readmeta.ConsistencyInconsistent
	}
	return reviewBracket{
		OwnerID: ownerID, IID: iid, MRID: mrID, SourceID: sourceID, TargetID: targetID,
		SourceBranch: sourceBranch, TargetBranch: targetBranch, Head: head, Base: base, Start: start,
	}, "", ""
}

func reviewVersionPaging(hdr http.Header, sdkNext *int64) (exhausted bool) {
	vals, present := queueNextPageValues(hdr)
	if !present || len(vals) != 1 {
		return false
	}
	raw := vals[0]
	if strings.TrimSpace(raw) != raw {
		return false
	}
	if raw == "" || raw == "0" {
		return sdkNext == nil || *sdkNext == 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != raw {
		return false
	}
	if sdkNext != nil && *sdkNext != n {
		return false
	}
	return false
}

func provedVersionRow(env map[string]json.RawMessage) (id, mrID int64, head, base, start string, ok bool) {
	id, idOK := jsonInt(env["id"])
	mrID, mrOK := jsonInt(env["merge_request_id"])
	head, hok := readmeta.ObservedHeadSHA(reviewJSONString(env["head_commit_sha"]))
	base, bok := readmeta.ObservedHeadSHA(reviewJSONString(env["base_commit_sha"]))
	start, sok := readmeta.ObservedHeadSHA(reviewJSONString(env["start_commit_sha"]))
	if !idOK || !mrOK || !hok || !bok || !sok {
		return 0, 0, "", "", "", false
	}
	return id, mrID, head, base, start, true
}

func selectReviewVersion(ctx context.Context, body []byte, hdr http.Header, b reviewBracket) (int64, string) {
	var rows []json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil {
		return 0, "ambiguous"
	}
	var selected int64
	matches := 0
	headDisagree := false
	for _, raw := range rows {
		if err := reviewChargeItem(ctx); err != nil {
			return 0, "budget"
		}
		var env map[string]json.RawMessage
		if err := json.Unmarshal(raw, &env); err != nil {
			return 0, "ambiguous"
		}
		id, mrID, head, base, start, ok := provedVersionRow(env)
		if !ok {
			return 0, "ambiguous"
		}
		full := mrID == b.MRID && head == b.Head && base == b.Base && start == b.Start
		if full {
			matches++
			selected = id
			continue
		}
		if head == b.Head && (base != b.Base || start != b.Start) {
			headDisagree = true
		}
	}
	if !reviewVersionPaging(hdr, nil) {
		return 0, "ambiguous"
	}
	if matches > 1 {
		return 0, "ambiguous"
	}
	if matches == 1 && !headDisagree {
		return selected, "selected"
	}
	if headDisagree {
		return 0, readmeta.CodeInconsistent
	}
	if matches == 0 {
		return 0, "unmatched"
	}
	return 0, "ambiguous"
}

func bracketsEqual(a, b reviewBracket) bool {
	return a.OwnerID == b.OwnerID && a.IID == b.IID && a.MRID == b.MRID &&
		a.SourceID == b.SourceID && a.TargetID == b.TargetID &&
		a.SourceBranch == b.SourceBranch && a.TargetBranch == b.TargetBranch &&
		a.Head == b.Head && a.Base == b.Base && a.Start == b.Start &&
		a.SourceSHA == b.SourceSHA && a.TargetSHA == b.TargetSHA &&
		a.VersionID == b.VersionID && a.VersionID > 0 && a.SourceSHA == a.Head
}

func (rt *reviewRuntime) readApprovals(owner CanonicalProject, iid int64, out *reviewContextItemOut) {
	res, raw, err := readReviewApprovals(rt.ctx, rt.d, owner, iid)
	sec := res.Section
	if sec.RetrievedAt == "" {
		sec = newApprovalsSection(rt.now)
	}
	if err != nil {
		code := reviewClassify(err)
		sec.ContentComplete = readmeta.ContentCompleteUnknown
		sec.Consistency = readmeta.ConsistencyUnknown
		sec.HeadSHA = nil
		sec.NextCursor = nil
		switch code {
		case readmeta.CodeHTTPError:
			sec.AddLimitation(readmeta.CodeHTTPError, "upstream server error")
		case readmeta.CodeInaccessible:
			sec.AddLimitation(readmeta.CodeInaccessible, "authorization denied")
		case readmeta.CodeUnsupported:
			msg := "approval resource unavailable"
			if strings.Contains(err.Error(), "method not allowed") {
				msg = "method not allowed"
			}
			sec.AddLimitation(readmeta.CodeUnsupported, msg)
		}
		dig, digErr := approvalFailureDigest(endpointApprovalState, code, sec.Limitations)
		skeleton := newApprovalReadResult(endpointApprovalState, sec)
		out.Approvals = &skeleton
		out.Sections["approvals"] = sec
		if digErr == nil {
			out.ApprovalDigest = &dig
		}
		if code == readmeta.CodeBudgetRequests || code == readmeta.CodeBudgetBytes || code == readmeta.CodeBudgetElapsed || code == readmeta.CodeBudgetItems || code == readmeta.CodeCancelled {
			out.Cause = code
		}
		return
	}
	dig, digErr := approvalSemanticDigest(res.Endpoint, raw, res.Section.Limitations)
	out.Approvals = &res
	out.Sections["approvals"] = res.Section
	if digErr == nil {
		out.ApprovalDigest = &dig
	}
}

func (rt *reviewRuntime) markSection(out *reviewContextItemOut, name, complete, consistency string) {
	sec := out.Sections[name]
	if sec.CapabilityVersion == "" {
		sec = unsupportedReviewSection(rt.now, name)
		sec.Limitations = []readmeta.Limitation{}
		sec.CapabilityVersion = capabilityReviewContextMetaV1
	}
	sec.ContentComplete = complete
	sec.Consistency = consistency
	sec.HeadSHA = nil
	out.Sections[name] = sec
}

func metadataEvidence(now time.Time, b reviewBracket) (metadataDTO, string, readmeta.Section) {
	dto := metadataDTO{
		ProjectID: b.OwnerID, IID: b.IID, MergeRequestID: b.MRID,
		SourceProjectID: b.SourceID, TargetProjectID: b.TargetID,
		SourceBranch: b.SourceBranch, TargetBranch: b.TargetBranch,
		HeadSHA: b.Head, BaseSHA: b.Base, StartSHA: b.Start,
		SourceSHA: b.SourceSHA, TargetSHA: b.TargetSHA, VersionID: b.VersionID,
	}
	dig := metadataDigest(dto)
	head := b.Head
	sec := readmeta.Section{
		RetrievedAt:         now.UTC().Format(time.RFC3339),
		Source:              readmeta.SourceGitLabREST,
		Provider:            readmeta.ProviderGitLab,
		CapabilityVersion:   capabilityReviewContextMetaV1,
		HeadSHA:             &head,
		PaginationExhausted: true,
		ContentComplete:     readmeta.ContentCompleteTrue,
		Consistency:         readmeta.ConsistencyConsistent,
		Limitations:         []readmeta.Limitation{},
		NextCursor:          nil,
		Counts:              readmeta.Counts{},
		ManifestCoverage:    readmeta.CoverageUnknown,
		PatchCoverage:       readmeta.CoverageUnknown,
	}
	return dto, dig, sec
}

func metadataDigest(dto metadataDTO) string {
	fields := []taggedDigestField{
		{Name: "base_sha", State: "value", Value: dto.BaseSHA},
		{Name: "head_sha", State: "value", Value: dto.HeadSHA},
		{Name: "iid", State: "value", Value: strconv.FormatInt(dto.IID, 10)},
		{Name: "merge_request_id", State: "value", Value: strconv.FormatInt(dto.MergeRequestID, 10)},
		{Name: "owner_project_id", State: "value", Value: strconv.FormatInt(dto.ProjectID, 10)},
		{Name: "source_branch", State: "value", Value: dto.SourceBranch},
		{Name: "source_project_id", State: "value", Value: strconv.FormatInt(dto.SourceProjectID, 10)},
		{Name: "source_sha", State: "value", Value: dto.SourceSHA},
		{Name: "start_sha", State: "value", Value: dto.StartSHA},
		{Name: "target_branch", State: "value", Value: dto.TargetBranch},
		{Name: "target_project_id", State: "value", Value: strconv.FormatInt(dto.TargetProjectID, 10)},
		{Name: "target_sha", State: "value", Value: dto.TargetSHA},
		{Name: "version_id", State: "value", Value: strconv.FormatInt(dto.VersionID, 10)},
	}
	sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
	raw, _ := json.Marshal(fields)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type taggedDigestField struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Value string `json:"value,omitempty"`
}

func metaDigestOf(out reviewContextItemOut) map[string]string {
	dig := map[string]string{}
	if out.Metadata != nil && containsStringLocal(out.Sections, "metadata") {
		dig["metadata"] = out.Metadata.Digest
	}
	if out.Approvals != nil && out.ApprovalDigest != nil && out.Approvals.Section.ContentComplete == readmeta.ContentCompleteTrue && out.Approvals.Section.Consistency == readmeta.ConsistencyConsistent {
		dig["approvals"] = *out.ApprovalDigest
	}
	if out.Discussions != nil && out.Discussions.evidence != "" {
		if sec, ok := out.Sections["discussions"]; ok && sec.ContentComplete == readmeta.ContentCompleteTrue && sec.Consistency == readmeta.ConsistencyConsistent {
			dig["discussions"] = out.Discussions.evidence
		}
	}
	if out.diffManifestDigest != "" {
		if sec, ok := out.Sections["diff_manifest"]; ok && sec.ContentComplete == readmeta.ContentCompleteTrue && sec.Consistency == readmeta.ConsistencyConsistent {
			dig["diff_manifest"] = out.diffManifestDigest
		}
	}
	return dig
}

func containsStringLocal(sections map[string]readmeta.Section, name string) bool {
	_, ok := sections[name]
	return ok
}

func approvalComplete(res *mrApprovalReadResult) bool {
	if res == nil {
		return false
	}
	sec := res.Section
	if sec.ContentComplete != readmeta.ContentCompleteTrue || sec.Consistency != readmeta.ConsistencyConsistent {
		return false
	}
	for _, lim := range sec.Limitations {
		if lim.Code == readmeta.CodeUnsupported || lim.Code == readmeta.CodeHTTPError {
			return false
		}
	}
	return true
}

func expectedMatch(want *string, head string) *bool {
	if want == nil {
		return nil
	}
	ok := *want == head
	return &ok
}

func (rt *reviewRuntime) mint(owner CanonicalProject, item reviewContextItemIn, b reviewBracket, complete, excluded []string, digests map[string]string) (string, error) {
	if len(complete) == 0 {
		return "", fmt.Errorf("%s", readmeta.ConsistencyUnknown)
	}
	retrieved := rt.now.UTC().Format(time.RFC3339)
	fresh := rt.now.UTC().Add(cursor.ReviewWriteFresh).Format(time.RFC3339)
	expires := rt.now.UTC().Add(cursor.DefaultTTL).Format(time.RFC3339)
	if excluded == nil {
		excluded = []string{}
	}
	if digests == nil {
		digests = map[string]string{}
	}
	iid := item.MergeRequestIID
	payload := cursor.Payload{
		SchemaVersion: cursor.SchemaV1,
		Instance:      rt.instance,
		ActorID:       rt.actorID,
		PolicyFP:      rt.policyFP,
		Tool:          cursor.ToolReviewContext,
		Section:       cursor.SectionReviewContext,
		Scope: cursor.Scope{
			Kind:            cursor.ScopeReviewContext,
			ProjectID:       strconv.FormatInt(owner.ID, 10),
			MergeRequestIID: &iid,
		},
		Filters: cursor.Filters{
			Selection: strings.Join(item.Sections, ","),
			Until:     retrieved,
			PerPage:   1,
		},
		ImmutableRefs: nil,
		UpperBound:    retrieved,
		ExpiresAt:     expires,
		ContextRef: &cursor.ContextRef{
			OwnerProjectID:    b.OwnerID,
			SourceProjectID:   b.SourceID,
			TargetProjectID:   b.TargetID,
			SourceBranch:      b.SourceBranch,
			TargetBranch:      b.TargetBranch,
			SourceSHA:         b.SourceSHA,
			TargetSHA:         b.TargetSHA,
			VersionID:         b.VersionID,
			VersionHead:       b.Head,
			VersionBase:       b.Base,
			VersionStart:      b.Start,
			Requested:         item.Sections,
			Complete:          complete,
			Excluded:          excluded,
			Digests:           digests,
			RetrievedAt:       retrieved,
			WriteFreshUntil:   fresh,
			BracketConsistent: true,
		},
	}
	for _, name := range complete {
		if name == "diff_manifest" {
			payload.ContextRef.Evidence = map[string]string{"diff_manifest": cursor.DiffManifestEvidenceV1}
		}
	}
	tok, err := cursor.Encode(rt.d.Config.CursorKey, payload)
	if err != nil {
		return "", fmt.Errorf("%s", cursor.ResyncRequired)
	}
	return tok, nil
}

func jsonInt(raw json.RawMessage) (int64, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func reviewJSONString(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

func causeOf(err error) string {
	return reviewClassify(err)
}
