package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	capabilityReviewQueueV1   = "readmeta.review_queue.v1"
	queueMaxProjects          = 32
	queueMaxSeeds             = 32
	queueMaxCandidates        = 64
	queueReturnedJSONCapBytes = 256 << 10
	queueBitReviewer          = 1
	queueBitOngoing           = 2
	queueBitAuthored          = 4
	errCursorKeyMissingQueue  = "GITLAB_MCP_CURSOR_KEY is required for get_merge_request_review_queue; configure a raw secret of at least 32 bytes"
)

type knownMRSeed struct {
	ProjectID       string `json:"project_id"`
	MergeRequestIID int64  `json:"iid"`
}

type getMergeRequestReviewQueueIn struct {
	GroupID       string        `json:"group_id" jsonschema:"Canonical group id or path"`
	Kinds         []string      `json:"kinds" jsonschema:"Nonempty subset of reviewer, ongoing, authored"`
	ActorID       *int64        `json:"actor_id,omitempty" jsonschema:"Optional positive discovery actor id; omit for authenticated user"`
	States        []string      `json:"states,omitempty" jsonschema:"opened, closed, merged, or all"`
	ProjectIDs    []string      `json:"project_ids,omitempty" jsonschema:"Optional bounded canonical project scope"`
	UpdatedAfter  *string       `json:"updated_after,omitempty"`
	UpdatedBefore *string       `json:"updated_before,omitempty"`
	PageSize      int           `json:"page_size,omitempty" jsonschema:"Default 20, max 50"`
	Cursor        *string       `json:"cursor,omitempty"`
	KnownMRs      []knownMRSeed `json:"known_mrs,omitempty" jsonschema:"Bounded seeds required for ongoing"`
}

type reviewQueueItem struct {
	ProjectID int64    `json:"project_id"`
	IID       int64    `json:"iid"`
	Kinds     []string `json:"kinds"`
	HeadSHA   *string  `json:"head_sha"`
}

func registerMergeRequestReviewQueue(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        cursor.ToolReviewQueue,
		Description: "Build a canonical group merge-request review queue for requested membership kinds",
	}, getMergeRequestReviewQueue)
}

func getMergeRequestReviewQueue(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestReviewQueueIn, d Deps) (*mcp.CallToolResult, any, error) {
	now := d.now()
	section := newReviewQueueSection(now)

	norm, err := normalizeReviewQueueInput(in)
	if err != nil {
		return nil, nil, err
	}

	ctx, budget, release := ensureQueueBudget(ctx)
	defer release()

	if d.Config == nil || len(d.Config.CursorKey) == 0 {
		return nil, nil, fmt.Errorf("%s", errCursorKeyMissingQueue)
	}

	instance, err := cursorInstance(d.Config)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: instance", cursor.ResyncRequired)
	}
	policyFP := d.Config.PolicyFingerprint()

	var resume *cursor.Payload
	if tok := strings.TrimSpace(ptrStr(in.Cursor)); tok != "" {
		p, err := cursor.Decode(d.Config.CursorKey, tok, now)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: cursor validation failed", cursor.ResyncRequired)
		}
		resume = &p
		// Local signature/expiry/structure already enforced by Decode. Compare
		// normalized input + config bindings BEFORE any SDK/identity transport.
		preflightActor := resume.ActorID
		if in.ActorID != nil {
			preflightActor = *in.ActorID
		}
		preflightFilters := buildQueueFilters(norm, preflightActor)
		if err := preflightQueueResumeLocal(*resume, instance, policyFP, preflightFilters, norm); err != nil {
			return nil, nil, err
		}
	}

	authActor, err := resolveCursorActor(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	if resume != nil && authActor != resume.ActorID {
		// Authenticated principal mismatch: resync before group/discovery work.
		return nil, nil, fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}

	discoveryActor := authActor
	if in.ActorID != nil {
		discoveryActor, err = resolveDiscoveryActor(ctx, d, *in.ActorID)
		if err != nil {
			return nil, nil, err
		}
	}

	// Always build filters from CURRENT normalized input first. Do not copy
	// cursor Since/CallerUntil/Until over request filters before MatchBinding.
	filters := buildQueueFilters(norm, discoveryActor)

	group, err := AuthorizeCanonicalGroup(ctx, d, norm.groupID)
	if err != nil {
		return nil, nil, sanitizeQueueErr(err)
	}
	groupID := strconv.FormatInt(group.ID, 10)
	scope := cursor.Scope{Kind: cursor.ScopeGroupQueue, GroupID: groupID}
	upper := ""
	expires := now.Add(cursor.DefaultTTL).UTC().Format(time.RFC3339)

	var qc *cursor.QueueCont
	if resume != nil {
		if err := matchQueueResumeBinding(*resume, instance, authActor, policyFP, scope, filters); err != nil {
			return nil, nil, err
		}
		// After match: signed expiry / pinned window / upper come from the token.
		expires = resume.ExpiresAt
		upper = resume.UpperBound
		filters = resume.Filters
		qc = resume.QueueCont
		if qc == nil {
			return nil, nil, fmt.Errorf("%s: missing queue_cont", cursor.ResyncRequired)
		}
	} else {
		pinUntil(&filters, now, norm.before)
		upper = filters.Until
		qc = newQueueCont(norm)
	}

	st := &queueRuntime{
		d: d, budget: budget, group: group, groupID: groupID,
		authActor: authActor, discoveryActor: discoveryActor,
		norm: norm, filters: filters, section: &section,
		qc: qc, instance: instance, policyFP: policyFP,
		upper: upper, expires: expires, now: now,
	}
	st.reapplyPersistedLimitations()
	// Review-queue discovery is a moving window: never claim snapshot consistency.
	st.movingDiscovery = true
	st.persistLimitation(readmeta.CodeInconsistent, "moving discovery window")

	if qc.Phase == "discover" {
		if !st.qc.Term {
			if err := st.runDiscover(ctx); err != nil && !errors.Is(err, errQueueStop) {
				if isTypedBudget(err) {
					st.noteBudget(err)
				} else if !st.handleProviderFail(err) {
					return nil, nil, sanitizeQueueErr(err)
				}
			}
		}
		// Capacity/ambiguous terminals still emit confirmed successes once, without a next cursor.
		if st.discoverComplete() || st.qc.Term || st.cmFull {
			st.qc.Phase = "emit"
			if st.qc.EI < 0 {
				st.qc.EI = 0
			}
		}
	}

	var items []reviewQueueItem
	if st.qc.Phase == "emit" {
		items, err = st.runEmit(ctx)
		if err != nil && !errors.Is(err, errQueueStop) {
			if isTypedBudget(err) {
				st.noteBudget(err)
			} else {
				return nil, nil, sanitizeQueueErr(err)
			}
		}
	}

	out, err := st.finalize(items)
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(out), nil
}

type normalizedQueue struct {
	groupID    string
	kinds      []string
	states     []string
	projects   []string // canonical tokens as provided, unique sorted
	projectSet map[string]struct{}
	seeds      []knownMRSeed // sorted unique
	seedsFP    string
	after      *time.Time
	before     *time.Time
	pageSize   int
	wantRev    bool
	wantOng    bool
	wantAuth   bool
	order      string
}

func normalizeReviewQueueInput(in getMergeRequestReviewQueueIn) (normalizedQueue, error) {
	var n normalizedQueue
	n.groupID = strings.TrimSpace(in.GroupID)
	if n.groupID == "" {
		return n, fmt.Errorf("group_id is required")
	}
	if in.ActorID != nil && *in.ActorID <= 0 {
		return n, fmt.Errorf("actor_id must be a positive integer")
	}
	kindSet := map[string]struct{}{}
	for _, k := range in.Kinds {
		k = strings.TrimSpace(k)
		switch k {
		case "reviewer":
			n.wantRev = true
		case "ongoing":
			n.wantOng = true
		case "authored":
			n.wantAuth = true
		case "":
			continue
		default:
			return n, fmt.Errorf("kinds must be reviewer, ongoing, or authored")
		}
		kindSet[k] = struct{}{}
	}
	if len(kindSet) == 0 {
		return n, fmt.Errorf("kinds must be a nonempty subset of reviewer, ongoing, authored")
	}
	for _, k := range []string{"authored", "ongoing", "reviewer"} {
		if _, ok := kindSet[k]; ok {
			n.kinds = append(n.kinds, k)
		}
	}

	hasAll := false
	stateSet := map[string]struct{}{}
	for _, s := range in.States {
		s = strings.TrimSpace(s)
		switch s {
		case "all":
			hasAll = true
		case "opened", "closed", "merged":
			stateSet[s] = struct{}{}
		case "":
			continue
		default:
			return n, fmt.Errorf("states must be opened, closed, merged, or all")
		}
	}
	if hasAll && len(stateSet) > 0 {
		return n, fmt.Errorf("states all cannot combine with other states")
	}
	if hasAll {
		n.states = []string{"closed", "merged", "opened"}
	} else if len(stateSet) == 0 {
		n.states = []string{"opened"}
	} else {
		for _, s := range []string{"closed", "merged", "opened"} {
			if _, ok := stateSet[s]; ok {
				n.states = append(n.states, s)
			}
		}
	}

	seenProj := map[string]struct{}{}
	for _, p := range in.ProjectIDs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, ok := seenProj[p]; ok {
			continue
		}
		seenProj[p] = struct{}{}
		n.projects = append(n.projects, p)
	}
	sort.Strings(n.projects)
	if len(n.projects) > queueMaxProjects {
		return n, fmt.Errorf("project_ids exceeds cap %d", queueMaxProjects)
	}
	n.projectSet = seenProj

	seedSeen := map[string]struct{}{}
	for _, s := range in.KnownMRs {
		pid := strings.TrimSpace(s.ProjectID)
		if pid == "" || s.MergeRequestIID < 1 {
			return n, fmt.Errorf("known_mrs entries require project_id and positive iid")
		}
		key := pid + ":" + strconv.FormatInt(s.MergeRequestIID, 10)
		if _, ok := seedSeen[key]; ok {
			continue
		}
		seedSeen[key] = struct{}{}
		n.seeds = append(n.seeds, knownMRSeed{ProjectID: pid, MergeRequestIID: s.MergeRequestIID})
	}
	sort.Slice(n.seeds, func(i, j int) bool {
		if n.seeds[i].ProjectID != n.seeds[j].ProjectID {
			return n.seeds[i].ProjectID < n.seeds[j].ProjectID
		}
		return n.seeds[i].MergeRequestIID < n.seeds[j].MergeRequestIID
	})
	if len(n.seeds) > queueMaxSeeds {
		return n, fmt.Errorf("known_mrs exceeds cap %d", queueMaxSeeds)
	}
	if n.wantOng && len(n.seeds) == 0 {
		// allowed input; discovery reports unsupported
	}
	n.seedsFP = seedsFingerprint(n.seeds)

	after, err := parseListMRTime("updated_after", in.UpdatedAfter)
	if err != nil {
		return n, err
	}
	before, err := parseListMRTime("updated_before", in.UpdatedBefore)
	if err != nil {
		return n, err
	}
	if after != nil && before != nil && after.After(*before) {
		return n, fmt.Errorf("updated_after must be <= updated_before")
	}
	n.after, n.before = after, before

	n.pageSize = in.PageSize
	if n.pageSize < 1 {
		n.pageSize = 20
	}
	if n.pageSize > 50 {
		return n, fmt.Errorf("page_size must be 1..50")
	}
	n.order = "updated_at:desc|tie=project_id,iid"
	return n, nil
}

func seedsFingerprint(seeds []knownMRSeed) string {
	if len(seeds) == 0 {
		return "none"
	}
	h := sha256.New()
	for i, s := range seeds {
		if i > 0 {
			_, _ = h.Write([]byte{'\n'})
		}
		_, _ = h.Write([]byte(s.ProjectID + ":" + strconv.FormatInt(s.MergeRequestIID, 10)))
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func buildQueueFilters(n normalizedQueue, discoveryActor int64) cursor.Filters {
	sel := "kinds=" + strings.Join(n.kinds, ",") +
		"|actor=" + strconv.FormatInt(discoveryActor, 10) +
		"|states=" + strings.Join(n.states, ",") +
		"|projects="
	if len(n.projects) == 0 {
		sel += "*"
	} else {
		sel += strings.Join(n.projects, ",")
	}
	sel += "|seeds=" + n.seedsFP
	var since, callerUntil string
	if n.after != nil {
		since = n.after.UTC().Format(time.RFC3339Nano)
	}
	if n.before != nil {
		callerUntil = n.before.UTC().Format(time.RFC3339Nano)
	}
	until := callerUntil
	// until pinned by caller at first mint; here placeholder filled by caller with min(before, now)
	return cursor.Filters{
		Since:       since,
		CallerUntil: callerUntil,
		Until:       until,
		Order:       n.order,
		Selection:   sel,
		PerPage:     n.pageSize,
	}
}

func pinUntil(filters *cursor.Filters, now time.Time, before *time.Time) {
	u := now.UTC()
	if before != nil && before.Before(u) {
		u = before.UTC()
	}
	filters.Until = u.Format(time.RFC3339)
}

// preflightQueueResumeLocal compares resume bindings that need no SDK transport:
// instance/policy/tool/section/scope kind, normalized selection/date/order/page,
// and strict queue progress. Does not overwrite caller bounds from the token.
// Canonical group alias identity is deferred until after this preflight passes.
func preflightQueueResumeLocal(resume cursor.Payload, instance, policyFP string, reqFilters cursor.Filters, norm normalizedQueue) error {
	if resume.Instance != instance || resume.PolicyFP != policyFP {
		return fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	if resume.Tool != cursor.ToolReviewQueue || resume.Section != cursor.SectionReviewQueue {
		return fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	if resume.Scope.Kind != cursor.ScopeGroupQueue {
		return fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	bound := resume.Filters
	if reqFilters.Since != bound.Since || reqFilters.CallerUntil != bound.CallerUntil {
		return fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	if reqFilters.Selection != bound.Selection || reqFilters.Order != bound.Order || reqFilters.PerPage != bound.PerPage {
		return fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	if bound.Until != resume.UpperBound {
		return fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	if resume.QueueCont == nil {
		return fmt.Errorf("%s: missing queue_cont", cursor.ResyncRequired)
	}
	if err := validateQueueContProgress(resume.QueueCont, norm); err != nil {
		return fmt.Errorf("%s: queue progress", cursor.ResyncRequired)
	}
	return nil
}

// matchQueueResumeBinding checks current normalized selection against the signed
// cursor without overwriting request filters with cursor filters first.
func matchQueueResumeBinding(resume cursor.Payload, instance string, authActor int64, policyFP string, scope cursor.Scope, reqFilters cursor.Filters) error {
	bound := resume.Filters
	if reqFilters.Since != bound.Since || reqFilters.CallerUntil != bound.CallerUntil {
		return fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	if reqFilters.Selection != bound.Selection || reqFilters.Order != bound.Order || reqFilters.PerPage != bound.PerPage {
		return fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	if bound.Until != resume.UpperBound {
		return fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	// MatchBinding compares full Filters including pinned Until from the token.
	expected := bound
	expected.Since = reqFilters.Since
	expected.CallerUntil = reqFilters.CallerUntil
	expected.Selection = reqFilters.Selection
	expected.Order = reqFilters.Order
	expected.PerPage = reqFilters.PerPage
	if err := cursor.MatchBinding(resume, instance, authActor, policyFP, cursor.ToolReviewQueue, cursor.SectionReviewQueue, scope, expected, nil, resume.UpperBound); err != nil {
		return fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	return nil
}

func validateQueueContProgress(qc *cursor.QueueCont, norm normalizedQueue) error {
	if qc == nil || qc.V != cursor.QueueContSchemaRQ2 {
		return errQueueStop
	}
	if len(qc.Kinds) != len(norm.kinds) {
		return errQueueStop
	}
	for i := range qc.Kinds {
		if qc.Kinds[i] != norm.kinds[i] {
			return errQueueStop
		}
	}
	wantBits := 0
	if norm.wantRev {
		wantBits |= queueBitReviewer
	}
	if norm.wantOng {
		wantBits |= queueBitOngoing
	}
	if norm.wantAuth {
		wantBits |= queueBitAuthored
	}
	for _, c := range qc.CM {
		if c.B == 0 || c.B&^wantBits != 0 {
			return errQueueStop
		}
	}
	if qc.KI < 0 || qc.KI > len(qc.KP) {
		return errQueueStop
	}
	if norm.wantOng {
		if qc.OG == nil || qc.OG.SI < 0 || qc.OG.SI > len(norm.seeds) || qc.OG.DP < 1 {
			return errQueueStop
		}
	} else if qc.OG != nil {
		return errQueueStop
	}
	if qc.Phase == "discover" && qc.EI != 0 {
		return errQueueStop
	}
	if qc.EI < 0 || qc.EI > len(qc.CM) {
		return errQueueStop
	}
	return nil
}

func parsePinnedBound(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		u := t.UTC()
		return &u, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, err
	}
	u := t.UTC()
	return &u, nil
}

func (st *queueRuntime) pinnedUpdatedBounds() (after, before *time.Time, err error) {
	after, err = parsePinnedBound(st.filters.Since)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: pinned since", cursor.ResyncRequired)
	}
	before, err = parsePinnedBound(st.filters.Until)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: pinned until", cursor.ResyncRequired)
	}
	return after, before, nil
}

func newQueueCont(n normalizedQueue) *cursor.QueueCont {
	qc := &cursor.QueueCont{
		V:     cursor.QueueContSchemaRQ2,
		Phase: "discover",
		Kinds: append([]string{}, n.kinds...),
		CM:    nil,
		EI:    0,
		KI:    0,
	}
	for _, kind := range n.kinds {
		if kind == "ongoing" {
			continue
		}
		for _, st := range n.states {
			qc.KP = append(qc.KP, cursor.QueueKindProg{
				Kind: kind, State: st, P: 1, N: 0, E: false, CN: 0, PD: "", PSz: n.pageSize,
			})
		}
	}
	if n.wantOng {
		qc.OG = &cursor.QueueOngoingProg{SI: 0, DP: 1, CN: 0, PD: "", PSz: n.pageSize, E: false}
	}
	return qc
}

var errQueueStop = errors.New("queue_stop")

type queueRuntime struct {
	d                Deps
	budget           *igl.Budget
	group            CanonicalGroup
	groupID          string
	authActor        int64
	discoveryActor   int64
	norm             normalizedQueue
	filters          cursor.Filters
	section          *readmeta.Section
	qc               *cursor.QueueCont
	instance         string
	policyFP         string
	upper, expires   string
	now              time.Time
	cmFull           bool
	membershipIncomp bool
	movingDiscovery  bool
	returnedOmit     int
	dateUnknown      bool
}

func (st *queueRuntime) persistLimitation(code, message string) {
	st.section.AddLimitation(code, message)
	st.section.ContentComplete = readmeta.ContentCompleteFalse
	if st.qc == nil {
		return
	}
	for _, c := range st.qc.Lim {
		if c == code {
			return
		}
	}
	st.qc.Lim = append(st.qc.Lim, code)
}

func (st *queueRuntime) reapplyPersistedLimitations() {
	if st.qc == nil {
		return
	}
	for _, code := range st.qc.Lim {
		msg := "resumed limitation"
		switch code {
		case readmeta.CodeDedupeCapacity:
			msg = "canonical candidate map capacity reached"
			st.cmFull = true
		case readmeta.CodeMembershipIncomplete:
			msg = "unobserved memberships beyond capacity"
			st.membershipIncomp = true
		case readmeta.CodeProviderPageAmbiguous:
			msg = "provider page incomplete"
		case readmeta.CodeCursorCapacity:
			msg = "cursor capacity"
		case readmeta.CodeInconsistent:
			msg = "moving discovery window"
			st.movingDiscovery = true
		case readmeta.CodeUnsupported:
			msg = "unsupported selection"
		case readmeta.CodePartial:
			msg = "partial queue result"
		}
		st.section.AddLimitation(code, msg)
	}
	if st.qc.Term {
		st.section.ContentComplete = readmeta.ContentCompleteFalse
	}
}

func (st *queueRuntime) markTerminal(code, message string) {
	st.persistLimitation(code, message)
	if st.qc != nil {
		st.qc.Term = true
	}
}

func (st *queueRuntime) runDiscover(ctx context.Context) error {
	for st.qc.KI < len(st.qc.KP) {
		if err := st.discoverKindStream(ctx, st.qc.KI); err != nil {
			return err
		}
		if st.qc.KP[st.qc.KI].E {
			st.qc.KI++
		} else {
			return errQueueStop
		}
	}
	if st.norm.wantOng {
		if len(st.norm.seeds) == 0 {
			st.persistLimitation(readmeta.CodeUnsupported, "known_mrs required for ongoing")
			if st.qc.OG != nil {
				st.qc.OG.E = true
			}
		} else if err := st.discoverOngoing(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (st *queueRuntime) discoverComplete() bool {
	for _, kp := range st.qc.KP {
		if !kp.E {
			return false
		}
	}
	if st.norm.wantOng {
		if st.qc.OG == nil || !st.qc.OG.E {
			return false
		}
	}
	return true
}

func (st *queueRuntime) discoverKindStream(ctx context.Context, idx int) error {
	kp := &st.qc.KP[idx]
	bit := queueBitReviewer
	if kp.Kind == "authored" {
		bit = queueBitAuthored
	}
	for !kp.E {
		if err := ctx.Err(); err != nil {
			return err
		}
		pageKeys, next, exhausted, err := st.fetchGroupMRPage(ctx, kp)
		if err != nil {
			if isTypedBudget(err) {
				return err
			}
			if st.handleProviderFail(err) {
				st.qc.Term = true
				return errQueueStop
			}
			return err
		}
		keys := make([]string, len(pageKeys))
		for i := range pageKeys {
			keys[i] = pageKeys[i].replayFact()
		}
		// replay guard binds membership/update/head facts, not key alone
		if kp.CN > 0 {
			if len(keys) < kp.CN || prefixDigest(keys[:kp.CN]) != kp.PD {
				st.markTerminal(readmeta.CodeProviderPageAmbiguous, "provider page prefix drift")
				return errQueueStop
			}
		}
		for i := kp.CN; i < len(pageKeys); i++ {
			ent := pageKeys[i]
			if err := st.noteCandidate(ctx, ent, bit); err != nil {
				if errors.Is(err, errQueueStop) || isTypedBudget(err) {
					kp.CN = i
					kp.PD = prefixDigest(keys[:i])
					kp.N = next
					return err
				}
				return err
			}
			kp.CN = i + 1
			kp.PD = prefixDigest(keys[:kp.CN])
		}
		if exhausted {
			kp.E = true
			kp.N = 0
			kp.CN = 0
			kp.PD = ""
			return nil
		}
		if next <= 0 {
			st.markTerminal(readmeta.CodeProviderPageAmbiguous, "paging metadata unavailable")
			return errQueueStop
		}
		kp.P = int(next)
		kp.N = next
		kp.CN = 0
		kp.PD = ""
		// Continue to next provider page within same invocation when budget allows.
	}
	return nil
}

type mrPageEnt struct {
	Key       string
	ProjectID int64
	IID       int64
	Updated   string
	Head      *string
	RawBits   int // unused
}

func (e mrPageEnt) replayFact() string {
	h := ""
	if e.Head != nil {
		h = *e.Head
	}
	return e.Key + "|" + e.Updated + "|" + h
}

func (st *queueRuntime) fetchGroupMRPage(ctx context.Context, kp *cursor.QueueKindProg) ([]mrPageEnt, int64, bool, error) {
	opt := &gitlab.ListGroupMergeRequestsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(kp.P), PerPage: int64(kp.PSz)},
		State:       gitlab.Ptr(kp.State),
		Scope:       gitlab.Ptr("all"),
		OrderBy:     gitlab.Ptr("updated_at"),
		Sort:        gitlab.Ptr("desc"),
	}
	if kp.Kind == "reviewer" {
		opt.ReviewerID = gitlab.ReviewerID(st.discoveryActor)
	} else {
		opt.AuthorID = gitlab.Ptr(st.discoveryActor)
	}
	after, before, err := st.pinnedUpdatedBounds()
	if err != nil {
		return nil, 0, false, err
	}
	var reqOpts []gitlab.RequestOptionFunc
	if after != nil || before != nil {
		reqOpts = append(reqOpts, igl.WithUpdatedBounds(after, before))
	}
	path := fmt.Sprintf("groups/%s/merge_requests", gitlab.PathEscape(st.groupID))
	var ents []mrPageEnt
	resp, err := igl.StreamJSONArrayQueue(ctx, st.d.Client, http.MethodGet, path, opt, func(raw json.RawMessage) error {
		if err := st.budget.AddItem(); err != nil {
			return err
		}
		var mr struct {
			ID        int64   `json:"id"`
			IID       int64   `json:"iid"`
			ProjectID int64   `json:"project_id"`
			UpdatedAt *string `json:"updated_at"`
			SHA       string  `json:"sha"`
		}
		if err := json.Unmarshal(raw, &mr); err != nil || mr.ProjectID < 1 || mr.IID < 1 {
			return fmt.Errorf("%s: malformed merge request element", cursor.ResyncRequired)
		}
		u := ""
		if mr.UpdatedAt != nil {
			u = *mr.UpdatedAt
		}
		var head *string
		if h, ok := readmeta.ObservedHeadSHA(mr.SHA); ok {
			hh := h
			head = &hh
		}
		ents = append(ents, mrPageEnt{
			Key:       strconv.FormatInt(mr.ProjectID, 10) + ":" + strconv.FormatInt(mr.IID, 10),
			ProjectID: mr.ProjectID, IID: mr.IID, Updated: u, Head: head,
		})
		return nil
	}, reqOpts...)
	if err != nil {
		return ents, 0, false, err
	}
	var hdr http.Header
	var sdkNext int64
	if resp != nil {
		sdkNext = resp.NextPage
		if resp.Response != nil {
			hdr = resp.Response.Header
		}
	}
	obs := readmeta.ObservePaging(hdr, sdkNext)
	if obs.ExhaustedObserved {
		return ents, 0, true, nil
	}
	if obs.PagingKnown && sdkNext > 0 {
		return ents, sdkNext, false, nil
	}
	if !obs.PagingKnown {
		// Missing X-Next-Page must never be inferred exhausted (even for small pages).
		return ents, 0, false, fmt.Errorf("%s: paging metadata unavailable", readmeta.CodeProviderPageAmbiguous)
	}
	if sdkNext > 0 {
		return ents, sdkNext, false, nil
	}
	// Header present but unusable / conflicting with SDK next.
	return ents, 0, false, fmt.Errorf("%s: paging metadata unavailable", readmeta.CodeProviderPageAmbiguous)
}

func (st *queueRuntime) noteCandidate(ctx context.Context, ent mrPageEnt, bit int) error {
	if ent.ProjectID < 1 {
		st.persistLimitation(readmeta.CodeIdentityUnresolved, "missing project_id")
		return nil
	}
	if len(st.norm.projectSet) > 0 {
		tok := strconv.FormatInt(ent.ProjectID, 10)
		if _, ok := st.norm.projectSet[tok]; !ok {
			inScope, err := st.projectInScope(ctx, ent.ProjectID)
			if err != nil {
				return err
			}
			if !inScope {
				return nil
			}
		}
	}
	// Discovery stores observed list facts only. Exact MR/source/downstream proof
	// is lazy on emit (shared budget; no unbounded up-front reauth of ≤64 keys).
	if ent.Updated == "" {
		st.dateUnknown = true
		st.persistLimitation(readmeta.CodePartial, "updated_at unknown")
		return nil
	}
	for i := range st.qc.CM {
		if st.qc.CM[i].K == ent.Key {
			st.qc.CM[i].B |= bit
			st.qc.CM[i].U = ent.Updated
			if ent.Head != nil {
				st.qc.CM[i].H = ent.Head
			}
			return nil
		}
	}
	if len(st.qc.CM) >= queueMaxCandidates {
		st.cmFull = true
		st.membershipIncomp = true
		st.markTerminal(readmeta.CodeDedupeCapacity, "canonical candidate map capacity reached")
		st.persistLimitation(readmeta.CodeMembershipIncomplete, "unobserved memberships beyond capacity")
		return errQueueStop
	}
	st.qc.CM = append(st.qc.CM, cursor.QueueCandidate{K: ent.Key, B: bit, U: ent.Updated, H: ent.Head})
	return nil
}

func (st *queueRuntime) projectInScope(ctx context.Context, id int64) (bool, error) {
	for _, p := range st.norm.projects {
		c, err := AuthorizeCanonicalProject(ctx, st.d, p)
		if err != nil {
			if isTypedBudget(err) {
				return false, err
			}
			msg := err.Error()
			if strings.HasPrefix(msg, readmeta.CodeAuthzDenied) || strings.HasPrefix(msg, readmeta.CodeIdentityUnresolved) {
				continue
			}
			return false, err
		}
		if c.ID == id {
			return true, nil
		}
	}
	return false, nil
}

func (st *queueRuntime) authorizeGroupProject(ctx context.Context, projectID string) (CanonicalProject, error) {
	c, err := AuthorizeCanonicalProject(ctx, st.d, projectID)
	if err != nil {
		return CanonicalProject{}, err
	}
	if c.NamespaceKind != "group" {
		return CanonicalProject{}, authzDenied("project namespace not under group")
	}
	ok, err := groupAncestryContains(ctx, st.d, c.NamespaceID, map[int64]struct{}{st.group.ID: {}})
	if err != nil {
		return CanonicalProject{}, err
	}
	if !ok {
		return CanonicalProject{}, authzDenied("project not under authorized group")
	}
	return c, nil
}

// verifyMR resolves narrow MR metadata and authorizes owner + source/downstream
// before any candidate map mutation or emission.
func (st *queueRuntime) verifyMR(ctx context.Context, projectID, iid int64) (seedMRMeta, error) {
	var meta seedMRMeta
	if projectID < 1 || iid < 1 {
		return meta, identityErr("missing project_id")
	}
	if err := st.budget.AddItem(); err != nil {
		return meta, err
	}
	owner, err := st.authorizeGroupProject(ctx, strconv.FormatInt(projectID, 10))
	if err != nil {
		return meta, err
	}
	mr, _, err := st.d.Client.MergeRequests.GetMergeRequest(owner.ID, iid, nil, gitlab.WithContext(ctx))
	if err != nil {
		if isTypedBudget(err) {
			return meta, err
		}
		return meta, identityErr("merge request")
	}
	if mr == nil {
		return meta, identityErr("merge request")
	}
	if mr.IID != iid {
		return meta, identityErr("merge request iid mismatch")
	}
	if mr.ProjectID < 1 {
		return meta, identityErr("missing project_id")
	}
	if mr.ProjectID != owner.ID {
		return meta, identityErr("merge request project_id mismatch")
	}
	stateOK := false
	for _, s := range st.norm.states {
		if mr.State == s {
			stateOK = true
			break
		}
	}
	if !stateOK {
		return meta, fmt.Errorf("%s: seed state", readmeta.CodeUnsupported)
	}
	// Do not fabricate updated_at; nil stays unknown.
	if mr.UpdatedAt != nil {
		meta.updated = mr.UpdatedAt.UTC().Format(time.RFC3339Nano)
		if after, aerr := parsePinnedBound(st.filters.Since); aerr == nil && after != nil && mr.UpdatedAt.Before(*after) {
			return meta, fmt.Errorf("%s: date window", readmeta.CodeUnsupported)
		}
		if before, berr := parsePinnedBound(st.filters.Until); berr == nil && before != nil && mr.UpdatedAt.After(*before) {
			return meta, fmt.Errorf("%s: date window", readmeta.CodeUnsupported)
		}
	}
	if h, ok := readmeta.ObservedHeadSHA(mr.SHA); ok {
		hh := h
		meta.head = &hh
	}
	if mr.SourceProjectID <= 0 {
		return meta, identityErr("unproven merge request source project")
	}
	meta.source = mr.SourceProjectID
	var extra []string
	if mr.SourceProjectID != owner.ID {
		extra = append(extra, strconv.FormatInt(mr.SourceProjectID, 10))
	}
	if mr.ProjectID > 0 && mr.ProjectID != owner.ID {
		meta.down = mr.ProjectID
		extra = append(extra, strconv.FormatInt(mr.ProjectID, 10))
	}
	if len(extra) > 0 {
		if _, err := AuthorizeAdditionalProjects(ctx, st.d, extra...); err != nil {
			if isTypedBudget(err) {
				return meta, err
			}
			return meta, err
		}
	}
	return meta, nil
}

func (st *queueRuntime) discoverOngoing(ctx context.Context) error {
	og := st.qc.OG
	if og == nil {
		return nil
	}
	for og.SI < len(st.norm.seeds) {
		// Critical: reset exhaustion so each seed is scanned independently.
		og.E = false
		if og.DP < 1 {
			og.DP = 1
		}
		seed := st.norm.seeds[og.SI]
		owner, mrMeta, err := st.loadSeedMR(ctx, seed)
		if err != nil {
			msg := err.Error()
			if strings.HasPrefix(msg, readmeta.CodeUnsupported) || strings.HasPrefix(msg, readmeta.CodeIdentityUnresolved) || strings.HasPrefix(msg, readmeta.CodeAuthzDenied) {
				st.persistLimitation(strings.Split(msg, ":")[0], "seed rejected")
				og.SI++
				og.DP = 1
				og.CN = 0
				og.PD = ""
				og.E = false
				continue
			}
			if isTypedBudget(err) {
				return err
			}
			return err
		}
		if mrMeta.updated == "" {
			st.dateUnknown = true
			st.persistLimitation(readmeta.CodePartial, "updated_at unknown")
			og.SI++
			og.DP = 1
			og.CN = 0
			og.PD = ""
			og.E = false
			continue
		}
		qual, err := st.scanSeedParticipation(ctx, owner, mrMeta, og)
		if err != nil {
			return err
		}
		if qual {
			key := strconv.FormatInt(owner.ID, 10) + ":" + strconv.FormatInt(seed.MergeRequestIID, 10)
			ent := mrPageEnt{Key: key, ProjectID: owner.ID, IID: seed.MergeRequestIID, Updated: mrMeta.updated, Head: mrMeta.head}
			// Candidate already verified via loadSeedMR; OR ongoing bit without re-Get when facts known.
			if err := st.noteOngoingCandidate(ent); err != nil {
				return err
			}
		}
		og.SI++
		og.DP = 1
		og.CN = 0
		og.PD = ""
		og.E = false
	}
	og.E = true
	return nil
}

func (st *queueRuntime) noteOngoingCandidate(ent mrPageEnt) error {
	if ent.Updated == "" {
		st.dateUnknown = true
		st.persistLimitation(readmeta.CodePartial, "updated_at unknown")
		return nil
	}
	for i := range st.qc.CM {
		if st.qc.CM[i].K == ent.Key {
			st.qc.CM[i].B |= queueBitOngoing
			st.qc.CM[i].U = ent.Updated
			if ent.Head != nil {
				st.qc.CM[i].H = ent.Head
			}
			return nil
		}
	}
	if len(st.qc.CM) >= queueMaxCandidates {
		st.cmFull = true
		st.membershipIncomp = true
		st.markTerminal(readmeta.CodeDedupeCapacity, "canonical candidate map capacity reached")
		st.persistLimitation(readmeta.CodeMembershipIncomplete, "unobserved memberships beyond capacity")
		return errQueueStop
	}
	st.qc.CM = append(st.qc.CM, cursor.QueueCandidate{K: ent.Key, B: queueBitOngoing, U: ent.Updated, H: ent.Head})
	return nil
}

type seedMRMeta struct {
	updated string
	head    *string
	source  int64
	down    int64
}

func (st *queueRuntime) loadSeedMR(ctx context.Context, seed knownMRSeed) (CanonicalProject, seedMRMeta, error) {
	var meta seedMRMeta
	if strings.TrimSpace(seed.ProjectID) == "" || seed.MergeRequestIID < 1 {
		return CanonicalProject{}, meta, identityErr("seed project_id/iid")
	}
	owner, err := st.authorizeGroupProject(ctx, seed.ProjectID)
	if err != nil {
		return CanonicalProject{}, meta, err
	}
	if err := st.budget.AddItem(); err != nil {
		return CanonicalProject{}, meta, err
	}
	mr, _, err := st.d.Client.MergeRequests.GetMergeRequest(owner.ID, seed.MergeRequestIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		if isTypedBudget(err) {
			return CanonicalProject{}, meta, err
		}
		return CanonicalProject{}, meta, identityErr("seed merge request")
	}
	if mr == nil {
		return CanonicalProject{}, meta, identityErr("seed merge request")
	}
	if mr.IID != seed.MergeRequestIID {
		return CanonicalProject{}, meta, identityErr("seed project_id/iid mismatch")
	}
	if mr.ProjectID < 1 {
		return CanonicalProject{}, meta, identityErr("missing project_id")
	}
	if mr.ProjectID != owner.ID {
		return CanonicalProject{}, meta, identityErr("seed project_id/iid mismatch")
	}
	stateOK := false
	for _, s := range st.norm.states {
		if mr.State == s {
			stateOK = true
			break
		}
	}
	if !stateOK {
		return CanonicalProject{}, meta, fmt.Errorf("%s: seed state", readmeta.CodeUnsupported)
	}
	// Never fabricate SeedUpdatedAt as now.
	if mr.UpdatedAt != nil {
		meta.updated = mr.UpdatedAt.UTC().Format(time.RFC3339Nano)
		if after, aerr := parsePinnedBound(st.filters.Since); aerr == nil && after != nil && mr.UpdatedAt.Before(*after) {
			return CanonicalProject{}, meta, fmt.Errorf("%s: seed date", readmeta.CodeUnsupported)
		}
		if before, berr := parsePinnedBound(st.filters.Until); berr == nil && before != nil && mr.UpdatedAt.After(*before) {
			return CanonicalProject{}, meta, fmt.Errorf("%s: seed date", readmeta.CodeUnsupported)
		}
	}
	if h, ok := readmeta.ObservedHeadSHA(mr.SHA); ok {
		hh := h
		meta.head = &hh
	}
	if mr.SourceProjectID <= 0 {
		return CanonicalProject{}, meta, identityErr("unproven merge request source project")
	}
	meta.source = mr.SourceProjectID
	var extra []string
	if mr.SourceProjectID != owner.ID {
		extra = append(extra, strconv.FormatInt(mr.SourceProjectID, 10))
	}
	if mr.ProjectID > 0 && mr.ProjectID != owner.ID {
		meta.down = mr.ProjectID
		extra = append(extra, strconv.FormatInt(mr.ProjectID, 10))
	}
	if len(extra) > 0 {
		if _, err := AuthorizeAdditionalProjects(ctx, st.d, extra...); err != nil {
			return CanonicalProject{}, meta, err
		}
	}
	return owner, meta, nil
}

func (st *queueRuntime) scanSeedParticipation(ctx context.Context, owner CanonicalProject, meta seedMRMeta, og *cursor.QueueOngoingProg) (bool, error) {
	iid := mustIIDFromOG(st, og)
	key := strconv.FormatInt(owner.ID, 10) + ":" + strconv.FormatInt(iid, 10)
	// Resume-safe: qualification may already live in cm from a prior page/budget stop.
	qualified := st.cmHasBit(key, queueBitOngoing)
	for !og.E {
		opt := &gitlab.ListMergeRequestDiscussionsOptions{
			ListOptions: gitlab.ListOptions{Page: int64(og.DP), PerPage: int64(og.PSz)},
		}
		path := fmt.Sprintf("projects/%s/merge_requests/%d/discussions", gitlab.PathEscape(strconv.FormatInt(owner.ID, 10)), iid)
		type discEnt struct {
			fact  string
			notes []json.RawMessage
		}
		var page []discEnt
		resp, err := igl.StreamJSONArrayQueue(ctx, st.d.Client, http.MethodGet, path, opt, func(raw json.RawMessage) error {
			if err := st.budget.AddItem(); err != nil {
				return err
			}
			var d struct {
				ID    string            `json:"id"`
				Notes []json.RawMessage `json:"notes"`
			}
			if err := json.Unmarshal(raw, &d); err != nil || d.ID == "" {
				return fmt.Errorf("%s: malformed discussion", cursor.ResyncRequired)
			}
			page = append(page, discEnt{fact: discussionReplayFact(d.ID, d.Notes, st.discoveryActor), notes: d.Notes})
			return nil
		})
		if err != nil {
			if isTypedBudget(err) {
				return qualified, err
			}
			return qualified, err
		}
		facts := make([]string, len(page))
		for i := range page {
			facts[i] = page[i].fact
		}
		if og.CN > 0 {
			if len(facts) < og.CN || prefixDigest(facts[:og.CN]) != og.PD {
				st.markTerminal(readmeta.CodeProviderPageAmbiguous, "discussion page prefix drift")
				return qualified, errQueueStop
			}
		}
		for i := og.CN; i < len(page); i++ {
			for _, nraw := range page[i].notes {
				if err := st.budget.AddItem(); err != nil {
					og.CN = i
					og.PD = prefixDigest(facts[:i])
					return qualified, err
				}
				if noteQualifiesOngoingRaw(nraw, st.discoveryActor) {
					qualified = true
					// Persist immediately so budget/page continuation cannot lose qualification.
					ent := mrPageEnt{Key: key, ProjectID: owner.ID, IID: iid, Updated: meta.updated, Head: meta.head}
					if err := st.noteOngoingCandidate(ent); err != nil {
						og.CN = i
						og.PD = prefixDigest(facts[:i])
						return qualified, err
					}
				}
			}
			og.CN = i + 1
			og.PD = prefixDigest(facts[:og.CN])
		}
		var hdr http.Header
		var sdkNext int64
		if resp != nil {
			sdkNext = resp.NextPage
			if resp.Response != nil {
				hdr = resp.Response.Header
			}
		}
		obs := readmeta.ObservePaging(hdr, sdkNext)
		if obs.ExhaustedObserved {
			og.E = true
			og.CN = 0
			og.PD = ""
			break
		}
		if !obs.PagingKnown || sdkNext <= 0 {
			st.markTerminal(readmeta.CodeProviderPageAmbiguous, "discussion paging metadata unavailable")
			return qualified, errQueueStop
		}
		og.DP = int(sdkNext)
		og.CN = 0
		og.PD = ""
	}
	return qualified, nil
}

func (st *queueRuntime) cmHasBit(key string, bit int) bool {
	for i := range st.qc.CM {
		if st.qc.CM[i].K == key && st.qc.CM[i].B&bit != 0 {
			return true
		}
	}
	return false
}

func discussionReplayFact(id string, notes []json.RawMessage, actor int64) string {
	parts := make([]string, 0, len(notes)+1)
	parts = append(parts, id)
	for _, n := range notes {
		parts = append(parts, noteReplayFact(n, actor))
	}
	return strings.Join(parts, ";")
}

func noteReplayFact(raw json.RawMessage, actor int64) string {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "bad"
	}
	sys := "miss"
	if sysRaw, ok := m["system"]; ok {
		var s bool
		if json.Unmarshal(sysRaw, &s) == nil {
			if s {
				sys = "1"
			} else {
				sys = "0"
			}
		} else {
			sys = "unk"
		}
	}
	bodyBlank := "1"
	if b, ok := m["body"]; ok {
		var body string
		if json.Unmarshal(b, &body) == nil && strings.TrimSpace(body) != "" {
			bodyBlank = "0"
		}
	}
	aid := int64(0)
	if a, ok := m["author"]; ok {
		var author struct {
			ID int64 `json:"id"`
		}
		if json.Unmarshal(a, &author) == nil {
			aid = author.ID
		}
	}
	q := "0"
	if noteQualifiesOngoingRaw(raw, actor) {
		q = "1"
	}
	nid := "0"
	if idRaw, ok := m["id"]; ok {
		var id int64
		if json.Unmarshal(idRaw, &id) == nil {
			nid = strconv.FormatInt(id, 10)
		}
	}
	return nid + "|s=" + sys + "|a=" + strconv.FormatInt(aid, 10) + "|b=" + bodyBlank + "|q=" + q
}

func mustIIDFromOG(st *queueRuntime, og *cursor.QueueOngoingProg) int64 {
	if og.SI >= 0 && og.SI < len(st.norm.seeds) {
		return st.norm.seeds[og.SI].MergeRequestIID
	}
	return 0
}

// noteQualifiesOngoingRaw enforces PRESENT system==false via JSON (nil/unknown/null reject).
func noteQualifiesOngoingRaw(raw json.RawMessage, actor int64) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	sysRaw, ok := m["system"]
	if !ok {
		return false
	}
	trimmed := strings.TrimSpace(string(sysRaw))
	if trimmed == "" || trimmed == "null" {
		return false
	}
	var sys bool
	if json.Unmarshal(sysRaw, &sys) != nil || sys {
		return false
	}
	var body string
	b, ok := m["body"]
	if !ok {
		return false
	}
	if json.Unmarshal(b, &body) != nil {
		return false
	}
	if strings.TrimSpace(body) == "" {
		return false
	}
	var author struct {
		ID int64 `json:"id"`
	}
	a, ok := m["author"]
	if !ok || json.Unmarshal(a, &author) != nil {
		return false
	}
	return author.ID > 0 && author.ID == actor
}

func (st *queueRuntime) runEmit(ctx context.Context) ([]reviewQueueItem, error) {
	sorted := append([]cursor.QueueCandidate{}, st.qc.CM...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, errI := time.Parse(time.RFC3339Nano, sorted[i].U)
		if errI != nil {
			ti, _ = time.Parse(time.RFC3339, sorted[i].U)
		}
		tj, errJ := time.Parse(time.RFC3339Nano, sorted[j].U)
		if errJ != nil {
			tj, _ = time.Parse(time.RFC3339, sorted[j].U)
		}
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		pi, ii := splitKey(sorted[i].K)
		pj, ij := splitKey(sorted[j].K)
		if pi != pj {
			return pi < pj
		}
		return ii < ij
	})
	// Keep qc.CM in sorted order for stable ei across resumes once emit starts.
	st.qc.CM = sorted

	var items []reviewQueueItem
	for st.qc.EI < len(st.qc.CM) && len(items) < st.norm.pageSize {
		c := st.qc.CM[st.qc.EI]
		pid, iid := splitKey(c.K)
		if pid < 1 || iid < 1 {
			st.persistLimitation(readmeta.CodeIdentityUnresolved, "missing project_id")
			st.qc.EI++
			continue
		}
		meta, err := st.verifyMR(ctx, pid, iid)
		if err != nil {
			if isTypedBudget(err) {
				// Preserve unprocessed progress: do not advance EI for this candidate.
				return items, err
			}
			msg := err.Error()
			if strings.HasPrefix(msg, readmeta.CodeAuthzDenied) || strings.HasPrefix(msg, readmeta.CodeIdentityUnresolved) || strings.HasPrefix(msg, readmeta.CodeUnsupported) {
				st.persistLimitation(strings.Split(msg, ":")[0], "emit candidate denied")
				st.qc.EI++
				continue
			}
			return items, err
		}
		if meta.updated == "" {
			st.dateUnknown = true
			st.persistLimitation(readmeta.CodePartial, "updated_at unknown")
			st.qc.EI++
			continue
		}
		head := c.H
		if meta.head != nil {
			head = meta.head
		}
		items = append(items, reviewQueueItem{
			ProjectID: pid,
			IID:       iid,
			Kinds:     bitsToKinds(c.B, st.norm),
			HeadSHA:   head,
		})
		st.qc.EI++
		raw, _ := json.Marshal(items)
		if len(raw) > queueReturnedJSONCapBytes {
			items = items[:len(items)-1]
			st.qc.EI--
			st.markTerminal(readmeta.CodeCursorCapacity, "returned projection byte cap")
			st.returnedOmit = len(st.qc.CM) - st.qc.EI
			break
		}
	}
	return items, nil
}

func bitsToKinds(b int, n normalizedQueue) []string {
	var out []string
	if n.wantAuth && b&queueBitAuthored != 0 {
		out = append(out, "authored")
	}
	if n.wantOng && b&queueBitOngoing != 0 {
		out = append(out, "ongoing")
	}
	if n.wantRev && b&queueBitReviewer != 0 {
		out = append(out, "reviewer")
	}
	return out
}

func splitKey(k string) (int64, int64) {
	parts := strings.Split(k, ":")
	if len(parts) != 2 {
		return 0, 0
	}
	a, _ := strconv.ParseInt(parts[0], 10, 64)
	b, _ := strconv.ParseInt(parts[1], 10, 64)
	return a, b
}

func (st *queueRuntime) finalize(items []reviewQueueItem) (map[string]any, error) {
	// Moving queue: always unknown consistency + limitation; never snapshot-consistent.
	st.movingDiscovery = true
	st.persistLimitation(readmeta.CodeInconsistent, "moving discovery window")
	st.section.Consistency = readmeta.ConsistencyUnknown
	st.section.ContentComplete = readmeta.ContentCompleteFalse

	var next *string
	terminal := st.qc != nil && (st.qc.Term || st.cmFull || st.membershipIncomp)
	drainedEmit := st.qc.Phase == "emit" && st.qc.EI >= len(st.qc.CM)

	switch {
	case terminal:
		// Capacity / ambiguous / encoding overflow: preserve partial successes, no cursor.
		next = nil
	case st.qc.Phase == "discover":
		tok, err := st.mintCursor()
		if err != nil {
			st.markTerminal(readmeta.CodeCursorCapacity, "cursor encoding overflow")
			next = nil
		} else {
			next = &tok
		}
	case !drainedEmit:
		tok, err := st.mintCursor()
		if err != nil {
			st.markTerminal(readmeta.CodeCursorCapacity, "cursor encoding overflow")
			next = nil
		} else {
			next = &tok
		}
	default:
		// Emit drained after discovery. Moving queue: no exhaustion=>complete inference.
		next = nil
	}

	st.section.PaginationExhausted = false
	st.section.NextCursor = next
	nItems := len(items)
	st.section.Counts.Items = &nItems

	return map[string]any{
		"items":   items,
		"section": st.section,
	}, nil
}

func (st *queueRuntime) mintCursor() (string, error) {
	if st.qc != nil && st.qc.Term {
		return "", fmt.Errorf("%s: terminal", readmeta.CodeCursorCapacity)
	}
	p := cursor.Payload{
		SchemaVersion: cursor.SchemaV1,
		Instance:      st.instance,
		ActorID:       st.authActor,
		PolicyFP:      st.policyFP,
		Tool:          cursor.ToolReviewQueue,
		Section:       cursor.SectionReviewQueue,
		Scope:         cursor.Scope{Kind: cursor.ScopeGroupQueue, GroupID: st.groupID},
		Filters:       st.filters,
		ImmutableRefs: nil,
		UpperBound:    st.upper,
		ExpiresAt:     st.expires,
		PageState:     cursor.PageState{},
		QueueCont:     st.qc,
	}
	return cursor.Encode(st.d.Config.CursorKey, p)
}

func (st *queueRuntime) noteBudget(err error) {
	switch {
	case errors.Is(err, igl.ErrBudgetItems):
		st.persistLimitation(readmeta.CodeBudgetItems, "item budget exhausted")
	case errors.Is(err, igl.ErrBudgetBytes):
		st.persistLimitation(readmeta.CodeBudgetBytes, "byte budget exhausted")
	case errors.Is(err, igl.ErrBudgetRequests):
		st.persistLimitation(readmeta.CodeBudgetRequests, "request budget exhausted")
	case errors.Is(err, igl.ErrBudgetElapsed):
		st.persistLimitation(readmeta.CodeBudgetElapsed, "elapsed budget exhausted")
	}
}

func (st *queueRuntime) handleProviderFail(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, readmeta.CodeProviderPageAmbiguous) || strings.Contains(msg, cursor.ResyncRequired) {
		st.markTerminal(readmeta.CodeProviderPageAmbiguous, "provider page incomplete")
		return true
	}
	return false
}

func ensureQueueBudget(ctx context.Context) (context.Context, *igl.Budget, func()) {
	const localMax = 30 * time.Second
	var (
		budget *igl.Budget
		owned  bool
	)
	if b := igl.BudgetFromContext(ctx); b != nil {
		budget = b
		budget.CapLimits(igl.DefaultMaxItems, igl.DefaultMaxBytes, igl.DefaultMaxRequests)
	} else {
		budget = igl.DefaultBudget()
		ctx = igl.WithBudget(ctx, budget)
		owned = true
	}
	deadline := time.Now().Add(localMax)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dctx, dcancel := context.WithDeadline(ctx, deadline)
	release := func() {
		dcancel()
		if owned {
			budget.Cancel()
		}
	}
	return dctx, budget, release
}

func newReviewQueueSection(now time.Time) readmeta.Section {
	return readmeta.Section{
		RetrievedAt:         now.UTC().Format(time.RFC3339),
		Source:              readmeta.SourceGitLabREST,
		Provider:            readmeta.ProviderGitLab,
		CapabilityVersion:   capabilityReviewQueueV1,
		HeadSHA:             nil,
		PaginationExhausted: false,
		ContentComplete:     readmeta.ContentCompleteUnknown,
		Consistency:         readmeta.ConsistencyUnknown,
		Limitations:         []readmeta.Limitation{},
		NextCursor:          nil,
		Counts:              readmeta.Counts{},
		ManifestCoverage:    readmeta.CoverageUnknown,
		PatchCoverage:       readmeta.CoverageUnknown,
	}
}

func resolveDiscoveryActor(ctx context.Context, d Deps, id int64) (int64, error) {
	if id < 1 {
		return 0, identityErr("discovery actor")
	}
	u, _, err := d.Client.Users.GetUser(id, nil, gitlab.WithContext(ctx))
	if err != nil || u == nil || u.ID != id {
		return 0, identityErr("discovery actor")
	}
	return u.ID, nil
}

func sanitizeQueueErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, readmeta.CodeAuthzDenied):
		return fmt.Errorf("%s: denied", readmeta.CodeAuthzDenied)
	case strings.HasPrefix(msg, readmeta.CodeIdentityUnresolved):
		return fmt.Errorf("%s: unresolved", readmeta.CodeIdentityUnresolved)
	case strings.HasPrefix(msg, readmeta.CodeUnsupported):
		return fmt.Errorf("%s: unsupported", readmeta.CodeUnsupported)
	case strings.HasPrefix(msg, readmeta.CodeBudgetItems):
		return fmt.Errorf("%s", readmeta.CodeBudgetItems)
	case strings.HasPrefix(msg, readmeta.CodeBudgetBytes):
		return fmt.Errorf("%s", readmeta.CodeBudgetBytes)
	case strings.HasPrefix(msg, readmeta.CodeBudgetRequests):
		return fmt.Errorf("%s", readmeta.CodeBudgetRequests)
	case strings.HasPrefix(msg, readmeta.CodeBudgetElapsed):
		return fmt.Errorf("%s", readmeta.CodeBudgetElapsed)
	case strings.HasPrefix(msg, cursor.ResyncRequired):
		return fmt.Errorf("%s", cursor.ResyncRequired)
	case strings.HasPrefix(msg, readmeta.CodeProviderPageAmbiguous):
		return fmt.Errorf("%s", readmeta.CodeProviderPageAmbiguous)
	case strings.HasPrefix(msg, readmeta.CodeHTTPError):
		return fmt.Errorf("%s: request failed", readmeta.CodeHTTPError)
	case strings.HasPrefix(msg, "GITLAB_MCP_CURSOR_KEY"):
		return fmt.Errorf("%s", errCursorKeyMissingQueue)
	default:
		// Never echo provider/SDK details.
		return fmt.Errorf("%s: request failed", readmeta.CodeHTTPError)
	}
}

func isTypedBudget(err error) bool {
	return errors.Is(err, igl.ErrBudgetItems) || errors.Is(err, igl.ErrBudgetBytes) ||
		errors.Is(err, igl.ErrBudgetElapsed) || errors.Is(err, igl.ErrBudgetRequests)
}

func prefixDigest(keys []string) string {
	return cursor.SequenceDigest(keys)
}

func ptrStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
