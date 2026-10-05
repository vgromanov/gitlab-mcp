package tools

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

func TestReviewContext_pipelineGraphEmbedsDigest(t *testing.T) {
	log := &pathLog{}
	script := &reviewScript{log: log}
	graph := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, shaN(1), "feature-1")},
		jobs:    map[string]string{"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]"},
		mrPipes: `[{"id":100,"project_id":42,"sha":"` + shaN(1) + `","ref":"feature-1","status":"success"}]`,
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pipelines") || strings.Contains(r.URL.Path, "/jobs") || strings.Contains(r.URL.Path, "/bridges") {
			graph.ServeHTTP(w, r)
			return
		}
		script.serve(w, r)
	})
	d := newReviewDeps(t, h)
	out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{
		{ProjectID: "42", MergeRequestIID: 1, Sections: []string{"metadata", "pipeline_graph"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := out.Items[0]
	if item.Cause != "" || item.ContextRef == nil || item.PipelineGraph == nil || item.pipelineGraphDigest == "" {
		t.Fatalf("item %#v cause=%s", item.Sections["pipeline_graph"], item.Cause)
	}
	sec := item.Sections["pipeline_graph"]
	if sec.ContentComplete != readmeta.ContentCompleteTrue || sec.NextCursor != nil {
		t.Fatalf("section %#v", sec)
	}
	if item.PipelineGraph.Assessment != assessReady || item.PipelineGraph.DownstreamCoverage != downstreamCoverageComplete {
		t.Fatalf("graph assessment %s coverage %s", item.PipelineGraph.Assessment, item.PipelineGraph.DownstreamCoverage)
	}
	payload, err := cursor.Decode(d.Config.CursorKey, *item.ContextRef, d.now())
	if err != nil {
		t.Fatal(err)
	}
	if payload.ContextRef.Digests["pipeline_graph"] != item.pipelineGraphDigest {
		t.Fatalf("digest %v", payload.ContextRef.Digests)
	}
}

func TestReviewContext_pipelineGraphIndependentError(t *testing.T) {
	log := &pathLog{}
	d := newReviewDeps(t, http.HandlerFunc((&reviewScript{log: log}).serve))
	out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{
		{ProjectID: "42", MergeRequestIID: 1, Sections: []string{"metadata", "pipeline_graph"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := out.Items[0]
	if item.ContextRef == nil || item.Metadata == nil {
		t.Fatalf("metadata should still mint: cause=%s", item.Cause)
	}
	sec := item.Sections["pipeline_graph"]
	if sec.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatalf("broken CI must not complete graph: %#v", sec)
	}
	payload, err := cursor.Decode(d.Config.CursorKey, *item.ContextRef, d.now())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := payload.ContextRef.Digests["pipeline_graph"]; ok {
		t.Fatal("incomplete graph digest")
	}
}

func TestReviewContext_graphCursorRequiresMRParent(t *testing.T) {
	log := &pathLog{}
	script := &reviewScript{log: log}
	graph := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, shaN(1), "feature-1"),
			"42/200": walkPipe(200, 42, shaN(1), "other"),
		},
		jobs: map[string]string{
			"42/200/1": "[" + jobJSON(1, "x", "success", "false") + "]",
			"42/200/2": "[" + jobJSON(2, "y", "success", "false") + "]",
		},
		mrPipes: `[{"id":100,"project_id":42,"sha":"` + shaN(1) + `","ref":"feature-1","status":"success"}]`,
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pipelines") || strings.Contains(r.URL.Path, "/jobs") || strings.Contains(r.URL.Path, "/bridges") {
			graph.ServeHTTP(w, r)
			return
		}
		script.serve(w, r)
	})
	d := newReviewDeps(t, h)
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, pipelineGraphIn{
		ProjectID: "42", MergeRequestIID: 1, PipelineID: 200, PerPage: 1, ExpectedSourceSHA: shaN(1),
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := graphSection(t, raw.(map[string]any))["next_cursor"].(string)
	if tok == "" {
		t.Fatal("need graph cursor")
	}
	out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{
		{
			ProjectID: "42", MergeRequestIID: 1, Sections: []string{"metadata", "pipeline_graph"},
			Cursors: []reviewContextCursorIn{{Section: "pipeline_graph", Cursor: tok}},
		},
	})
	if err != nil {
		if !strings.Contains(err.Error(), cursor.ResyncRequired) {
			t.Fatal(err)
		}
		return
	}
	item := out.Items[0]
	if item.Cause != cursor.ResyncRequired {
		t.Fatalf("foreign pipeline cursor cause=%s graph=%v", item.Cause, item.PipelineGraph)
	}
	if item.PipelineGraph != nil && item.PipelineGraph.Section.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatal("must not complete a pinned non-parent pipeline")
	}
}

func TestReviewContext_resumedGraphStaysIncomplete(t *testing.T) {
	log := &pathLog{}
	script := &reviewScript{log: log}
	graph := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, shaN(1), "feature-1"),
			"99/200": walkPipe(200, 99, shaN(2), "child"),
			"99/201": walkPipe(201, 99, shaN(2), "child2"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "child-a", "success", "false") + "]",
			"99/200/2": "[" + jobJSON(11, "child-b", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(12, "child-c", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "one", 99, 200, shaN(2)) + "]",
			"42/100/2": "[" + bridgeJSON(51, "two", 99, 201, shaN(2)) + "]",
		},
		mrPipes: `[{"id":100,"project_id":42,"sha":"` + shaN(1) + `","ref":"feature-1","status":"success"}]`,
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pipelines") || strings.Contains(r.URL.Path, "/jobs") || strings.Contains(r.URL.Path, "/bridges") {
			graph.ServeHTTP(w, r)
			return
		}
		script.serve(w, r)
	})
	d := newReviewDeps(t, h)
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, pipelineGraphIn{
		ProjectID: "42", MergeRequestIID: 1, PerPage: 1, ExpectedSourceSHA: shaN(1),
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := graphSection(t, raw.(map[string]any))["next_cursor"].(string)
	if tok == "" {
		t.Fatal("need graph cursor")
	}
	out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{
		{
			ProjectID: "42", MergeRequestIID: 1, Sections: []string{"metadata", "pipeline_graph"},
			Cursors: []reviewContextCursorIn{{Section: "pipeline_graph", Cursor: tok}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := out.Items[0]
	if item.PipelineGraph != nil && item.PipelineGraph.Section.ContentComplete == readmeta.ContentCompleteTrue {
		t.Fatal("resumed tail certified complete")
	}
	if item.pipelineGraphDigest != "" {
		t.Fatal("resumed tail minted digest")
	}
}

func TestReviewContext_graphBorrowsBudget(t *testing.T) {
	log := &pathLog{}
	script := &reviewScript{log: log}
	graph := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, shaN(1), "feature-1")},
		jobs:    map[string]string{"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]"},
		mrPipes: `[{"id":100,"project_id":42,"sha":"` + shaN(1) + `","ref":"feature-1","status":"success"}]`,
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pipelines") || strings.Contains(r.URL.Path, "/jobs") || strings.Contains(r.URL.Path, "/bridges") {
			graph.ServeHTTP(w, r)
			return
		}
		script.serve(w, r)
	})
	d := newReviewDeps(t, h)
	ctx := igl.WithBudget(context.Background(), reviewBudget(3))
	out, err := callReviewDirect(t, d, ctx, []reviewContextItemIn{
		{ProjectID: "42", MergeRequestIID: 1, Sections: []string{"metadata", "pipeline_graph"}},
	})
	if err != nil {
		if strings.Contains(err.Error(), readmeta.CodeBudgetRequests) {
			return
		}
		t.Fatal(err)
	}
	item := out.Items[0]
	if item.Cause != readmeta.CodeBudgetRequests && (item.PipelineGraph == nil || item.PipelineGraph.Section.ContentComplete == readmeta.ContentCompleteTrue) {
		t.Fatalf("graph ignored review budget cause=%s complete=%v", item.Cause, item.Sections["pipeline_graph"])
	}
}

func TestReviewContext_graphFollowsMorePagesThanVisitedCap(t *testing.T) {
	log := &pathLog{}
	script := &reviewScript{log: log}
	jobs := map[string]string{}
	for page := 1; page <= 17; page++ {
		jobs["42/100/"+strconv.Itoa(page)] = jobPageJSON((page-1)*20+1, 20)
	}
	graph := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, shaN(1), "feature-1")},
		jobs:    jobs,
		mrPipes: `[{"id":100,"project_id":42,"sha":"` + shaN(1) + `","ref":"feature-1","status":"success"}]`,
	}
	jobCalls := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/jobs") {
			jobCalls++
		}
		if strings.Contains(r.URL.Path, "/pipelines") || strings.Contains(r.URL.Path, "/jobs") || strings.Contains(r.URL.Path, "/bridges") {
			graph.ServeHTTP(w, r)
			return
		}
		script.serve(w, r)
	})
	d := newReviewDeps(t, h)
	out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{
		{ProjectID: "42", MergeRequestIID: 1, Sections: []string{"metadata", "pipeline_graph"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := out.Items[0]
	if jobCalls <= 31 {
		t.Fatalf("loop stopped at MaxGraphVisited jobCalls=%d cause=%s", jobCalls, item.Cause)
	}
	if item.PipelineGraph != nil && item.PipelineGraph.Section.NextCursor != nil && item.Cause == "" {
		// Exhaustion subject to the shared budget is enough; leftover cursor is OK only on budget.
		t.Fatalf("cursor retained without a budget stop: %#v", item.PipelineGraph.Section)
	}
}

func TestReviewContext_internalGraphPagesMarkCoverageFull(t *testing.T) {
	log := &pathLog{}
	script := &reviewScript{log: log}
	graph := &walkServer{
		pipes: map[string]string{"42/100": walkPipe(100, 42, shaN(1), "feature-1")},
		jobs: map[string]string{
			"42/100/1": jobPageJSON(1, 20),
			"42/100/2": jobPageJSON(21, 1),
		},
		mrPipes: `[{"id":100,"project_id":42,"sha":"` + shaN(1) + `","ref":"feature-1","status":"success"}]`,
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pipelines") || strings.Contains(r.URL.Path, "/jobs") || strings.Contains(r.URL.Path, "/bridges") {
			graph.ServeHTTP(w, r)
			return
		}
		script.serve(w, r)
	})
	d := newReviewDeps(t, h)
	out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{
		{ProjectID: "42", MergeRequestIID: 1, Sections: []string{"metadata", "pipeline_graph"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := out.Items[0]
	sec := item.Sections["pipeline_graph"]
	if item.Cause != "" || item.pipelineGraphDigest == "" || sec.ContentComplete != readmeta.ContentCompleteTrue {
		t.Fatalf("incomplete multi-page graph cause=%s sec=%#v", item.Cause, sec)
	}
	if sec.ManifestCoverage != readmeta.CoverageFull {
		t.Fatalf("merged graph coverage %q", sec.ManifestCoverage)
	}
}

func TestReviewContext_childJobPagesStayOffRootJobs(t *testing.T) {
	log := &pathLog{}
	script := &reviewScript{log: log}
	graph := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, shaN(1), "feature-1"),
			"99/200": walkPipe(200, 99, shaN(2), "child"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]",
			"99/200/1": jobPageJSON(10, 20),
			"99/200/2": jobPageJSON(30, 1),
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "one", 99, 200, shaN(2)) + "]",
		},
		mrPipes: `[{"id":100,"project_id":42,"sha":"` + shaN(1) + `","ref":"feature-1","status":"success"}]`,
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/pipelines") || strings.Contains(r.URL.Path, "/jobs") || strings.Contains(r.URL.Path, "/bridges") {
			graph.ServeHTTP(w, r)
			return
		}
		script.serve(w, r)
	})
	d := newReviewDeps(t, h)
	out, err := callReviewDirect(t, d, context.Background(), []reviewContextItemIn{
		{ProjectID: "42", MergeRequestIID: 1, Sections: []string{"metadata", "pipeline_graph"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := out.Items[0]
	if item.PipelineGraph == nil {
		t.Fatalf("missing graph cause=%s", item.Cause)
	}
	for _, job := range item.PipelineGraph.Jobs {
		if job.ID != 1 {
			t.Fatalf("root jobs leaked child id %d: %#v", job.ID, item.PipelineGraph.Jobs)
		}
	}
	if len(item.PipelineGraph.Jobs) != 1 {
		t.Fatalf("root jobs %#v", item.PipelineGraph.Jobs)
	}
	var childJobs int
	for _, n := range item.PipelineGraph.Nodes {
		if n.PipelineID == 200 {
			childJobs += len(n.Jobs)
		}
	}
	if childJobs < 21 {
		t.Fatalf("child jobs stayed off nodes: %#v", item.PipelineGraph.Nodes)
	}
	if got := item.PipelineGraph.Section.HeadSHA; got == nil || *got != shaN(1) {
		t.Fatalf("aggregate graph section head_sha %v, want root %s", got, shaN(1))
	}
}

func TestMergePipelineGraph_refreshesRootPipelineMetadata(t *testing.T) {
	id := int64(100)
	running, failed := "running", "failed"
	first := pipelineGraphOut{
		ProjectID:  "42",
		PipelineID: &id,
		Pipeline:   &pipelineView{ID: 100, Status: &running},
		Nodes:      []graphNodeView{{ProjectID: "42", PipelineID: 100, Pipeline: &pipelineView{ID: 100, Status: &running}}},
	}
	second := pipelineGraphOut{
		ProjectID:  "42",
		PipelineID: &id,
		Pipeline:   &pipelineView{ID: 100, Status: &failed},
		Nodes:      []graphNodeView{{ProjectID: "42", PipelineID: 100, Pipeline: &pipelineView{ID: 100, Status: &failed}}},
	}
	merged := pipelineGraphOut{}
	mergePipelineGraph(&merged, first)
	mergePipelineGraph(&merged, second)
	if merged.Pipeline == nil || merged.Pipeline.Status == nil || *merged.Pipeline.Status != failed {
		t.Fatalf("stale top-level root pipeline %#v", merged.Pipeline)
	}
	if got := merged.Nodes[0].Pipeline.Status; got == nil || *got != failed {
		t.Fatalf("root node pipeline %#v", got)
	}

	childID := int64(200)
	child := pipelineGraphOut{ProjectID: "42", PipelineID: &childID, Pipeline: &pipelineView{ID: 200, Status: &running}}
	mergePipelineGraph(&merged, child)
	if merged.Pipeline.ID != 100 {
		t.Fatalf("child page replaced root pipeline %#v", merged.Pipeline)
	}
}
