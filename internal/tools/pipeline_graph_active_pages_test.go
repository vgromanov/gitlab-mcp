package tools

import (
	"context"
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
			"99/201/1": "[" + jobJSON(20, "old-a", "success", "false") + "]",
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
