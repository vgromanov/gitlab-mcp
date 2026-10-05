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
}

type pipelineView struct {
	ID          int64   `json:"id"`
	Status      *string `json:"status"`
	Source      *string `json:"source"`
	Ref         *string `json:"ref"`
	SHA         *string `json:"sha"`
	StatusKnown bool    `json:"status_known"`
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
	Retried      string  `json:"retried"`
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
}

type graphSelection struct {
	ExpectedSHA string
	MRIID       int64
	Filter      jobFilter
	PerPage     int
}

type graphPage struct {
	Jobs    []graphJob
	Partial bool
	Reason  string
	Paging  readmeta.PagingObservation
}

// getMergeRequestPipelineGraph reads one parent pipeline and one page of its
// jobs. It does not traverse bridges, play jobs, or embed review context.
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
	if in.MergeRequestIID < 0 || in.PipelineID < 0 || in.PerPage < 0 || in.MaxItems < 0 || in.MaxRequests < 0 {
		return nil, nil, fmt.Errorf("invalid pipeline graph input")
	}
	if in.PerPage > 50 {
		return nil, nil, fmt.Errorf("per_page must be between 1 and 50")
	}
	sel, err := normalizeGraphSelection(in)
	if err != nil {
		return nil, nil, err
	}
	budget := igl.DefaultBudget()
	if in.MaxItems > 0 {
		budget.CapLimits(in.MaxItems, 0, 0)
	}
	if in.MaxRequests > 0 {
		budget.CapLimits(0, 0, in.MaxRequests)
	}
	ctx = igl.WithBudget(ctx, budget)
	defer budget.Cancel()

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
	return graphSelection{ExpectedSHA: expected, MRIID: in.MergeRequestIID, Filter: filter, PerPage: per}, nil
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
	page, err := collectJobPage(ctx, d, budget, pid, chosen.ID, 1, sel.PerPage, nil)
	if err != nil {
		return nil, nil, err
	}
	upper := now.UTC().Format(time.RFC3339Nano)
	expires := now.UTC().Add(d.Config.CursorTTL()).Format(time.RFC3339Nano)
	return nil, Out(finishGraph(section, pid, mrIID, chosen, rel, page, sel, d, actorID, upper, expires, 1, true)), nil
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
	if payload.Scope.ProjectID != pid || payload.Scope.PipelineID == nil || in.PipelineID != *payload.Scope.PipelineID {
		return nil, nil, fmt.Errorf("%s: project scope mismatch", cursor.ResyncRequired)
	}
	if graphFilters(sel, payload.UpperBound) != payload.Filters {
		return nil, nil, fmt.Errorf("%s: filter mismatch", cursor.ResyncRequired)
	}
	if err := reauthorizeCursorProject(ctx, d, canon); err != nil {
		return nil, nil, err
	}
	pipe, err := loadPipeline(ctx, d, pid, *payload.Scope.PipelineID)
	if err != nil {
		return nil, nil, err
	}
	if pipe == nil || pipe.SHA == nil || len(payload.ImmutableRefs) != 1 || *pipe.SHA != payload.ImmutableRefs[0] {
		return nil, nil, fmt.Errorf("%s: pinned pipeline SHA mismatch", cursor.ResyncRequired)
	}
	scope := cursor.Scope{Kind: cursor.ScopePipeline, ProjectID: pid, PipelineID: payload.Scope.PipelineID}
	if err := cursor.MatchBinding(payload, instance, actorID, d.Config.PolicyFingerprint(), toolPipelineGraph, sectionPipelineGraph, scope, payload.Filters, payload.ImmutableRefs, payload.UpperBound); err != nil {
		return nil, nil, fmt.Errorf("%s: binding mismatch", cursor.ResyncRequired)
	}
	guard, err := collectJobPage(ctx, d, budget, pid, pipe.ID, payload.PageState.Page, sel.PerPage, nil)
	if err != nil || guard.Partial {
		return nil, nil, fmt.Errorf("%s: previous-page guard failed", cursor.ResyncRequired)
	}
	ids := jobIDStrings(guard.Jobs)
	last := ""
	if len(ids) > 0 {
		last = ids[len(ids)-1]
	}
	if cursor.SequenceDigest(ids) != payload.PageState.SequenceDigest || last != payload.PageState.LastSHA || len(ids) != payload.PageState.ItemsOnPage {
		return nil, nil, fmt.Errorf("%s: previous-page boundary drift", cursor.ResyncRequired)
	}
	if !guard.Paging.PagingKnown || guard.Paging.SDKNextPage != payload.PageState.ProviderNextPage || payload.PageState.ProviderNextPage != int64(payload.PageState.Page)+1 {
		return nil, nil, fmt.Errorf("%s: previous-page boundary drift", cursor.ResyncRequired)
	}
	nextPage := int(payload.PageState.ProviderNextPage)
	page, err := collectJobPage(ctx, d, budget, pid, pipe.ID, nextPage, sel.PerPage, idSet(ids))
	if err != nil {
		return nil, nil, err
	}
	rel := classifyRelation(relationInput{
		MRIID:       sel.MRIID,
		ListChecked: false,
		Ref:         deref(pipe.Ref),
		PipelineSHA: deref(pipe.SHA),
		ExpectedSHA: sel.ExpectedSHA,
	})
	if sel.MRIID > 0 {
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
	section := newPipelineGraphSection(now)
	var mrIID *int64
	if sel.MRIID > 0 {
		mrIID = &sel.MRIID
	}
	return nil, Out(finishGraph(section, pid, mrIID, pipe, rel, page, sel, d, actorID, payload.UpperBound, payload.ExpiresAt, nextPage, false)), nil
}

type mrFacts struct {
	SourceBranch string
	HeadID       int64
}

func resolveParentPipeline(ctx context.Context, d Deps, pid string, pipelineID int64, sel graphSelection) (*pipelineView, relationResult, []readmeta.Limitation, error) {
	rel := relationResult{Kind: relUnproven, Evidence: []string{}, SHAComparison: shaUnknown}
	if pipelineID > 0 {
		pipe, err := loadPipeline(ctx, d, pid, pipelineID)
		if err != nil {
			return nil, rel, nil, err
		}
		if pipe == nil {
			rel.SHAComparison = compareSHA("", sel.ExpectedSHA)
			return nil, rel, []readmeta.Limitation{{Code: readmeta.CodeInaccessible, Message: "pipeline not found"}}, nil
		}
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
	ids, exhausted, err := listMRPipelineIDs(ctx, d, pid, sel.MRIID)
	if err != nil {
		return nil, rel, nil, err
	}
	chosenID, ambiguous, missing := selectParentID(mr.HeadID, ids, exhausted)
	if missing || ambiguous {
		lim := readmeta.Limitation{Code: readmeta.CodeUnknownCount, Message: "parent pipeline is ambiguous"}
		if missing {
			lim = readmeta.Limitation{Code: readmeta.CodeInaccessible, Message: "merge request has no pipeline"}
		}
		return nil, relationResult{Kind: relUnproven, Evidence: []string{}, SHAComparison: compareSHA("", sel.ExpectedSHA)}, []readmeta.Limitation{lim}, nil
	}
	pipe, err := loadPipeline(ctx, d, pid, chosenID)
	if err != nil {
		return nil, rel, nil, err
	}
	if pipe == nil {
		return nil, rel, []readmeta.Limitation{{Code: readmeta.CodeInaccessible, Message: "pipeline not found"}}, nil
	}
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
	}
	if !exhausted {
		return 0, true, false
	}
	if head > 0 {
		if _, ok := seen[head]; !ok && len(ids) == 1 {
			return ids[0], false, false
		}
		if _, ok := seen[head]; ok {
			return head, false, false
		}
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

func finishGraph(section readmeta.Section, pid string, mrIID *int64, pipe *pipelineView, rel relationResult, page graphPage, sel graphSelection, d Deps, actorID int64, upper, expires string, pageNum int, fromStart bool) pipelineGraphOut {
	section.AddLimitation(readmeta.CodeUnsupported, "downstream coverage is unknown")
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
	groups := buildLineage(page.Jobs)
	var outcomes []policyOutcome
	for _, g := range groups {
		outcomes = append(outcomes, g.Outcomes...)
	}
	assessment, reasons := assessParent(assessInput{
		Filter:         sel.Filter.active(),
		JobsPartial:    page.Partial || !page.Paging.ExhaustedObserved,
		RelationProven: rel.Proven,
		Outcomes:       outcomes,
	})
	views := make([]jobView, 0, len(page.Jobs))
	lineage := make([]lineageView, 0, len(groups))
	attemptOf := map[int64]string{}
	for _, g := range groups {
		for id, attempt := range g.Attempts {
			attemptOf[id] = attempt
		}
		latest := g.LatestIDs
		if latest == nil {
			latest = []int64{}
		}
		history := g.HistoryIDs
		if history == nil {
			history = []int64{}
		}
		lineage = append(lineage, lineageView{Name: g.Name, LatestKnown: g.LatestKnown, LatestIDs: latest, HistoryIDs: history})
	}
	for _, job := range page.Jobs {
		if sel.Filter.active() && !sel.Filter.match(job) {
			continue
		}
		views = append(views, jobToView(job, attemptOf[job.ID]))
	}
	if views == nil {
		views = []jobView{}
	}
	n := len(views)
	section.Counts.Items = &n
	exhausted := page.Paging.ExhaustedObserved && !page.Partial
	section.PaginationExhausted = exhausted
	complete := fromStart && exhausted && !sel.Filter.active() && !page.Partial && page.Paging.PagingKnown
	if complete {
		section.ContentComplete = readmeta.ContentCompleteTrue
		section.ManifestCoverage = readmeta.CoverageFull
	} else if page.Partial || sel.Filter.active() || !exhausted {
		section.ContentComplete = readmeta.ContentCompleteFalse
		section.ManifestCoverage = readmeta.CoveragePartial
	} else {
		section.ContentComplete = readmeta.ContentCompleteFalse
		section.ManifestCoverage = readmeta.CoveragePartial
	}
	section.PatchCoverage = readmeta.CoverageUnknown
	if rel.Proven {
		section.Consistency = readmeta.ConsistencyConsistent
	} else {
		section.Consistency = readmeta.ConsistencyUnknown
	}
	if !page.Partial && page.Paging.PagingKnown && !page.Paging.ExhaustedObserved && page.Paging.SDKNextPage == int64(pageNum)+1 && ok && len(page.Jobs) > 0 {
		if tok, err := mintGraphCursor(d, actorID, pid, pipe.ID, sha, sel, upper, expires, pageNum, page); err == nil {
			section.NextCursor = &tok
		} else {
			section.AddLimitation(readmeta.CodePartial, "continuation cursor was not issued")
			section.ContentComplete = readmeta.ContentCompleteFalse
		}
	} else if !exhausted && section.NextCursor == nil && !page.Paging.PagingKnown {
		section.AddLimitation(readmeta.CodeUnknownCount, "paging metadata unavailable")
		if section.ContentComplete == readmeta.ContentCompleteTrue {
			section.ContentComplete = readmeta.ContentCompleteUnknown
		}
	}
	if assessment == "ready" {
		assessment = assessUnknown
		reasons = sortReasons(append(reasons, "downstream_unknown"))
	}
	if rel.Evidence == nil {
		rel.Evidence = []string{}
	}
	id := pipe.ID
	return pipelineGraphOut{
		Section:            section,
		ProjectID:          pid,
		MergeRequestIID:    mrIID,
		PipelineID:         &id,
		Pipeline:           pipe,
		Relation:           relationView{Kind: rel.Kind, Proven: rel.Proven, Evidence: rel.Evidence, SHAComparison: rel.SHAComparison},
		Jobs:               views,
		Lineage:            lineage,
		Assessment:         assessment,
		DownstreamCoverage: downstreamCoverageUnknown,
		BridgesVisited:     false,
		JobFilterApplied:   sel.Filter.active(),
		Reasons:            reasons,
	}
}

func mintGraphCursor(d Deps, actorID int64, pid string, pipelineID int64, sha string, sel graphSelection, upper, expires string, pageNum int, page graphPage) (string, error) {
	instance, err := cursorInstance(d.Config)
	if err != nil {
		return "", err
	}
	ids := jobIDStrings(page.Jobs)
	last := ids[len(ids)-1]
	id := pipelineID
	payload := cursor.Payload{
		SchemaVersion: cursor.SchemaV1,
		Instance:      instance,
		ActorID:       actorID,
		PolicyFP:      d.Config.PolicyFingerprint(),
		Tool:          toolPipelineGraph,
		Section:       sectionPipelineGraph,
		Scope:         cursor.Scope{Kind: cursor.ScopePipeline, ProjectID: pid, PipelineID: &id},
		Filters:       graphFilters(sel, upper),
		ImmutableRefs: []string{sha},
		UpperBound:    upper,
		ExpiresAt:     expires,
		PageState: cursor.PageState{
			Page:             pageNum,
			PerPage:          sel.PerPage,
			SequenceDigest:   cursor.SequenceDigest(ids),
			LastSHA:          last,
			ItemsOnPage:      len(page.Jobs),
			ProviderNextPage: page.Paging.SDKNextPage,
		},
	}
	return cursor.Encode(d.Config.CursorKey, payload)
}

func graphFilters(sel graphSelection, upper string) cursor.Filters {
	mr := ""
	if sel.MRIID > 0 {
		mr = strconv.FormatInt(sel.MRIID, 10)
	}
	return cursor.Filters{
		RefName:   sel.ExpectedSHA,
		Path:      filterCanonical(sel.Filter),
		Since:     mr,
		Until:     upper,
		Order:     "provider",
		Selection: "parent_jobs",
		PerPage:   sel.PerPage,
	}
}

func jobToView(job graphJob, attempt string) jobView {
	if attempt == "" {
		attempt = attemptUnknown
	}
	view := jobView{
		ID:           job.ID,
		AllowFailure: allowPresenceLabel(job.Allow, job.AllowValid),
		Retried:      allowPresenceLabel(job.Retried, job.RetriedValid),
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
		return nil, safeProviderErr(err)
	}
	if p == nil || p.ID < 1 {
		return nil, fmt.Errorf("%s: pipeline identity", readmeta.CodeIdentityUnresolved)
	}
	view := &pipelineView{ID: p.ID}
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
	}
	return facts, nil
}

func pipelineListed(ctx context.Context, d Deps, pid string, iid, pipelineID int64) (bool, bool, error) {
	ids, exhausted, err := listMRPipelineIDs(ctx, d, pid, iid)
	if err != nil {
		return false, false, err
	}
	for _, id := range ids {
		if id == pipelineID {
			return true, exhausted, nil
		}
	}
	return false, exhausted, nil
}

func listMRPipelineIDs(ctx context.Context, d Deps, pid string, iid int64) ([]int64, bool, error) {
	var ids []int64
	page := 1
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, ctx.Err()
		}
		path := fmt.Sprintf("projects/%s/merge_requests/%d/pipelines", gitlab.PathEscape(pid), iid)
		opt := &gitlab.ListOptions{Page: int64(page), PerPage: graphDefaultPerPage}
		var batch []int64
		broken := false
		resp, err := igl.StreamJSONArray(ctx, d.Client, http.MethodGet, path, opt, func(raw json.RawMessage) error {
			id, ok := parseObjectID(raw)
			if !ok {
				broken = true
				return fmt.Errorf("%s: malformed pipeline list", readmeta.CodePartial)
			}
			batch = append(batch, id)
			return nil
		})
		if broken {
			return ids, false, nil
		}
		if err != nil {
			return nil, false, safeProviderErr(err)
		}
		ids = append(ids, batch...)
		obs := observeResp(resp)
		if !obs.PagingKnown {
			return ids, false, nil
		}
		if obs.ExhaustedObserved {
			return ids, true, nil
		}
		if obs.SDKNextPage != int64(page)+1 {
			return ids, false, nil
		}
		page++
		if page > 50 {
			return ids, false, nil
		}
	}
}

func collectJobPage(ctx context.Context, d Deps, budget *igl.Budget, pid string, pipelineID int64, page, perPage int, prior map[int64]struct{}) (graphPage, error) {
	path := fmt.Sprintf("projects/%s/pipelines/%d/jobs", gitlab.PathEscape(pid), pipelineID)
	opt := &gitlab.ListJobsOptions{
		ListOptions:    gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		IncludeRetried: gitlab.Ptr(true),
	}
	out := graphPage{}
	seen := map[int64]struct{}{}
	resp, err := igl.StreamJSONArray(ctx, d.Client, http.MethodGet, path, opt, func(raw json.RawMessage) error {
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
		return graphPage{}, safeProviderErr(err)
	}
	out.Paging = observeResp(resp)
	return out, nil
}

func observeResp(resp *gitlab.Response) readmeta.PagingObservation {
	if resp == nil || resp.Response == nil {
		return readmeta.PagingObservation{}
	}
	return readmeta.ObservePaging(resp.Response.Header, resp.NextPage)
}

func parseObjectID(raw json.RawMessage) (int64, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &fields); err != nil {
		return 0, false
	}
	return positiveJSONID(fields["id"])
}

func jobIDStrings(jobs []graphJob) []string {
	out := make([]string, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, strconv.FormatInt(job.ID, 10))
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
