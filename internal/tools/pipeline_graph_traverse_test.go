package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

const graphChildSHA = "cccccccccccccccccccccccccccccccccccccccc"

type walkHit struct {
	method string
	path   string
}

type walkServer struct {
	hits             *[]walkHit
	pipes            map[string]string
	jobs             map[string]string
	bridges          map[string]string
	mr               string
	mrPipes          string
	status           map[string]int
	omitBridgePaging bool
	bridgeNextPage   string
	jobNextPage      string
	jobNextPageByPipe map[string]string
}

func (s *walkServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if s.hits != nil {
		*s.hits = append(*s.hits, walkHit{method: r.Method, path: r.URL.Path})
	}
	if code, ok := s.status[r.URL.Path]; ok {
		http.Error(w, `{"message":"denied"}`, code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	case path == "/api/v4/user":
		_, _ = io.WriteString(w, `{"id":7,"username":"alice"}`)
	case strings.HasPrefix(path, "/api/v4/projects/") && !strings.Contains(path, "/merge_requests") && !strings.Contains(path, "/pipelines"):
		id := strings.Trim(strings.TrimPrefix(path, "/api/v4/projects/"), "/")
		if i := strings.Index(id, "/"); i >= 0 {
			id = id[:i]
		}
		if _, err := strconv.Atoi(id); err != nil {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":%s,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group"}}`, id)
	case strings.Contains(path, "/merge_requests/") && strings.HasSuffix(path, "/pipelines"):
		w.Header().Set("X-Next-Page", "0")
		if s.mrPipes == "" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		_, _ = io.WriteString(w, s.mrPipes)
	case strings.Contains(path, "/merge_requests/"):
		if s.mr == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, s.mr)
	case strings.Contains(path, "/bridges"):
		s.writePaged(w, r, s.bridges, path)
	case strings.HasSuffix(path, "/jobs"):
		s.writePaged(w, r, s.jobs, path)
	case strings.Contains(path, "/pipelines/"):
		key := pipeKey(path)
		body, ok := s.pipes[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	default:
		http.NotFound(w, r)
	}
}

func (s *walkServer) writePaged(w http.ResponseWriter, r *http.Request, pages map[string]string, path string) {
	page := r.URL.Query().Get("page")
	if page == "" {
		page = "1"
	}
	key := pipeKey(path) + "/" + page
	body, ok := pages[key]
	next := "0"
	if ok {
		if n, err := strconv.Atoi(page); err == nil {
			if _, more := pages[pipeKey(path)+"/"+strconv.Itoa(n+1)]; more {
				next = strconv.Itoa(n + 1)
			}
		}
	} else {
		body = "[]"
	}
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(path, "/bridges") && s.omitBridgePaging {
		// Missing X-Next-Page must not be treated as exhaustion.
	} else if strings.Contains(path, "/bridges") && s.bridgeNextPage != "" {
		w.Header().Set("X-Next-Page", s.bridgeNextPage)
	} else if strings.HasSuffix(path, "/jobs") {
		if s.jobNextPageByPipe != nil {
			if override, ok := s.jobNextPageByPipe[pipeKey(path)]; ok {
				w.Header().Set("X-Next-Page", override)
			} else if s.jobNextPage != "" {
				w.Header().Set("X-Next-Page", s.jobNextPage)
			} else {
				w.Header().Set("X-Next-Page", next)
			}
		} else if s.jobNextPage != "" {
			w.Header().Set("X-Next-Page", s.jobNextPage)
		} else {
			w.Header().Set("X-Next-Page", next)
		}
	} else {
		w.Header().Set("X-Next-Page", next)
	}
	w.Header().Set("X-Page", page)
	_, _ = io.WriteString(w, body)
}

func pipeKey(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	var proj, pipe string
	for i, p := range parts {
		if p == "projects" && i+1 < len(parts) {
			proj = parts[i+1]
		}
		if p == "pipelines" && i+1 < len(parts) {
			pipe = parts[i+1]
		}
	}
	return proj + "/" + pipe
}

func walkPipe(id, project int, sha, ref string) string {
	return fmt.Sprintf(`{"id":%d,"project_id":%d,"sha":%q,"ref":%q,"status":"success","source":"push"}`, id, project, sha, ref)
}

func bridgeJSON(id int, name string, childProject, childPipe int, childSHA string) string {
	if childProject < 1 {
		return fmt.Sprintf(`{"id":%d,"name":%q,"stage":"test","status":"success","allow_failure":false,"downstream_pipeline":null}`, id, name)
	}
	return fmt.Sprintf(`{"id":%d,"name":%q,"stage":"test","status":"success","allow_failure":false,"downstream_pipeline":{"id":%d,"project_id":%d,"sha":%q,"status":"success"}}`, id, name, childPipe, childProject, childSHA)
}

func callWalk(t *testing.T, h http.Handler, in pipelineGraphIn, cfg *config.Config) (map[string]any, []walkHit, error) {
	t.Helper()
	var hits []walkHit
	wrapped := h
	if srv, ok := h.(*walkServer); ok {
		srv.hits = &hits
		wrapped = srv
	}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, wrapped, cfg, clk)
	_, out, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		return nil, hits, err
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("out %T", out)
	}
	return m, hits, nil
}

func edgeKinds(t *testing.T, out map[string]any) []string {
	t.Helper()
	raw, _ := out["edges"].([]any)
	kinds := make([]string, 0, len(raw))
	for _, item := range raw {
		e := item.(map[string]any)
		kinds = append(kinds, fmt.Sprint(e["kind"]))
	}
	return kinds
}

func hasKind(kinds []string, want string) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}

func TestPipelineGraph_freshCompleteDetectsHeadPipelineDrift(t *testing.T) {
	var mrLoads int
	base := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"42/101": walkPipe(101, 42, graphPipeSHA, "feature"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]",
			"42/101/1": "[" + jobJSON(2, "test", "success", "false") + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	var listN int
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.Contains(path, "/merge_requests/7/pipelines") {
			listN++
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Next-Page", "0")
			if listN == 1 {
				_, _ = io.WriteString(w, graphPipes("feature"))
				return
			}
			_, _ = io.WriteString(w, `[{"id":101,"project_id":42,"sha":"`+graphPipeSHA+`","ref":"feature","status":"success"}]`)
			return
		}
		if strings.Contains(path, "/merge_requests/7") && !strings.Contains(path, "/pipelines") {
			mrLoads++
			w.Header().Set("Content-Type", "application/json")
			if mrLoads >= 2 {
				_, _ = fmt.Fprintf(w, `{"id":1,"iid":7,"project_id":42,"source_branch":"feature","sha":%q,"head_pipeline":{"id":101,"project_id":42,"sha":%q,"ref":"feature","status":"success"}}`, graphSrcSHA, graphPipeSHA)
				return
			}
		}
		base.ServeHTTP(w, r)
	})
	_, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, MaxRequests: 64}, nil)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("head pipeline drift: %v", err)
	}
}

func TestPipelineGraph_readyEmptyDownstream(t *testing.T) {
	h := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs:    map[string]string{"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]"},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	out, hits, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireReadyComplete(t, out)
	if graphSection(t, out)["content_complete"] != readmeta.ContentCompleteTrue || out["digest"] == nil {
		t.Fatalf("complete graph %#v", graphSection(t, out))
	}
	sawBridges := false
	for _, hit := range hits {
		if strings.Contains(hit.path, "/bridges") {
			sawBridges = true
		}
	}
	if !sawBridges {
		t.Fatal("empty downstream still lists bridges")
	}
}

func TestPipelineGraph_twoPageBridgesAndJobs(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
			"99/201": walkPipe(201, 99, graphChildSHA, "child2"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "child-a", "success", "false") + "]",
			"99/200/2": "[" + jobJSON(11, "child-b", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(12, "child-c", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "one", 99, 200, graphChildSHA) + "]",
			"42/100/2": "[" + bridgeJSON(51, "two", 99, 201, graphChildSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	var hits []walkHit
	h.hits = &hits
	d := newCursorDeps(t, h, nil, clk)
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	last, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("out %T", raw)
	}
	tok, _ := graphSection(t, last)["next_cursor"].(string)
	if tok == "" {
		t.Fatal("first page must continue")
	}
	payload, err := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
	if err != nil || payload.GraphCont == nil || len(payload.GraphCont.Q) == 0 {
		t.Fatalf("graph cont %#v err %v", payload.GraphCont, err)
	}
	qn, qok := parseQueuedNode(payload.GraphCont.Q[0])
	if !qok || qn.ParentSHA != graphPipeSHA || len(qn.Ancestors) != 1 || qn.Ancestors[0] != (graphNodeKey{Project: "42", Pipeline: 100}) {
		t.Fatalf("queue ancestors %#v", qn)
	}
	for i := 0; i < 6 && tok != ""; i++ {
		in.Cursor = tok
		_, raw, err = getMergeRequestPipelineGraph(context.Background(), nil, in, d)
		if err != nil {
			t.Fatal(err)
		}
		last = raw.(map[string]any)
		tok, _ = graphSection(t, last)["next_cursor"].(string)
	}
	if last["downstream_coverage"] != downstreamCoverageComplete {
		t.Fatalf("paginated graph stuck: assessment=%v coverage=%v cursor=%v nodes=%#v hits=%#v section=%#v", last["assessment"], last["downstream_coverage"], tok, last["nodes"], hits, last["section"])
	}
	rel, _ := last["relation"].(map[string]any)
	if rel["proven"] != true {
		t.Fatalf("child resume lost root relation %#v", rel)
	}
}

func TestPipelineGraph_deniedChildNoContent(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
		},
		jobs:    map[string]string{"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]"},
		bridges: map[string]string{"42/100/1": "[" + bridgeJSON(50, "sec", 99, 200, graphChildSHA) + "]"},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	cfg := &config.Config{AllowedProjectIDs: []string{"42"}}
	out, hits, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	if !hasKind(edgeKinds(t, out), edgeKindDenied) {
		t.Fatalf("edges %#v", out["edges"])
	}
	found := false
	for _, raw := range out["edges"].([]any) {
		e, _ := raw.(map[string]any)
		if e["kind"] == edgeKindDenied && e["from_project"] == "42" && fmt.Sprint(e["from_pipeline"]) == "100" {
			found = true
		}
		if e["kind"] == edgeKindDenied {
			if v, ok := e["to_project"]; ok && v != nil && fmt.Sprint(v) != "" && fmt.Sprint(v) != "0" {
				t.Fatalf("denied edge leaked destination %#v", e)
			}
			if v, ok := e["to_pipeline"]; ok && v != nil && fmt.Sprint(v) != "" && fmt.Sprint(v) != "0" {
				t.Fatalf("denied edge leaked destination %#v", e)
			}
		}
	}
	if !found {
		t.Fatalf("denied edge missing source %#v", out["edges"])
	}
	for _, hit := range hits {
		if strings.Contains(hit.path, "/projects/99/pipelines") {
			t.Fatalf("unauthorized child content %s", hit.path)
		}
	}
}

func TestPipelineGraph_forbiddenChild(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
		},
		jobs:    map[string]string{"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]"},
		bridges: map[string]string{"42/100/1": "[" + bridgeJSON(50, "sec", 99, 200, graphChildSHA) + "]"},
		status:  map[string]int{"/api/v4/projects/99/pipelines/200": http.StatusForbidden},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	if !hasKind(edgeKinds(t, out), edgeKindInaccessible) {
		t.Fatalf("edges %#v", out["edges"])
	}
	if out["assessment"] == assessReady {
		t.Fatal("inaccessible child ready")
	}
}

func TestPipelineGraph_unknownIdentityAndMissing(t *testing.T) {
	h := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs:    map[string]string{"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]"},
		bridges: map[string]string{"42/100/1": "[" + bridgeJSON(50, "ghost", 0, 0, "") + "]"},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	if !hasKind(edgeKinds(t, out), edgeKindMissing) {
		t.Fatalf("edges %#v", out["edges"])
	}
}

func TestPipelineGraph_depthAndNodeStop(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
			"99/201": walkPipe(201, 99, graphChildSHA, "child2"),
			"99/202": walkPipe(202, 99, graphChildSHA, "child3"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "c", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(11, "c", "success", "false") + "]",
			"99/202/1": "[" + jobJSON(12, "c", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "a", 99, 200, graphChildSHA) + "," + bridgeJSON(51, "b", 99, 201, graphChildSHA) + "," + bridgeJSON(52, "c", 99, 202, graphChildSHA) + "]",
			"99/200/1": "[" + bridgeJSON(60, "g", 99, 202, graphChildSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, MaxDepth: 1, MaxNodes: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	kinds := edgeKinds(t, out)
	if !hasKind(kinds, edgeKindDepthStop) && !hasKind(kinds, edgeKindNodeStop) {
		t.Fatalf("expected depth or node stop, got %#v", kinds)
	}
}

func TestPipelineGraph_resumeSiblingCycleReachability(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "a"),
			"99/201": walkPipe(201, 99, graphChildSHA, "b"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "a", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(11, "b1", "success", "false") + "]",
			"99/201/2": "[" + jobJSON(12, "b2", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "to-a", 99, 200, graphChildSHA) + "," + bridgeJSON(51, "to-b", 99, 201, graphChildSHA) + "]",
			"99/200/1": "[" + bridgeJSON(60, "a-to-b", 99, 201, graphChildSHA) + "]",
			"99/201/1": "[" + bridgeJSON(61, "b-to-a", 99, 200, graphChildSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, MaxRequests: 64, PerPage: 1}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	var tok string
	for i := 0; i < 12; i++ {
		in.Cursor = tok
		_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
		if err != nil {
			t.Fatal(err)
		}
		sec := graphSection(t, raw.(map[string]any))
		if sec["next_cursor"] == nil {
			break
		}
		tok, _ = sec["next_cursor"].(string)
		payload, err := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if payload.GraphCont != nil && payload.GraphCont.NI == 201 && payload.GraphCont.Phase == cursor.GraphPhaseJobs {
			break
		}
	}
	if tok == "" {
		t.Fatal("expected paused continuation on sibling B jobs")
	}
	in.Cursor = tok
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	out := raw.(map[string]any)
	if !hasKind(edgeKinds(t, out), edgeKindCycle) {
		t.Fatalf("resumed sibling cycle missing cycle edge %#v", out["edges"])
	}
}

func TestPipelineGraph_siblingBranchCycleNotReady(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "a"),
			"99/201": walkPipe(201, 99, graphChildSHA, "b"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "a", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(11, "b", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "to-a", 99, 200, graphChildSHA) + "," + bridgeJSON(51, "to-b", 99, 201, graphChildSHA) + "]",
			"99/200/1": "[" + bridgeJSON(60, "a-to-b", 99, 201, graphChildSHA) + "]",
			"99/201/1": "[" + bridgeJSON(61, "b-to-a", 99, 200, graphChildSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, MaxRequests: 64}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kinds := edgeKinds(t, out)
	if !hasKind(kinds, edgeKindCycle) {
		t.Fatalf("sibling branch cycle missing cycle edge %#v", out["edges"])
	}
	if out["assessment"] == assessReady || out["downstream_coverage"] == downstreamCoverageComplete {
		t.Fatalf("sibling cycle certified ready: %#v", out)
	}
}

func TestPipelineGraph_diamondAndCycle(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "b"),
			"99/201": walkPipe(201, 99, graphChildSHA, "c"),
			"99/202": walkPipe(202, 99, graphChildSHA, "d"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "b", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(11, "c", "success", "false") + "]",
			"99/202/1": "[" + jobJSON(12, "d", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "to-b", 99, 200, graphChildSHA) + "," + bridgeJSON(51, "to-c", 99, 201, graphChildSHA) + "]",
			"99/200/1": "[" + bridgeJSON(60, "to-d", 99, 202, graphChildSHA) + "]",
			"99/201/1": "[" + bridgeJSON(61, "to-d", 99, 202, graphChildSHA) + "," + bridgeJSON(62, "to-root", 42, 100, graphPipeSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	out, hits, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, MaxRequests: 64}, nil)
	if err != nil {
		t.Fatal(err)
	}
	kinds := edgeKinds(t, out)
	if !hasKind(kinds, edgeKindShared) || !hasKind(kinds, edgeKindCycle) {
		t.Fatalf("diamond/cycle edges %#v", kinds)
	}
	dJobs := 0
	for _, hit := range hits {
		if strings.Contains(hit.path, "/projects/99/pipelines/202/jobs") {
			dJobs++
		}
	}
	if dJobs != 1 {
		t.Fatalf("shared child refetched %d times", dJobs)
	}
}

func TestPipelineGraph_mixedSHAKeepsBridgeProvenance(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "child", "success", "false") + "]",
		},
		bridges: map[string]string{"42/100/1": "[" + bridgeJSON(50, "trig", 99, 200, graphChildSHA) + "]"},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, ExpectedSourceSHA: graphSrcSHA, MaxRequests: 64}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rel := out["relation"].(map[string]any)
	if rel["proven"] != true || rel["sha_comparison"] != shaDifferent {
		t.Fatalf("parent relation %#v", rel)
	}
	found := false
	for _, item := range out["edges"].([]any) {
		e := item.(map[string]any)
		if e["kind"] == edgeKindBridge && e["sha_comparison"] == shaDifferent && e["to_pipeline"] == float64(200) {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing mixed-SHA bridge %#v", out["edges"])
	}
	if out["assessment"] != assessReady {
		t.Fatalf("distinct SHAs with bridge proof: %v", out["assessment"])
	}
}

func TestPipelineGraph_bridges404UnknownCapability(t *testing.T) {
	h := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs:    map[string]string{"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]"},
		status:  map[string]int{"/api/v4/projects/42/pipelines/100/bridges": http.StatusNotFound},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	if out["downstream_coverage"] != downstreamCoverageUnknown || out["bridge_capability"] != bridgeCapabilityUnknown {
		t.Fatalf("404 bridges %#v", out)
	}
}

func TestParseQueuedNodeRestoresAncestors(t *testing.T) {
	n := queuedGraphNode{
		Key:       graphNodeKey{Project: "99", Pipeline: 200},
		Depth:     1,
		ParentSHA: graphPipeSHA,
		Ancestors: []graphNodeKey{{Project: "42", Pipeline: 100}},
		BridgeID:  50,
	}
	enc, err := encodeQueuedNode(n)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := parseQueuedNode(enc)
	if !ok || got.Key != n.Key || got.Depth != 1 || got.ParentSHA != graphPipeSHA || got.BridgeID != 50 || len(got.Ancestors) != 1 || got.Ancestors[0] != n.Ancestors[0] {
		t.Fatalf("roundtrip %#v from %q", got, enc)
	}
	if _, ok := parseQueuedNode("99:200:1"); ok {
		t.Fatal("legacy queue key accepted")
	}
}

func TestEncodeGraphDigestIncludesJobEvidence(t *testing.T) {
	name := "test"
	stage := "test"
	okStatus := "success"
	failStatus := "failed"
	base := graphNodeView{ProjectID: "42", PipelineID: 100, Role: nodeRoleParent, Jobs: []jobView{
		{ID: 1, Name: &name, Stage: &stage, Status: &okStatus, AllowFailure: "false", Attempt: attemptLatest, Policy: policyPass},
	}}
	alt := base
	alt.Jobs = []jobView{{ID: 2, Name: &name, Stage: &stage, Status: &failStatus, AllowFailure: "false", Attempt: attemptLatest, Policy: policyBlock}}
	a := encodeGraphDigest([]graphNodeView{base}, nil, assessReady, downstreamCoverageComplete, nil)
	b := encodeGraphDigest([]graphNodeView{alt}, nil, assessReady, downstreamCoverageComplete, nil)
	if a == "" || a == b {
		t.Fatalf("digest ignored job evidence %s %s", a, b)
	}
}

func TestEncodeGraphDigestDistinguishesJobFieldDelimiters(t *testing.T) {
	status := "success"
	nameComma := "a,b"
	stageC := "c"
	nameA := "a"
	stageComma := "b,c"
	left := graphNodeView{ProjectID: "42", PipelineID: 100, Role: nodeRoleParent, Jobs: []jobView{
		{ID: 1, Name: &nameComma, Stage: &stageC, Status: &status, AllowFailure: "false", Attempt: attemptLatest, Policy: policyPass},
	}}
	right := graphNodeView{ProjectID: "42", PipelineID: 100, Role: nodeRoleParent, Jobs: []jobView{
		{ID: 1, Name: &nameA, Stage: &stageComma, Status: &status, AllowFailure: "false", Attempt: attemptLatest, Policy: policyPass},
	}}
	a := encodeGraphDigest([]graphNodeView{left}, nil, assessReady, downstreamCoverageComplete, nil)
	b := encodeGraphDigest([]graphNodeView{right}, nil, assessReady, downstreamCoverageComplete, nil)
	if a == "" || a == b {
		t.Fatalf("digest collided on unescaped job delimiters %s %s", a, b)
	}
}

func TestEncodeGraphDigestIncludesPipelineMetadata(t *testing.T) {
	ok := "success"
	failed := "failed"
	ref := "feature"
	src := "push"
	shaA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	name := "test"
	stage := "test"
	base := graphNodeView{
		ProjectID: "42", PipelineID: 100, Depth: 0, Role: nodeRoleParent,
		Pipeline: &pipelineView{ID: 100, Status: &ok, Source: &src, Ref: &ref, SHA: &shaA, StatusKnown: true},
		Jobs:     []jobView{{ID: 1, Name: &name, Stage: &stage, Status: &ok, AllowFailure: "false", Attempt: attemptLatest, Policy: policyPass}},
	}
	altSHA := base
	altPipe := *base.Pipeline
	altPipe.SHA = &shaB
	altSHA.Pipeline = &altPipe
	altStatus := base
	stPipe := *base.Pipeline
	stPipe.Status = &failed
	altStatus.Pipeline = &stPipe
	altDepth := base
	altDepth.Depth = 1
	got := encodeGraphDigest([]graphNodeView{base}, nil, assessReady, downstreamCoverageComplete, nil)
	if got == "" {
		t.Fatal("empty digest")
	}
	if got == encodeGraphDigest([]graphNodeView{altSHA}, nil, assessReady, downstreamCoverageComplete, nil) {
		t.Fatal("digest ignored pipeline SHA")
	}
	if got == encodeGraphDigest([]graphNodeView{altStatus}, nil, assessReady, downstreamCoverageComplete, nil) {
		t.Fatal("digest ignored pipeline status")
	}
	if got == encodeGraphDigest([]graphNodeView{altDepth}, nil, assessReady, downstreamCoverageComplete, nil) {
		t.Fatal("digest ignored node depth")
	}
}

func TestEncodeGraphDigestIncludesLineage(t *testing.T) {
	ok := "success"
	name := "retry"
	stage := "test"
	jobs := []jobView{{ID: 1, Name: &name, Stage: &stage, Status: &ok, AllowFailure: "false", Attempt: attemptLatest, Policy: policyPass}}
	base := graphNodeView{
		ProjectID: "42", PipelineID: 100, Role: nodeRoleParent, Jobs: jobs,
		Lineage: []lineageView{{Name: &name, LatestKnown: true, LatestIDs: []int64{2}, HistoryIDs: []int64{1}}},
	}
	alt := graphNodeView{
		ProjectID: "42", PipelineID: 100, Role: nodeRoleParent, Jobs: jobs,
		Lineage: []lineageView{{Name: &name, LatestKnown: true, LatestIDs: []int64{1}, HistoryIDs: []int64{2}}},
	}
	a := encodeGraphDigest([]graphNodeView{base}, nil, assessReady, downstreamCoverageComplete, nil)
	b := encodeGraphDigest([]graphNodeView{alt}, nil, assessReady, downstreamCoverageComplete, nil)
	if a == "" || a == b {
		t.Fatalf("digest ignored node lineage %s %s", a, b)
	}
}

func TestEncodeGraphDigestIncludesReasons(t *testing.T) {
	ok := "success"
	name := "bridge"
	stage := "test"
	node := graphNodeView{
		ProjectID: "42", PipelineID: 100, Role: nodeRoleParent,
		Jobs: []jobView{{ID: 1, Name: &name, Stage: &stage, Status: &ok, AllowFailure: "false", Attempt: attemptLatest, Policy: policyPass}},
	}
	bridgeID := int64(50)
	edge := graphEdgeView{
		FromProject: "42", FromPipeline: 100,
		ToProject: "99", ToPipeline: 200,
		BridgeID:  &bridgeID,
		Kind:      edgeKindBridge,
		Provenance: []string{"bridge_edge"},
	}
	base := encodeGraphDigest([]graphNodeView{node}, []graphEdgeView{edge}, assessBlocked, downstreamCoverageComplete, []string{"failed_required"})
	alt := encodeGraphDigest([]graphNodeView{node}, []graphEdgeView{edge}, assessBlocked, downstreamCoverageComplete, []string{"canceled_required"})
	if base == "" || base == alt {
		t.Fatalf("digest ignored assessment reasons %s %s", base, alt)
	}
}

func TestPipelineGraph_resumeKeepsIncompleteEdge(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/201": walkPipe(201, 99, graphChildSHA, "child2"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(12, "child-c", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "one", 0, 0, "") + "]",
			"42/100/2": "[" + bridgeJSON(51, "two", 99, 201, graphChildSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := graphSection(t, raw.(map[string]any))["next_cursor"].(string)
	if tok == "" {
		t.Fatal("need bridge continuation")
	}
	in.Cursor = tok
	_, raw, err = getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	out := raw.(map[string]any)
	if out["downstream_coverage"] == downstreamCoverageComplete {
		t.Fatalf("incomplete first-page edge was cleared %#v", out)
	}
}

func TestPipelineGraph_resumeKeepsReasons(t *testing.T) {
	h := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs:    map[string]string{"42/100/1": "[" + jobJSON(1, "test", "failed", "false") + "]", "42/100/2": "[" + jobJSON(2, "lint", "success", "false") + "]"},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := graphSection(t, raw.(map[string]any))["next_cursor"].(string)
	if tok == "" {
		t.Fatal("need jobs continuation")
	}
	in.Cursor = tok
	_, raw, err = getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	out := raw.(map[string]any)
	reasons, _ := out["reasons"].([]any)
	found := false
	for _, r := range reasons {
		if fmt.Sprint(r) == "failed_required" {
			found = true
		}
	}
	if !found {
		t.Fatalf("lost failed_required %#v", out["reasons"])
	}
}

func TestPipelineGraph_bridgeGuardSeesDownstreamChange(t *testing.T) {
	h := &walkServer{
		pipes:   map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs:    map[string]string{"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]"},
		bridges: map[string]string{"42/100/1": "[" + bridgeJSON(50, "one", 0, 0, "") + "]", "42/100/2": "[" + bridgeJSON(51, "two", 99, 201, graphChildSHA) + "]"},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := graphSection(t, raw.(map[string]any))["next_cursor"].(string)
	if tok == "" {
		t.Fatal("need bridge continuation")
	}
	h.bridges["42/100/1"] = "[" + bridgeJSON(50, "one", 99, 200, graphChildSHA) + "]"
	in.Cursor = tok
	_, _, err = getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("downstream identity change: %v", err)
	}
}

func jobPageJSON(start, n int) string {
	parts := make([]string, n)
	for i := 0; i < n; i++ {
		id := start + i
		parts[i] = jobJSON(id, "j"+strconv.Itoa(id), "success", "false")
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func TestPipelineGraph_missingBridgeNextPageNotComplete(t *testing.T) {
	h := &walkServer{
		pipes:            map[string]string{"42/100": walkPipe(100, 42, graphPipeSHA, "feature")},
		jobs:             map[string]string{"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]"},
		bridges:          map[string]string{"42/100/1": "[]"},
		mr:               graphMR("feature"),
		mrPipes:          graphPipes("feature"),
		omitBridgePaging: true,
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	if out["downstream_coverage"] == downstreamCoverageComplete {
		t.Fatalf("missing X-Next-Page completed the graph %#v", out)
	}
	if graphSection(t, out)["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("complete evidence without bridge exhaustion")
	}
}

func TestPipelineGraph_incompleteSiblingJobsSurvivesLaterSibling(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "a"),
			"99/201": walkPipe(201, 99, graphChildSHA, "b"),
			"99/202": walkPipe(202, 99, graphChildSHA, "c"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "root", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "a", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(11, "b1", "success", "false") + "]",
			"99/202/1": "[" + jobJSON(12, "c", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "to-a", 99, 200, graphChildSHA) + "," + bridgeJSON(51, "to-b", 99, 201, graphChildSHA) + "," + bridgeJSON(52, "to-c", 99, 202, graphChildSHA) + "]",
			"99/200/1": "[" + bridgeJSON(60, "a-to-b", 99, 201, graphChildSHA) + "]",
			"99/201/1": "[]",
			"99/202/1": "[]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
		jobNextPageByPipe: map[string]string{
			"99/201": "9",
		},
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, MaxRequests: 64}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	if out["assessment"] == assessReady || out["downstream_coverage"] == downstreamCoverageComplete {
		t.Fatalf("later sibling cleared incomplete B: %#v", out)
	}
	if graphSection(t, out)["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("signed complete graph with incomplete sibling jobs")
	}
}

func TestPipelineGraph_childJobsForbiddenPreservesGraph(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
			"99/201": walkPipe(201, 99, graphChildSHA, "sibling"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "root", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(12, "sibling", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "to-child", 99, 200, graphChildSHA) + "," + bridgeJSON(51, "to-sibling", 99, 201, graphChildSHA) + "]",
			"99/201/1": "[]",
		},
		status: map[string]int{
			"/api/v4/projects/99/pipelines/200/jobs": http.StatusForbidden,
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, MaxRequests: 64}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out["pipeline"] == nil {
		t.Fatal("root pipeline dropped on child jobs 403")
	}
	requireNotReady(t, out)
	if !hasKind(edgeKinds(t, out), edgeKindInaccessible) {
		t.Fatalf("missing inaccessible edge %#v", out["edges"])
	}
}

func TestPipelineGraph_unacceptedChildJobNextPageNotComplete(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "child", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "one", 99, 200, graphChildSHA) + "]",
			"99/200/1": "[]",
		},
		mr:          graphMR("feature"),
		mrPipes:     graphPipes("feature"),
		jobNextPage: "9",
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	if out["downstream_coverage"] == downstreamCoverageComplete {
		t.Fatalf("unaccepted child job X-Next-Page completed the graph %#v", out)
	}
	if graphSection(t, out)["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatal("complete evidence with skipped child job pages")
	}
}

func TestPipelineGraph_unacceptedBridgeNextPageNotComplete(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "child", "success", "false") + "]",
		},
		bridges:        map[string]string{"42/100/1": "[" + bridgeJSON(50, "one", 99, 200, graphChildSHA) + "]"},
		mr:             graphMR("feature"),
		mrPipes:        graphPipes("feature"),
		bridgeNextPage: "9",
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	if out["downstream_coverage"] == downstreamCoverageComplete {
		t.Fatalf("unaccepted X-Next-Page completed the graph %#v", out)
	}
}

func TestPipelineGraph_pausedChildKeepsAncestryForCycle(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "child-a", "success", "false") + "]",
			"99/200/2": "[" + jobJSON(11, "child-b", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "to-child", 99, 200, graphChildSHA) + "]",
			"99/200/1": "[" + bridgeJSON(60, "to-root", 42, 100, graphPipeSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	var last map[string]any
	var sawAncestry bool
	tok := ""
	for i := 0; i < 8; i++ {
		in.Cursor = tok
		_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
		if err != nil {
			t.Fatal(err)
		}
		last = raw.(map[string]any)
		tok, _ = graphSection(t, last)["next_cursor"].(string)
		if tok == "" {
			break
		}
		payload, err := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
		if err != nil {
			t.Fatal(err)
		}
		if gc := payload.GraphCont; gc != nil && gc.NI == 200 && len(gc.Anc) == 1 && gc.Anc[0] == "42:100" {
			sawAncestry = true
		}
	}
	if !sawAncestry {
		t.Fatal("continuation for the paused child did not carry its ancestors")
	}
	kinds := edgeKinds(t, last)
	if !hasKind(kinds, edgeKindCycle) || hasKind(kinds, edgeKindShared) {
		t.Fatalf("back-edge from resumed child misclassified: %#v", kinds)
	}
}

func TestPipelineGraph_cycleEdgeNeverReady(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "child", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "to-child", 99, 200, graphChildSHA) + "]",
			"99/200/1": "[" + bridgeJSON(60, "to-root", 42, 100, graphPipeSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	out, _, err := callWalk(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, MaxRequests: 64}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasKind(edgeKinds(t, out), edgeKindCycle) {
		t.Fatalf("expected cycle edge %#v", out["edges"])
	}
	if out["assessment"] == assessReady || out["downstream_coverage"] == downstreamCoverageComplete {
		t.Fatalf("cycle graph certified: assessment=%v coverage=%v", out["assessment"], out["downstream_coverage"])
	}
	if sec := graphSection(t, out); sec["content_complete"] == true {
		t.Fatalf("cycle graph content_complete %#v", sec)
	}
}

func blockedRootDriftServer() *walkServer {
	return &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "failed", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "child-a", "success", "false") + "]",
			"99/200/2": "[" + jobJSON(11, "child-b", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "to-child", 99, 200, graphChildSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
}

func TestPipelineGraph_blockedCompleteDetectsRootJobDrift(t *testing.T) {
	h := blockedRootDriftServer()
	d, in, tok := childContinuation(t, h)
	h.jobs["42/100/1"] = "[" + jobJSON(1, "parent", "success", "false") + "]"
	in.Cursor = tok
	_, _, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("blocked complete graph must revalidate root jobs: %v", err)
	}
}

func ancestorDriftServer() *walkServer {
	return &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "child"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "failed", "true") + "]",
			"99/200/1": "[" + jobJSON(10, "child-a", "success", "false") + "]",
			"99/200/2": "[" + jobJSON(11, "child-b", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "to-child", 99, 200, graphChildSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
}

func childContinuation(t *testing.T, h *walkServer) (Deps, pipelineGraphIn, string) {
	t.Helper()
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := graphSection(t, raw.(map[string]any))["next_cursor"].(string)
	payload, err := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
	if err != nil || payload.GraphCont == nil || payload.GraphCont.NI != 200 || len(payload.GraphCont.Anc) != 1 {
		t.Fatalf("expected child continuation, got %#v err %v", payload.GraphCont, err)
	}
	return d, in, tok
}

func TestPipelineGraph_childCursorDetectsRootJobDrift(t *testing.T) {
	h := ancestorDriftServer()
	d, in, tok := childContinuation(t, h)
	h.jobs["42/100/1"] = "[" + jobJSON(1, "parent", "success", "true") + "]"
	in.Cursor = tok
	_, _, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("root job drift not detected: %v", err)
	}
}

func TestPipelineGraph_childCursorDetectsRootBridgeDrift(t *testing.T) {
	h := ancestorDriftServer()
	d, in, tok := childContinuation(t, h)
	h.bridges["42/100/1"] = "[" + bridgeJSON(50, "to-child", 99, 200, graphChildSHA) + "," + bridgeJSON(51, "late", 98, 300, graphChildSHA) + "]"
	in.Cursor = tok
	_, _, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("root bridge drift not detected: %v", err)
	}
}

func TestPipelineGraph_childCursorStableAncestorsResume(t *testing.T) {
	h := ancestorDriftServer()
	d, in, tok := childContinuation(t, h)
	in.Cursor = tok
	_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	if raw.(map[string]any)["downstream_coverage"] != downstreamCoverageComplete {
		t.Fatalf("stable ancestors should resume to completion: %#v", raw)
	}
}

func twoChildDriftServer() *walkServer {
	return &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "first"),
			"99/201": walkPipe(201, 99, graphChildSHA, "second"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "parent", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "first-a", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(20, "second-a", "success", "false") + "]",
			"99/201/2": "[" + jobJSON(21, "second-b", "success", "false") + "]",
		},
		bridges: map[string]string{
			"42/100/1": "[" + bridgeJSON(50, "one", 99, 200, graphChildSHA) + "," + bridgeJSON(51, "two", 99, 201, graphChildSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
}

func secondChildContinuation(t *testing.T, h *walkServer) (Deps, pipelineGraphIn, string) {
	t.Helper()
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	var tok string
	for i := 0; i < 6; i++ {
		in.Cursor = tok
		_, raw, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
		if err != nil {
			t.Fatal(err)
		}
		tok, _ = graphSection(t, raw.(map[string]any))["next_cursor"].(string)
		payload, derr := cursor.Decode(d.Config.CursorKey, tok, clk.Now())
		if tok != "" && derr == nil && payload.GraphCont != nil && payload.GraphCont.NI == 201 {
			return d, in, tok
		}
	}
	t.Fatal("never reached a continuation scoped to the second child")
	return d, in, ""
}

func TestPipelineGraph_childCursorDetectsCompletedSiblingDrift(t *testing.T) {
	h := twoChildDriftServer()
	d, in, tok := secondChildContinuation(t, h)
	h.jobs["99/200/1"] = "[" + jobJSON(10, "first-a", "failed", "false") + "]"
	in.Cursor = tok
	_, _, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("completed sibling drift not detected: %v", err)
	}
}

func TestPipelineGraph_revalidationChargesCallBudget(t *testing.T) {
	h := ancestorDriftServer()
	d, in, tok := childContinuation(t, h)
	h.jobs["42/100/1"] = jobPageJSON(1, 12)
	in.Cursor = tok
	in.MaxItems = 3
	_, _, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !errors.Is(err, igl.ErrBudgetItems) {
		t.Fatalf("revalidation should charge caller budget: %v", err)
	}
}

func TestPipelineGraph_childCursorDetectsRootStatusOnlyDrift(t *testing.T) {
	h := ancestorDriftServer()
	d, in, tok := childContinuation(t, h)
	h.pipes["42/100"] = strings.Replace(h.pipes["42/100"], `"status":"success"`, `"status":"failed"`, 1)
	in.Cursor = tok
	_, _, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("root status-only drift not detected: %v", err)
	}
}

func TestPipelineGraph_childCursorDetectsSiblingStatusOnlyDrift(t *testing.T) {
	h := twoChildDriftServer()
	d, in, tok := secondChildContinuation(t, h)
	h.pipes["99/200"] = strings.Replace(h.pipes["99/200"], `"status":"success"`, `"status":"failed"`, 1)
	in.Cursor = tok
	_, _, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("sibling status-only drift not detected: %v", err)
	}
}

func TestPipelineGraph_restoredTrimmedReachClosingEdgeIsCycle(t *testing.T) {
	h := &walkServer{
		pipes: map[string]string{
			"42/100": walkPipe(100, 42, graphPipeSHA, "feature"),
			"99/200": walkPipe(200, 99, graphChildSHA, "a"),
			"99/201": walkPipe(201, 99, graphChildSHA, "b"),
		},
		jobs: map[string]string{
			"42/100/1": "[" + jobJSON(1, "test", "success", "false") + "]",
			"99/200/1": "[" + jobJSON(10, "a", "success", "false") + "]",
			"99/201/1": "[" + jobJSON(11, "b", "success", "false") + "]",
		},
		bridges: map[string]string{
			"99/201/1": "[" + bridgeJSON(61, "b-to-a", 99, 200, graphChildSHA) + "]",
		},
		mr:      graphMR("feature"),
		mrPipes: graphPipes("feature"),
	}
	d := newCursorDeps(t, h, nil, &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)})
	childSHA, ref, status, source := graphChildSHA, "b", "success", "push"
	pipe := &pipelineView{ID: 201, SHA: &childSHA, Ref: &ref, Status: &status, StatusKnown: true, Source: &source}
	gc := &cursor.GraphCont{
		V:     cursor.GraphContSchemaG1,
		Phase: cursor.GraphPhaseBridges,
		NP:    "99",
		NI:    201,
		D:     1,
		N:     3,
		Cov:   downstreamCoveragePartial,
		Cap:   bridgeCapabilityBridges,
		Inc:   true,
		Rgx:   true,
		RP:    "42",
		RI:    100,
		RK:    relBranch,
		Prv:   true,
		RS:    shaUnknown,
	}
	walk, err := restoreGraphWalk(pipe, gc, graphDefaultMaxDepth, graphDefaultMaxNodes)
	if err != nil {
		t.Fatal(err)
	}
	walk.visited["99:200"] = struct{}{}
	walk.visited["99:201"] = struct{}{}
	walk.cap = bridgeCapabilityBridges
	br := graphBridge{
		Job:           graphJob{ID: 61, Name: "b-to-a", NameKnown: true, Status: "success", StatusKnown: true},
		ChildProject:  99,
		ChildPipeline: 200,
		ChildSHA:      graphChildSHA,
		ChildPresent:  true,
	}
	parent := graphNodeKey{Project: "99", Pipeline: 201}
	if err := walk.addBridgeEdge(context.Background(), d, parent, graphChildSHA, br); err != nil {
		t.Fatal(err)
	}
	if len(walk.edges) != 1 {
		t.Fatalf("edges %#v", walk.edges)
	}
	if walk.edges[0].Kind != edgeKindCycle {
		t.Fatalf("trimmed reach must not certify shared sibling edge: %#v", walk.edges[0])
	}
}

func TestRestoreGraphReachabilityNotInEmittedEdges(t *testing.T) {
	item, err := cursor.FormatGraphReachEdge("42:100", "42:200", edgeKindBridge)
	if err != nil {
		t.Fatal(err)
	}
	gc := &cursor.GraphCont{
		V:     cursor.GraphContSchemaG1,
		Phase: cursor.GraphPhaseBridges,
		NP:    "42",
		NI:    100,
		Rg:    []string{item},
	}
	pipe := &pipelineView{ID: 100}
	w, err := restoreGraphWalk(pipe, gc, graphDefaultMaxDepth, graphDefaultMaxNodes)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.edges) != 0 {
		t.Fatalf("restored reachability leaked into emitted edges: %#v", w.edges)
	}
	if len(w.reach) != 1 || w.reach[0].ToPipeline != 200 {
		t.Fatalf("reach %#v", w.reach)
	}
}

