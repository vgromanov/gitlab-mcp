package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
)

func TestPipelineGraph_trimmedEvidenceReplayStaysResumable(t *testing.T) {
	denseReachBridgeCursorFitsAndResumes(t)
}

func TestPipelineGraph_denseReachBridgeCursorFitsAndResumes(t *testing.T) {
	denseReachBridgeCursorFitsAndResumes(t)
}

func denseReachBridgeCursorFitsAndResumes(t *testing.T) {
	const perPage = 20
	bridges := map[string]string{
		"42/100/1": bridgePageJSON(1000, perPage),
		"42/100/2": "[" + bridgeJSON(1020, "last-bridge", 0, 0, "") + "]",
	}
	jobs := map[string]string{
		"42/100/1": "[" + jobJSON(1, "root-job", "success", "false") + "]",
	}
	pipes := map[string]string{
		"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
	}
	for i := 101; i <= 115; i++ {
		key := fmt.Sprintf("42/%d", i)
		pipes[key] = walkPipe(i, 42, graphPipeSHA, "child")
		jobs[key+"/1"] = "[" + jobJSON(i, "child-job", "success", "false") + "]"
		bridges[key+"/1"] = "[]"
	}
	h := &walkServer{
		pipes:   pipes,
		jobs:    jobs,
		bridges: bridges,
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	setupBudget := igl.DefaultBudget()
	setupBudget.SetMaxItems(1000)
	setupBudget.SetMaxRequests(256)
	ctx := igl.WithBudget(context.Background(), setupBudget)
	budget := igl.BudgetFromContext(ctx)

	sha, ref, status, source := graphPipeSHA, "feature", "success", "push"
	pipe := &pipelineView{ID: 100, SHA: &sha, Ref: &ref, Status: &status, StatusKnown: true, Source: &source}
	jobsEv, err := readJobEvidence(ctx, d, budget, "42", 100, perPage, "", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	bp, err := collectBridgePage(ctx, d, budget, "42", 100, 1, perPage, nil)
	if err != nil || bp.Partial || !bp.Paging.PagingKnown {
		t.Fatalf("bridge page 1: err=%v partial=%v paging=%#v", err, bp.Partial, bp.Paging)
	}
	next, more := pagingContinues(bp.Paging, 1)
	if !more || next != 2 {
		t.Fatalf("expected bridge page 2 continuation %#v", bp.Paging)
	}

	walk := newGraphWalk(pipe, "42", graphDefaultMaxDepth, graphDefaultMaxNodes)
	walk.evidence = map[string][3]string{}
	walk.evidenceItems = map[string]int{}
	walk.jobsEv = jobsEv
	walk.bridgesEv = chainEvidence("", bridgePageTokens(bp)...)
	walk.phase = cursor.GraphPhaseBridges
	walk.nodeCount = cursor.MaxGraphVisited
	for i := 0; i < cursor.MaxGraphVisited; i++ {
		for j := 0; j < cursor.MaxGraphVisited; j++ {
			walk.recordReachEdge(graphEdgeView{
				FromProject:  "42",
				FromPipeline: int64(100 + i),
				ToProject:    "42",
				ToPipeline:   int64(200 + j),
				Kind:         edgeKindBridge,
			})
		}
	}
	if len(walk.reach) != cursor.MaxGraphReachEdges {
		t.Fatalf("reach edges %d", len(walk.reach))
	}
	for i := 101; i <= 115; i++ {
		key := graphNodeKey{Project: "42", Pipeline: int64(i)}.String()
		child, err := loadPipeline(ctx, d, "42", int64(i))
		if err != nil || child == nil {
			t.Fatal(err)
		}
		jd, err := readJobEvidence(ctx, d, budget, "42", int64(i), perPage, "", 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		bd, err := readBridgeEvidence(ctx, d, budget, "42", int64(i), perPage, "", 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		walk.evidence[key] = [3]string{jd, bd, pipelineMetaDigest(child)}
		walk.evidenceItems[key] = 2
		walk.visited[key] = struct{}{}
	}

	lineageJobs := make([]graphJob, 0, lineageFPCap+1)
	for i := 0; i < lineageFPCap+1; i++ {
		lineageJobs = append(lineageJobs, graphJob{
			ID:          int64(9000 + i),
			Name:        fmt.Sprintf("carry-%03d-%s", i, strings.Repeat("n", 180)),
			NameKnown:   true,
			Status:      "success",
			StatusKnown: true,
		})
	}
	prior, err := decodeLineageCarry(encodeLineageCarry(mergeLineageCarry(lineageCarry{}, lineageJobs)))
	if err != nil {
		t.Fatal(err)
	}

	section := newPipelineGraphSection(clk.Now())
	sel := graphSelection{MRIID: 7, PerPage: perPage, MaxDepth: graphDefaultMaxDepth, MaxNodes: graphDefaultMaxNodes}
	upper := clk.Now().UTC().Format(time.RFC3339Nano)
	expires := clk.Now().UTC().Add(d.Config.CursorTTL()).Format(time.RFC3339Nano)
	if err := mintBridgeCursor(&section, d, 7, "42", 100, graphPipeSHA, sel, upper, expires, 1, bp, "42", walk, next, prior); err != nil {
		t.Fatal(err)
	}
	if section.NextCursor == nil {
		t.Fatalf("bridge cursor not issued: %#v", section.Limitations)
	}
	payload, err := cursor.Decode(d.Config.CursorKey, *section.NextCursor, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > cursor.MaxPayloadBytes {
		t.Fatalf("payload %d exceeds %d", len(raw), cursor.MaxPayloadBytes)
	}
	if len(payload.GraphCont.Rg) >= cursor.MaxGraphReachEdges {
		t.Fatalf("expected trimmed reach snapshot, got %d entries", len(payload.GraphCont.Rg))
	}
	if !payload.GraphCont.Rgx {
		t.Fatal("expected reach snapshot truncation flag")
	}
	evKeys := map[string]struct{}{}
	for _, item := range payload.GraphCont.Ev {
		k, _, _, _, ok := cursor.ParseGraphEvidence(item)
		if ok {
			evKeys[k] = struct{}{}
		}
	}
	for _, item := range payload.GraphCont.Ei {
		k, _, ok := cursor.ParseGraphEvidenceReplay(item)
		if !ok {
			t.Fatalf("bad ei %q", item)
		}
		if _, ok := evKeys[k]; !ok {
			t.Fatalf("ei key %q missing from trimmed ev", k)
		}
	}

	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: perPage, MaxItems: 100, MaxRequests: 256}
	in.Cursor = *section.NextCursor
	_, rawOut, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatalf("bridge resume err: %v", err)
	}
	out := rawOut.(map[string]any)
	sec := graphSection(t, out)
	if sectionMessage(sec, "budget_items") || sectionMessage(sec, "continuation cursor was not issued") {
		t.Fatalf("bridge resume failed %#v", sec["limitations"])
	}
	for _, edge := range out["edges"].([]any) {
		e := edge.(map[string]any)
		if e["bridge_id"] == float64(1020) {
			return
		}
	}
	if sec["next_cursor"] == nil && out["downstream_coverage"] != downstreamCoverageComplete {
		t.Fatalf("bridge page 2 not recovered %#v", out)
	}
}
