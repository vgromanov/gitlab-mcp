package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

const (
	graphSrcSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	graphPipeSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type graphHit struct {
	method string
	path   string
	raw    string
}

func graphHandler(hits *[]graphHit, pipes map[int]string, jobs map[string]string, mr string, mrPipes string, missingPipe bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits = append(*hits, graphHit{method: r.Method, path: r.URL.Path, raw: r.URL.RawQuery})
		}
		if strings.Contains(r.URL.Path, "/bridges") {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Next-Page", "0")
			_, _ = io.WriteString(w, `[]`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v4/user":
			_, _ = io.WriteString(w, `{"id":7,"username":"alice"}`)
		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && !strings.Contains(r.URL.Path, "/merge_requests") && !strings.Contains(r.URL.Path, "/pipelines"):
			id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v4/projects/"), "/")
			if i := strings.Index(id, "/"); i >= 0 {
				id = id[:i]
			}
			if _, err := strconv.Atoi(id); err != nil {
				http.NotFound(w, r)
				return
			}
			_, _ = fmt.Fprintf(w, `{"id":%s,"path_with_namespace":"g/p"}`, id)
		case strings.Contains(r.URL.Path, "/merge_requests/") && strings.HasSuffix(r.URL.Path, "/pipelines"):
			w.Header().Set("X-Next-Page", "0")
			if mrPipes == "" {
				_, _ = io.WriteString(w, `[]`)
				return
			}
			_, _ = io.WriteString(w, mrPipes)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			if mr == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, mr)
		case strings.Contains(r.URL.Path, "/pipelines/") && strings.HasSuffix(r.URL.Path, "/jobs"):
			page := r.URL.Query().Get("page")
			if page == "" {
				page = "1"
			}
			body, ok := jobs[page]
			if !ok {
				http.NotFound(w, r)
				return
			}
			next := "0"
			if n, err := strconv.Atoi(page); err == nil {
				if _, more := jobs[strconv.Itoa(n+1)]; more {
					next = strconv.Itoa(n + 1)
				}
			}
			w.Header().Set("X-Next-Page", next)
			_, _ = io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/pipelines/"):
			if missingPipe {
				http.NotFound(w, r)
				return
			}
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			n, _ := strconv.Atoi(id)
			body, ok := pipes[n]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	})
}

func graphMR(ref string) string {
	return fmt.Sprintf(`{"id":1,"iid":7,"project_id":42,"source_branch":"feature","sha":%q,"head_pipeline":{"id":100,"project_id":42,"sha":%q,"ref":%q,"status":"success"}}`, graphSrcSHA, graphPipeSHA, ref)
}

func graphPipe(ref, source string) string {
	return fmt.Sprintf(`{"id":100,"project_id":42,"sha":%q,"ref":%q,"status":"success","source":%q}`, graphPipeSHA, ref, source)
}

func graphPipes(ref string) string {
	return fmt.Sprintf(`[{"id":100,"project_id":42,"sha":%q,"ref":%q,"status":"success","source":"merge_request_event"}]`, graphPipeSHA, ref)
}

func jobJSON(id int, name, status, allow string) string {
	allowJSON := ""
	if allow != "" {
		allowJSON = `,"allow_failure":` + allow
	}
	return fmt.Sprintf(`{"id":%d,"name":%q,"stage":"test","status":%q%s}`, id, name, status, allowJSON)
}

func callGraph(t *testing.T, h http.Handler, in pipelineGraphIn, clk *cursor.FakeClock) (map[string]any, error) {
	t.Helper()
	if clk == nil {
		clk = &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	}
	d := newCursorDeps(t, h, nil, clk)
	_, out, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		return nil, err
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("out %T", out)
	}
	return m, nil
}

func graphSection(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	sec, _ := out["section"].(map[string]any)
	if sec == nil {
		t.Fatalf("section %#v", out["section"])
	}
	return sec
}

func requireNotReady(t *testing.T, out map[string]any) {
	t.Helper()
	if out["assessment"] == "ready" {
		t.Fatalf("assessment=%v downstream=%v bridges=%v", out["assessment"], out["downstream_coverage"], out["bridges_visited"])
	}
}

func requireReadyComplete(t *testing.T, out map[string]any) {
	t.Helper()
	if out["assessment"] != assessReady || out["downstream_coverage"] != downstreamCoverageComplete || out["bridges_visited"] != true {
		t.Fatalf("assessment=%v downstream=%v bridges=%v", out["assessment"], out["downstream_coverage"], out["bridges_visited"])
	}
}

func requireJobsOnlyUnknownDownstream(t *testing.T, out map[string]any) {
	t.Helper()
	requireNotReady(t, out)
	if out["downstream_coverage"] != "unknown" || out["bridges_visited"] != false {
		t.Fatalf("assessment=%v downstream=%v bridges=%v", out["assessment"], out["downstream_coverage"], out["bridges_visited"])
	}
}

func TestPipelineGraph_manualAndMissingFields(t *testing.T) {
	var hits []graphHit
	jobs := map[string]string{
		"1": "[" + jobJSON(1, "gate", "manual", "false") + "," + jobJSON(2, "opt", "manual", "true") + "," + jobJSON(3, "mystery", "manual", "") + "]",
	}
	h := graphHandler(&hits, map[int]string{100: graphPipe("refs/merge-requests/7/merge", "merge_request_event")}, jobs, graphMR("refs/merge-requests/7/merge"), graphPipes("refs/merge-requests/7/merge"), false)
	out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, ExpectedSourceSHA: graphSrcSHA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	if out["assessment"] != assessBlocked {
		t.Fatalf("assessment %v", out["assessment"])
	}
	sec := graphSection(t, out)
	for _, key := range []string{"retrieved_at", "source", "provider", "capability_version", "pagination_exhausted", "content_complete", "consistency", "limitations"} {
		if _, ok := sec[key]; !ok {
			t.Fatalf("missing section %s", key)
		}
	}
	if sec["head_sha"] != graphPipeSHA || sec["content_complete"] != readmeta.ContentCompleteTrue {
		t.Fatalf("section %#v", sec)
	}
	rel := out["relation"].(map[string]any)
	if rel["kind"] != relMergedResult || rel["proven"] != true || rel["sha_comparison"] != shaDifferent {
		t.Fatalf("relation %#v", rel)
	}
	foundAbsent := false
	for _, item := range out["jobs"].([]any) {
		job := item.(map[string]any)
		if job["name"] == "mystery" {
			foundAbsent = job["allow_failure"] == "absent"
		}
	}
	if !foundAbsent {
		t.Fatalf("jobs %#v", out["jobs"])
	}
	sawBridges := false
	for _, hit := range hits {
		if strings.Contains(hit.path, "/bridges") {
			sawBridges = true
		}
		if strings.HasSuffix(hit.path, "/jobs") && !strings.Contains(hit.raw, "include_retried=true") {
			t.Fatalf("jobs query %s", hit.raw)
		}
	}
	if !sawBridges {
		t.Fatal("expected exhausted parent jobs to list bridges")
	}
}

func TestPipelineGraph_shaAloneIsNotProof(t *testing.T) {
	jobs := map[string]string{"1": "[" + jobJSON(1, "test", "success", "false") + "]"}
	h := graphHandler(nil, map[int]string{100: graphPipe("refs/merge-requests/7/merge", "merge_request_event")}, jobs, "", "", false)
	out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", PipelineID: 100, ExpectedSourceSHA: graphPipeSHA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	rel := out["relation"].(map[string]any)
	if rel["kind"] != relUnproven || rel["proven"] != false || rel["sha_comparison"] != shaEqual {
		t.Fatalf("relation %#v", rel)
	}
	if out["assessment"] == assessBlocked {
		t.Fatal("success parent must not block")
	}
}

func TestPipelineGraph_branchRef(t *testing.T) {
	jobs := map[string]string{"1": "[" + jobJSON(1, "test", "success", "false") + "]"}
	h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
	out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, ExpectedSourceSHA: graphSrcSHA}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireReadyComplete(t, out)
	rel := out["relation"].(map[string]any)
	if rel["kind"] != relBranch || rel["proven"] != true || rel["sha_comparison"] != shaDifferent {
		t.Fatalf("relation %#v", rel)
	}
}

func TestPipelineGraph_missingFilterPartialAndRetry(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		h := graphHandler(nil, nil, nil, graphMR("feature"), `[]`, false)
		out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
		if err != nil {
			t.Fatal(err)
		}
		requireNotReady(t, out)
		if out["jobs"] != nil || out["pipeline"] != nil || graphSection(t, out)["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatalf("%#v", out)
		}
	})
	t.Run("missing pipeline id", func(t *testing.T) {
		h := graphHandler(nil, nil, nil, "", "", true)
		out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", PipelineID: 100}, nil)
		if err != nil {
			t.Fatal(err)
		}
		requireNotReady(t, out)
		if out["jobs"] != nil || graphSection(t, out)["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatalf("%#v", out)
		}
	})
	t.Run("filter hides required manual from the list only", func(t *testing.T) {
		jobs := map[string]string{"1": "[" + jobJSON(1, "test", "success", "false") + "," + jobJSON(2, "deploy", "success", "false") + "," + jobJSON(3, "gate", "manual", "false") + "]"}
		h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
		out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, JobNames: []string{"test"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		requireNotReady(t, out)
		if out["assessment"] != assessBlocked || out["job_filter_applied"] != true {
			t.Fatalf("assessment %v filter %v", out["assessment"], out["job_filter_applied"])
		}
		jobsOut := out["jobs"].([]any)
		if len(jobsOut) != 1 || jobsOut[0].(map[string]any)["name"] != "test" {
			t.Fatalf("jobs %#v", jobsOut)
		}
		lineage := out["lineage"].([]any)
		if len(lineage) != 1 || lineage[0].(map[string]any)["name"] != "test" {
			t.Fatalf("filtered lineage %#v", lineage)
		}
		if lineageHasID(lineage, 2) || lineageHasID(lineage, 3) {
			t.Fatalf("filtered ids in lineage %#v", lineage)
		}
		if graphSection(t, out)["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatal("filter must not be content complete")
		}
	})
	t.Run("status filter drops excluded attempt ids", func(t *testing.T) {
		jobs := map[string]string{"1": "[" + jobJSON(2, "test", "success", "false") + "," + jobJSON(4, "test", "failed", "false") + "]"}
		h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
		out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, JobStatuses: []string{"success"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		requireNotReady(t, out)
		if out["assessment"] != assessBlocked {
			t.Fatalf("filtered latest failure must still block: %v", out["assessment"])
		}
		jobsOut := out["jobs"].([]any)
		if len(jobsOut) != 1 || jobsOut[0].(map[string]any)["id"] != float64(2) {
			t.Fatalf("jobs %#v", jobsOut)
		}
		lineage := out["lineage"].([]any)
		if len(lineage) != 1 || lineage[0].(map[string]any)["latest_known"] != false || lineageHasID(lineage, 4) {
			t.Fatalf("lineage %#v", lineage)
		}
		history := lineage[0].(map[string]any)["history_ids"].([]any)
		if len(history) != 1 || history[0] != float64(2) {
			t.Fatalf("history %#v", history)
		}
	})
	t.Run("retry history", func(t *testing.T) {
		jobs := map[string]string{"1": "[" + jobJSON(1, "test", "failed", "false") + "," + jobJSON(2, "test", "success", "false") + "]"}
		h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
		out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
		if err != nil {
			t.Fatal(err)
		}
		requireReadyComplete(t, out)
		if out["assessment"] == assessBlocked {
			t.Fatal("retried failure must not block the latest success")
		}
		if len(out["jobs"].([]any)) != 2 {
			t.Fatalf("history dropped %#v", out["jobs"])
		}
		lineage := out["lineage"].([]any)
		if len(lineage) != 1 || lineage[0].(map[string]any)["latest_known"] != true {
			t.Fatalf("lineage %#v", lineage)
		}
	})
}

func TestPipelineGraph_paginationCursorAndKey(t *testing.T) {
	jobs := map[string]string{
		"1": "[" + jobJSON(1, "test", "success", "false") + "]",
		"2": "[" + jobJSON(2, "test", "running", "false") + "]",
	}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	var hits []graphHit
	h := graphHandler(&hits, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
	d := newCursorDeps(t, h, nil, clk)
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	_, out, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	page1 := out.(map[string]any)
	requireNotReady(t, page1)
	if page1["assessment"] != assessPartial {
		t.Fatalf("page1 assessment %v", page1["assessment"])
	}
	sec := graphSection(t, page1)
	tok, _ := sec["next_cursor"].(string)
	if tok == "" || sec["content_complete"] == readmeta.ContentCompleteTrue || sec["pagination_exhausted"] == true {
		t.Fatalf("page1 section %#v", sec)
	}
	in.Cursor = tok
	_, out2, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	page2 := out2.(map[string]any)
	requireNotReady(t, page2)
	sec2 := graphSection(t, page2)
	if sec2["pagination_exhausted"] != true || sec2["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("page2 section %#v", sec2)
	}
	if page2["assessment"] != assessPartial {
		t.Fatalf("running page %v", page2["assessment"])
	}
	wrong := in
	wrong.PipelineID = 99
	_, _, err = getMergeRequestPipelineGraph(context.Background(), nil, wrong, d)
	if err == nil || !strings.Contains(err.Error(), "project scope mismatch") {
		t.Fatalf("mismatched pipeline id %v", err)
	}

	clk.Advance(3 * time.Hour)
	_, _, err = getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("expiry err %v", err)
	}

	d.Config.CursorKey = nil
	var late []graphHit
	d2 := newCursorDeps(t, graphHandler(&late, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false), nil, clk)
	d2.Config.CursorKey = nil
	_, _, err = getMergeRequestPipelineGraph(context.Background(), nil, pipelineGraphIn{ProjectID: "42", PipelineID: 100}, d2)
	if err == nil || !strings.Contains(err.Error(), "GITLAB_MCP_CURSOR_KEY") {
		t.Fatalf("missing key %v", err)
	}
	if len(late) != 0 {
		t.Fatalf("missing key issued HTTP %#v", late)
	}
}

func TestPipelineGraph_resumeRejectsRaisedBounds(t *testing.T) {
	jobs := map[string]string{
		"1": "[" + jobJSON(1, "test", "success", "false") + "]",
		"2": "[" + jobJSON(2, "test", "running", "false") + "]",
	}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
	d := newCursorDeps(t, h, nil, clk)
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1, MaxNodes: 4}
	_, out, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := graphSection(t, out.(map[string]any))["next_cursor"].(string)
	if tok == "" {
		t.Fatal("missing cursor")
	}
	for _, mut := range []pipelineGraphIn{
		{ProjectID: "42", MergeRequestIID: 7, PerPage: 1, MaxNodes: 4, MaxDepth: 32, Cursor: tok},
		{ProjectID: "42", MergeRequestIID: 7, PerPage: 1, MaxNodes: 16, Cursor: tok},
		{ProjectID: "42", MergeRequestIID: 7, PerPage: 20, MaxNodes: 4, Cursor: tok},
		{ProjectID: "42", MergeRequestIID: 7, PerPage: 1, MaxNodes: 4, JobNames: []string{"test"}, Cursor: tok},
	} {
		_, _, err = getMergeRequestPipelineGraph(context.Background(), nil, mut, d)
		if err == nil || !strings.Contains(err.Error(), "filter mismatch") {
			t.Fatalf("raised bounds %v: %v", mut, err)
		}
	}
}

func TestPipelineGraph_forkHeadUsesSourceProject(t *testing.T) {
	var hits []graphHit
	jobs := map[string]string{
		"1": "[" + jobJSON(1, "test", "success", "false") + "]",
		"2": "[" + jobJSON(2, "test", "success", "false") + "]",
	}
	mr := fmt.Sprintf(`{"id":1,"iid":7,"project_id":42,"source_project_id":77,"target_project_id":42,"source_branch":"feature","sha":%q,"head_pipeline":{"id":100,"project_id":77,"sha":%q,"ref":"feature","status":"success"}}`, graphSrcSHA, graphPipeSHA)
	mrPipes := fmt.Sprintf(`[{"id":100,"project_id":77,"sha":%q,"ref":"feature","status":"success"}]`, graphPipeSHA)
	pipe := fmt.Sprintf(`{"id":100,"project_id":77,"sha":%q,"ref":"feature","status":"success","source":"push"}`, graphPipeSHA)
	h := graphHandler(&hits, map[int]string{100: pipe}, jobs, mr, mrPipes, false)
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	_, out, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	requireJobsOnlyUnknownDownstream(t, out.(map[string]any))
	sec := graphSection(t, out.(map[string]any))
	tok, _ := sec["next_cursor"].(string)
	if tok == "" {
		t.Fatal("expected a continuation cursor")
	}
	in.Cursor = tok
	_, out2, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	requireReadyComplete(t, out2.(map[string]any))
	sawForkPipe, sawForkJobs := false, false
	forkAuth, forkPipe := -1, -1
	for i, hit := range hits {
		if strings.Contains(hit.path, "/projects/42/pipelines/") {
			t.Fatalf("target project pipeline fetch %s", hit.path)
		}
		if hit.path == "/api/v4/projects/77" && forkAuth < 0 {
			forkAuth = i
		}
		if hit.path == "/api/v4/projects/77/pipelines/100" {
			sawForkPipe = true
			if forkPipe < 0 {
				forkPipe = i
			}
		}
		if strings.Contains(hit.path, "/projects/77/pipelines/100/jobs") {
			sawForkJobs = true
		}
	}
	if !sawForkPipe || !sawForkJobs || forkAuth < 0 || forkPipe < forkAuth {
		t.Fatalf("auth=%d pipe=%d hits %#v", forkAuth, forkPipe, hits)
	}
}

func TestPipelineGraph_retrySplitAcrossPages(t *testing.T) {
	jobs := map[string]string{
		"1": "[" + jobJSON(2, "test", "success", "false") + "]",
		"2": "[" + jobJSON(1, "test", "failed", "false") + "]",
	}
	h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	_, out, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	page1 := out.(map[string]any)
	tok, _ := graphSection(t, page1)["next_cursor"].(string)
	if tok == "" {
		t.Fatal("missing cursor")
	}
	in.Cursor = tok
	_, out2, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	page2 := out2.(map[string]any)
	requireReadyComplete(t, page2)
	if page2["assessment"] == assessBlocked {
		t.Fatal("older failed attempt on the next page blocked")
	}
	lineage := page2["lineage"].([]any)
	if len(lineage) != 1 || lineage[0].(map[string]any)["latest_known"] != false {
		t.Fatalf("lineage %#v", lineage)
	}
	jobsOut := page2["jobs"].([]any)
	if len(jobsOut) != 1 || jobsOut[0].(map[string]any)["attempt"] != attemptHistory {
		t.Fatalf("jobs %#v", jobsOut)
	}
}

func TestPipelineGraph_headMismatchStaysUnproven(t *testing.T) {
	var hits []graphHit
	mr := fmt.Sprintf(`{"id":1,"iid":7,"project_id":42,"source_branch":"feature","sha":%q,"head_pipeline":{"id":100,"project_id":42,"sha":%q,"ref":"feature","status":"success"}}`, graphSrcSHA, graphPipeSHA)
	listed := fmt.Sprintf(`[{"id":200,"project_id":42,"sha":%q,"ref":"old","status":"success"}]`, graphPipeSHA)
	historical := fmt.Sprintf(`{"id":200,"project_id":42,"sha":%q,"ref":"old","status":"success","source":"push"}`, graphPipeSHA)
	h := graphHandler(&hits, map[int]string{200: historical}, nil, mr, listed, false)
	out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireNotReady(t, out)
	if out["pipeline"] != nil || out["jobs"] != nil {
		t.Fatalf("selected a pipeline %#v", out["pipeline"])
	}
	for _, hit := range hits {
		if strings.Contains(hit.path, "/pipelines/200") || strings.Contains(hit.path, "/pipelines/100") {
			t.Fatalf("fetched %s", hit.path)
		}
	}
}

func TestPipelineGraph_stopsAtHeadPipeline(t *testing.T) {
	var hits []graphHit
	jobs := map[string]string{"1": "[" + jobJSON(1, "test", "success", "false") + "]"}
	base := graphHandler(&hits, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), "", false)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/merge_requests/7/pipelines") {
			hits = append(hits, graphHit{method: r.Method, path: r.URL.Path, raw: r.URL.RawQuery})
			page := r.URL.Query().Get("page")
			if page == "" || page == "1" {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Next-Page", "2")
				_, _ = io.WriteString(w, graphPipes("feature"))
				return
			}
			t.Errorf("pipeline list continued to page %s", page)
			http.Error(w, "continued", http.StatusInternalServerError)
			return
		}
		base.ServeHTTP(w, r)
	})
	out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireReadyComplete(t, out)
	if out["pipeline"] == nil || out["pipeline"].(map[string]any)["id"] != float64(100) {
		t.Fatalf("pipeline %#v", out["pipeline"])
	}
	lists := 0
	for _, hit := range hits {
		if strings.HasSuffix(hit.path, "/merge_requests/7/pipelines") {
			lists++
		}
	}
	if lists != 1 {
		t.Fatalf("pipeline list requests %d %#v", lists, hits)
	}
}

func TestPipelineGraph_headFoundDoesNotExhaustBudget(t *testing.T) {
	var hits []graphHit
	jobs := map[string]string{"1": "[" + jobJSON(1, "test", "success", "false") + "]"}
	base := graphHandler(&hits, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), "", false)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/merge_requests/7/pipelines") {
			hits = append(hits, graphHit{method: r.Method, path: r.URL.Path, raw: r.URL.RawQuery})
			page := r.URL.Query().Get("page")
			if page == "" || page == "1" {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Next-Page", "2")
				_, _ = io.WriteString(w, graphPipes("feature"))
				return
			}
			n, _ := strconv.Atoi(page)
			w.Header().Set("Content-Type", "application/json")
			if n >= 20 {
				w.Header().Set("X-Next-Page", "0")
				_, _ = io.WriteString(w, `[]`)
				return
			}
			w.Header().Set("X-Next-Page", strconv.Itoa(n+1))
			_, _ = fmt.Fprintf(w, `[{"id":%d,"project_id":42,"sha":%q,"ref":"old","status":"success"}]`, 200+n, graphPipeSHA)
			return
		}
		base.ServeHTTP(w, r)
	})
	out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, MaxRequests: 24}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireReadyComplete(t, out)
	if out["pipeline"] == nil || out["pipeline"].(map[string]any)["id"] != float64(100) {
		t.Fatalf("pipeline %#v", out["pipeline"])
	}
	lists := 0
	for _, hit := range hits {
		if strings.HasSuffix(hit.path, "/merge_requests/7/pipelines") {
			lists++
		}
	}
	if lists != 1 {
		t.Fatalf("pipeline list requests %d", lists)
	}
}

func TestPipelineGraph_noHeadWalksPipelineList(t *testing.T) {
	var hits []graphHit
	mr := fmt.Sprintf(`{"id":1,"iid":7,"project_id":42,"source_branch":"feature","sha":%q}`, graphSrcSHA)
	pipe := fmt.Sprintf(`{"id":200,"project_id":42,"sha":%q,"ref":"feature","status":"success","source":"push"}`, graphPipeSHA)
	jobs := map[string]string{"1": "[" + jobJSON(1, "test", "success", "false") + "]"}
	base := graphHandler(&hits, map[int]string{200: pipe}, jobs, mr, "", false)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/merge_requests/7/pipelines") {
			hits = append(hits, graphHit{method: r.Method, path: r.URL.Path, raw: r.URL.RawQuery})
			w.Header().Set("Content-Type", "application/json")
			page := r.URL.Query().Get("page")
			if page == "" || page == "1" {
				w.Header().Set("X-Next-Page", "2")
				_, _ = fmt.Fprintf(w, `[{"id":200,"project_id":42,"sha":%q,"ref":"feature","status":"success"}]`, graphPipeSHA)
				return
			}
			w.Header().Set("X-Next-Page", "0")
			_, _ = io.WriteString(w, `[]`)
			return
		}
		base.ServeHTTP(w, r)
	})
	out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7}, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireReadyComplete(t, out)
	if out["pipeline"] == nil || out["pipeline"].(map[string]any)["id"] != float64(200) {
		t.Fatalf("pipeline %#v", out["pipeline"])
	}
	lists := 0
	for _, hit := range hits {
		if strings.HasSuffix(hit.path, "/merge_requests/7/pipelines") {
			lists++
		}
	}
	if lists != 2 {
		t.Fatalf("pipeline list requests %d", lists)
	}
}

func TestPipelineGraph_compactLineageCursor(t *testing.T) {
	page1 := make([]string, 50)
	names := make([]string, 50)
	for i := 0; i < 50; i++ {
		names[i] = fmt.Sprintf("job-%03d-%s", i, strings.Repeat("n", 180))
		page1[i] = jobJSON(1000+i, names[i], "success", "false")
	}
	page2 := []string{
		jobJSON(1, names[0], "failed", "false"),
		jobJSON(9000, "unseen-required-failure", "failed", "false"),
	}
	jobs := map[string]string{
		"1": "[" + strings.Join(page1, ",") + "]",
		"2": "[" + strings.Join(page2, ",") + "]",
	}
	h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 50, MaxItems: 250}
	_, out, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	page := out.(map[string]any)
	tok, _ := graphSection(t, page)["next_cursor"].(string)
	if tok == "" {
		t.Fatalf("cursor not issued: %#v", graphSection(t, page)["limitations"])
	}
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
	if len(payload.PageState.LineageMax) != lineageFPCap+1 || payload.PageState.LineageMax[0] != "*" {
		t.Fatalf("lineage state %#v", payload.PageState.LineageMax)
	}
	for _, item := range payload.PageState.LineageMax {
		if strings.Contains(item, "job-") {
			t.Fatalf("job name stored in cursor: %s", item)
		}
	}
	in.Cursor = tok
	_, out2, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	next := out2.(map[string]any)
	requireNotReady(t, next)
	if next["assessment"] == assessReady {
		t.Fatal("saturated unseen required failure must not be ready")
	}
	if next["assessment"] == assessBlocked {
		t.Fatal("saturated or historical failure blocked")
	}
	for _, item := range next["lineage"].([]any) {
		if item.(map[string]any)["latest_known"] != false {
			t.Fatalf("lineage %#v", next["lineage"])
		}
	}
	attempts := map[float64]string{}
	for _, item := range next["jobs"].([]any) {
		job := item.(map[string]any)
		attempts[job["id"].(float64)] = job["attempt"].(string)
	}
	if attempts[1] != attemptHistory || attempts[9000] != attemptUnknown {
		t.Fatalf("attempts %#v", attempts)
	}
}

func lineageHasID(lineage []any, id float64) bool {
	for _, item := range lineage {
		g := item.(map[string]any)
		for _, key := range []string{"latest_ids", "history_ids"} {
			ids, _ := g[key].([]any)
			for _, raw := range ids {
				if raw == id {
					return true
				}
			}
		}
	}
	return false
}

func TestSelectParentID(t *testing.T) {
	if id, ambiguous, missing := selectParentID(100, []int64{200}, true); id != 0 || !ambiguous || missing {
		t.Fatalf("mismatch id=%d ambiguous=%v missing=%v", id, ambiguous, missing)
	}
	if id, ambiguous, missing := selectParentID(0, []int64{200}, true); id != 200 || ambiguous || missing {
		t.Fatalf("no-head fallback id=%d ambiguous=%v missing=%v", id, ambiguous, missing)
	}
	if id, _, _ := selectParentID(100, []int64{100, 200}, false); id != 100 {
		t.Fatalf("head in a partial list = %d", id)
	}
}

func TestPipelineGraph_statusOutcomesAndCancel(t *testing.T) {
	for _, status := range []string{"canceled", "running", "pending", "failed"} {
		status := status
		t.Run(status, func(t *testing.T) {
			jobs := map[string]string{"1": "[" + jobJSON(1, "test", status, "false") + "]"}
			h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
			out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", PipelineID: 100, MergeRequestIID: 7}, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireNotReady(t, out)
			switch status {
			case "canceled", "failed":
				if out["assessment"] != assessBlocked {
					t.Fatalf("%s assessment %v", status, out["assessment"])
				}
			default:
				if out["assessment"] != assessPartial {
					t.Fatalf("%s assessment %v", status, out["assessment"])
				}
			}
		})
	}
	t.Run("unknown status", func(t *testing.T) {
		jobs := map[string]string{"1": "[" + jobJSON(1, "test", "not-a-status", "false") + "]"}
		h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
		out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", PipelineID: 100}, nil)
		if err != nil {
			t.Fatal(err)
		}
		requireNotReady(t, out)
		if out["assessment"] != assessUnknown {
			t.Fatalf("assessment %v", out["assessment"])
		}
	})
	t.Run("cancelled context", func(t *testing.T) {
		h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, map[string]string{"1": "[]"}, "", "", false)
		d := newCursorDeps(t, h, nil, &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, out, err := getMergeRequestPipelineGraph(ctx, nil, pipelineGraphIn{ProjectID: "42", PipelineID: 100}, d)
		if err == nil || out != nil {
			t.Fatalf("err %v out %#v", err, out)
		}
	})
}

func TestPipelineGraph_jobGuardSeesStatusChange(t *testing.T) {
	jobs := map[string]string{
		"1": "[" + jobJSON(1, "test", "running", "false") + "]",
		"2": "[" + jobJSON(2, "test", "success", "false") + "]",
	}
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
	d := newCursorDeps(t, h, nil, clk)
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 1}
	_, out, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := graphSection(t, out.(map[string]any))["next_cursor"].(string)
	if tok == "" {
		t.Fatal("need job continuation")
	}
	jobs["1"] = "[" + jobJSON(1, "test", "failed", "false") + "]"
	in.Cursor = tok
	_, _, err = getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) {
		t.Fatalf("job status drift: %v", err)
	}
}

func TestPipelineGraph_mrContinuationKeepsBudgetItem(t *testing.T) {
	jobs := map[string]string{
		"1": "[" + jobJSON(1, "test", "success", "false") + "," + jobJSON(2, "lint", "success", "false") + "]",
		"2": "[" + jobJSON(3, "deploy", "success", "false") + "," + jobJSON(4, "gate", "manual", "false") + "]",
	}
	h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, graphMR("feature"), graphPipes("feature"), false)
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	in := pipelineGraphIn{ProjectID: "42", MergeRequestIID: 7, PerPage: 2}
	_, out, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := graphSection(t, out.(map[string]any))["next_cursor"].(string)
	if tok == "" {
		t.Fatal("missing cursor")
	}
	in.Cursor = tok
	in.MaxItems = 3
	_, out2, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
	if err != nil {
		t.Fatal(err)
	}
	page := out2.(map[string]any)
	requireNotReady(t, page)
	got := page["jobs"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["id"] != float64(3) {
		t.Fatalf("jobs %#v", got)
	}
	sec := graphSection(t, page)
	if !sectionMessage(sec, "budget_items") || sec["next_cursor"] != nil || sec["pagination_exhausted"] == true {
		t.Fatalf("section %#v", sec)
	}
}

func TestPipelineGraph_truncatedJobPageKeepsPrefix(t *testing.T) {
	job := jobJSON(1, "test", "success", "false")
	for _, tc := range []struct {
		name    string
		body    string
		message string
	}{
		{"framing", "[" + job + ",{", "malformed response"},
		{"malformed object", "[" + job + ",{}]", "malformed job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, nil, "", "", false)
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/jobs") {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Next-Page", "2")
					_, _ = io.WriteString(w, tc.body)
					return
				}
				base.ServeHTTP(w, r)
			})
			out, err := callGraph(t, h, pipelineGraphIn{ProjectID: "42", PipelineID: 100}, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireNotReady(t, out)
			got := out["jobs"].([]any)
			if len(got) != 1 || got[0].(map[string]any)["id"] != float64(1) {
				t.Fatalf("jobs %#v", got)
			}
			sec := graphSection(t, out)
			if !sectionMessage(sec, tc.message) || sec["next_cursor"] != nil || sec["pagination_exhausted"] == true || sec["content_complete"] == readmeta.ContentCompleteTrue {
				t.Fatalf("section %#v", sec)
			}
		})
	}
}

func TestPipelineGraph_filterSentinelBinding(t *testing.T) {
	jobs := map[string]string{
		"1": "[" + jobJSON(1, "test", "success", "false") + "]",
		"2": "[" + jobJSON(2, "test", "success", "false") + "]",
	}
	h := graphHandler(nil, map[int]string{100: graphPipe("feature", "push")}, jobs, "", "", false)
	clk := &cursor.FakeClock{T: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}
	d := newCursorDeps(t, h, nil, clk)
	mint := func(in pipelineGraphIn) string {
		t.Helper()
		_, out, err := getMergeRequestPipelineGraph(context.Background(), nil, in, d)
		if err != nil {
			t.Fatal(err)
		}
		tok, _ := graphSection(t, out.(map[string]any))["next_cursor"].(string)
		if tok == "" {
			t.Fatal("missing cursor")
		}
		return tok
	}
	base := pipelineGraphIn{ProjectID: "42", PipelineID: 100, PerPage: 1}
	for _, tc := range []struct {
		name   string
		dashed pipelineGraphIn
	}{
		{"names", pipelineGraphIn{ProjectID: "42", PipelineID: 100, PerPage: 1, JobNames: []string{"-"}}},
		{"stages", pipelineGraphIn{ProjectID: "42", PipelineID: 100, PerPage: 1, JobStages: []string{"-"}}},
		{"statuses", pipelineGraphIn{ProjectID: "42", PipelineID: 100, PerPage: 1, JobStatuses: []string{"-"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := mint(base)
			resume := tc.dashed
			resume.Cursor = plain
			_, _, err := getMergeRequestPipelineGraph(context.Background(), nil, resume, d)
			if err == nil || !strings.Contains(err.Error(), "filter mismatch") {
				t.Fatalf("empty cursor resumed with dash filter: %v", err)
			}
			dashed := mint(tc.dashed)
			resume = base
			resume.Cursor = dashed
			_, _, err = getMergeRequestPipelineGraph(context.Background(), nil, resume, d)
			if err == nil || !strings.Contains(err.Error(), "filter mismatch") {
				t.Fatalf("dash cursor resumed without filter: %v", err)
			}
		})
	}
}

func sectionMessage(sec map[string]any, message string) bool {
	lims, _ := sec["limitations"].([]any)
	for _, item := range lims {
		lim, _ := item.(map[string]any)
		if lim["message"] == message || lim["code"] == message {
			return true
		}
	}
	return false
}
