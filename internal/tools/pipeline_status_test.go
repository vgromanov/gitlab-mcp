package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

const (
	psSHA   = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	psOther = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// pipeWorld is a fake GitLab for pipeline reads. Keys: lists by sha ("sha" or
// "sha|ref"), jobs/bridges/status by "project/pipeline", MRs by "project/iid".
// Every body goes through writeFixture; failures are 403/404 (5xx would be retried).
type pipeWorld struct {
	mu      sync.Mutex
	log     []string
	lists   map[string][]string
	jobs    map[string][]string
	bridges map[string][]string
	status  map[string]int
	mrs     map[string]string
	cs      *mcp.ClientSession
}

var pipeRe = regexp.MustCompile(`^/api/v4/projects/([^/]+)/(?:pipelines(?:/(\d+)/(jobs|bridges))?|merge_requests/(\d+))$`)

func pipeInfo(id, project int, sha, status, source string) string {
	return fmt.Sprintf(`{"id":%d,"iid":1,"project_id":%d,"sha":%q,"status":%q,"source":%q,"ref":"main","web_url":"https://gl/x"}`, id, project, sha, status, source)
}

func pipeJob(id int, name, stage, status string, allow bool) string {
	return fmt.Sprintf(`{"id":%d,"name":%q,"stage":%q,"status":%q,"allow_failure":%t,"web_url":"https://gl/j","runner":{"id":1},"user":{"id":1},"artifacts":[{"filename":"a"}]}`, id, name, stage, status, allow)
}

func pipeBridge(id int, name, status, downstream string) string {
	return fmt.Sprintf(`{"id":%d,"name":%q,"stage":"trigger","status":%q,"allow_failure":false,"downstream_pipeline":%s}`, id, name, status, downstream)
}

func newPipeWorld(t *testing.T, cfg *config.Config, w *pipeWorld) *pipeWorld {
	t.Helper()
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		w.log = append(w.log, r.URL.Path+"?"+r.URL.RawQuery)
		w.mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		m := pipeRe.FindStringSubmatch(r.URL.Path)
		if m == nil {
			rw.WriteHeader(http.StatusNotFound)
			writeFixture(rw, `{"message":"404 Not Found"}`)
			return
		}
		project, q := m[1], r.URL.Query()
		var items []string
		switch {
		case m[4] != "":
			body, ok := w.mrs[project+"/"+m[4]]
			if !ok {
				rw.WriteHeader(http.StatusNotFound)
				writeFixture(rw, `{"message":"404 Not Found"}`)
				return
			}
			writeFixture(rw, body)
			return
		case m[2] == "":
			key := q.Get("sha")
			if ref := q.Get("ref"); ref != "" {
				key += "|" + ref
			}
			if st := w.status[project+"/list"]; st != 0 {
				rw.WriteHeader(st)
				writeFixture(rw, `{"message":"nope"}`)
				return
			}
			items = w.lists[key]
		default:
			key := project + "/" + m[2]
			if st := w.status[key]; st != 0 {
				rw.WriteHeader(st)
				writeFixture(rw, `{"message":"nope"}`)
				return
			}
			items = map[string]map[string][]string{"jobs": {key: w.jobs[key]}, "bridges": {key: w.bridges[key]}}[m[3]][key]
		}
		page, _ := strconv.Atoi(q.Get("page"))
		if per := q.Get("per_page"); page < 1 || per != "100" {
			rw.WriteHeader(http.StatusBadRequest)
			writeFixture(rw, `{"message":"page and per_page=100 must be sent"}`)
			return
		}
		lo, hi := (page-1)*100, min(page*100, len(items))
		if page*100 < len(items) {
			rw.Header().Set("X-Next-Page", strconv.Itoa(page+1))
		}
		if lo > hi {
			lo = hi
		}
		writeFixture(rw, "["+strings.Join(items[lo:hi], ",")+"]")
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterMergeRequests(srv, Deps{Config: cfg, Client: cli})
	w.cs = testutil.MCPConnect(t, srv)
	return w
}

func (w *pipeWorld) count(sub string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, p := range w.log {
		if strings.Contains(p, sub) {
			n++
		}
	}
	return n
}

func (w *pipeWorld) call(t *testing.T, tool string, args map[string]any) (map[string]any, string) {
	t.Helper()
	res, err := w.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		return nil, contentText(res)
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content %T", res.StructuredContent)
	}
	return m, ""
}

func (w *pipeWorld) status1(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	out, errText := w.call(t, "get_pipeline_status", args)
	if errText != "" {
		t.Fatal(errText)
	}
	return out
}

func rowsOf(t *testing.T, out map[string]any, key string) []map[string]any {
	t.Helper()
	raw, _ := out[key].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		rows = append(rows, r.(map[string]any))
	}
	return rows
}

func jobIDs(rows []map[string]any) []float64 {
	ids := []float64{}
	for _, r := range rows {
		ids = append(ids, r["id"].(float64))
	}
	return ids
}

// treeWorld: root pipeline 100 (project 42, 250 jobs over 3 pages, 2 bridge pages
// worth of bridges is covered by the 250 jobs; bridges here are 2), a child 200 in
// another project (77) whose own bridge leads to grandchild 400, and a child 300
// in project 88 that answers 403.
func treeWorld(rootStatus string) *pipeWorld {
	jobs := []string{pipeJob(1, "build", "build", "success", false), pipeJob(2, "lint", "test", "failed", true), pipeJob(3, "deploy", "deploy", "manual", true), pipeJob(4, "gate", "deploy", "manual", false), pipeJob(5, "unit", "test", "failed", false)}
	for i := len(jobs) + 1; i <= 250; i++ {
		jobs = append(jobs, pipeJob(i, fmt.Sprintf("j%d", i), "test", "success", false))
	}
	return &pipeWorld{
		lists: map[string][]string{psSHA: {pipeInfo(100, 42, psSHA, rootStatus, "push")}},
		jobs: map[string][]string{
			"42/100": jobs,
			"77/200": {pipeJob(201, "sec", "scan", "success", false)},
			"77/400": {pipeJob(401, "deep", "scan", "success", false)},
		},
		bridges: map[string][]string{
			"42/100": {
				pipeBridge(901, "to-77", "success", pipeInfo(200, 77, psOther, "success", "pipeline")),
				pipeBridge(902, "to-88", "success", pipeInfo(300, 88, psOther, "success", "pipeline")),
			},
			"77/200": {pipeBridge(903, "deeper", "success", pipeInfo(400, 77, psOther, "success", "parent_pipeline"))},
		},
		status: map[string]int{"88/300": http.StatusForbidden},
	}
}

// AC1 + AC2: several job pages, a bridge to another project, an inaccessible
// child (complete=false, never success), manual and allow_failure classified.
func TestGetPipelineStatus_treeWithInaccessibleChild(t *testing.T) {
	w := newPipeWorld(t, &config.Config{}, treeWorld("success"))
	out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA})

	if out["complete"] != false || out["overall_status"] != "unknown" || out["project_id"] != "42" || out["sha"] != psSHA {
		t.Fatalf("top = %v", out)
	}
	if reason, _ := out["truncated_reason"].(string); !strings.Contains(reason, "pipeline 300 (project 88)") || !strings.Contains(reason, "403") {
		t.Fatalf("truncated_reason = %v", out["truncated_reason"])
	}
	ps := rowsOf(t, out, "pipelines")
	if len(ps) != 4 || jobIDs(ps)[0] != 100 || jobIDs(ps)[1] != 200 || jobIDs(ps)[2] != 300 || jobIDs(ps)[3] != 400 {
		t.Fatalf("pipelines = %v", jobIDs(ps))
	}
	root := ps[0]
	if len(root["jobs"].([]any)) != 252 { // 250 over 3 pages + 2 bridge rows
		t.Fatalf("root jobs = %d, want 252", len(root["jobs"].([]any)))
	}
	if root["depth"] != nil || root["parent_pipeline_id"] != nil || root["incomplete"] != nil || root["source"] != "push" || root["project_id"] != float64(42) {
		t.Fatalf("root row = %v", root)
	}
	if ps[1]["depth"] != float64(1) || ps[1]["parent_pipeline_id"] != float64(100) || ps[1]["project_id"] != float64(77) {
		t.Fatalf("cross-project child = %v", ps[1])
	}
	if ps[3]["depth"] != float64(2) || ps[3]["parent_pipeline_id"] != float64(200) {
		t.Fatalf("grandchild = %v", ps[3])
	}
	// the inaccessible child keeps the status its bridge reports, no jobs, and says why
	if ps[2]["status"] != "success" || len(ps[2]["jobs"].([]any)) != 0 || !strings.Contains(ps[2]["incomplete"].(string), "403") {
		t.Fatalf("inaccessible child = %v", ps[2])
	}
	// rows are compact: exactly the listed keys, flags only when true
	j0 := root["jobs"].([]any)[0].(map[string]any)
	if !slices.Equal(sortedMapKeys(j0), []string{"id", "name", "stage", "status"}) {
		t.Fatalf("job keys = %v", sortedMapKeys(j0))
	}
	if raw := fmt.Sprint(out); strings.Contains(raw, "runner") || strings.Contains(raw, "artifacts") || strings.Contains(raw, "gl/j") {
		t.Fatal("job blobs leaked into the compact rows")
	}
	jobs := root["jobs"].([]any)
	if j := jobs[1].(map[string]any); j["allow_failure"] != true || j["status"] != "failed" {
		t.Fatalf("allowed failure row = %v", j)
	}
	if j := jobs[2].(map[string]any); j["manual"] != true || j["allow_failure"] != true {
		t.Fatalf("optional manual row = %v", j)
	}
	if j := jobs[250].(map[string]any); j["bridge"] != true || j["name"] != "to-77" {
		t.Fatalf("bridge row = %v", j)
	}
	// classification
	failed, allowed, manual := rowsOf(t, out, "failed_jobs"), rowsOf(t, out, "allowed_failed_jobs"), rowsOf(t, out, "manual_jobs")
	if !slices.Equal(jobIDs(failed), []float64{5}) || failed[0]["pipeline_id"] != float64(100) || failed[0]["project_id"] != float64(42) || failed[0]["allow_failure"] != nil {
		t.Fatalf("failed_jobs = %v", failed)
	}
	if !slices.Equal(jobIDs(allowed), []float64{2}) || allowed[0]["allow_failure"] != true {
		t.Fatalf("allowed_failed_jobs = %v", allowed)
	}
	if !slices.Equal(jobIDs(manual), []float64{3, 4}) || manual[0]["allow_failure"] != true || manual[1]["allow_failure"] != nil {
		t.Fatalf("manual_jobs = %v", manual)
	}
	// 3 job pages + 1 bridge page for the root, jobs+bridges for 200 and 400, the 403 for 300 and the list
	if w.count("/pipelines/100/jobs") != 3 || w.count("/pipelines/100/bridges") != 1 || w.count("/pipelines/300/jobs") != 1 || w.count("/pipelines/300/bridges") != 0 {
		t.Fatalf("requests = %v", w.log)
	}
	if out["requests"] != float64(len(w.log)) || len(w.log) != 1+4+2+1+2 {
		t.Fatalf("requests = %v, log %d", out["requests"], len(w.log))
	}
}

// jobs=problems: green jobs are only counted; classification and the verdict are unchanged.
func TestGetPipelineStatus_jobsProblems(t *testing.T) {
	w := newPipeWorld(t, &config.Config{}, treeWorld("success"))
	all := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA, "jobs": "all"})
	if rowsOf(t, all, "pipelines")[0]["job_counts"] != nil {
		t.Fatal("jobs=all must not carry job_counts")
	}
	out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA, "jobs": "problems"})
	root := rowsOf(t, out, "pipelines")[0]
	if !slices.Equal(jobIDs(rowsOf(t, root, "jobs")), []float64{2, 3, 4, 5}) {
		t.Fatalf("problem jobs = %v", root["jobs"])
	}
	if c := fmt.Sprint(root["job_counts"]); c != "map[failed:2 manual:2 success:248]" {
		t.Fatalf("job_counts = %s", c)
	}
	if ps := rowsOf(t, out, "pipelines"); len(ps[1]["jobs"].([]any)) != 0 || fmt.Sprint(ps[1]["job_counts"]) != "map[success:2]" {
		t.Fatalf("child = %v", ps[1])
	}
	for _, k := range []string{"overall_status", "complete", "failed_jobs", "allowed_failed_jobs", "manual_jobs", "requests", "truncated_reason"} {
		if fmt.Sprint(out[k]) != fmt.Sprint(all[k]) {
			t.Fatalf("%s differs between modes: %v vs %v", k, out[k], all[k])
		}
	}
	a, _ := json.Marshal(all)
	b, _ := json.Marshal(out)
	if len(b) >= len(a)/4 {
		t.Fatalf("problems output (%d bytes) is not much smaller than all (%d bytes)", len(b), len(a))
	}
}

func TestGetPipelineStatus_completeTreeSucceeds(t *testing.T) {
	tw := treeWorld("success")
	delete(tw.status, "88/300")
	tw.jobs["88/300"] = []string{pipeJob(301, "x", "s", "success", false)}
	tw.jobs["42/100"] = tw.jobs["42/100"][:0]
	tw.bridges["88/300"] = nil
	w := newPipeWorld(t, &config.Config{}, tw)
	out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA})
	if out["overall_status"] != "success" || out["complete"] != true || out["truncated_reason"] != nil {
		t.Fatalf("complete tree = %v", out)
	}
	for _, k := range []string{"failed_jobs", "allowed_failed_jobs", "manual_jobs"} {
		if rows, ok := out[k].([]any); !ok || len(rows) != 0 {
			t.Fatalf("%s = %v, want an empty list", k, out[k])
		}
	}
}

// A downstream failure counts even when the parent (no strategy: depend) is green.
func TestGetPipelineStatus_childFailureFailsOverall(t *testing.T) {
	tw := treeWorld("success")
	tw.status = nil
	tw.jobs["88/300"] = nil
	tw.bridges["42/100"][1] = pipeBridge(902, "to-88", "failed", pipeInfo(300, 88, psOther, "failed", "pipeline"))
	w := newPipeWorld(t, &config.Config{}, tw)
	out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA})
	if out["overall_status"] != "failed" || out["complete"] != true {
		t.Fatalf("overall = %v complete = %v", out["overall_status"], out["complete"])
	}
}

// Depth: roots are depth 0, children are followed down to depth 2; anything
// below is listed from its bridge, not read, and the graph is incomplete.
func TestGetPipelineStatus_depthLimit(t *testing.T) {
	w := newPipeWorld(t, &config.Config{}, &pipeWorld{
		lists: map[string][]string{psSHA: {pipeInfo(1, 42, psSHA, "success", "push")}},
		jobs:  map[string][]string{"42/1": nil, "42/2": nil, "42/3": nil},
		bridges: map[string][]string{
			"42/1": {pipeBridge(11, "b1", "success", pipeInfo(2, 42, psSHA, "success", "parent_pipeline"))},
			"42/2": {pipeBridge(12, "b2", "success", pipeInfo(3, 42, psSHA, "success", "parent_pipeline"))},
			"42/3": {pipeBridge(13, "b3", "success", pipeInfo(4, 42, psSHA, "failed", "parent_pipeline"))},
		},
	})
	out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA})
	ps := rowsOf(t, out, "pipelines")
	if len(ps) != 4 || ps[3]["depth"] != float64(3) || !strings.Contains(ps[3]["incomplete"].(string), "depth limit") {
		t.Fatalf("pipelines = %v", ps)
	}
	if w.count("/pipelines/4/") != 0 {
		t.Fatalf("a pipeline below the depth limit was read: %v", w.log)
	}
	if out["complete"] != false || out["overall_status"] != "failed" { // the bridge-reported failure still counts
		t.Fatalf("top = %v / %v", out["complete"], out["overall_status"])
	}
	// with a green pipeline 4 the incomplete graph must not report success
	w2 := newPipeWorld(t, &config.Config{}, &pipeWorld{
		lists: map[string][]string{psSHA: {pipeInfo(1, 42, psSHA, "success", "push")}},
		jobs:  map[string][]string{"42/1": nil, "42/2": nil, "42/3": nil},
		bridges: map[string][]string{
			"42/1": {pipeBridge(11, "b1", "success", pipeInfo(2, 42, psSHA, "success", "parent_pipeline"))},
			"42/2": {pipeBridge(12, "b2", "success", pipeInfo(3, 42, psSHA, "success", "parent_pipeline"))},
			"42/3": {pipeBridge(13, "b3", "success", pipeInfo(4, 42, psSHA, "success", "parent_pipeline"))},
		},
	})
	if out := w2.status1(t, map[string]any{"project_id": "42", "sha": psSHA}); out["overall_status"] != "unknown" || out["complete"] != false {
		t.Fatalf("depth-limited graph = %v / %v", out["overall_status"], out["complete"])
	}
}

func TestGetPipelineStatus_requestCap(t *testing.T) {
	w := newPipeWorld(t, &config.Config{}, treeWorld("success"))
	out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA, "max_requests": 4})
	if out["requests"] != float64(4) || len(w.log) != 4 || out["complete"] != false || out["overall_status"] != "unknown" {
		t.Fatalf("cap: requests=%v log=%d complete=%v overall=%v", out["requests"], len(w.log), out["complete"], out["overall_status"])
	}
	reason := out["truncated_reason"].(string)
	if !strings.Contains(reason, "max_requests=4") || !strings.Contains(reason, "pipeline 100") {
		t.Fatalf("truncated_reason = %q", reason)
	}
	// list + 3 job pages used the whole budget: the bridges were never read, so no child exists yet
	if ps := rowsOf(t, out, "pipelines"); len(ps) != 1 || !strings.Contains(ps[0]["incomplete"].(string), "max_requests=4") {
		t.Fatalf("pipelines = %v", ps)
	}
	// a cap that lets the root through but not its children marks the unread ones
	w2 := newPipeWorld(t, &config.Config{}, treeWorld("success"))
	out = w2.status1(t, map[string]any{"project_id": "42", "sha": psSHA, "max_requests": 6})
	ps := rowsOf(t, out, "pipelines")
	if len(ps) != 3 || ps[1]["incomplete"] == nil || !strings.Contains(ps[2]["incomplete"].(string), "not read") {
		t.Fatalf("pipelines under a 6-request cap = %v", ps)
	}
	// limits: the cap is clamped to 200
	if got := w2.status1(t, map[string]any{"project_id": "42", "sha": psSHA, "max_requests": 100000})["requests"]; got != float64(1+4+2+1+2) {
		t.Fatalf("requests = %v", got)
	}
}

func TestGetPipelineStatus_requestCapOnRootList(t *testing.T) {
	infos := []string{}
	for i := 1; i <= 150; i++ {
		infos = append(infos, pipeInfo(i, 42, psSHA, "success", "push"))
	}
	w := newPipeWorld(t, &config.Config{}, &pipeWorld{lists: map[string][]string{psSHA: infos}})
	out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA, "max_requests": 1})
	if out["complete"] != false || !strings.Contains(out["truncated_reason"].(string), "before the last page") || len(rowsOf(t, out, "pipelines")) != 100 {
		t.Fatalf("root list cap = %v / %d rows", out["truncated_reason"], len(rowsOf(t, out, "pipelines")))
	}
}

func TestGetPipelineStatus_disallowedProject(t *testing.T) {
	w := newPipeWorld(t, &config.Config{AllowedProjectIDs: []string{"42", "88"}}, treeWorld("success"))
	out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA})
	ps := rowsOf(t, out, "pipelines")
	if ps[1]["incomplete"] == nil || !strings.Contains(ps[1]["incomplete"].(string), "GITLAB_ALLOWED_PROJECT_IDS") || out["complete"] != false {
		t.Fatalf("disallowed child = %v", ps[1])
	}
	if w.count("projects/77/") != 0 {
		t.Fatalf("a disallowed project was requested: %v", w.log)
	}
	// the root itself is checked before any request
	w2 := newPipeWorld(t, &config.Config{AllowedProjectIDs: []string{"88"}}, treeWorld("success"))
	if _, errText := w2.call(t, "get_pipeline_status", map[string]any{"project_id": "42", "sha": psSHA}); !strings.Contains(errText, "not allowed") || len(w2.log) != 0 {
		t.Fatalf("root project: %q, %d requests", errText, len(w2.log))
	}
}

func TestGetPipelineStatus_mrModeAndRef(t *testing.T) {
	head := `{"id":100,"project_id":42,"sha":"` + psSHA + `","status":"running","source":"merge_request_event"}`
	mr := func(hp string) string {
		return `{"iid":7,"sha":"` + psSHA + `","head_pipeline":` + hp + `}`
	}
	w := newPipeWorld(t, &config.Config{}, &pipeWorld{
		mrs:   map[string]string{"42/7": mr(head), "42/8": mr("null"), "42/9": mr("null")},
		lists: map[string][]string{psSHA: {pipeInfo(101, 42, psSHA, "pending", "push")}},
		jobs:  map[string][]string{"42/100": {pipeJob(1, "a", "s", "running", false)}, "42/101": nil},
	})
	// head pipeline: one MR read, no list
	out := w.status1(t, map[string]any{"project_id": 42, "mr_iid": 7})
	ps := rowsOf(t, out, "pipelines")
	if len(ps) != 1 || ps[0]["id"] != float64(100) || ps[0]["source"] != "merge_request_event" || out["overall_status"] != "running" || out["sha"] != psSHA {
		t.Fatalf("mr head = %v", out)
	}
	if w.count("/pipelines?") != 0 || w.count("/merge_requests/7") != 1 || out["requests"] != float64(3) {
		t.Fatalf("requests = %v", w.log)
	}
	// no head pipeline attached: fall back to the pipelines of the MR's head sha
	out = w.status1(t, map[string]any{"project_id": "42", "mr_iid": 8})
	if ps := rowsOf(t, out, "pipelines"); len(ps) != 1 || ps[0]["id"] != float64(101) || out["overall_status"] != "pending" {
		t.Fatalf("fallback = %v", out)
	}
	// ref narrows the sha list; no match is none, complete (a fresh push has no pipeline yet)
	out = w.status1(t, map[string]any{"project_id": "42", "sha": strings.ToUpper(psSHA), "ref": "main"})
	if out["overall_status"] != "none" || out["complete"] != true || len(rowsOf(t, out, "pipelines")) != 0 || out["sha"] != psSHA {
		t.Fatalf("empty = %v", out)
	}
	if w.count("sha="+psSHA) == 0 || w.count("ref=main") != 1 {
		t.Fatalf("ref/sha query = %v", w.log)
	}
}

func TestGetPipelineStatus_parentPipelineRootIsFoundThroughItsBridge(t *testing.T) {
	// REST lists a child (source parent_pipeline) next to its parent; it must show once, at depth 1.
	w := newPipeWorld(t, &config.Config{}, &pipeWorld{
		lists: map[string][]string{psSHA: {pipeInfo(1, 42, psSHA, "success", "push"), pipeInfo(2, 42, psSHA, "success", "parent_pipeline")}},
		jobs:  map[string][]string{"42/1": nil, "42/2": nil},
		bridges: map[string][]string{
			"42/1": {pipeBridge(11, "child", "success", pipeInfo(2, 42, psSHA, "success", "parent_pipeline"))},
		},
	})
	out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA})
	ps := rowsOf(t, out, "pipelines")
	if len(ps) != 2 || ps[1]["depth"] != float64(1) || ps[1]["parent_pipeline_id"] != float64(1) || out["overall_status"] != "success" {
		t.Fatalf("pipelines = %v", ps)
	}
	// a sha whose only pipelines are children keeps them as roots
	w = newPipeWorld(t, &config.Config{}, &pipeWorld{
		lists: map[string][]string{psSHA: {pipeInfo(2, 42, psSHA, "success", "parent_pipeline")}},
		jobs:  map[string][]string{"42/2": nil},
	})
	if out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA}); len(rowsOf(t, out, "pipelines")) != 1 {
		t.Fatalf("children-only list = %v", out)
	}
}

// Two bridges (or a root that is also a downstream) leading to one pipeline list and read it once.
func TestGetPipelineStatus_sharedDownstreamIsReadOnce(t *testing.T) {
	child := pipeInfo(2, 42, psSHA, "success", "pipeline")
	w := newPipeWorld(t, &config.Config{}, &pipeWorld{
		lists: map[string][]string{psSHA: {pipeInfo(1, 42, psSHA, "success", "push"), pipeInfo(3, 42, psSHA, "success", "push")}},
		jobs:  map[string][]string{"42/1": nil, "42/2": nil, "42/3": nil},
		bridges: map[string][]string{
			"42/1": {pipeBridge(11, "a", "success", child), pipeBridge(12, "b", "success", child), pipeBridge(13, "root-again", "success", pipeInfo(3, 42, psSHA, "success", "pipeline"))},
		},
	})
	out := w.status1(t, map[string]any{"project_id": "42", "sha": psSHA})
	if ps := rowsOf(t, out, "pipelines"); len(ps) != 3 || !slices.Equal(jobIDs(ps), []float64{1, 3, 2}) || w.count("/pipelines/2/jobs") != 1 || w.count("/pipelines/3/jobs") != 1 {
		t.Fatalf("pipelines = %v, log %v", jobIDs(rowsOf(t, out, "pipelines")), w.log)
	}
}

func TestGetPipelineStatus_bridgeWithoutDownstream(t *testing.T) {
	build := func(st string) *pipeWorld {
		return &pipeWorld{
			lists:   map[string][]string{psSHA: {pipeInfo(1, 42, psSHA, "success", "push")}},
			jobs:    map[string][]string{"42/1": nil},
			bridges: map[string][]string{"42/1": {pipeBridge(11, "hidden", st, "null")}},
		}
	}
	// ran or succeeded but no downstream visible: unreadable, so incomplete
	out := newPipeWorld(t, &config.Config{}, build("success")).status1(t, map[string]any{"project_id": "42", "sha": psSHA})
	if out["complete"] != false || out["overall_status"] != "unknown" || !strings.Contains(out["truncated_reason"].(string), `bridge "hidden"`) {
		t.Fatalf("hidden downstream = %v / %v", out["complete"], out["truncated_reason"])
	}
	// a bridge that never started (manual / created) or failed has nothing to follow
	for _, st := range []string{"manual", "created", "failed"} {
		if out := newPipeWorld(t, &config.Config{}, build(st)).status1(t, map[string]any{"project_id": "42", "sha": psSHA}); out["complete"] != true {
			t.Fatalf("bridge %s: complete = %v", st, out["complete"])
		}
	}
}

func TestGetPipelineStatus_rootErrorsAndInputs(t *testing.T) {
	w := newPipeWorld(t, &config.Config{}, &pipeWorld{
		lists:  map[string][]string{psSHA: {pipeInfo(1, 42, psSHA, "success", "push")}},
		status: map[string]int{"42/1": http.StatusForbidden, "43/list": http.StatusForbidden},
		mrs:    map[string]string{},
	})
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"root jobs 403", map[string]any{"project_id": "42", "sha": psSHA}, "read pipeline 1"},
		{"list 403", map[string]any{"project_id": "43", "sha": psSHA}, "list pipelines of " + psSHA},
		{"mr 404", map[string]any{"project_id": "42", "mr_iid": 5}, "404"},
		{"neither", map[string]any{"project_id": "42"}, "exactly one of sha or mr_iid"},
		{"both", map[string]any{"project_id": "42", "sha": psSHA, "mr_iid": 1}, "exactly one of sha or mr_iid"},
		{"negative iid", map[string]any{"project_id": "42", "mr_iid": -1}, "mr_iid must be >= 1"},
		{"short sha", map[string]any{"project_id": "42", "sha": "abc123"}, "full 40-character"},
		{"branch as sha", map[string]any{"project_id": "42", "sha": "main"}, "full 40-character"},
		{"ref with mr", map[string]any{"project_id": "42", "mr_iid": 1, "ref": "main"}, "ref can only be combined with sha"},
		{"bad jobs mode", map[string]any{"project_id": "42", "sha": psSHA, "jobs": "none"}, `invalid jobs "none": must be one of all, problems`},
		{"bad project", map[string]any{"project_id": 1.5, "sha": psSHA}, "project_id"},
	} {
		if _, errText := w.call(t, "get_pipeline_status", tc.args); !strings.Contains(errText, tc.want) {
			t.Errorf("%s: error = %q, want %q", tc.name, errText, tc.want)
		}
	}
}

func TestFoldStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		statuses []string
		complete bool
		want     string
	}{
		{"nothing, complete", nil, true, "none"},
		{"nothing, incomplete", nil, false, "unknown"},
		{"all success", []string{"success", "success"}, true, "success"},
		{"success + skipped", []string{"success", "skipped"}, true, "success"},
		{"all skipped", []string{"skipped", "skipped"}, true, "skipped"},
		{"success but incomplete", []string{"success"}, false, "unknown"},
		{"skipped but incomplete", []string{"skipped"}, false, "unknown"},
		{"failed beats running", []string{"running", "failed", "success"}, true, "failed"},
		{"failed beats everything even when incomplete", []string{"manual", "failed", "canceled"}, false, "failed"},
		{"canceled beats running", []string{"running", "canceled"}, true, "canceled"},
		{"canceling is canceled", []string{"canceling", "success"}, true, "canceled"},
		{"running beats pending", []string{"pending", "running"}, true, "running"},
		{"running beats manual", []string{"manual", "running"}, true, "running"},
		{"created is pending", []string{"success", "created"}, true, "pending"},
		{"waiting_for_resource is pending", []string{"waiting_for_resource"}, true, "pending"},
		{"preparing is pending", []string{"preparing"}, true, "pending"},
		{"scheduled is pending", []string{"scheduled"}, true, "pending"},
		{"pending beats manual", []string{"manual", "pending"}, true, "pending"},
		{"manual gate", []string{"success", "manual"}, true, "manual"},
		{"manual stays manual when incomplete", []string{"manual"}, false, "manual"},
		{"unknown status is never success", []string{"success", "frobnicated"}, true, "unknown"},
	} {
		if got := foldStatus(tc.statuses, tc.complete); got != tc.want {
			t.Errorf("%s: foldStatus(%v, %t) = %q, want %q", tc.name, tc.statuses, tc.complete, got, tc.want)
		}
	}
}

// Snapshot: pipeline is opt-in, reuses the same read, and its failure stays inside its section.
func TestGetReviewSnapshot_pipelineSection(t *testing.T) {
	head := `{"id":100,"project_id":42,"sha":"` + psSHA + `","status":"success","source":"merge_request_event"}`
	mr := func(iid, hp string) string {
		return `{"iid":` + iid + `,"sha":"` + psSHA + `","title":"t","head_pipeline":` + hp + `}`
	}
	w := newPipeWorld(t, &config.Config{}, &pipeWorld{
		mrs: map[string]string{"42/1": mr("1", head), "42/2": mr("2", head)},
		jobs: map[string][]string{
			"42/100": {pipeJob(1, "a", "s", "success", false)},
		},
		status: map[string]int{},
	})
	mrs := []any{map[string]any{"project_id": "42", "iid": 1}}

	// opt-in: an omitted include never reads the pipeline section
	out, errText := w.call(t, "get_review_snapshot", map[string]any{"mrs": mrs})
	if errText != "" {
		t.Fatal(errText)
	}
	if e := snapEntries(t, out)[0]; e["pipeline"] != nil || w.count("/pipelines") != 0 || strings.Contains(fmt.Sprint(out["include"]), "pipeline") {
		t.Fatalf("default include read the pipeline: %v / %v", sortedMapKeys(e), out["include"])
	}

	// explicit: same shape as get_pipeline_status for that MR
	w.mu.Lock()
	w.log = nil
	w.mu.Unlock()
	out, errText = w.call(t, "get_review_snapshot", map[string]any{"mrs": mrs, "include": []any{"pipeline"}})
	if errText != "" {
		t.Fatal(errText)
	}
	sec := snapEntries(t, out)[0]["pipeline"].(map[string]any)
	direct := w.status1(t, map[string]any{"project_id": "42", "mr_iid": 1, "jobs": "problems"})
	a, _ := json.Marshal(sec)
	b, _ := json.Marshal(direct)
	if string(a) != string(b) || sec["overall_status"] != "success" || sec["complete"] != true {
		t.Fatalf("section != tool output:\n%s\n%s", a, b)
	}
	if fmt.Sprint(out["include"]) != "[pipeline]" {
		t.Fatalf("include = %v", out["include"])
	}

	// a failing pipeline read is that section's {error}; the sibling section and the batch survive
	w.status["42/100"] = http.StatusForbidden
	out, errText = w.call(t, "get_review_snapshot", map[string]any{"mrs": []any{
		map[string]any{"project_id": "42", "iid": 1}, map[string]any{"project_id": "42", "iid": 2},
	}, "include": []any{"pipeline"}})
	if errText != "" {
		t.Fatal(errText)
	}
	for i, e := range snapEntries(t, out) {
		sec, _ := e["pipeline"].(map[string]any)
		if sec == nil || !strings.Contains(fmt.Sprint(sec["error"]), "read pipeline 100") || e["sha"] != psSHA || e["error"] != nil {
			t.Fatalf("entry %d pipeline = %v (entry keys %v)", i, sec, sortedMapKeys(e))
		}
	}
}

func TestGetPipelineStatus_registeredInReviewProfile(t *testing.T) {
	if !slices.Contains(ReviewTools(), "get_pipeline_status") || slices.Contains(DailyTools(), "get_pipeline_status") {
		t.Fatal("get_pipeline_status belongs to the review profile only (not the daily set)")
	}
}
