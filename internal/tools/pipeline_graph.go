package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	capabilityPipelineGraphV1 = "readmeta.pipeline_graph.v1"
	toolPipelineGraph         = "get_merge_request_pipeline_graph"
	sectionPipelineGraph      = "pipeline_graph"
	errCursorKeyMissingGraph  = "GITLAB_MCP_CURSOR_KEY is required for get_merge_request_pipeline_graph; configure a raw secret of at least 32 bytes"
	graphDefaultPerPage       = 20
)

type pipelineGraphIn struct {
	ProjectID         string   `json:"project_id"`
	MergeRequestIID   int64    `json:"merge_request_iid,omitempty"`
	PipelineID        int64    `json:"pipeline_id,omitempty"`
	ExpectedSourceSHA string   `json:"expected_source_sha,omitempty"`
	Cursor            string   `json:"cursor,omitempty"`
	JobNames          []string `json:"job_names,omitempty"`
	JobStages         []string `json:"job_stages,omitempty"`
	JobStatuses       []string `json:"job_statuses,omitempty"`
	PerPage           int      `json:"per_page,omitempty"`
	MaxItems          int      `json:"max_items,omitempty"`
	MaxRequests       int      `json:"max_requests,omitempty"`
	MaxDepth          int      `json:"max_depth,omitempty"`
	MaxNodes          int      `json:"max_nodes,omitempty"`
}

type pipelineView struct {
	ID           int64   `json:"id"`
	Status       *string `json:"status"`
	Source       *string `json:"source"`
	Ref          *string `json:"ref"`
	SHA          *string `json:"sha"`
	StatusKnown  bool    `json:"status_known"`
	ProjectID    int64   `json:"-"`
	ScopeProject string  `json:"-"`
}

type relationView struct {
	Kind          string   `json:"kind"`
	Proven        bool     `json:"proven"`
	Evidence      []string `json:"evidence"`
	SHAComparison string   `json:"sha_comparison"`
}

type jobView struct {
	ID           int64   `json:"id"`
	Name         *string `json:"name"`
	Stage        *string `json:"stage"`
	Status       *string `json:"status"`
	AllowFailure string  `json:"allow_failure"`
	Attempt      string  `json:"attempt"`
	Policy       string  `json:"policy"`
}

type lineageView struct {
	Name        *string `json:"name"`
	LatestKnown bool    `json:"latest_known"`
	LatestIDs   []int64 `json:"latest_ids"`
	HistoryIDs  []int64 `json:"history_ids"`
}

type pipelineGraphOut struct {
	Section            readmeta.Section `json:"section"`
	ProjectID          string           `json:"project_id"`
	MergeRequestIID    *int64           `json:"merge_request_iid"`
	PipelineID         *int64           `json:"pipeline_id"`
	Pipeline           *pipelineView    `json:"pipeline"`
	Relation           relationView     `json:"relation"`
	Jobs               []jobView        `json:"jobs"`
	Lineage            []lineageView    `json:"lineage"`
	Assessment         string           `json:"assessment"`
	DownstreamCoverage string           `json:"downstream_coverage"`
	BridgesVisited     bool             `json:"bridges_visited"`
	JobFilterApplied   bool             `json:"job_filter_applied"`
	Reasons            []string         `json:"reasons"`
	Nodes              []graphNodeView  `json:"nodes"`
	Edges              []graphEdgeView  `json:"edges"`
	BridgeCapability   string           `json:"bridge_capability,omitempty"`
	Digest             *string          `json:"digest,omitempty"`
}

type graphSelection struct {
	ExpectedSHA string
	MRIID       int64
	Filter      jobFilter
	PerPage     int
	MaxDepth    int
	MaxNodes    int
}

type graphPage struct {
	Jobs    []graphJob
	Partial bool
	Reason  string
	Paging  readmeta.PagingObservation
}

// getMergeRequestPipelineGraph reads a parent pipeline, paginated jobs, and
// authorized downstream bridges with visited-set cycle detection.
func getMergeRequestPipelineGraph(ctx context.Context, _ *mcp.CallToolRequest, in pipelineGraphIn, d Deps) (*mcp.CallToolResult, any, error) {
	rawCursor := in.Cursor
	if rawCursor != "" {
		trimmed := strings.TrimSpace(rawCursor)
		if trimmed == "" || trimmed != rawCursor {
			return nil, nil, fmt.Errorf("%s: malformed cursor", cursor.ResyncRequired)
		}
	}
	if d.Config == nil || !d.Config.CursorSigningEnabled() {
		return nil, nil, errors.New(errCursorKeyMissingGraph)
	}
	if in.MergeRequestIID < 0 || in.PipelineID < 0 || in.PerPage < 0 || in.MaxItems < 0 || in.MaxRequests < 0 || in.MaxDepth < 0 || in.MaxNodes < 0 {
		return nil, nil, fmt.Errorf("invalid pipeline graph input")
	}
	if in.PerPage > 50 {
		return nil, nil, fmt.Errorf("per_page must be between 1 and 50")
	}
	sel, err := normalizeGraphSelection(in)
	if err != nil {
		return nil, nil, err
	}
	budget := igl.BudgetFromContext(ctx)
	ownsBudget := false
	if budget == nil {
		budget = igl.DefaultBudget()
		ownsBudget = true
		ctx = igl.WithBudget(ctx, budget)
	}
	if in.MaxItems > 0 {
		budget.CapLimits(in.MaxItems, 0, 0)
	}
	if in.MaxRequests > 0 {
		budget.CapLimits(0, 0, in.MaxRequests)
	}
	if ownsBudget {
		defer budget.Cancel()
	}

	now := d.now()
	if rawCursor != "" {
		return resumePipelineGraph(ctx, in, sel, d, budget, now, rawCursor)
	}
	return initialPipelineGraph(ctx, in, sel, d, budget, now)
}

func normalizeGraphSelection(in pipelineGraphIn) (graphSelection, error) {
	if in.PipelineID == 0 && in.MergeRequestIID == 0 {
		return graphSelection{}, fmt.Errorf("pipeline_id or merge_request_iid is required")
	}
	expected := strings.TrimSpace(in.ExpectedSourceSHA)
	if expected != "" {
		sha, ok := readmeta.ObservedHeadSHA(expected)
		if !ok {
			return graphSelection{}, fmt.Errorf("expected_source_sha must be a 40-character hex commit")
		}
		expected = sha
	}
	filter, err := normalizeJobFilter(in.JobNames, in.JobStages, in.JobStatuses)
	if err != nil {
		return graphSelection{}, err
	}
	per := in.PerPage
	if per == 0 {
		per = graphDefaultPerPage
	}
	depth, nodes, err := parseGraphBounds(in)
	if err != nil {
		return graphSelection{}, err
	}
	return graphSelection{ExpectedSHA: expected, MRIID: in.MergeRequestIID, Filter: filter, PerPage: per, MaxDepth: depth, MaxNodes: nodes}, nil
}

func initialPipelineGraph(ctx context.Context, in pipelineGraphIn, sel graphSelection, d Deps, budget *igl.Budget, now time.Time) (*mcp.CallToolResult, any, error) {
	section := newPipelineGraphSection(now)
	pid, err := resolveCursorProjectCanonical(ctx, d, in.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	actorID, err := resolveCursorActor(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	var mrIID *int64
	if sel.MRIID > 0 {
		mrIID = &sel.MRIID
	}

	chosen, rel, limits, err := resolveParentPipeline(ctx, d, pid, in.PipelineID, sel)
	if err != nil {
		return nil, nil, err
	}
	if chosen == nil {
		return nil, Out(unresolvedGraph(section, pid, mrIID, rel, limits, sel.Filter.active())), nil
	}
	for _, lim := range limits {
		section.AddLimitation(lim.Code, lim.Message)
	}
	pipePID := chosen.ScopeProject
	if pipePID == "" {
		pipePID = pid
	}
	page, err := collectJobPage(ctx, d, budget, pipePID, chosen.ID, 1, sel.PerPage, nil)
	if err != nil {
		return nil, nil, err
	}
	upper := now.UTC().Format(time.RFC3339Nano)
	expires := now.UTC().Add(d.Config.CursorTTL()).Format(time.RFC3339Nano)
	walk := newGraphWalk(chosen, pipePID, sel.MaxDepth, sel.MaxNodes)
	walk.bindRelation(rel)
	out, err := finishGraph(ctx, section, pid, pipePID, mrIID, chosen, rel, page, sel, d, actorID, upper, expires, 1, true, lineageCarry{}, walk, budget)
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(out), nil
}

func resumePipelineGraph(ctx context.Context, in pipelineGraphIn, sel graphSelection, d Deps, budget *igl.Budget, now time.Time, tok string) (*mcp.CallToolResult, any, error) {
	payload, err := cursor.Decode(d.Config.CursorKey, tok, now)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: cursor validation failed", cursor.ResyncRequired)
	}
	if payload.Tool != toolPipelineGraph || payload.Section != sectionPipelineGraph || payload.Scope.Kind != cursor.ScopePipeline {
		return nil, nil, fmt.Errorf("%s: tool/section mismatch", cursor.ResyncRequired)
	}
	instance, err := cursorInstance(d.Config)
	if err != nil {
		return nil, nil, err
	}
	if payload.Instance != instance || payload.PolicyFP != d.Config.PolicyFingerprint() {
		return nil, nil, fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	actorID, err := resolveCursorActor(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	if actorID != payload.ActorID {
		return nil, nil, fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	canon, err := resolveCursorProjectIdentity(ctx, d, in.ProjectID)
	if err != nil {
		return nil, nil, err
	}
	pid := strconv.FormatInt(canon.ID, 10)
	// pipeline_id may be omitted. The first call can select the pipeline from
	// the merge request, and the signed cursor already binds that id.
	if payload.Scope.PipelineID == nil || *payload.Scope.PipelineID < 1 || (in.PipelineID != 0 && in.PipelineID != *payload.Scope.PipelineID) {
		return nil, nil, fmt.Errorf("%s: project scope mismatch", cursor.ResyncRequired)
	}
	wantFilters := graphFilters(sel, payload.UpperBound, pid)
	if wantFilters != payload.Filters {
		return nil, nil, fmt.Errorf("%s: filter mismatch", cursor.ResyncRequired)
	}
	if err := reauthorizeCursorProject(ctx, d, canon); err != nil {
		return nil, nil, err
	}
	pipeCanon := canon
	pipePID := pid
	if payload.Scope.ProjectID != pid {
		pipeCanon, err = resolveCursorProjectIdentity(ctx, d, payload.Scope.ProjectID)
		if err != nil {
			return nil, nil, err
		}
		pipePID = strconv.FormatInt(pipeCanon.ID, 10)
		if pipePID != payload.Scope.ProjectID {
			return nil, nil, fmt.Errorf("%s: project scope mismatch", cursor.ResyncRequired)
		}
		if err := reauthorizeCursorProject(ctx, d, pipeCanon); err != nil {
			return nil, nil, err
		}
	}
	pipe, err := loadPipeline(ctx, d, pipePID, *payload.Scope.PipelineID)
	if err != nil {
		return nil, nil, wrapPipelineLoadErr(err)
	}
	if pipe == nil || pipe.SHA == nil || len(payload.ImmutableRefs) != 1 || *pipe.SHA != payload.ImmutableRefs[0] {
		return nil, nil, fmt.Errorf("%s: pinned pipeline SHA mismatch", cursor.ResyncRequired)
	}
	scope := cursor.Scope{Kind: cursor.ScopePipeline, ProjectID: pipePID, PipelineID: payload.Scope.PipelineID}
	if err := cursor.MatchBinding(payload, instance, actorID, d.Config.PolicyFingerprint(), toolPipelineGraph, sectionPipelineGraph, scope, wantFilters, payload.ImmutableRefs, payload.UpperBound); err != nil {
		return nil, nil, fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	section := newPipelineGraphSection(now)
	var mrIID *int64
	if sel.MRIID > 0 {
		mrIID = &sel.MRIID
	}
	rel := classifyRelation(relationInput{
		MRIID:       sel.MRIID,
		ListChecked: false,
		Ref:         deref(pipe.Ref),
		PipelineSHA: deref(pipe.SHA),
		ExpectedSHA: sel.ExpectedSHA,
	})
	gc := payload.GraphCont
	childResume := gc != nil && gc.RI > 0 && (gc.D > 0 || gc.RI != *payload.Scope.PipelineID || gc.RP != pipePID)
	if childResume {
		rel = relationResult{Kind: gc.RK, Proven: gc.Prv, Evidence: []string{"graph_cursor"}, SHAComparison: gc.RS}
		if rel.Kind == "" {
			rel.Kind = relUnproven
		}
	} else if sel.MRIID > 0 {
		linked, exhausted, listErr := pipelineListed(ctx, d, pid, sel.MRIID, pipe.ID)
		if listErr != nil {
			return nil, nil, listErr
		}
		branch := ""
		if mr, mrErr := loadMergeRequest(ctx, d, pid, sel.MRIID); mrErr != nil {
			return nil, nil, mrErr
		} else if mr != nil {
			branch = mr.SourceBranch
		}
		rel = classifyRelation(relationInput{
			MRIID:         sel.MRIID,
			SourceBranch:  branch,
			ListChecked:   true,
			Linked:        linked,
			ListExhausted: exhausted,
			Ref:           deref(pipe.Ref),
			PipelineSHA:   deref(pipe.SHA),
			ExpectedSHA:   sel.ExpectedSHA,
		})
	}
	walk, werr := restoreGraphWalk(pipe, payload.GraphCont, sel.MaxDepth, sel.MaxNodes)
	if werr != nil {
		return nil, nil, werr
	}
	walk.bindRelation(rel)
	if err := walk.revalidateAncestors(ctx, d, budget, sel.PerPage); err != nil {
		return nil, nil, err
	}
	if payload.GraphCont != nil && payload.GraphCont.Phase == cursor.GraphPhaseBridges {
		return resumeGraphBridges(ctx, &section, pid, pipePID, mrIID, pipe, rel, sel, d, actorID, &payload, walk, budget)
	}
	guard, err := collectJobPage(ctx, d, budget, pipePID, pipe.ID, payload.PageState.Page, sel.PerPage, nil)
	if err != nil || guard.Partial {
		return nil, nil, fmt.Errorf("%s: previous-page guard failed", cursor.ResyncRequired)
	}
	tokens := jobGuardTokens(guard.Jobs)
	ids := jobIDStrings(guard.Jobs)
	last := ""
	if len(tokens) > 0 {
		last = tokens[len(tokens)-1]
	}
	if cursor.SequenceDigest(tokens) != payload.PageState.SequenceDigest || last != payload.PageState.LastSHA || len(tokens) != payload.PageState.ItemsOnPage {
		return nil, nil, fmt.Errorf("%s: previous-page boundary drift", cursor.ResyncRequired)
	}
	if !guard.Paging.PagingKnown || observedNextPage(guard.Paging) != payload.PageState.ProviderNextPage || payload.PageState.ProviderNextPage != int64(payload.PageState.Page)+1 {
		return nil, nil, fmt.Errorf("%s: previous-page boundary drift", cursor.ResyncRequired)
	}
	prior, err := decodeLineageCarry(payload.PageState.LineageMax)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: previous-page boundary drift", cursor.ResyncRequired)
	}
	if !lineageCarryCovers(prior, guard.Jobs) {
		return nil, nil, fmt.Errorf("%s: previous-page boundary drift", cursor.ResyncRequired)
	}
	nextPage := int(payload.PageState.ProviderNextPage)
	page, err := collectJobPage(ctx, d, budget, pipePID, pipe.ID, nextPage, sel.PerPage, idSet(ids))
	if err != nil {
		return nil, nil, err
	}
	out, err := finishGraph(ctx, section, pid, pipePID, mrIID, pipe, rel, page, sel, d, actorID, payload.UpperBound, payload.ExpiresAt, nextPage, false, prior, walk, budget)
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(out), nil
}

func resumeGraphBridges(ctx context.Context, section *readmeta.Section, pid, pipePID string, mrIID *int64, pipe *pipelineView, rel relationResult, sel graphSelection, d Deps, actorID int64, payload *cursor.Payload, walk *graphWalk, budget *igl.Budget) (*mcp.CallToolResult, any, error) {
	if section == nil {
		return nil, nil, fmt.Errorf("%s: graph continuation missing", cursor.ResyncRequired)
	}
	guard, err := collectBridgePage(ctx, d, budget, pipePID, pipe.ID, payload.PageState.Page, sel.PerPage, nil)
	if err != nil || guard.Partial || guard.Unsupported || guard.Inaccessible {
		return nil, nil, fmt.Errorf("%s: previous-page guard failed", cursor.ResyncRequired)
	}
	ids := make([]string, 0, len(guard.Bridges))
	for _, br := range guard.Bridges {
		ids = append(ids, bridgeGuardToken(br))
	}
	last := ""
	if len(ids) > 0 {
		last = ids[len(ids)-1]
	}
	if cursor.SequenceDigest(ids) != payload.PageState.SequenceDigest || last != payload.PageState.LastSHA || len(ids) != payload.PageState.ItemsOnPage {
		return nil, nil, fmt.Errorf("%s: previous-page boundary drift", cursor.ResyncRequired)
	}
	if !guard.Paging.PagingKnown || observedNextPage(guard.Paging) != payload.PageState.ProviderNextPage || payload.PageState.ProviderNextPage != int64(payload.PageState.Page)+1 {
		return nil, nil, fmt.Errorf("%s: previous-page boundary drift", cursor.ResyncRequired)
	}
	nextPage := int(payload.PageState.ProviderNextPage)
	page, err := collectBridgePage(ctx, d, budget, pipePID, pipe.ID, nextPage, sel.PerPage, idSet(ids))
	if err != nil {
		return nil, nil, err
	}
	walk.ingestBridges(graphNodeKey{Project: pipePID, Pipeline: pipe.ID}, deref(pipe.SHA), page)
	if page.Partial {
		section.AddLimitation(readmeta.CodePartial, page.Reason)
		walk.unseen = true
		walk.coverage = downstreamCoveragePartial
	} else if next, more := pagingContinues(page.Paging, nextPage); more && len(page.Bridges) > 0 {
		sha, _ := readmeta.ObservedHeadSHA(deref(pipe.SHA))
		if err := mintBridgeCursor(section, d, actorID, pipePID, pipe.ID, sha, sel, payload.UpperBound, payload.ExpiresAt, nextPage, page, pid, walk, next); err != nil {
			return nil, nil, err
		}
	} else {
		if !bridgePagingExhausted(page) {
			markIncompleteBridgePaging(section, walk, page)
		}
		walk.completeNode(graphNodeKey{Project: pipePID, Pipeline: pipe.ID})
		if err := walkQueuedChildren(ctx, section, pid, sel, d, actorID, payload.UpperBound, payload.ExpiresAt, walk, budget); err != nil {
			return nil, nil, err
		}
		if len(walk.queue) == 0 && !walk.unseen && walk.cap == bridgeCapabilityBridges && bridgePagingExhausted(page) {
			walk.coverage = downstreamCoverageComplete
			walk.unseen = false
		}
	}
	empty := graphPage{Paging: readmeta.PagingObservation{PagingKnown: true, ExhaustedObserved: true}}
	out, err := finishGraph(ctx, *section, pid, pipePID, mrIID, pipe, rel, empty, sel, d, actorID, payload.UpperBound, payload.ExpiresAt, nextPage, false, lineageCarry{}, walk, budget)
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(out), nil
}

type mrFacts struct {
	SourceBranch string
	HeadID       int64
	HeadProject  int64
}

type pipelineRef struct {
	ID        int64
	ProjectID int64
}

func resolveParentPipeline(ctx context.Context, d Deps, pid string, pipelineID int64, sel graphSelection) (*pipelineView, relationResult, []readmeta.Limitation, error) {
	rel := relationResult{Kind: relUnproven, Evidence: []string{}, SHAComparison: shaUnknown}
	if pipelineID > 0 {
		pipe, err := loadPipeline(ctx, d, pid, pipelineID)
		if err != nil {
			return nil, rel, nil, wrapPipelineLoadErr(err)
		}
		if pipe == nil {
			rel.SHAComparison = compareSHA("", sel.ExpectedSHA)
			return nil, rel, []readmeta.Limitation{{Code: readmeta.CodeInaccessible, Message: "pipeline not found"}}, nil
		}
		scope, err := bindPipelineProject(ctx, d, pid, pipe.ProjectID)
		if err != nil {
			return nil, rel, nil, err
		}
		pipe.ScopeProject = scope
		rel = relationResult{Kind: relUnproven, Evidence: []string{}, SHAComparison: compareSHA(deref(pipe.SHA), sel.ExpectedSHA)}
		if sel.MRIID > 0 {
			linked, exhausted, err := pipelineListed(ctx, d, pid, sel.MRIID, pipelineID)
			if err != nil {
				return nil, rel, nil, err
			}
			branch := ""
			mr, err := loadMergeRequest(ctx, d, pid, sel.MRIID)
			if err != nil {
				return nil, rel, nil, err
			}
			if mr != nil {
				branch = mr.SourceBranch
			}
			rel = classifyRelation(relationInput{
				MRIID:         sel.MRIID,
				SourceBranch:  branch,
				ListChecked:   true,
				Linked:        linked,
				ListExhausted: exhausted,
				Ref:           deref(pipe.Ref),
				PipelineSHA:   deref(pipe.SHA),
				ExpectedSHA:   sel.ExpectedSHA,
			})
		}
		return pipe, rel, nil, nil
	}
	mr, err := loadMergeRequest(ctx, d, pid, sel.MRIID)
	if err != nil {
		return nil, rel, nil, err
	}
	if mr == nil {
		return nil, rel, []readmeta.Limitation{{Code: readmeta.CodeInaccessible, Message: "merge request not found"}}, nil
	}
	refs, exhausted, err := listMRPipelineRefs(ctx, d, pid, sel.MRIID, mr.HeadID)
	if err != nil {
		return nil, rel, nil, err
	}
	ids := pipelineRefIDs(refs)
	chosenID, ambiguous, missing := selectParentID(mr.HeadID, ids, exhausted)
	if missing || ambiguous {
		lim := readmeta.Limitation{Code: readmeta.CodeUnknownCount, Message: "parent pipeline is ambiguous"}
		if missing {
			lim = readmeta.Limitation{Code: readmeta.CodeInaccessible, Message: "merge request has no pipeline"}
		}
		return nil, relationResult{Kind: relUnproven, Evidence: []string{}, SHAComparison: compareSHA("", sel.ExpectedSHA)}, []readmeta.Limitation{lim}, nil
	}
	scope, err := bindPipelineProject(ctx, d, pid, projectForChosen(chosenID, mr, refs))
	if err != nil {
		return nil, rel, nil, err
	}
	pipe, err := loadPipeline(ctx, d, scope, chosenID)
	if err != nil {
		return nil, rel, nil, wrapPipelineLoadErr(err)
	}
	if pipe == nil {
		return nil, rel, []readmeta.Limitation{{Code: readmeta.CodeInaccessible, Message: "pipeline not found"}}, nil
	}
	if pipe.ProjectID > 0 {
		scope, err = bindPipelineProject(ctx, d, scope, pipe.ProjectID)
		if err != nil {
			return nil, rel, nil, err
		}
	}
	pipe.ScopeProject = scope
	rel = classifyRelation(relationInput{
		MRIID:         sel.MRIID,
		SourceBranch:  mr.SourceBranch,
		ListChecked:   true,
		Linked:        true,
		ListExhausted: exhausted,
		Ref:           deref(pipe.Ref),
		PipelineSHA:   deref(pipe.SHA),
		ExpectedSHA:   sel.ExpectedSHA,
	})
	return pipe, rel, nil, nil
}

func selectParentID(head int64, ids []int64, exhausted bool) (int64, bool, bool) {
	seen := map[int64]struct{}{}
	for _, id := range ids {
		seen[id] = struct{}{}
	}
	if head > 0 {
		if _, ok := seen[head]; ok {
			return head, false, false
		}
		// A listed pipeline that is not the head is a different pipeline.
		// Do not substitute it. A single listed id is only a fallback when
		// the merge request did not provide a head pipeline id.
		return 0, true, false
	}
	if !exhausted {
		return 0, true, false
	}
	switch len(ids) {
	case 0:
		return 0, false, true
	case 1:
		return ids[0], false, false
	default:
		return 0, true, false
	}
}

func unresolvedGraph(section readmeta.Section, pid string, mrIID *int64, rel relationResult, limits []readmeta.Limitation, filter bool) pipelineGraphOut {
	for _, lim := range limits {
		section.AddLimitation(lim.Code, lim.Message)
	}
	section.AddLimitation(readmeta.CodeUnsupported, "downstream coverage is unknown")
	section.ContentComplete = readmeta.ContentCompleteFalse
	section.Consistency = readmeta.ConsistencyUnknown
	section.PaginationExhausted = false
	section.ManifestCoverage = readmeta.CoverageUnknown
	section.PatchCoverage = readmeta.CoverageUnknown
	missing := true
	ambiguous := false
	for _, lim := range limits {
		if lim.Message == "parent pipeline is ambiguous" {
			missing = false
			ambiguous = true
		}
	}
	assessment, reasons := assessParent(assessInput{PipelineMissing: missing, PipelineAmbiguous: ambiguous, Filter: filter})
	if rel.Evidence == nil {
		rel.Evidence = []string{}
	}
	return pipelineGraphOut{
		Section:            section,
		ProjectID:          pid,
		MergeRequestIID:    mrIID,
		Relation:           relationView{Kind: rel.Kind, Proven: rel.Proven, Evidence: rel.Evidence, SHAComparison: rel.SHAComparison},
		Jobs:               nil,
		Lineage:            []lineageView{},
		Assessment:         assessment,
		DownstreamCoverage: downstreamCoverageUnknown,
		BridgesVisited:     false,
		JobFilterApplied:   filter,
		Reasons:            reasons,
	}
}

func finishGraph(ctx context.Context, section readmeta.Section, pid, pipePID string, mrIID *int64, pipe *pipelineView, rel relationResult, page graphPage, sel graphSelection, d Deps, actorID int64, upper, expires string, pageNum int, fromStart bool, prior lineageCarry, walk *graphWalk, budget *igl.Budget) (pipelineGraphOut, error) {
	if !rel.Proven {
		section.AddLimitation(readmeta.CodeUnknownCount, "pipeline relation is unproven")
	}
	if sel.Filter.active() {
		section.AddLimitation(readmeta.CodePartial, "job filter excludes work")
	}
	if page.Partial {
		section.AddLimitation(readmeta.CodePartial, page.Reason)
	}
	sha, ok := readmeta.ObservedHeadSHA(deref(pipe.SHA))
	if ok {
		section.HeadSHA = &sha
	}
	groups := buildLineage(page.Jobs, prior)
	if walk != nil {
		walk.recordOutcomes(groups)
		walk.jobsEv = chainEvidence(walk.jobsEv, jobGuardTokens(page.Jobs)...)
	}
	attemptOf := map[int64]string{}
	for _, g := range groups {
		for id, attempt := range g.Attempts {
			attemptOf[id] = attempt
		}
	}
	views := make([]jobView, 0, len(page.Jobs))
	returned := map[int64]struct{}{}
	for _, job := range page.Jobs {
		if sel.Filter.active() && !sel.Filter.match(job) {
			continue
		}
		returned[job.ID] = struct{}{}
		views = append(views, jobToView(job, attemptOf[job.ID]))
	}
	lineage := make([]lineageView, 0, len(groups))
	for _, g := range groups {
		view, ok := lineageViewFor(g, returned, sel.Filter.active())
		if !ok {
			continue
		}
		lineage = append(lineage, view)
	}
	if views == nil {
		views = []jobView{}
	}
	n := len(views)
	section.Counts.Items = &n
	exhausted := page.Paging.ExhaustedObserved && !page.Partial
	jobsPartial := page.Partial || !page.Paging.ExhaustedObserved
	role := nodeRoleParent
	if walk != nil && walk.depth > 0 {
		role = nodeRoleDownstream
	}
	if walk != nil && (len(page.Jobs) > 0 || (walk.phase == cursor.GraphPhaseJobs && !graphHasNode(walk, pipePID, pipe.ID))) {
		walk.nodes = append(walk.nodes, graphNodeView{
			ProjectID:  pipePID,
			PipelineID: pipe.ID,
			Depth:      walk.depth,
			Role:       role,
			Pipeline:   pipe,
			Jobs:       views,
			Lineage:    lineage,
		})
	}
	section.PatchCoverage = readmeta.CoverageUnknown
	if rel.Proven {
		section.Consistency = readmeta.ConsistencyConsistent
	} else {
		section.Consistency = readmeta.ConsistencyUnknown
	}

	next, more := pagingContinues(page.Paging, pageNum)
	needJobsCursor := !page.Partial && more && ok && len(page.Jobs) > 0
	if needJobsCursor && walk != nil {
		walk.phase = cursor.GraphPhaseJobs
		walk.coverage = downstreamCoverageUnknown
		walk.unseen = true
		nextLineage := encodeLineageCarry(mergeLineageCarry(prior, page.Jobs))
		if tok, err := mintGraphCursor(d, actorID, pipePID, pipe.ID, sha, sel, upper, expires, pageNum, jobGuardTokens(page.Jobs), next, len(page.Jobs), pid, nextLineage, walk.snapshotCont()); err == nil {
			section.NextCursor = &tok
		} else {
			section.AddLimitation(readmeta.CodePartial, "continuation cursor was not issued")
		}
	} else if !exhausted && !page.Paging.PagingKnown {
		section.AddLimitation(readmeta.CodeUnknownCount, "paging metadata unavailable")
		if walk != nil {
			walk.unseen = true
		}
	}

	if exhausted && walk != nil && section.NextCursor == nil && walk.phase == cursor.GraphPhaseJobs {
		if err := walkBridgesAndChildren(ctx, &section, pid, pipePID, pipe, sha, sel, d, actorID, upper, expires, walk, budget); err != nil {
			return pipelineGraphOut{}, err
		}
	}

	coverage := downstreamCoverageUnknown
	unseen := true
	outcomes := groupsOutcomes(groups)
	if walk != nil {
		coverage = walk.coverage
		unseen = walk.unseen
		outcomes = walk.outcomes
		if walk.block && !hasPolicyOutcome(outcomes, policyBlock) {
			outcomes = append(outcomes, policyOutcome{Outcome: policyBlock})
		}
		if walk.partial && !hasPolicyOutcome(outcomes, policyPartial) {
			outcomes = append(outcomes, policyOutcome{Outcome: policyPartial})
		}
		if walk.unknown && !hasPolicyOutcome(outcomes, policyUnknown) {
			outcomes = append(outcomes, policyOutcome{Outcome: policyUnknown})
		}
		if walk.bridgesOn {
			section.AddLimitation(readmeta.CodeUnsupported, "")
			section.Limitations = dropEmptyUnsupported(section.Limitations)
		}
	} else {
		section.AddLimitation(readmeta.CodeUnsupported, "downstream coverage is unknown")
	}
	if coverage == downstreamCoverageUnknown {
		section.AddLimitation(readmeta.CodeUnsupported, "downstream coverage is unknown")
	} else if coverage == downstreamCoveragePartial {
		section.AddLimitation(readmeta.CodePartial, "downstream coverage is partial")
	}
	assessment, reasons := assessGraph(assessInput{
		Filter:         sel.Filter.active(),
		JobsPartial:    jobsPartial || (walk != nil && walk.partial),
		RelationProven: rel.Proven,
		Outcomes:       outcomes,
		Downstream:     coverage,
		UnseenEdge:     unseen,
	})
	if walk != nil {
		reasons = appendUniqueReasons(append([]string(nil), walk.reasons...), reasons)
	}
	graphComplete := fromStart && exhausted && !sel.Filter.active() && !page.Partial && page.Paging.PagingKnown && coverage == downstreamCoverageComplete && !unseen && section.NextCursor == nil
	if graphComplete {
		section.ContentComplete = readmeta.ContentCompleteTrue
		section.ManifestCoverage = readmeta.CoverageFull
		section.PaginationExhausted = true
	} else {
		section.ContentComplete = readmeta.ContentCompleteFalse
		section.ManifestCoverage = readmeta.CoveragePartial
		section.PaginationExhausted = exhausted && coverage == downstreamCoverageComplete && section.NextCursor == nil
	}
	if rel.Evidence == nil {
		rel.Evidence = []string{}
	}
	id := pipe.ID
	out := pipelineGraphOut{
		Section:            section,
		ProjectID:          pid,
		MergeRequestIID:    mrIID,
		PipelineID:         &id,
		Pipeline:           pipe,
		Relation:           relationView{Kind: rel.Kind, Proven: rel.Proven, Evidence: rel.Evidence, SHAComparison: rel.SHAComparison},
		Jobs:               views,
		Lineage:            lineage,
		Assessment:         assessment,
		DownstreamCoverage: coverage,
		BridgesVisited:     walk != nil && walk.bridgesOn,
		JobFilterApplied:   sel.Filter.active(),
		Reasons:            reasons,
	}
	if walk != nil {
		out.Nodes = walk.nodes
		out.Edges = walk.edges
		out.BridgeCapability = walk.cap
		if out.Nodes == nil {
			out.Nodes = []graphNodeView{}
		}
		if out.Edges == nil {
			out.Edges = []graphEdgeView{}
		}
	}
	if graphComplete {
		dig := encodeGraphDigest(out.Nodes, out.Edges, assessment, coverage)
		if dig != "" {
			out.Digest = &dig
		}
	}
	return out, nil
}

func pagingContinues(obs readmeta.PagingObservation, pageNum int) (int64, bool) {
	if !obs.PagingKnown || obs.ExhaustedObserved {
		return 0, false
	}
	want := int64(pageNum) + 1
	if obs.SDKNextPage == want {
		return obs.SDKNextPage, true
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(obs.HeaderValue), 10, 64); err == nil && n == want {
		return n, true
	}
	return 0, false
}

func observedNextPage(obs readmeta.PagingObservation) int64 {
	if obs.SDKNextPage > 0 {
		return obs.SDKNextPage
	}
	n, err := strconv.ParseInt(strings.TrimSpace(obs.HeaderValue), 10, 64)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

func wrapPipelineLoadErr(err error) error {
	if errors.Is(err, errPipelineForbidden) {
		return fmt.Errorf("%s: gitlab request failed", readmeta.CodeHTTPError)
	}
	return err
}

func graphHasNode(w *graphWalk, project string, pipelineID int64) bool {
	if w == nil {
		return false
	}
	for _, n := range w.nodes {
		if n.ProjectID == project && n.PipelineID == pipelineID {
			return true
		}
	}
	return false
}

func groupsOutcomes(groups []lineageGroup) []policyOutcome {
	var out []policyOutcome
	for _, g := range groups {
		out = append(out, g.Outcomes...)
	}
	return out
}

func dropEmptyUnsupported(in []readmeta.Limitation) []readmeta.Limitation {
	out := in[:0]
	for _, lim := range in {
		if lim.Code == readmeta.CodeUnsupported && lim.Message == "" {
			continue
		}
		out = append(out, lim)
	}
	return out
}

func walkBridgesAndChildren(ctx context.Context, section *readmeta.Section, rootPID, pipePID string, pipe *pipelineView, sha string, sel graphSelection, d Deps, actorID int64, upper, expires string, walk *graphWalk, budget *igl.Budget) error {
	if section == nil {
		return fmt.Errorf("%s: graph continuation missing", cursor.ResyncRequired)
	}
	walk.phase = cursor.GraphPhaseBridges
	bpage, err := collectBridgePage(ctx, d, budget, pipePID, pipe.ID, 1, sel.PerPage, nil)
	if err != nil {
		return err
	}
	if bpage.Partial {
		section.AddLimitation(readmeta.CodePartial, bpage.Reason)
		walk.unseen = true
		walk.coverage = downstreamCoveragePartial
		walk.bridgesOn = true
		next, _ := pagingContinues(bpage.Paging, 1)
		return mintBridgeCursor(section, d, actorID, pipePID, pipe.ID, sha, sel, upper, expires, 1, bpage, rootPID, walk, next)
	}
	walk.ingestBridges(graphNodeKey{Project: pipePID, Pipeline: pipe.ID}, deref(pipe.SHA), bpage)
	if bpage.Unsupported || bpage.Inaccessible {
		return nil
	}
	next, moreBridges := pagingContinues(bpage.Paging, 1)
	if moreBridges && len(bpage.Bridges) > 0 {
		walk.unseen = true
		walk.coverage = downstreamCoveragePartial
		return mintBridgeCursor(section, d, actorID, pipePID, pipe.ID, sha, sel, upper, expires, 1, bpage, rootPID, walk, next)
	}
	if !bridgePagingExhausted(bpage) {
		markIncompleteBridgePaging(section, walk, bpage)
	}
	walk.completeNode(graphNodeKey{Project: pipePID, Pipeline: pipe.ID})
	if err := walkQueuedChildren(ctx, section, rootPID, sel, d, actorID, upper, expires, walk, budget); err != nil {
		return err
	}
	if len(walk.queue) == 0 && !walk.unseen && walk.cap == bridgeCapabilityBridges && bridgePagingExhausted(bpage) {
		walk.coverage = downstreamCoverageComplete
		walk.unseen = false
	} else if len(walk.queue) > 0 || !bridgePagingExhausted(bpage) {
		if walk.coverage == downstreamCoverageComplete {
			walk.coverage = downstreamCoveragePartial
		}
		walk.unseen = true
	}
	return nil
}

func bridgePagingExhausted(page bridgePage) bool {
	return !page.Partial && page.Paging.ExhaustedObserved
}

func markIncompleteBridgePaging(section *readmeta.Section, walk *graphWalk, page bridgePage) {
	if walk != nil {
		walk.unseen = true
		walk.stickyIncomplete = true
		if page.Paging.PagingKnown {
			walk.coverage = downstreamCoveragePartial
		} else {
			walk.coverage = downstreamCoverageUnknown
		}
	}
	if section == nil {
		return
	}
	if page.Paging.PagingKnown {
		section.AddLimitation(readmeta.CodePartial, "bridge paging is not exhausted")
		return
	}
	section.AddLimitation(readmeta.CodeUnknownCount, "paging metadata unavailable")
}

func mintBridgeCursor(section *readmeta.Section, d Deps, actorID int64, pipePID string, pipelineID int64, sha string, sel graphSelection, upper, expires string, pageNum int, page bridgePage, mrProject string, walk *graphWalk, nextPage int64) error {
	if section == nil || page.Partial || !page.Paging.PagingKnown || len(page.Bridges) == 0 || nextPage < 1 {
		return nil
	}
	ids := make([]string, 0, len(page.Bridges))
	for _, br := range page.Bridges {
		ids = append(ids, bridgeGuardToken(br))
	}
	walk.phase = cursor.GraphPhaseBridges
	tok, err := mintGraphCursor(d, actorID, pipePID, pipelineID, sha, sel, upper, expires, pageNum, ids, nextPage, len(page.Bridges), mrProject, nil, walk.snapshotCont())
	if err != nil {
		section.AddLimitation(readmeta.CodePartial, "continuation cursor was not issued")
		return nil
	}
	section.NextCursor = &tok
	return nil
}

func walkQueuedChildren(ctx context.Context, section *readmeta.Section, rootPID string, sel graphSelection, d Deps, actorID int64, upper, expires string, walk *graphWalk, budget *igl.Budget) error {
	if section == nil {
		return fmt.Errorf("%s: graph continuation missing", cursor.ResyncRequired)
	}
	for {
		if err := ctx.Err(); err != nil {
			walk.unseen = true
			walk.coverage = downstreamCoveragePartial
			return nil
		}
		next, ok := walk.popNext()
		if !ok {
			return nil
		}
		walk.current = next.Key
		walk.depth = next.Depth
		walk.ancestors = next.Ancestors
		walk.jobsEv, walk.bridgesEv = "", ""
		kind, err := walk.authorize(ctx, d, next.Key.Project)
		if err != nil {
			return err
		}
		if kind != "" {
			walk.markQueuedFailure(next, kind, "authorize_before_content")
			continue
		}
		child, err := loadPipeline(ctx, d, next.Key.Project, next.Key.Pipeline)
		if err != nil {
			if errors.Is(err, errPipelineForbidden) {
				walk.markQueuedFailure(next, edgeKindInaccessible, "child_403")
				continue
			}
			return err
		}
		if child == nil {
			walk.markQueuedFailure(next, edgeKindInaccessible, "child_missing")
			continue
		}
		scope, err := bindPipelineProject(ctx, d, next.Key.Project, child.ProjectID)
		if err != nil {
			kind, aerr := walk.authorize(ctx, d, next.Key.Project)
			if aerr != nil {
				return aerr
			}
			if kind == "" {
				return err
			}
			walk.markQueuedFailure(next, kind, "bind_child")
			continue
		}
		child.ScopeProject = scope
		page, err := collectJobPage(ctx, d, budget, scope, child.ID, 1, sel.PerPage, nil)
		if err != nil {
			return err
		}
		walk.current = next.Key
		walk.pipe = child
		walk.depth = next.Depth
		walk.phase = cursor.GraphPhaseJobs
		walk.jobsEv = chainEvidence("", jobGuardTokens(page.Jobs)...)
		groups := buildLineage(page.Jobs, lineageCarry{})
		walk.recordOutcomes(groups)
		attemptOf := map[int64]string{}
		for _, g := range groups {
			for id, attempt := range g.Attempts {
				attemptOf[id] = attempt
			}
		}
		views := make([]jobView, 0, len(page.Jobs))
		returned := map[int64]struct{}{}
		for _, job := range page.Jobs {
			if sel.Filter.active() && !sel.Filter.match(job) {
				continue
			}
			returned[job.ID] = struct{}{}
			views = append(views, jobToView(job, attemptOf[job.ID]))
		}
		lineage := make([]lineageView, 0, len(groups))
		for _, g := range groups {
			view, lok := lineageViewFor(g, returned, sel.Filter.active())
			if !lok {
				continue
			}
			lineage = append(lineage, view)
		}
		walk.nodes = append(walk.nodes, graphNodeView{
			ProjectID:  scope,
			PipelineID: child.ID,
			Depth:      next.Depth,
			Role:       nodeRoleDownstream,
			Pipeline:   child,
			Jobs:       views,
			Lineage:    lineage,
		})
		childSHA, _ := readmeta.ObservedHeadSHA(deref(child.SHA))
		nextJobs, moreJobs := pagingContinues(page.Paging, 1)
		childJobsExhausted := !moreJobs && !page.Partial && page.Paging.PagingKnown
		if !childJobsExhausted {
			walk.unseen = true
			walk.coverage = downstreamCoveragePartial
			if moreJobs && childSHA != "" && len(page.Jobs) > 0 {
				nextLineage := encodeLineageCarry(mergeLineageCarry(lineageCarry{}, page.Jobs))
				if tok, err := mintGraphCursor(d, actorID, scope, child.ID, childSHA, sel, upper, expires, 1, jobGuardTokens(page.Jobs), nextJobs, len(page.Jobs), rootPID, nextLineage, walk.snapshotCont()); err == nil {
					section.NextCursor = &tok
				}
			}
			return nil
		}
		if err := walkBridgesAndChildren(ctx, section, rootPID, scope, child, childSHA, sel, d, actorID, upper, expires, walk, budget); err != nil {
			return err
		}
		if section.NextCursor != nil {
			return nil
		}
	}
}

func mintGraphCursor(d Deps, actorID int64, pid string, pipelineID int64, sha string, sel graphSelection, upper, expires string, pageNum int, ids []string, nextPage int64, itemsOnPage int, mrProject string, lineage []string, gc *cursor.GraphCont) (string, error) {
	instance, err := cursorInstance(d.Config)
	if err != nil {
		return "", err
	}
	last := ""
	if len(ids) > 0 {
		last = ids[len(ids)-1]
	}
	id := pipelineID
	payload := cursor.Payload{
		SchemaVersion: cursor.SchemaV1,
		Instance:      instance,
		ActorID:       actorID,
		PolicyFP:      d.Config.PolicyFingerprint(),
		Tool:          toolPipelineGraph,
		Section:       sectionPipelineGraph,
		Scope:         cursor.Scope{Kind: cursor.ScopePipeline, ProjectID: pid, PipelineID: &id},
		Filters:       graphFilters(sel, upper, mrProject),
		ImmutableRefs: []string{sha},
		UpperBound:    upper,
		ExpiresAt:     expires,
		GraphCont:     gc,
		PageState: cursor.PageState{
			Page:             pageNum,
			PerPage:          sel.PerPage,
			SequenceDigest:   cursor.SequenceDigest(ids),
			LastSHA:          last,
			ItemsOnPage:      itemsOnPage,
			ProviderNextPage: nextPage,
			LineageMax:       lineage,
		},
	}
	return cursor.Encode(d.Config.CursorKey, payload)
}

func graphFilters(sel graphSelection, upper, mrProject string) cursor.Filters {
	mr := ""
	if sel.MRIID > 0 {
		mr = strconv.FormatInt(sel.MRIID, 10)
	}
	return cursor.Filters{
		RefName:     sel.ExpectedSHA,
		Path:        filterCanonical(sel.Filter) + fmt.Sprintf(";depth=%d;nodes=%d", sel.MaxDepth, sel.MaxNodes),
		Since:       mr,
		CallerUntil: mrProject,
		Until:       upper,
		Order:       "provider",
		Selection:   "graph",
		PerPage:     sel.PerPage,
	}
}

func applySignedGraphBounds(in *pipelineGraphIn, f cursor.Filters) {
	if in == nil {
		return
	}
	if f.PerPage > 0 {
		in.PerPage = f.PerPage
	}
	if depth, nodes, ok := graphBoundsFromFilterPath(f.Path); ok {
		in.MaxDepth = depth
		in.MaxNodes = nodes
	}
}

func graphBoundsFromFilterPath(path string) (depth, nodes int, ok bool) {
	const depthKey = ";depth="
	const nodesKey = ";nodes="
	di := strings.LastIndex(path, depthKey)
	ni := strings.LastIndex(path, nodesKey)
	if di < 0 || ni <= di {
		return 0, 0, false
	}
	d, derr := strconv.Atoi(path[di+len(depthKey) : ni])
	n, nerr := strconv.Atoi(path[ni+len(nodesKey):])
	if derr != nil || nerr != nil || d < 1 || n < 1 {
		return 0, 0, false
	}
	return d, n, true
}

// lineageViewFor copies a group into the response. Assessment still uses
// every group on the page. A filter omits a group with no returned job and
// drops ids that were not returned. LatestKnown stays true when that
// latest id is in the returned set.
func lineageViewFor(g lineageGroup, returned map[int64]struct{}, filter bool) (lineageView, bool) {
	if filter && !lineageHasReturned(g, returned) {
		return lineageView{}, false
	}
	latest, history := g.LatestIDs, g.HistoryIDs
	known := g.LatestKnown
	if filter {
		latest = idsInReturned(g.LatestIDs, returned)
		history = idsInReturned(g.HistoryIDs, returned)
		known = false
		for _, id := range g.LatestIDs {
			if _, ok := returned[id]; ok {
				known = true
				break
			}
		}
	}
	if latest == nil {
		latest = []int64{}
	}
	if history == nil {
		history = []int64{}
	}
	return lineageView{Name: g.Name, LatestKnown: known, LatestIDs: latest, HistoryIDs: history}, true
}

func lineageHasReturned(g lineageGroup, returned map[int64]struct{}) bool {
	for id := range g.Attempts {
		if _, ok := returned[id]; ok {
			return true
		}
	}
	return false
}

func idsInReturned(ids []int64, returned map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := returned[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

func lineageCarryCovers(prior lineageCarry, jobs []graphJob) bool {
	for _, job := range jobs {
		if !job.NameKnown {
			continue
		}
		maxID, ok := prior.max[jobNameFP(job.Name)]
		if ok {
			if job.ID > maxID {
				return false
			}
			continue
		}
		if prior.saturated {
			continue
		}
		return false
	}
	return true
}

func hasPolicyOutcome(outcomes []policyOutcome, want string) bool {
	for _, o := range outcomes {
		if o.Outcome == want {
			return true
		}
	}
	return false
}

func bridgeGuardToken(br graphBridge) string {
	sha := br.ChildSHA
	if sha == "" {
		sha = "-"
	}
	present := "0"
	if br.ChildPresent {
		present = "1"
	}
	return strings.Join([]string{
		strconv.FormatInt(br.Job.ID, 10),
		strconv.FormatInt(br.ChildProject, 10),
		strconv.FormatInt(br.ChildPipeline, 10),
		sha,
		present,
		br.Job.Status,
		allowPresenceLabel(br.Job.Allow, br.Job.AllowValid),
	}, ":")
}

func jobToView(job graphJob, attempt string) jobView {
	if attempt == "" {
		attempt = attemptUnknown
	}
	view := jobView{
		ID:           job.ID,
		AllowFailure: allowPresenceLabel(job.Allow, job.AllowValid),
		Attempt:      attempt,
		Policy:       jobPolicy(job).Outcome,
	}
	if job.NameKnown {
		name := job.Name
		view.Name = &name
	}
	if job.StageKnown {
		stage := job.Stage
		view.Stage = &stage
	}
	if job.StatusKnown {
		status := job.Status
		view.Status = &status
	}
	return view
}

func allowPresenceLabel(p readmeta.Presence, valid bool) string {
	if !valid {
		return "unknown"
	}
	switch p {
	case readmeta.PresenceTrue:
		return "true"
	case readmeta.PresenceFalse:
		return "false"
	case readmeta.PresenceNull:
		return "null"
	default:
		return "absent"
	}
}

func newPipelineGraphSection(now time.Time) readmeta.Section {
	return readmeta.Section{
		RetrievedAt:       now.UTC().Format(time.RFC3339),
		Source:            readmeta.SourceGitLabREST,
		Provider:          readmeta.ProviderGitLab,
		CapabilityVersion: capabilityPipelineGraphV1,
		HeadSHA:           nil,
		Limitations:       []readmeta.Limitation{},
		Counts:            readmeta.Counts{},
		ContentComplete:   readmeta.ContentCompleteUnknown,
		Consistency:       readmeta.ConsistencyUnknown,
		ManifestCoverage:  readmeta.CoverageUnknown,
		PatchCoverage:     readmeta.CoverageUnknown,
	}
}

func loadPipeline(ctx context.Context, d Deps, pid string, id int64) (*pipelineView, error) {
	p, _, err := d.Client.Pipelines.GetPipeline(pid, id, gitlab.WithContext(ctx))
	if err != nil {
		if providerStatus(err) == http.StatusNotFound {
			return nil, nil
		}
		if providerStatus(err) == http.StatusForbidden {
			return nil, errPipelineForbidden
		}
		return nil, safeProviderErr(err)
	}
	if p == nil || p.ID < 1 {
		return nil, fmt.Errorf("%s: pipeline identity", readmeta.CodeIdentityUnresolved)
	}
	view := &pipelineView{ID: p.ID, ProjectID: p.ProjectID, ScopeProject: pid}
	if s := strings.TrimSpace(p.Status); s != "" {
		view.Status = &s
		view.StatusKnown = true
	}
	if s := strings.TrimSpace(string(p.Source)); s != "" {
		view.Source = &s
	}
	if s := strings.TrimSpace(p.Ref); s != "" {
		view.Ref = &s
	}
	if sha, ok := readmeta.ObservedHeadSHA(p.SHA); ok {
		view.SHA = &sha
	}
	return view, nil
}

func loadMergeRequest(ctx context.Context, d Deps, pid string, iid int64) (*mrFacts, error) {
	mr, _, err := d.Client.MergeRequests.GetMergeRequest(pid, iid, nil, gitlab.WithContext(ctx))
	if err != nil {
		if providerStatus(err) == http.StatusNotFound {
			return nil, nil
		}
		return nil, safeProviderErr(err)
	}
	if mr == nil || mr.IID < 1 {
		return nil, fmt.Errorf("%s: merge request identity", readmeta.CodeIdentityUnresolved)
	}
	facts := &mrFacts{SourceBranch: strings.TrimSpace(mr.SourceBranch)}
	if mr.HeadPipeline != nil && mr.HeadPipeline.ID > 0 {
		facts.HeadID = mr.HeadPipeline.ID
		if mr.HeadPipeline.ProjectID > 0 {
			facts.HeadProject = mr.HeadPipeline.ProjectID
		}
	}
	return facts, nil
}

// bindPipelineProject authorizes the pipeline's own project. A fork head
// pipeline lives in the source project, not the merge request target.
// reported <= 0 keeps the already authorized project.
func bindPipelineProject(ctx context.Context, d Deps, authorized string, reported int64) (string, error) {
	if reported < 1 {
		return authorized, nil
	}
	want := strconv.FormatInt(reported, 10)
	if want == authorized {
		return authorized, nil
	}
	return resolveCursorProjectCanonical(ctx, d, want)
}

func projectForChosen(chosen int64, mr *mrFacts, refs []pipelineRef) int64 {
	if mr != nil && chosen == mr.HeadID && mr.HeadProject > 0 {
		return mr.HeadProject
	}
	for _, ref := range refs {
		if ref.ID == chosen && ref.ProjectID > 0 {
			return ref.ProjectID
		}
	}
	return 0
}

func pipelineRefIDs(refs []pipelineRef) []int64 {
	ids := make([]int64, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.ID)
	}
	return ids
}

func pipelineListed(ctx context.Context, d Deps, pid string, iid, pipelineID int64) (bool, bool, error) {
	refs, exhausted, err := listMRPipelineRefs(ctx, d, pid, iid, pipelineID)
	if err != nil {
		return false, false, err
	}
	for _, ref := range refs {
		if ref.ID == pipelineID {
			return true, exhausted, nil
		}
	}
	return false, exhausted, nil
}

// listMRPipelineRefs lists merge request pipelines. stopAfter > 0 returns
// on the page that contains that id and never walks the rest of the list.
// The page cap below runs only when the merge request has no head id.
func listMRPipelineRefs(ctx context.Context, d Deps, pid string, iid, stopAfter int64) ([]pipelineRef, bool, error) {
	if stopAfter > 0 {
		return listMRPipelinesUntil(ctx, d, pid, iid, stopAfter)
	}
	return listMRPipelinesExhaustive(ctx, d, pid, iid)
}

func listMRPipelinesUntil(ctx context.Context, d Deps, pid string, iid, stopAfter int64) ([]pipelineRef, bool, error) {
	var ids []pipelineRef
	for page := 1; page <= 50; page++ {
		batch, obs, stop, err := fetchMRPipelinePage(ctx, d, pid, iid, page)
		if err != nil {
			return nil, false, err
		}
		if stop {
			return ids, false, nil
		}
		ids = append(ids, batch...)
		for _, ref := range batch {
			if ref.ID == stopAfter {
				return ids, false, nil
			}
		}
		if !obs.PagingKnown || obs.ExhaustedObserved || obs.SDKNextPage != int64(page)+1 {
			return ids, obs.PagingKnown && obs.ExhaustedObserved, nil
		}
	}
	return ids, false, nil
}

func listMRPipelinesExhaustive(ctx context.Context, d Deps, pid string, iid int64) ([]pipelineRef, bool, error) {
	var ids []pipelineRef
	for page := 1; page <= 50; page++ {
		batch, obs, stop, err := fetchMRPipelinePage(ctx, d, pid, iid, page)
		if err != nil {
			return nil, false, err
		}
		if stop {
			return ids, false, nil
		}
		ids = append(ids, batch...)
		if !obs.PagingKnown {
			return ids, false, nil
		}
		if obs.ExhaustedObserved {
			return ids, true, nil
		}
		if obs.SDKNextPage != int64(page)+1 {
			return ids, false, nil
		}
	}
	return ids, false, nil
}

func fetchMRPipelinePage(ctx context.Context, d Deps, pid string, iid int64, page int) ([]pipelineRef, readmeta.PagingObservation, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, readmeta.PagingObservation{}, false, err
	}
	path := fmt.Sprintf("projects/%s/merge_requests/%d/pipelines", gitlab.PathEscape(pid), iid)
	opt := &gitlab.ListOptions{Page: int64(page), PerPage: graphDefaultPerPage}
	var batch []pipelineRef
	broken := false
	resp, err := igl.StreamJSONArrayQueue(ctx, d.Client, http.MethodGet, path, opt, func(raw json.RawMessage) error {
		ref, ok := parsePipelineRef(raw)
		if !ok {
			broken = true
			return fmt.Errorf("%s: malformed pipeline list", readmeta.CodePartial)
		}
		batch = append(batch, ref)
		return nil
	})
	if broken {
		return batch, readmeta.PagingObservation{}, true, nil
	}
	if err != nil {
		return nil, readmeta.PagingObservation{}, false, safeProviderErr(err)
	}
	return batch, observeResp(resp), false, nil
}

func collectJobPage(ctx context.Context, d Deps, budget *igl.Budget, pid string, pipelineID int64, page, perPage int, prior map[int64]struct{}) (graphPage, error) {
	path := fmt.Sprintf("projects/%s/pipelines/%d/jobs", gitlab.PathEscape(pid), pipelineID)
	opt := &gitlab.ListJobsOptions{
		ListOptions:    gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		IncludeRetried: gitlab.Ptr(true),
	}
	out := graphPage{}
	seen := map[int64]struct{}{}
	resp, err := igl.StreamJSONArrayQueue(ctx, d.Client, http.MethodGet, path, opt, func(raw json.RawMessage) error {
		if err := ctx.Err(); err != nil {
			out.Partial = true
			out.Reason = "cancelled"
			return err
		}
		job, perr := parseGraphJob(raw)
		if perr != nil {
			out.Partial = true
			out.Reason = "malformed job"
			return fmt.Errorf("%s: malformed job", readmeta.CodePartial)
		}
		if _, ok := seen[job.ID]; ok {
			out.Partial = true
			out.Reason = "duplicate job"
			return fmt.Errorf("%s: duplicate job", readmeta.CodePartial)
		}
		if _, ok := prior[job.ID]; ok {
			out.Partial = true
			out.Reason = "previous-page overlap"
			return fmt.Errorf("%s: previous-page overlap", readmeta.CodePartial)
		}
		if err := budget.AddItem(); err != nil {
			out.Partial = true
			out.Reason = "budget_items"
			return err
		}
		seen[job.ID] = struct{}{}
		out.Jobs = append(out.Jobs, job)
		return nil
	})
	if out.Partial {
		if out.Reason == "previous-page overlap" {
			return out, fmt.Errorf("%s: previous-page overlap", cursor.ResyncRequired)
		}
		return out, nil
	}
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return graphPage{}, err
		}
		if errors.Is(err, igl.ErrBudgetItems) || errors.Is(err, igl.ErrBudgetBytes) || errors.Is(err, igl.ErrBudgetRequests) || errors.Is(err, igl.ErrBudgetElapsed) {
			out.Partial = true
			out.Reason = "budget"
			return out, nil
		}
		// A later framing error still leaves complete elements in out.Jobs.
		// Keep that prefix, and do not read paging from the broken response.
		if len(out.Jobs) > 0 && streamDecodeStopped(err) {
			out.Partial = true
			out.Reason = "malformed response"
			return out, nil
		}
		return graphPage{}, safeProviderErr(err)
	}
	out.Paging = observeResp(resp)
	return out, nil
}

func streamDecodeStopped(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "stream array:") || strings.Contains(msg, "stream array element:")
}

func observeResp(resp *gitlab.Response) readmeta.PagingObservation {
	if resp == nil || resp.Response == nil {
		return readmeta.PagingObservation{}
	}
	return readmeta.ObservePaging(resp.Response.Header, resp.NextPage)
}

func parsePipelineRef(raw json.RawMessage) (pipelineRef, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &fields); err != nil {
		return pipelineRef{}, false
	}
	id, ok := positiveJSONID(fields["id"])
	if !ok {
		return pipelineRef{}, false
	}
	projectID, _ := positiveJSONID(fields["project_id"])
	return pipelineRef{ID: id, ProjectID: projectID}, true
}

func jobIDStrings(jobs []graphJob) []string {
	out := make([]string, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, strconv.FormatInt(job.ID, 10))
	}
	return out
}

func jobGuardToken(job graphJob) string {
	raw, err := json.Marshal(struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Stage  string `json:"stage"`
		Status string `json:"status"`
		Allow  string `json:"allow_failure"`
	}{
		ID:     job.ID,
		Name:   job.Name,
		Stage:  job.Stage,
		Status: job.Status,
		Allow:  allowPresenceLabel(job.Allow, job.AllowValid),
	})
	if err != nil {
		return ""
	}
	return string(raw)
}

func jobGuardTokens(jobs []graphJob) []string {
	out := make([]string, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, jobGuardToken(job))
	}
	return out
}

func idSet(ids []string) map[int64]struct{} {
	out := map[int64]struct{}{}
	for _, s := range ids {
		n, err := strconv.ParseInt(s, 10, 64)
		if err == nil && n > 0 {
			out[n] = struct{}{}
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func providerStatus(err error) int {
	if errors.Is(err, gitlab.ErrNotFound) {
		return http.StatusNotFound
	}
	var er *gitlab.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		return er.Response.StatusCode
	}
	return 0
}

func safeProviderErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, igl.ErrBudgetItems) || errors.Is(err, igl.ErrBudgetBytes) || errors.Is(err, igl.ErrBudgetRequests) || errors.Is(err, igl.ErrBudgetElapsed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%s: gitlab request failed", readmeta.CodeHTTPError)
}
