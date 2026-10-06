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
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

func denseReachServer() *walkServer {
	const perPage = 20
	bridges := map[string]string{
		"42/100/1": bridgePageJSON(1000, perPage),
		"42/100/2": "[" + bridgeJSON(1020, "last-bridge", 0, 0, "") + "]",
	}
	jobs := map[string]string{"42/100/1": "[" + jobJSON(1, "root-job", "success", "false") + "]"}
	pipes := map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")}
	for i := 101; i <= 115; i++ {
		key := fmt.Sprintf("42/%d", i)
		pipes[key] = walkPipe(i, 42, graphPipeSHA, "child")
		jobs[key+"/1"] = "[" + jobJSON(i, "child-job", "success", "false") + "]"
		bridges[key+"/1"] = "[]"
	}
	return &walkServer{
		pipes:   pipes,
		jobs:    jobs,
		bridges: bridges,
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
}

func denseReachTrimmedBridgeCursor(t *testing.T, d Deps) string {
	const perPage = 20
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	setupBudget := igl.DefaultBudget()
	setupBudget.SetMaxItems(1000)
	setupBudget.SetMaxRequests(256)
	ctx := igl.WithBudget(context.Background(), setupBudget)
	budget := igl.BudgetFromContext(ctx)
	sha, ref, status, source := graphPipeSHA, "feature", "success", "push"
	pipe := &pipelineView{ID: 100, SHA: &sha, Ref: &ref, Status: &status, StatusKnown: true, Source: &source}
	jobsEv, err := readJobEvidence(ctx, d, budget, "42", 100, perPage, "", 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	bp, err := collectBridgePage(ctx, d, budget, "42", 100, 1, perPage, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, more := pagingContinues(bp.Paging, 1)
	if !more {
		t.Fatal("expected bridge page 2")
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
	for i := 101; i <= 115; i++ {
		key := graphNodeKey{Project: "42", Pipeline: int64(i)}.String()
		child, err := loadPipeline(ctx, d, "42", int64(i))
		if err != nil || child == nil {
			t.Fatal(err)
		}
		jd, err := readJobEvidence(ctx, d, budget, "42", int64(i), perPage, "", 1, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		bd, err := readBridgeEvidence(ctx, d, budget, "42", int64(i), perPage, "", 1, 0, nil)
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
			ID: int64(9000 + i), Name: fmt.Sprintf("carry-%03d-%s", i, strings.Repeat("n", 180)),
			NameKnown: true, Status: "success", StatusKnown: true,
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
		t.Fatal("cursor not issued")
	}
	return *section.NextCursor
}

func denseReachEvidenceTruncatedBridgeCursor(t *testing.T, d Deps) string {
	const perPage = 20
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	setupBudget := igl.DefaultBudget()
	setupBudget.SetMaxItems(1000)
	setupBudget.SetMaxRequests(256)
	ctx := igl.WithBudget(context.Background(), setupBudget)
	budget := igl.BudgetFromContext(ctx)
	sha, ref, status, source := graphPipeSHA, "feature", "success", "push"
	pipe := &pipelineView{ID: 100, SHA: &sha, Ref: &ref, Status: &status, StatusKnown: true, Source: &source}
	jobsEv, err := readJobEvidence(ctx, d, budget, "42", 100, perPage, "", 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	bp, err := collectBridgePage(ctx, d, budget, "42", 100, 1, perPage, nil)
	if err != nil {
		t.Fatal(err)
	}
	next, more := pagingContinues(bp.Paging, 1)
	if !more {
		t.Fatal("expected bridge page 2")
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
	evKeys := make([]string, 0, 15)
	for i := 101; i <= 115; i++ {
		key := graphNodeKey{Project: "42", Pipeline: int64(i)}.String()
		evKeys = append(evKeys, key)
		child, err := loadPipeline(ctx, d, "42", int64(i))
		if err != nil || child == nil {
			t.Fatal(err)
		}
		jd, err := readJobEvidence(ctx, d, budget, "42", int64(i), perPage, "", 1, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		bd, err := readBridgeEvidence(ctx, d, budget, "42", int64(i), perPage, "", 1, 0, nil)
		if err != nil {
			t.Fatal(err)
		}
		walk.evidence[key] = [3]string{jd, bd, pipelineMetaDigest(child)}
		walk.evidenceItems[key] = 2
		walk.visited[key] = struct{}{}
	}
	for i := len(evKeys) / 2; i < len(evKeys); i++ {
		delete(walk.evidence, evKeys[i])
		delete(walk.evidenceItems, evKeys[i])
	}
	walk.evidenceTruncated = true
	walk.stickyIncomplete = true
	walk.coverage = downstreamCoveragePartial
	lineageJobs := make([]graphJob, 0, lineageFPCap+1)
	for i := 0; i < lineageFPCap+1; i++ {
		lineageJobs = append(lineageJobs, graphJob{
			ID: int64(9000 + i), Name: fmt.Sprintf("carry-%03d-%s", i, strings.Repeat("n", 180)),
			NameKnown: true, Status: "success", StatusKnown: true,
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
		t.Fatal("cursor not issued")
	}
	return *section.NextCursor
}

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
	h := denseReachServer()
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	tok := denseReachTrimmedBridgeCursor(t, d)
	payload, err := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
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
	if payload.GraphCont.Evx {
		t.Fatal("reach-only trim must not set evidence truncation")
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
	in.Cursor = tok
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

func TestPipelineGraph_trimmedEvidenceBlocksCertification(t *testing.T) {
	h := denseReachServer()
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	tok := denseReachEvidenceTruncatedBridgeCursor(t, d)
	payload, err := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !payload.GraphCont.Evx {
		t.Fatal("expected evidence truncation flag on cursor")
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 20, MaxItems: 1000, MaxRequests: 256}
	in.Cursor = tok
	var last map[string]any
	for i := 0; i < 12; i++ {
		in.Cursor = tok
		_, rawOut, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
		if err != nil {
			t.Fatalf("resume %d: %v", i, err)
		}
		last = rawOut.(map[string]any)
		sec := graphSection(t, last)
		next, _ := sec["next_cursor"].(string)
		if next == "" {
			break
		}
		tok = next
	}
	sec := graphSection(t, last)
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("trimmed evidence must not certify content_complete")
	}
	if last["digest"] != nil {
		t.Fatal("trimmed evidence must not emit graph digest")
	}
}
