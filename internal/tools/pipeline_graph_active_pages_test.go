package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
)

func pagedActiveServer() *walkServer {
	return &walkServer{
		pipes: map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(3, "a", "success", "false") + "]",
			"42/100/2": "[" + jobJSON(2, "b", "success", "false") + "]",
			"42/100/3": "[" + jobJSON(1, "c", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "one", 0, 0, "") + "]",
			"42/100/2": "[" + bridgeJSON(51, "two", 0, 0, "") + "]",
			"42/100/3": "[" + bridgeJSON(52, "three", 0, 0, "") + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
}

func advanceGraph(t *testing.T, d Deps, in *pipelineGraphIn, tok string) (string, map[string]any) {
	t.Helper()
	in.Cursor = tok
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, *in, d)
	if err != nil {
		t.Fatal(err)
	}
	out := raw.(map[string]any)
	next, _ := graphSection(t, out)["next_cursor"].(string)
	return next, out
}

func pagedActiveDeps(t *testing.T, h *walkServer) (Deps, pipelineGraphIn) {
	t.Helper()
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	return newCursorDeps(t, h, nil, clk), pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
}

func requireResync(t *testing.T, d Deps, in pipelineGraphIn, tok, what string) {
	t.Helper()
	in.Cursor = tok
	_, _, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("%s not detected: %v", what, err)
	}
}

func TestPipelineGraph_activeJobPageOneDriftBeforePageThree(t *testing.T) {
	h := pagedActiveServer()
	d, in := pagedActiveDeps(t, h)
	tok, _ := advanceGraph(t, d, &in, "")
	tok, _ = advanceGraph(t, d, &in, tok)
	if tok == "" {
		t.Fatal("expected page-3 continuation")
	}
	h.jobs["42/100/1"] = "[" + jobJSON(3, "a", "failed", "false") + "]"
	requireResync(t, d, in, tok, "active page-1 job drift")
}

func TestPipelineGraph_activeJobPagesStableResume(t *testing.T) {
	h := pagedActiveServer()
	d, in := pagedActiveDeps(t, h)
	tok := ""
	var last map[string]any
	for i := 0; i < 10; i++ {
		tok, last = advanceGraph(t, d, &in, tok)
		if tok == "" {
			t.Logf("stop section=%#v", graphSection(t, last))
			break
		}
	}
	if tok != "" || last == nil {
		t.Fatalf("stable paged node must finish: cursor=%q", tok)
	}
}

func bridgePhaseToken(t *testing.T, d Deps, in *pipelineGraphIn, wantPage int) string {
	t.Helper()
	tok := ""
	for i := 0; i < 10; i++ {
		tok, _ = advanceGraph(t, d, in, tok)
		if tok == "" {
			t.Fatal("never reached bridge phase")
		}
		payload, err := cursor.Decode(d.Config.CursorKey, tok, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
		if err == nil && payload.GraphCont != nil && payload.GraphCont.Phase == cursor.GraphPhaseBridges && payload.PageState.Page == wantPage {
			return tok
		}
	}
	t.Fatal("bridge continuation not found")
	return ""
}

func TestPipelineGraph_activeBridgePageOneDriftBeforePageThree(t *testing.T) {
	h := pagedActiveServer()
	d, in := pagedActiveDeps(t, h)
	tok := bridgePhaseToken(t, d, &in, 2)
	h.bridges["42/100/1"] = "[" + bridgeJSON(50, "one", 99, 200, graphChildSHA) + "]"
	requireResync(t, d, in, tok, "active page-1 bridge drift")
}

func TestPipelineGraph_activeJobDriftDuringBridgePhase(t *testing.T) {
	h := pagedActiveServer()
	d, in := pagedActiveDeps(t, h)
	tok := bridgePhaseToken(t, d, &in, 1)
	h.jobs["42/100/2"] = "[" + jobJSON(2, "b", "failed", "false") + "]"
	requireResync(t, d, in, tok, "active job drift while paging bridges")
}

func TestPipelineGraph_bridgeResumeRejectsBoundaryOverlap(t *testing.T) {
	h := pagedActiveServer()
	d, in := pagedActiveDeps(t, h)
	tok := bridgePhaseToken(t, d, &in, 1)
	h.bridges["42/100/2"] = "[" + bridgeJSON(50, "one", 0, 0, "") + "]"
	in.Cursor = tok
	_, _, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("boundary bridge overlap not rejected: %v", err)
	}
}

func TestPipelineGraph_bridgeRetryLineageAcrossPages(t *testing.T) {
	failed := strings.Replace(bridgeJSON(51, "dep", 99, 201, graphChildSHA), `"status":"success","allow_failure":false`, `"status":"failed","allow_failure":false`, 1)
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
			"99/201": walkPipe(201, 99, graphChildSHA, "old-child"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "child-a", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(20, "old-a", "failed", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(52, "dep", 99, 200, graphChildSHA) + "]",
			"42/100/2": "[" + failed + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	d, in := pagedActiveDeps(t, h)
	tok := ""
	var last map[string]any
	for i := 0; i < 10; i++ {
		tok, last = advanceGraph(t, d, &in, tok)
		if tok == "" {
			t.Logf("stop section=%#v", graphSection(t, last))
			break
		}
	}
	requireReadyComplete(t, last)
	reasons, _ := last["reasons"].([]any)
	for _, r := range reasons {
		if r == "failed_required" {
			t.Fatalf("older bridge retry on a later page blocked the graph: %#v", last["reasons"])
		}
	}
}

func TestPipelineGraph_hundredOneJobsResumePageSixUnderDefaultBudget(t *testing.T) {
	const perPage = 20
	jobs := map[string]string{
		"42/100/1": jobPageJSON(1, perPage),
		"42/100/2": jobPageJSON(21, perPage),
		"42/100/3": jobPageJSON(41, perPage),
		"42/100/4": jobPageJSON(61, perPage),
		"42/100/5": jobPageJSON(81, perPage),
		"42/100/6": "[" + jobJSON(101, "last", "success", "false") + "]",
	}
	h := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs:    jobs,
		bridges: map[string]string{"42/100/1": "[]"},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: perPage, MaxItems: 100, MaxRequests: 128}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	tok := ""
	var last map[string]any
	for i := 0; i < 12; i++ {
		in.Cursor = tok
		_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		last = raw.(map[string]any)
		sec := graphSection(t, last)
		if sec["next_cursor"] == nil {
			break
		}
		tok, _ = sec["next_cursor"].(string)
		payload, err := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if payload.PageState.Page == 5 && payload.PageState.ProviderNextPage == 6 {
			break
		}
	}
	if tok == "" {
		t.Fatalf("need cursor before page 6 %#v", graphSection(t, last))
	}
	in.Cursor = tok
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatalf("resume err: %v", err)
	}
	out := raw.(map[string]any)
	sec := graphSection(t, out)
	if sectionMessage(sec, "budget_items") {
		payload, _ := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
		t.Fatalf("page 6 resume hit item budget page=%d next=%d jobs=%#v lim=%#v", payload.PageState.Page, payload.PageState.ProviderNextPage, out["jobs"], sec["limitations"])
	}
	jobsOut, _ := out["jobs"].([]any)
	if len(jobsOut) != 1 || jobsOut[0].(map[string]any)["id"] != float64(101) {
		t.Fatalf("page 6 jobs %#v", jobsOut)
	}
}

func TestPipelineGraph_fiftyJobsTightBudgetResumePageTwo(t *testing.T) {
	const perPage = 50
	jobs := map[string]string{
		"42/100/1": jobPageJSON(1, perPage),
		"42/100/2": "[" + jobJSON(51, "only", "success", "false") + "]",
	}
	h := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs:    jobs,
		bridges: map[string]string{"42/100/1": "[]"},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: perPage, MaxItems: 50, MaxRequests: 64}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	tok, last := advanceGraph(t, d, &in, "")
	if tok == "" {
		t.Fatalf("need page-2 cursor %#v", graphSection(t, last))
	}
	payload, err := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
	if err != nil || payload.PageState.Page != 1 || payload.PageState.ProviderNextPage != 2 {
		t.Fatalf("cursor %#v err=%v", payload.PageState, err)
	}
	in.Cursor = tok
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatalf("resume err: %v", err)
	}
	out := raw.(map[string]any)
	sec := graphSection(t, out)
	if sectionMessage(sec, "budget_items") {
		t.Fatalf("page 2 resume budget_items %#v", sec["limitations"])
	}
	jobsOut, _ := out["jobs"].([]any)
	if len(jobsOut) != 1 || jobsOut[0].(map[string]any)["id"] != float64(51) {
		t.Fatalf("page 2 jobs %#v", jobsOut)
	}
}

func TestPipelineGraph_bridgePerPageOneTightBudgetResumePageTwo(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs:  map[string]string{"42/100/1": "[" + jobJSON(1, "root", "success", "false") + "]"},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(1000, "b1", 0, 0, "") + "]",
			"42/100/2": "[" + bridgeJSON(1001, "b2", 0, 0, "") + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1, MaxItems: 64, MaxRequests: 64}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	tok := ""
	var last map[string]any
	for i := 0; i < 8; i++ {
		in.Cursor = tok
		_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		last = raw.(map[string]any)
		sec := graphSection(t, last)
		if sec["next_cursor"] == nil {
			break
		}
		tok, _ = sec["next_cursor"].(string)
		payload, err := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if payload.GraphCont != nil && payload.GraphCont.Phase == cursor.GraphPhaseBridges &&
			payload.PageState.Page == 1 && payload.PageState.ProviderNextPage == 2 {
			break
		}
	}
	if tok == "" {
		t.Fatalf("need bridge page-2 cursor %#v", graphSection(t, last))
	}
	in.Cursor = tok
	in.MaxItems = 1
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatalf("bridge resume err: %v", err)
	}
	out := raw.(map[string]any)
	sec := graphSection(t, out)
	if sectionMessage(sec, "budget_items") {
		t.Fatalf("bridge page 2 resume budget_items %#v", sec["limitations"])
	}
	if sec["next_cursor"] == nil && out["downstream_coverage"] == downstreamCoverageComplete {
		return
	}
	for _, edge := range out["edges"].([]any) {
		if edge.(map[string]any)["bridge_id"] == float64(1001) {
			return
		}
	}
	t.Fatalf("bridge page 2 not fetched %#v", out)
}

func bridgePageJSON(startID, n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		id := startID + i
		parts[i] = bridgeJSON(id, "b"+fmt.Sprint(id), 0, 0, "")
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestPipelineGraph_hundredOneJobsTwentyOneBridgesResumeBridgePageTwo(t *testing.T) {
	const perPage = 20
	jobs := map[string]string{
		"42/100/1": jobPageJSON(1, perPage),
		"42/100/2": jobPageJSON(21, perPage),
		"42/100/3": jobPageJSON(41, perPage),
		"42/100/4": jobPageJSON(61, perPage),
		"42/100/5": jobPageJSON(81, perPage),
		"42/100/6": "[" + jobJSON(101, "last", "success", "false") + "]",
	}
	bridges := map[string]string{
		"42/100/1": bridgePageJSON(1000, perPage),
		"42/100/2": "[" + bridgeJSON(1020, "last-bridge", 0, 0, "") + "]",
	}
	h := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs:    jobs,
		bridges: bridges,
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: perPage, MaxItems: 100, MaxRequests: 128}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	tok := ""
	var last map[string]any
	for i := 0; i < 24; i++ {
		in.Cursor = tok
		_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		last = raw.(map[string]any)
		sec := graphSection(t, last)
		if sec["next_cursor"] == nil {
			break
		}
		tok, _ = sec["next_cursor"].(string)
		payload, err := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if payload.GraphCont != nil && payload.GraphCont.Phase == cursor.GraphPhaseBridges &&
			payload.PageState.Page == 1 && payload.PageState.ProviderNextPage == 2 {
			break
		}
	}
	if tok == "" {
		t.Fatalf("need bridge page-2 cursor %#v", graphSection(t, last))
	}
	in.Cursor = tok
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatalf("bridge resume err: %v", err)
	}
	out := raw.(map[string]any)
	sec := graphSection(t, out)
	if sectionMessage(sec, "budget_items") {
		t.Fatalf("bridge page 2 resume hit item budget lim=%#v", sec["limitations"])
	}
	for _, raw := range out["edges"].([]any) {
		e := raw.(map[string]any)
		if e["bridge_id"] == float64(1020) {
			return
		}
	}
	if sec["next_cursor"] != nil || out["downstream_coverage"] == downstreamCoverageComplete {
		return
	}
	t.Fatalf("bridge page 2 resume did not advance bridges %#v", out)
}

func TestPipelineGraph_manyParentJobPagesResumeIntoChild(t *testing.T) {
	const jobsPerPage = 25
	const apiPages = 3 // 75 jobs (>64) across paged parent job lists
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
		},
		jobs: map[string]string{
			"42/100/1": jobPageJSON(1, jobsPerPage),
			"42/100/2": jobPageJSON(100, jobsPerPage),
			"42/100/3": jobPageJSON(200, jobsPerPage),
			"99/200/1": "[" + jobJSON(5000, "child-a", "success", "false") + "]",
			"99/200/2": "[" + jobJSON(4999, "child-b", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "to-child", 99, 200, graphChildSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: jobsPerPage, MaxItems: 1000, MaxRequests: 128}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	tok := ""
	var last map[string]any
	sawChild := false
	for i := 0; i < apiPages+8; i++ {
		in.Cursor = tok
		_, raw, callErr := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
		if callErr != nil {
			t.Fatalf("call %d: %v", i, callErr)
		}
		last = raw.(map[string]any)
		tok, _ = graphSection(t, last)["next_cursor"].(string)
		if tok == "" {
			break
		}
		if payload, derr := cursor.Decode(d.Config.CursorKey, tok, clk.Now()); derr == nil && payload.GraphCont != nil && payload.GraphCont.NI == 200 {
			sawChild = true
		}
	}
	for _, raw := range last["nodes"].([]any) {
		n := raw.(map[string]any)
		if fmt.Sprint(n["pipeline_id"]) == "200" {
			sawChild = true
		}
	}
	if !sawChild || last["downstream_coverage"] != downstreamCoverageComplete {
		t.Fatalf("graph with %d parent job pages did not finish: cursor=%q child=%v coverage=%v", apiPages, tok, sawChild, last["downstream_coverage"])
	}
}
