package tools

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
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
