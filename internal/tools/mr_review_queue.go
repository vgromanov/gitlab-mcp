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
	if in.Cursor != nil {
		rawCursor := *in.Cursor
		// Omitted cursor is nil or empty. Whitespace-only and padded tokens are malformed,
		// not a page-one fallback.
		if rawCursor != "" && strings.TrimSpace(rawCursor) != rawCursor {
			return nil, nil, fmt.Errorf("%s: malformed cursor", cursor.ResyncRequired)
		}
		if rawCursor != "" {
			p, err := cursor.Decode(d.Config.CursorKey, rawCursor, now)
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
	}

	authActor, err := queueResolvePrincipal(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	if resume != nil && authActor != resume.ActorID {
		// Authenticated principal mismatch: resync before group/discovery work.
		return nil, nil, fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}

	discoveryActor := authActor
	if in.ActorID != nil {
		discoveryActor, err = queueResolveDiscoveryActor(ctx, d, *in.ActorID)
		if err != nil {
			return nil, nil, err
		}
	}

	// Always build filters from CURRENT normalized input first. Do not copy
	// cursor Since/CallerUntil/Until over request filters before MatchBinding.
	filters := buildQueueFilters(norm, discoveryActor)

	group, err := queueAuthorizeCanonicalGroup(ctx, d, norm.groupID)
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
		upper: upper, expires: expires, now: now, ctx: ctx,
		seedlessOngoing: norm.wantOng && len(norm.seeds) == 0,
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
				} else if errors.Is(err, errQueueCancelled) {
					st.persistLimitation(readmeta.CodeCancelled, "invocation cancelled")
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
			} else if errors.Is(err, errQueueCancelled) {
				st.persistLimitation(readmeta.CodeCancelled, "invocation cancelled")
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

	if in.PageSize < 0 {
		return n, fmt.Errorf("page_size must be 1..50")
	}
	n.pageSize = in.PageSize
	if n.pageSize == 0 {
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
	filters.Until = u.Format(time.RFC3339Nano)
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
	want := expectedQueueStreams(norm)
	if len(qc.KP) != len(want) {
		return errQueueStop
	}
	for i := range want {
		if qc.KP[i].Kind != want[i].Kind || qc.KP[i].State != want[i].State {
			return errQueueStop
		}
		if qc.KP[i].E && (qc.KP[i].CN != 0 || qc.KP[i].PD != "" || qc.KP[i].N != 0) {
			return errQueueStop
		}
	}
	for i := 0; i < qc.KI && i < len(qc.KP); i++ {
		if !qc.KP[i].E {
			return errQueueStop
		}
	}
	if qc.Phase == "emit" {
		if qc.KI != len(qc.KP) {
			return errQueueStop
		}
		for _, kp := range qc.KP {
			if !kp.E {
				return errQueueStop
			}
		}
		if norm.wantOng && (qc.OG == nil || !qc.OG.E) {
			return errQueueStop
		}
	}
	if norm.wantOng && qc.OG != nil && qc.OG.E && len(norm.seeds) > 0 && qc.OG.SI != len(norm.seeds) {
		return errQueueStop
	}
	if norm.wantOng && qc.OG != nil && qc.OG.E && (qc.OG.CN != 0 || qc.OG.PD != "") {
		return errQueueStop
	}
	seenLim := map[string]struct{}{}
	for _, code := range qc.Lim {
		if _, dup := seenLim[code]; dup {
			return errQueueStop
		}
		seenLim[code] = struct{}{}
	}
	return nil
}

func expectedQueueStreams(n normalizedQueue) []cursor.QueueKindProg {
	var kp []cursor.QueueKindProg
	for _, kind := range n.kinds {
		if kind == "ongoing" {
			continue
		}
		for _, state := range n.states {
			kp = append(kp, cursor.QueueKindProg{Kind: kind, State: state})
		}
	}
	return kp
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
	d                    Deps
	budget               *igl.Budget
	group                CanonicalGroup
	groupID              string
	authActor            int64
	discoveryActor       int64
	norm                 normalizedQueue
	filters              cursor.Filters
	section              *readmeta.Section
	qc                   *cursor.QueueCont
	instance             string
	policyFP             string
	upper, expires       string
	now                  time.Time
	cmFull               bool
	membershipIncomp     bool
	movingDiscovery      bool
	returnedOmit         int
	dateUnknown          bool
	outputLimitBytes     int
	selectionCache       map[string]int64
	selectionIDs         map[int64]struct{}
	selectionReady       bool
	allowedProjects      map[int64]struct{}
	allowedGroups        map[int64]struct{}
	unobservedMembership *int
	seedlessOngoing      bool
	ctx                  context.Context
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

func (st *queueRuntime) invalidPage(ambiguous bool, err error) bool {
	return ambiguous || queuePageFraming(err)
}

// terminalKnownInvalidPage closes a provider page whose framing or paging was
// already invalid. Proved candidates stay; the cursor does not.
func (st *queueRuntime) terminalKnownInvalidPage(cause error) {
	st.markTerminal(readmeta.CodeProviderPageAmbiguous, "provider page incomplete")
	st.persistLimitation(readmeta.CodePartial, "decoded provider prefix retained")
	st.persistLimitation(readmeta.CodeMembershipIncomplete, "invalid provider page omitted unseen memberships")
	if isTypedBudget(cause) {
		st.noteBudget(cause)
	} else if errors.Is(cause, errQueueCancelled) {
		st.persistLimitation(readmeta.CodeCancelled, "invocation cancelled")
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
			st.seedlessOngoing = true
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
		if err := queueContextStop(ctx); err != nil {
			return err
		}
		page, err := st.fetchGroupMRPage(ctx, kp)
		if err != nil && len(page.ents) == 0 && isTypedBudget(err) {
			return err
		}
		if err != nil && len(page.ents) == 0 && !queuePageFraming(err) && !page.ambiguous {
			return err
		}
		keys := make([]string, len(page.ents))
		for i := range page.ents {
			keys[i] = page.ents[i].replayFact()
		}
		// Consumed-prefix guard runs before any resumed entry is accepted.
		if kp.CN > 0 {
			if len(keys) < kp.CN || prefixDigest(keys[:kp.CN]) != kp.PD {
				st.markTerminal(readmeta.CodeProviderPageAmbiguous, "provider page prefix drift")
				return errQueueStop
			}
		}
		for i := kp.CN; i < len(page.ents); i++ {
			ent := page.ents[i]
			if nerr := st.noteCandidate(ctx, ent, bit); nerr != nil {
				if errors.Is(nerr, errQueueStop) || isTypedBudget(nerr) || errors.Is(nerr, errQueueCancelled) {
					// Ambiguity is already known. A later proof stop must not
					// mint a continuation for this page.
					if st.invalidPage(page.ambiguous, err) {
						st.terminalKnownInvalidPage(nerr)
						return errQueueStop
					}
					kp.CN = i
					if i == 0 {
						kp.PD = ""
					} else {
						kp.PD = prefixDigest(keys[:i])
					}
					return nerr
				}
				return nerr
			}
			kp.CN = i + 1
			kp.PD = prefixDigest(keys[:kp.CN])
			if st.qc.Term {
				return errQueueStop
			}
			// Leave later provider entries as a progress index until they are proved.
			// Their key/update/head/membership facts stay out of the returned cursor.
			// An ambiguous or truncated page cannot be resumed: keep the proved prefix
			// and terminalize instead of minting a continuation.
			if len(st.qc.CM)-st.qc.EI >= st.norm.pageSize && i+1 < len(page.ents) {
				if st.invalidPage(page.ambiguous, err) {
					st.terminalKnownInvalidPage(nil)
					return errQueueStop
				}
				if isTypedBudget(err) || errors.Is(err, errQueueCancelled) {
					return err
				}
				return errQueueStop
			}
		}
		if isTypedBudget(err) || errors.Is(err, errQueueCancelled) {
			if st.invalidPage(page.ambiguous, err) {
				st.terminalKnownInvalidPage(err)
				return errQueueStop
			}
			return err
		}
		if err != nil || page.ambiguous {
			st.markTerminal(readmeta.CodeProviderPageAmbiguous, "provider page incomplete")
			return errQueueStop
		}
		if page.exhausted {
			kp.E = true
			kp.N = 0
			kp.CN = 0
			kp.PD = ""
			return nil
		}
		if page.next <= 0 {
			st.markTerminal(readmeta.CodeProviderPageAmbiguous, "paging metadata unavailable")
			return errQueueStop
		}
		kp.P = int(page.next)
		kp.N = page.next
		kp.CN = 0
		kp.PD = ""
	}
	return nil
}

func queuePageFraming(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "stream array") ||
		strings.Contains(msg, readmeta.CodeProviderPageAmbiguous) ||
		strings.Contains(msg, cursor.ResyncRequired) ||
		strings.Contains(msg, "malformed")
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

type mrPageFetch struct {
	ents      []mrPageEnt
	next      int64
	exhausted bool
	ambiguous bool
}

func (st *queueRuntime) fetchGroupMRPage(ctx context.Context, kp *cursor.QueueKindProg) (mrPageFetch, error) {
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
		return mrPageFetch{}, err
	}
	var reqOpts []gitlab.RequestOptionFunc
	if after != nil || before != nil {
		reqOpts = append(reqOpts, igl.WithUpdatedBounds(after, before))
	}
	path := fmt.Sprintf("groups/%s/merge_requests", gitlab.PathEscape(st.groupID))
	var fetched mrPageFetch
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
		fetched.ents = append(fetched.ents, mrPageEnt{
			Key:       strconv.FormatInt(mr.ProjectID, 10) + ":" + strconv.FormatInt(mr.IID, 10),
			ProjectID: mr.ProjectID, IID: mr.IID, Updated: u, Head: head,
		})
		return nil
	}, reqOpts...)
	var hdr http.Header
	var sdkNext int64
	if resp != nil {
		sdkNext = resp.NextPage
		if resp.Response != nil {
			hdr = resp.Response.Header
		}
	}
	obs := observeQueuePage(kp.P, hdr, sdkNext, len(fetched.ents))
	fetched.next = obs.next
	fetched.exhausted = obs.exhausted
	fetched.ambiguous = obs.ambiguous
	if err != nil {
		return fetched, err
	}
	if obs.ambiguous {
		fetched.ambiguous = true
		return fetched, fmt.Errorf("%s: paging metadata unavailable", readmeta.CodeProviderPageAmbiguous)
	}
	return fetched, nil
}

func (st *queueRuntime) noteCandidate(ctx context.Context, ent mrPageEnt, bit int) error {
	if ent.ProjectID < 1 {
		st.persistLimitation(readmeta.CodeIdentityUnresolved, "missing project_id")
		return nil
	}
	selected, err := st.projectSelected(ctx, ent.ProjectID)
	if err != nil {
		return err
	}
	if !selected {
		return nil
	}
	// Owner, group policy, exact MR, and source/downstream proof happen before any
	// key, timestamp, head, or membership bit is stored in the returned map.
	meta, err := st.verifyMR(ctx, ent.ProjectID, ent.IID)
	if err != nil {
		if isTypedBudget(err) || errors.Is(err, errQueueCancelled) {
			return err
		}
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, readmeta.CodeAuthzDenied), strings.HasPrefix(msg, readmeta.CodeIdentityUnresolved), strings.HasPrefix(msg, readmeta.CodeUnsupported):
			st.persistLimitation(strings.Split(msg, ":")[0], "candidate not confirmed")
			return nil
		default:
			return err
		}
	}
	if meta.updated == "" {
		st.dateUnknown = true
		st.persistLimitation(readmeta.CodePartial, "updated_at unknown")
		return nil
	}
	ent.Key = strconv.FormatInt(ent.ProjectID, 10) + ":" + strconv.FormatInt(ent.IID, 10)
	ent.Updated = meta.updated
	ent.Head = meta.head
	return st.insertProvenCandidate(ent, bit)
}

func (st *queueRuntime) insertProvenCandidate(ent mrPageEnt, bit int) error {
	for i := range st.qc.CM {
		if st.qc.CM[i].K == ent.Key {
			st.qc.CM[i].B |= bit
			if ent.Updated != "" {
				st.qc.CM[i].U = ent.Updated
			}
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

func (st *queueRuntime) authorizeGroupProject(ctx context.Context, projectID string) (CanonicalProject, error) {
	c, err := st.queueAuthorizeProject(ctx, projectID)
	if err != nil {
		return CanonicalProject{}, err
	}
	if c.NamespaceKind != "group" {
		return CanonicalProject{}, authzDenied("project namespace not under group")
	}
	ok, err := queueGroupAncestryContains(ctx, st.d, c.NamespaceID, map[int64]struct{}{st.group.ID: {}})
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
		if kept := queueBudgetOrContext(err); kept != nil {
			return meta, kept
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
		if err := st.queueAuthorizeAdditional(ctx, extra...); err != nil {
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
		seedID, serr := st.canonicalSelectionID(ctx, seed.ProjectID)
		if serr != nil {
			if isTypedBudget(serr) || errors.Is(serr, errQueueCancelled) {
				return serr
			}
			st.persistLimitation(readmeta.CodeIdentityUnresolved, "seed project")
			og.SI++
			og.DP = 1
			og.CN = 0
			og.PD = ""
			og.E = false
			continue
		}
		selected, serr := st.projectSelected(ctx, seedID)
		if serr != nil {
			return serr
		}
		if !selected {
			og.SI++
			og.DP = 1
			og.CN = 0
			og.PD = ""
			og.E = false
			continue
		}
		owner, mrMeta, err := st.loadSeedMR(ctx, seed)
		if err != nil {
			if isTypedBudget(err) || errors.Is(err, errQueueCancelled) {
				return err
			}
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
			if err := st.insertProvenCandidate(ent, queueBitOngoing); err != nil {
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
		if kept := queueBudgetOrContext(err); kept != nil {
			return CanonicalProject{}, meta, kept
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
		if err := st.queueAuthorizeAdditional(ctx, extra...); err != nil {
			return CanonicalProject{}, meta, err
		}
	}
	return owner, meta, nil
}

func (st *queueRuntime) scanSeedParticipation(ctx context.Context, owner CanonicalProject, meta seedMRMeta, og *cursor.QueueOngoingProg) (bool, error) {
	iid := mustIIDFromOG(st, og)
	key := strconv.FormatInt(owner.ID, 10) + ":" + strconv.FormatInt(iid, 10)
	qualified := st.cmHasBit(key, queueBitOngoing)
	for !og.E {
		if err := queueContextStop(ctx); err != nil {
			return qualified, err
		}
		opt := &gitlab.ListMergeRequestDiscussionsOptions{
			ListOptions: gitlab.ListOptions{Page: int64(og.DP), PerPage: int64(og.PSz)},
		}
		path := fmt.Sprintf("projects/%s/merge_requests/%d/discussions", gitlab.PathEscape(strconv.FormatInt(owner.ID, 10)), iid)
		var page []queueDisc
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
			page = append(page, queueDisc{id: d.ID, notes: d.Notes})
			return nil
		})
		var hdr http.Header
		var sdkNext int64
		if resp != nil {
			sdkNext = resp.NextPage
			if resp.Response != nil {
				hdr = resp.Response.Header
			}
		}
		obs := observeQueuePage(og.DP, hdr, sdkNext, len(page))
		invalid := st.invalidPage(obs.ambiguous, err)
		if err != nil && len(page) == 0 && isTypedBudget(err) {
			if invalid {
				st.terminalKnownInvalidPage(err)
				return qualified, errQueueStop
			}
			return qualified, err
		}
		if err != nil && len(page) == 0 && !invalid {
			return qualified, err
		}
		qualified, stop, serr := st.walkDiscussionPage(ctx, page, og, key, owner.ID, iid, meta)
		if serr != nil {
			if invalid && (isTypedBudget(serr) || errors.Is(serr, errQueueCancelled)) {
				st.terminalKnownInvalidPage(serr)
				return qualified, errQueueStop
			}
			return qualified, serr
		}
		if stop || st.qc.Term {
			return qualified, errQueueStop
		}
		if isTypedBudget(err) || errors.Is(err, errQueueCancelled) {
			if invalid {
				st.terminalKnownInvalidPage(err)
				return qualified, errQueueStop
			}
			return qualified, err
		}
		if err != nil || obs.ambiguous {
			st.markTerminal(readmeta.CodeProviderPageAmbiguous, "discussion page incomplete")
			return qualified, errQueueStop
		}
		if obs.exhausted {
			og.E = true
			og.CN = 0
			og.PD = ""
			break
		}
		og.DP = int(obs.next)
		og.CN = 0
		og.PD = ""
	}
	return qualified, nil
}

// walkDiscussionPage charges every note, including replayed prefixes, before reading fields.
// rq2 stores a discussion index only. A stop after any note of the current discussion has
// been inspected cannot be resumed safely, so that case is an explicit terminal omission.
type queueDisc struct {
	id    string
	notes []json.RawMessage
}

func (st *queueRuntime) walkDiscussionPage(ctx context.Context, page []queueDisc, og *cursor.QueueOngoingProg, key string, projectID, iid int64, meta seedMRMeta) (qualified bool, stop bool, err error) {
	_ = ctx
	qualified = st.cmHasBit(key, queueBitOngoing)
	if og.CN > 0 && len(page) < og.CN {
		st.markTerminal(readmeta.CodeProviderPageAmbiguous, "discussion page prefix drift")
		return qualified, true, nil
	}
	facts := make([]string, len(page))
	for i := 0; i < og.CN && i < len(page); i++ {
		fact, _, ferr := st.chargeDiscussionNotes(page[i], key, projectID, iid, meta, false)
		if ferr != nil {
			if stop, ret := st.classifyNoteStop(ferr, page, og.CN, noteFailIndex(ferr)); stop {
				return qualified, true, nil
			} else if ret != nil {
				return qualified, false, ret
			}
		}
		facts[i] = fact
	}
	if og.CN > 0 && prefixDigest(facts[:og.CN]) != og.PD {
		st.markTerminal(readmeta.CodeProviderPageAmbiguous, "discussion page prefix drift")
		return qualified, true, nil
	}
	for i := og.CN; i < len(page); i++ {
		fact, qual, ferr := st.chargeDiscussionNotes(page[i], key, projectID, iid, meta, true)
		if qual {
			qualified = true
		}
		if ferr != nil {
			if errors.Is(ferr, errQueueStop) {
				return qualified, true, nil
			}
			if stop, ret := st.classifyNoteStop(ferr, page, og.CN, noteFailIndex(ferr)); stop {
				return qualified, true, nil
			} else if ret != nil {
				return qualified, false, ret
			}
		}
		facts[i] = fact
		og.CN = i + 1
		og.PD = prefixDigest(facts[:og.CN])
	}
	return qualified, false, nil
}

type noteChargeError struct {
	index int
	err   error
}

func (e *noteChargeError) Error() string { return e.err.Error() }
func (e *noteChargeError) Unwrap() error { return e.err }

func noteFailIndex(err error) int {
	var n *noteChargeError
	if errors.As(err, &n) {
		return n.index
	}
	return 0
}

func (st *queueRuntime) chargeDiscussionNotes(d queueDisc, key string, projectID, iid int64, meta seedMRMeta, admit bool) (string, bool, error) {
	parts := make([]string, 0, len(d.notes)+1)
	parts = append(parts, d.id)
	qualified := false
	for i, raw := range d.notes {
		if err := st.budget.AddItem(); err != nil {
			return "", qualified, &noteChargeError{index: i, err: err}
		}
		parts = append(parts, noteReplayFact(raw, st.discoveryActor))
		if !noteQualifiesOngoingRaw(raw, st.discoveryActor) {
			continue
		}
		qualified = true
		if !admit || meta.updated == "" {
			if meta.updated == "" {
				st.dateUnknown = true
				st.persistLimitation(readmeta.CodePartial, "updated_at unknown")
			}
			continue
		}
		ent := mrPageEnt{Key: key, ProjectID: projectID, IID: iid, Updated: meta.updated, Head: meta.head}
		if err := st.insertProvenCandidate(ent, queueBitOngoing); err != nil {
			return strings.Join(parts, ";"), true, err
		}
	}
	return strings.Join(parts, ";"), qualified, nil
}

// classifyNoteStop decides whether a charged-note failure can be resumed.
// Item budget inside a discussion, or at a discussion boundary whose replay
// cannot admit one new note on a fresh item allowance, is terminal. Request,
// byte, and elapsed budgets stay typed at a representable boundary. Cancellation
// is never relabeled as an item-budget terminal.
func (st *queueRuntime) classifyNoteStop(err error, page []queueDisc, cursorCN, noteIdx int) (bool, error) {
	if err == nil || errors.Is(err, errQueueCancelled) {
		return false, err
	}
	if errors.Is(err, igl.ErrBudgetRequests) || errors.Is(err, igl.ErrBudgetBytes) || errors.Is(err, igl.ErrBudgetElapsed) {
		if noteIdx > 0 {
			st.terminalUnrepresentable(noteBudgetCode(err))
			return true, nil
		}
		return false, err
	}
	if !errors.Is(err, igl.ErrBudgetItems) {
		return false, err
	}
	// rq2 can store the discussion index only. A stop after the first note of
	// that discussion cannot move forward. A stop on note 0 is resumable only
	// when the next invocation can replay this boundary and still charge one
	// unseen note; otherwise the same cursor repeats forever.
	if noteIdx > 0 || !st.discussionBoundaryResumable(page, cursorCN) {
		st.terminalUnrepresentable(readmeta.CodeBudgetItems)
		return true, nil
	}
	return false, err
}

// discussionBoundaryResumable reports whether a later invocation, with a fresh
// item allowance, can reload this seed, re-read this discussion page, replay
// the consumed note prefix, and charge one unseen note.
func (st *queueRuntime) discussionBoundaryResumable(page []queueDisc, cursorCN int) bool {
	if st.budget == nil {
		return false
	}
	maxItems, _, _, _ := st.budget.LimitsSnapshot()
	if maxItems <= 0 {
		return true
	}
	notes := 0
	for i := 0; i < cursorCN && i < len(page); i++ {
		notes += len(page[i].notes)
	}
	// 1 seed reload + one item per discussion element on the re-read page
	// + replayed notes + the next unseen note.
	need := 1 + len(page) + notes + 1
	return need <= maxItems
}

func noteBudgetCode(err error) string {
	switch {
	case errors.Is(err, igl.ErrBudgetBytes):
		return readmeta.CodeBudgetBytes
	case errors.Is(err, igl.ErrBudgetElapsed):
		return readmeta.CodeBudgetElapsed
	case errors.Is(err, igl.ErrBudgetRequests):
		return readmeta.CodeBudgetRequests
	default:
		return readmeta.CodeBudgetItems
	}
}

func (st *queueRuntime) terminalUnrepresentable(code string) {
	msg := "note inspection stopped where rq2 cannot represent forward progress"
	if code == readmeta.CodeBudgetItems {
		msg = "note inspection stopped inside a discussion"
	}
	st.markTerminal(code, msg)
	st.persistLimitation(readmeta.CodePartial, "rq2 has no within-discussion note index; forward progress cannot be represented")
	st.persistLimitation(readmeta.CodeMembershipIncomplete, "unscanned discussion notes were not skipped")
	// One unfinished seed qualifies as zero or one candidate. A remaining note
	// count is not an exact membership count, so leave it null.
}

func (st *queueRuntime) cmHasBit(key string, bit int) bool {
	for i := range st.qc.CM {
		if st.qc.CM[i].K == key && st.qc.CM[i].B&bit != 0 {
			return true
		}
	}
	return false
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
		selected, serr := st.projectSelected(ctx, pid)
		if serr != nil {
			if st.terminalProjection() && (isTypedBudget(serr) || errors.Is(serr, errQueueCancelled)) {
				if item, ok := projectStoredCandidate(c, st.norm); ok {
					items = append(items, item)
					st.qc.EI++
					continue
				}
			}
			return items, serr
		}
		if !selected {
			st.qc.EI++
			continue
		}
		meta, err := st.verifyMR(ctx, pid, iid)
		if err != nil {
			if isTypedBudget(err) || errors.Is(err, errQueueCancelled) {
				if st.terminalProjection() {
					if item, ok := projectStoredCandidate(c, st.norm); ok {
						items = append(items, item)
						st.qc.EI++
						continue
					}
				}
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
	}
	return items, nil
}

func (st *queueRuntime) terminalProjection() bool {
	return st.qc != nil && (st.qc.Term || st.cmFull || st.membershipIncomp)
}

func projectStoredCandidate(c cursor.QueueCandidate, n normalizedQueue) (reviewQueueItem, bool) {
	pid, iid := splitKey(c.K)
	if pid < 1 || iid < 1 {
		return reviewQueueItem{}, false
	}
	return reviewQueueItem{ProjectID: pid, IID: iid, Kinds: bitsToKinds(c.B, n), HeadSHA: c.H}, true
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
	ctx := st.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	st.movingDiscovery = true
	st.persistLimitation(readmeta.CodeInconsistent, "moving discovery window")
	st.section.Consistency = readmeta.ConsistencyUnknown
	st.section.ContentComplete = readmeta.ContentCompleteFalse
	st.section.PaginationExhausted = false

	terminal := st.qc != nil && (st.qc.Term || st.cmFull || st.membershipIncomp)
	drainedEmit := st.qc.Phase == "emit" && st.qc.EI >= len(st.qc.CM)
	var next *string
	if !terminal && (st.qc.Phase == "discover" || !drainedEmit) {
		tok, err := st.mintCursor()
		if err != nil {
			st.markTerminal(readmeta.CodeCursorCapacity, "cursor encoding overflow")
			terminal = true
			if len(items) == 0 && st.qc != nil && st.qc.EI < len(st.qc.CM) {
				st.qc.Phase = "emit"
				emitted, eerr := st.runEmit(ctx)
				if isTypedBudget(eerr) {
					st.noteBudget(eerr)
				} else if errors.Is(eerr, errQueueCancelled) {
					st.persistLimitation(readmeta.CodeCancelled, "invocation cancelled")
				}
				items = emitted
			}
		} else {
			next = &tok
		}
	}
	items, next = st.fitOutput(items, next, &terminal)
	if terminal {
		next = nil
		if st.qc != nil {
			st.qc.Term = true
		}
	}
	return st.assemble(items, next, terminal), nil
}

func (st *queueRuntime) outputLimit() int {
	if st.outputLimitBytes > 0 {
		return st.outputLimitBytes
	}
	return queueReturnedJSONCapBytes
}

func (st *queueRuntime) fitOutput(items []reviewQueueItem, next *string, terminal *bool) ([]reviewQueueItem, *string) {
	limit := st.outputLimit()
	for {
		raw, err := json.Marshal(st.assemble(items, next, *terminal))
		if err != nil || len(raw) <= limit {
			return items, next
		}
		if !*terminal {
			st.markTerminal(readmeta.CodeCursorCapacity, "returned projection byte cap")
			*terminal = true
			next = nil
			continue
		}
		if len(items) == 0 {
			return items, nil
		}
		items = items[:len(items)-1]
		if st.qc != nil && st.qc.EI > 0 {
			st.qc.EI--
		}
	}
}

type queueCountBody struct {
	ConfirmedCandidates       int  `json:"confirmed_candidates"`
	ReturnedItems             int  `json:"returned_items"`
	KnownTerminalOmitted      int  `json:"known_terminal_omitted"`
	UnobservedMembershipCount *int `json:"unobserved_membership_count"`
}

func (st *queueRuntime) assemble(items []reviewQueueItem, next *string, terminal bool) map[string]any {
	st.section.NextCursor = next
	st.section.PaginationExhausted = false
	nItems := len(items)
	st.section.Counts.Items = &nItems
	sections := map[string]readmeta.Section{}
	for _, kind := range st.norm.kinds {
		sections[kind] = st.kindSection(kind)
	}
	omitted := 0
	if terminal && st.qc != nil {
		omitted = len(st.qc.CM) - st.qc.EI
		if omitted < 0 {
			omitted = 0
		}
	}
	return map[string]any{
		"items":    items,
		"section":  st.section,
		"sections": sections,
		"queue_counts": queueCountBody{
			ConfirmedCandidates:       len(st.qc.CM),
			ReturnedItems:             len(items),
			KnownTerminalOmitted:      omitted,
			UnobservedMembershipCount: st.unobservedMembership,
		},
	}
}

func (st *queueRuntime) kindSection(kind string) readmeta.Section {
	sec := newReviewQueueSection(st.now)
	sec.Consistency = readmeta.ConsistencyUnknown
	sec.ContentComplete = readmeta.ContentCompleteFalse
	sec.PaginationExhausted = st.kindExhausted(kind)
	sec.HeadSHA = nil
	sec.AddLimitation(readmeta.CodeInconsistent, "moving discovery window")
	for _, lim := range st.section.Limitations {
		if lim.Code == readmeta.CodeInconsistent {
			continue
		}
		if lim.Code == readmeta.CodeUnsupported && kind != "ongoing" {
			continue
		}
		sec.AddLimitation(lim.Code, lim.Message)
	}
	if st.kindScanned(kind) {
		n := st.confirmedKind(kind)
		sec.Counts.Items = &n
	}
	return sec
}

func (st *queueRuntime) confirmedKind(kind string) int {
	bit := queueBitReviewer
	switch kind {
	case "authored":
		bit = queueBitAuthored
	case "ongoing":
		bit = queueBitOngoing
	}
	n := 0
	if st.qc == nil {
		return 0
	}
	for _, c := range st.qc.CM {
		if c.B&bit != 0 {
			n++
		}
	}
	return n
}

func (st *queueRuntime) kindExhausted(kind string) bool {
	if st.qc == nil {
		return false
	}
	if kind == "ongoing" {
		// Seedless ongoing is unsupported on every invocation, including an
		// emit resume. OG.E from the discovery cursor is not provider exhaustion.
		if st.seedlessOngoing || len(st.norm.seeds) == 0 || st.qc.OG == nil {
			return false
		}
		return st.qc.OG.E
	}
	any := false
	for _, kp := range st.qc.KP {
		if kp.Kind != kind {
			continue
		}
		any = true
		if !kp.E {
			return false
		}
	}
	return any
}

func (st *queueRuntime) kindScanned(kind string) bool {
	if st.qc == nil {
		return false
	}
	if st.qc.Phase == "emit" || st.qc.Term {
		return true
	}
	if kind == "ongoing" {
		if st.seedlessOngoing {
			return true
		}
		if st.qc.OG == nil {
			return false
		}
		return st.qc.OG.E || st.qc.OG.SI > 0 || st.qc.OG.CN > 0
	}
	for i, kp := range st.qc.KP {
		if kp.Kind != kind {
			continue
		}
		if kp.E || kp.CN > 0 || kp.P > 1 || i < st.qc.KI {
			return true
		}
	}
	return false
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
	case errors.Is(err, errQueueCancelled), strings.HasPrefix(msg, readmeta.CodeCancelled):
		return fmt.Errorf("%s", readmeta.CodeCancelled)
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
