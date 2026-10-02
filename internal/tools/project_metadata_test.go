package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

const (
	plantedImportURL    = "https://user:s3cretPASS@evil.example/import.git"
	plantedRunnerToken  = "glrt-PLANTED_RUNNER_TOKEN_do_not_leak"
	plantedNestedSecret = "NESTED_SECRET_VALUE_do_not_leak"
	plantedFutureField  = "FUTURE_SDK_FIELD_do_not_leak"
	safeWebURL          = "https://gitlab.example.com/group/project"
)

func richPlantedProjectJSON(id int64, webURL string) string {
	return `{
  "id": ` + itoa(id) + `,
  "name": "project",
  "path": "project",
  "path_with_namespace": "group/project",
  "default_branch": "main",
  "visibility": "private",
  "web_url": ` + jsonString(webURL) + `,
  "archived": false,
  "import_url": "` + plantedImportURL + `",
  "runners_token": "` + plantedRunnerToken + `",
  "http_url_to_repo": "https://gitlab.example.com/group/project.git",
  "future_unknown_sdk_field": "` + plantedFutureField + `",
  "namespace": {
    "id": 7,
    "name": "group",
    "path": "group",
    "full_path": "group",
    "kind": "group",
    "parent_id": 1,
    "avatar_url": "https://evil.example/a.png",
    "web_url": "https://gitlab.example.com/group",
    "nested_secret": "` + plantedNestedSecret + `"
  }
}`
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestSafeProjectWebURL(t *testing.T) {
	cases := []struct {
		in   string
		keep bool
	}{
		{safeWebURL, true},
		{"http://gitlab.example.com/g/p", true},
		{"https://gitlab.example.com/group/project%20name", true},
		{"https://gitlab.example.com:443/g/p", true},
		{"https://[::1]/g/p", true},
		{"https://[2001:db8::1]:8443/g/p", true},
		{"", false},
		{"/relative/path", false},
		{"gitlab.example.com/g/p", false},
		{"ftp://gitlab.example.com/g/p", false},
		{"https:///nohost", false},
		{"https://:443/p", false}, // Host=":443" but Hostname() empty
		{"https://:80/", false},
		{"https://[]/", false},
		{"https://[::1/", false}, // malformed bracket
		{"https://user:pass@gitlab.example.com/g/p", false},
		{"https://user@gitlab.example.com/g/p", false},
		{"https://@gitlab.example.com/g/p", false},
		{"https://:pass@gitlab.example.com/g/p", false},
		{"https://gitlab.example.com/g/p?token=x", false},
		{"https://gitlab.example.com/g/p?unknown_key=1", false},
		{"https://gitlab.example.com/g/p?", false},
		{"https://gitlab.example.com/g/p#frag", false},
		{"https://gitlab.example.com/g/p#", false},
		{"https://gitlab.example.com/g/p?x#y", false},
		{"not a url", false},
		{"https://gitlab.example.com/g/p\x00", false},
		{"https://gitlab.example.com/g/p\n", false},
		{"http:opaque", false},
		// Percent-decoded controls in Path must not evade the raw-string check.
		{"https://gitlab.example.com/g/%0Asecret", false},
		{"https://gitlab.example.com/g/%0Dsecret", false},
		{"https://gitlab.example.com/g/%00x", false},
		// Strict percent-encoding: incomplete / invalid sequences.
		{"https://gitlab.example.com/g/%ZZ", false},
		{"https://gitlab.example.com/g/%A", false},
		{"https://gitlab.example.com/g/%", false},
	}
	for _, tc := range cases {
		got, ok := safeProjectWebURL(tc.in)
		if ok != tc.keep {
			t.Fatalf("safeProjectWebURL(%q) ok=%v want %v", tc.in, ok, tc.keep)
		}
		if tc.keep && got != tc.in {
			t.Fatalf("safe URL must be unchanged: got %q want %q", got, tc.in)
		}
		if !tc.keep && got != "" {
			t.Fatalf("unsafe URL must omit empty: got %q for %q", got, tc.in)
		}
	}
}

func TestUsableGetProjectResponse_binding(t *testing.T) {
	if err := usableGetProjectResponse(nil, "42"); err == nil || !strings.Contains(err.Error(), readmeta.CodeHTTPError) {
		t.Fatalf("nil: %v", err)
	}
	if err := usableGetProjectResponse(&gitlab.Project{ID: 0}, "42"); err == nil || !strings.Contains(err.Error(), readmeta.CodeHTTPError) {
		t.Fatalf("nonpositive: %v", err)
	}
	if err := usableGetProjectResponse(&gitlab.Project{ID: 99}, "42"); err == nil || !strings.Contains(err.Error(), readmeta.CodeHTTPError) {
		t.Fatalf("numeric mismatch: %v", err)
	}
	if err := usableGetProjectResponse(&gitlab.Project{ID: 42}, "42"); err != nil {
		t.Fatal(err)
	}
	// Non-numeric expect pid (legacy inactive-policy path token) skips numeric bind.
	if err := usableGetProjectResponse(&gitlab.Project{ID: 42}, "group/project"); err != nil {
		t.Fatal(err)
	}
}

func TestProjectMetadataFromSDK_allowlistAndNil(t *testing.T) {
	p := &gitlab.Project{
		ID:                42,
		Name:              "project",
		Path:              "project",
		PathWithNamespace: "group/project",
		DefaultBranch:     "main",
		Visibility:        gitlab.PrivateVisibility,
		WebURL:            "https://user:pass@evil.example/p?x=1",
		Archived:          true,
		ImportURL:         plantedImportURL,
		RunnersToken:      plantedRunnerToken,
		Namespace: &gitlab.ProjectNamespace{
			ID: 7, Name: "group", Path: "group", FullPath: "group", Kind: "group",
			ParentID: 9, AvatarURL: "https://x/a", WebURL: "https://x/g",
		},
	}
	out := projectMetadataFromSDK(p)
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	assertExactGetProjectKeys(t, m)
	if _, ok := m["web_url"]; ok {
		t.Fatalf("unsafe web_url must be absent: %s", raw)
	}
	for _, leak := range []string{plantedImportURL, plantedRunnerToken, "parent_id", "avatar_url", "import_url", "runners_token"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("leak %q in %s", leak, raw)
		}
	}
	assertExactNamespaceKeys(t, m["namespace"].(map[string]any))
}

// getProjectRouteTracker records shared-endpoint GetProject hits by project id token.
// Identity lookups (authz) and post-authz content fetches use the same route; tests
// distinguish them by hit counts (deny ⇒ 1 hit on denied id; allow ⇒ ≥2 on allowed id).
type getProjectRouteTracker struct {
	mu    sync.Mutex
	byID  map[string]int32
	total int32
}

func (h *getProjectRouteTracker) note(id string) int32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byID == nil {
		h.byID = map[string]int32{}
	}
	h.byID[id]++
	atomic.AddInt32(&h.total, 1)
	return h.byID[id]
}

func (h *getProjectRouteTracker) hits(id string) int32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byID[id]
}

func plantedGetProjectHandler(t *testing.T, tracker *getProjectRouteTracker, webURL string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok, ok := projectGetRouteToken(r)
		if r.Method != http.MethodGet || !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"unexpected_path_SECRET_do_not_leak"}`)
			return
		}
		tracker.note(tok)
		id := echoProjectRouteID(tok, 42)
		_, _ = io.WriteString(w, richPlantedProjectJSON(id, webURL))
	})
}

// projectGetRouteToken returns the single /projects/{id} path token from EscapedPath
// so path aliases like group%2Fproject are distinguished from nested routes.
func projectGetRouteToken(r *http.Request) (string, bool) {
	const prefix = "/api/v4/projects/"
	esc := r.URL.EscapedPath()
	if !strings.HasPrefix(esc, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(esc, prefix)
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

func echoProjectRouteID(tok string, fallback int64) int64 {
	if id, err := strconv.ParseInt(tok, 10, 64); err == nil && id > 0 {
		return id
	}
	return fallback
}

func callGetProject(t *testing.T, d Deps, projectID string) (*mcp.CallToolResult, error) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterProjects(srv, d)
	cs := testutil.MCPConnect(t, srv)
	return cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_project",
		Arguments: map[string]any{"project_id": projectID},
	})
}

func getProjectDeps(t *testing.T, h http.Handler, cfg *config.Config) Deps {
	t.Helper()
	cli, _ := testutil.NewGitLabClient(t, h)
	if cfg == nil {
		cfg = &config.Config{}
	}
	return Deps{Config: cfg, Client: cli}
}

func assertExactGetProjectKeys(t *testing.T, m map[string]any) {
	t.Helper()
	required := []string{"id", "name", "path", "path_with_namespace", "default_branch", "visibility", "archived"}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing required key %q in %#v", k, m)
		}
	}
	allowed := map[string]struct{}{
		"id": {}, "name": {}, "path": {}, "path_with_namespace": {},
		"default_branch": {}, "visibility": {}, "web_url": {}, "archived": {}, "namespace": {},
	}
	for k := range m {
		if _, ok := allowed[k]; !ok {
			t.Fatalf("unexpected key %q (future/SDK field must not appear)", k)
		}
	}
}

func assertExactNamespaceKeys(t *testing.T, m map[string]any) {
	t.Helper()
	allowed := map[string]struct{}{"id": {}, "name": {}, "path": {}, "full_path": {}, "kind": {}}
	for k := range m {
		if _, ok := allowed[k]; !ok {
			t.Fatalf("unexpected namespace key %q", k)
		}
	}
	for k := range allowed {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing namespace key %q", k)
		}
	}
}

func assertNoPlantedLeaks(t *testing.T, s string) {
	t.Helper()
	for _, leak := range []string{
		plantedImportURL, plantedRunnerToken, plantedNestedSecret, plantedFutureField,
		"s3cretPASS", "glrt-PLANTED", "FUTURE_SDK_FIELD", "NESTED_SECRET",
		"import_url", "runners_token", "http_url_to_repo", "future_unknown_sdk_field",
		"parent_id", "avatar_url", "nested_secret", "unexpected_path_SECRET",
	} {
		if strings.Contains(s, leak) {
			t.Fatalf("planted leak %q in %s", leak, s)
		}
	}
}

func toolResultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if res == nil {
		return ""
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestGetProject_CallToolProjectionAndSchema(t *testing.T) {
	tracker := &getProjectRouteTracker{}
	d := getProjectDeps(t, plantedGetProjectHandler(t, tracker, safeWebURL), &config.Config{})
	res, err := callGetProject(t, d, "42")
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("IsError: %s", toolErrorText(t, res))
	}
	if res.StructuredContent == nil {
		t.Fatal("StructuredContent nil — registered outputSchema roundtrip required")
	}
	rawSC, _ := json.Marshal(res.StructuredContent)
	assertNoPlantedLeaks(t, string(rawSC))
	assertNoPlantedLeaks(t, toolResultText(t, res))

	var m map[string]any
	if err := json.Unmarshal(rawSC, &m); err != nil {
		t.Fatal(err)
	}
	assertExactGetProjectKeys(t, m)
	if m["id"].(float64) != 42 {
		t.Fatalf("id=%v", m["id"])
	}
	if m["path_with_namespace"] != "group/project" {
		t.Fatalf("path_with_namespace=%v", m["path_with_namespace"])
	}
	if m["web_url"] != safeWebURL {
		t.Fatalf("web_url=%v want %q", m["web_url"], safeWebURL)
	}
	assertExactNamespaceKeys(t, m["namespace"].(map[string]any))
	// No policy: single post-resolve content GetProject on shared endpoint.
	if tracker.hits("42") != 1 {
		t.Fatalf("no-policy content hits=%d want 1", tracker.hits("42"))
	}
}

func TestGetProject_CallToolOmitsUnsafeWebURLAndNilNamespace(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, ok := projectGetRouteToken(r); !ok {
			t.Fatalf("path %s esc %s", r.URL.Path, r.URL.EscapedPath())
		}
		_, _ = io.WriteString(w, `{
  "id": 5,
  "name": "p",
  "path": "p",
  "path_with_namespace": "g/p",
  "default_branch": "main",
  "visibility": "public",
  "web_url": "https://gitlab.example.com/g/p?unknown_key=1",
  "archived": false,
  "namespace": null
}`)
	})
	d := getProjectDeps(t, h, &config.Config{})
	res, err := callGetProject(t, d, "5")
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s", toolErrorText(t, res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	assertExactGetProjectKeys(t, m)
	assertGetProjectTypedValues(t, m, 5, "g/p", false)
	if _, ok := m["namespace"]; ok {
		t.Fatalf("nil namespace must be omitted: %s", raw)
	}
}

func TestGetProject_CallToolAuthzPathsTable(t *testing.T) {
	// Bounded CallTool matrix: default project, path/numeric alias, group-only,
	// intersection deny — strict routes; identity vs post-authz content hit counts.
	type want struct {
		allow         bool
		errSubstr     string
		minHitsID     map[string]int32 // shared GetProject endpoint counts by path token
		exactHitsID   map[string]int32
		expectID      float64
		expectPathNS  string
		expectSafeURL bool
	}
	cases := []struct {
		name   string
		cfg    *config.Config
		reqPID string
		want   want
	}{
		{
			name:   "default_project_resolution",
			cfg:    &config.Config{DefaultProjectID: "42"},
			reqPID: "",
			want: want{
				allow: true, expectID: 42, expectPathNS: "group/project", expectSafeURL: true,
				exactHitsID: map[string]int32{"42": 1}, // no policy ⇒ content-only GetProject
			},
		},
		{
			name:   "numeric_allow",
			cfg:    &config.Config{AllowedProjectIDs: []string{"42"}},
			reqPID: "42",
			want: want{
				allow: true, expectID: 42, expectPathNS: "group/project", expectSafeURL: true,
				minHitsID: map[string]int32{"42": 2}, // identity (+allowlist) then content
			},
		},
		{
			name:   "path_alias_equiv_numeric_allowlist",
			cfg:    &config.Config{AllowedProjectIDs: []string{"42"}},
			reqPID: "group/project",
			want: want{
				allow: true, expectID: 42, expectPathNS: "group/project", expectSafeURL: true,
				// path identity once; numeric 42 for allowlist resolve + post-authz content
				exactHitsID: map[string]int32{"group%2Fproject": 1},
				minHitsID:   map[string]int32{"42": 2},
			},
		},
		{
			name:   "numeric_deny_zero_content",
			cfg:    &config.Config{AllowedProjectIDs: []string{"42"}},
			reqPID: "99",
			want: want{
				allow: false, errSubstr: readmeta.CodeAuthzDenied,
				exactHitsID: map[string]int32{"99": 1}, // identity only
			},
		},
		{
			name:   "group_only_allow",
			cfg:    &config.Config{AllowedGroupIDs: []string{"10"}},
			reqPID: "1",
			want: want{
				allow: true, expectID: 1, expectPathNS: "acme/p", expectSafeURL: true,
				minHitsID: map[string]int32{"1": 2},
			},
		},
		{
			name:   "group_only_deny",
			cfg:    &config.Config{AllowedGroupIDs: []string{"10"}},
			reqPID: "99",
			want: want{
				allow: false, errSubstr: readmeta.CodeAuthzDenied,
				exactHitsID: map[string]int32{"99": 1},
			},
		},
		{
			name: "intersection_allow",
			cfg: &config.Config{
				AllowedProjectIDs: []string{"1"},
				AllowedGroupIDs:   []string{"10"},
			},
			reqPID: "1",
			want: want{
				allow: true, expectID: 1, expectPathNS: "acme/p", expectSafeURL: true,
				minHitsID: map[string]int32{"1": 2},
			},
		},
		{
			name: "intersection_deny_project_not_in_project_allowlist",
			cfg: &config.Config{
				AllowedProjectIDs: []string{"1"},
				AllowedGroupIDs:   []string{"10"},
			},
			reqPID: "2",
			want: want{
				allow: false, errSubstr: readmeta.CodeAuthzDenied,
				exactHitsID: map[string]int32{"2": 1},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := &getProjectRouteTracker{}
			d := getProjectDeps(t, getProjectAuthz018Fixture(t, tracker), tc.cfg)
			res, err := callGetProject(t, d, tc.reqPID)
			if err != nil {
				t.Fatalf("CallTool: %v", err)
			}
			if !tc.want.allow {
				if res == nil || !res.IsError {
					t.Fatal("want IsError")
				}
				msg := toolErrorText(t, res)
				if !strings.Contains(msg, tc.want.errSubstr) {
					t.Fatalf("err %q missing %q", msg, tc.want.errSubstr)
				}
				assertNoPlantedLeaks(t, msg)
				if res.StructuredContent != nil {
					t.Fatalf("deny must not return StructuredContent: %#v", res.StructuredContent)
				}
			} else {
				if res.IsError {
					t.Fatalf("allow IsError: %s", toolErrorText(t, res))
				}
				raw, _ := json.Marshal(res.StructuredContent)
				assertNoPlantedLeaks(t, string(raw))
				assertNoPlantedLeaks(t, toolResultText(t, res))
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				assertExactGetProjectKeys(t, m)
				assertGetProjectTypedValues(t, m, tc.want.expectID, tc.want.expectPathNS, tc.want.expectSafeURL)
			}
			for id, wantN := range tc.want.exactHitsID {
				if tracker.hits(id) != wantN {
					t.Fatalf("hits[%q]=%d want exact %d (byID=%v)", id, tracker.hits(id), wantN, tracker.snapshot())
				}
			}
			for id, wantMin := range tc.want.minHitsID {
				if tracker.hits(id) < wantMin {
					t.Fatalf("hits[%q]=%d want ≥%d (byID=%v)", id, tracker.hits(id), wantMin, tracker.snapshot())
				}
			}
		})
	}
}

// getProjectAuthz018Fixture serves only expected GetProject/GetGroup identity routes
// (018-style), planting secrets on project bodies so allow paths must project them away.
func getProjectAuthz018Fixture(t *testing.T, tracker *getProjectRouteTracker) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		path := r.URL.Path
		esc := r.URL.EscapedPath()
		switch {
		case isGroupIdentityPath(path) || groupGetRouteToken(r) != "":
			tok := groupGetRouteToken(r)
			if tok == "" {
				tok = strings.TrimPrefix(path, "/api/v4/groups/")
			}
			switch {
			case tok == "10" || tok == "acme":
				_, _ = io.WriteString(w, `{"id":10,"full_path":"acme","parent_id":0}`)
			case tok == "99" || strings.Contains(esc, "other"):
				_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":0}`)
			default:
				http.NotFound(w, r)
			}
		default:
			tok, ok := projectGetRouteToken(r)
			if !ok {
				http.NotFound(w, r)
				return
			}
			tracker.note(tok)
			switch {
			case tok == "42" || tok == "group%2Fproject":
				_, _ = io.WriteString(w, richPlantedProjectJSON(42, safeWebURL))
			case tok == "1":
				_, _ = io.WriteString(w, `{
  "id": 1,
  "name": "p",
  "path": "p",
  "path_with_namespace": "acme/p",
  "default_branch": "main",
  "visibility": "private",
  "web_url": "`+safeWebURL+`",
  "archived": false,
  "runners_token": "`+plantedRunnerToken+`",
  "import_url": "`+plantedImportURL+`",
  "future_unknown_sdk_field": "`+plantedFutureField+`",
  "namespace": {"id":10,"name":"acme","path":"acme","full_path":"acme","kind":"group","parent_id":0,"nested_secret":"`+plantedNestedSecret+`"}
}`)
			case tok == "2":
				_, _ = io.WriteString(w, `{
  "id": 2,
  "name": "other",
  "path": "other",
  "path_with_namespace": "acme/other",
  "default_branch": "main",
  "visibility": "private",
  "web_url": "`+safeWebURL+`",
  "archived": false,
  "runners_token": "`+plantedRunnerToken+`",
  "namespace": {"id":10,"name":"acme","path":"acme","full_path":"acme","kind":"group","parent_id":0}
}`)
			case tok == "99":
				_, _ = io.WriteString(w, `{
  "id": 99,
  "name": "x",
  "path": "x",
  "path_with_namespace": "other/x",
  "default_branch": "main",
  "visibility": "private",
  "web_url": "`+safeWebURL+`",
  "archived": false,
  "runners_token": "`+plantedRunnerToken+`",
  "namespace": {"id":99,"name":"other","path":"other","full_path":"other","kind":"group","parent_id":0}
}`)
			default:
				http.NotFound(w, r)
			}
		}
	})
}

func groupGetRouteToken(r *http.Request) string {
	const prefix = "/api/v4/groups/"
	esc := r.URL.EscapedPath()
	if !strings.HasPrefix(esc, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(esc, prefix)
	if rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	return rest
}

func (h *getProjectRouteTracker) snapshot() map[string]int32 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]int32, len(h.byID))
	for k, v := range h.byID {
		out[k] = v
	}
	return out
}

func assertGetProjectTypedValues(t *testing.T, m map[string]any, id float64, pathNS string, expectURL bool) {
	t.Helper()
	gotID, ok := m["id"].(float64)
	if !ok || gotID != id {
		t.Fatalf("id type/value=%T %#v want %v", m["id"], m["id"], id)
	}
	for _, k := range []string{"name", "path", "path_with_namespace", "default_branch", "visibility"} {
		if _, ok := m[k].(string); !ok {
			t.Fatalf("%s want string, got %T", k, m[k])
		}
	}
	if _, ok := m["archived"].(bool); !ok {
		t.Fatalf("archived want bool, got %T", m["archived"])
	}
	if m["path_with_namespace"] != pathNS {
		t.Fatalf("path_with_namespace=%v want %q", m["path_with_namespace"], pathNS)
	}
	if expectURL {
		u, ok := m["web_url"].(string)
		if !ok || u != safeWebURL {
			t.Fatalf("web_url=%T %#v want %q", m["web_url"], m["web_url"], safeWebURL)
		}
	} else if _, ok := m["web_url"]; ok {
		t.Fatalf("web_url must be omitted, got %#v", m["web_url"])
	}
	if ns, ok := m["namespace"].(map[string]any); ok {
		assertExactNamespaceKeys(t, ns)
		if _, ok := ns["id"].(float64); !ok {
			t.Fatalf("namespace.id want number, got %T", ns["id"])
		}
		for _, k := range []string{"name", "path", "full_path", "kind"} {
			if _, ok := ns[k].(string); !ok {
				t.Fatalf("namespace.%s want string, got %T", k, ns[k])
			}
		}
	}
}

func TestGetProject_CallToolContentIDMismatchSafeError(t *testing.T) {
	// After resolveProjectAuthz binds canonical numeric pid 42, the post-authz
	// content GetProject must not accept a body with p.ID=99 (malformed response).
	var hits42 int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		tok, ok := projectGetRouteToken(r)
		if r.Method != http.MethodGet || !ok {
			http.NotFound(w, r)
			return
		}
		if tok != "42" {
			http.NotFound(w, r)
			return
		}
		n := atomic.AddInt32(&hits42, 1)
		if n <= 2 {
			// Identity + allowlist resolve: canonical 42.
			_, _ = io.WriteString(w, richPlantedProjectJSON(42, safeWebURL))
			return
		}
		// Final content GET: mismatched id 99 with planted secrets — must not project.
		_, _ = io.WriteString(w, richPlantedProjectJSON(99, safeWebURL))
	})
	d := getProjectDeps(t, h, &config.Config{AllowedProjectIDs: []string{"42"}})
	res, err := callGetProject(t, d, "42")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("want IsError on content id mismatch")
	}
	msg := toolErrorText(t, res)
	if !strings.Contains(msg, readmeta.CodeHTTPError) {
		t.Fatalf("want http_error, got %q", msg)
	}
	assertNoPlantedLeaks(t, msg)
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		t.Fatalf("mismatch must not return StructuredContent: %s", raw)
	}
	assertNoPlantedLeaks(t, toolResultText(t, res))
	if atomic.LoadInt32(&hits42) < 3 {
		t.Fatalf("hits42=%d want ≥3 (identity+allowlist then content)", hits42)
	}
}

func TestGetProject_CallToolAuthzDenyZeroContent(t *testing.T) {
	tracker := &getProjectRouteTracker{}
	d := getProjectDeps(t, plantedGetProjectHandler(t, tracker, safeWebURL), &config.Config{
		AllowedProjectIDs: []string{"42"},
	})

	res, err := callGetProject(t, d, "99")
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected authz deny IsError")
	}
	msg := toolErrorText(t, res)
	if !strings.Contains(msg, readmeta.CodeAuthzDenied) {
		t.Fatalf("want authz_denied, got %q", msg)
	}
	assertNoPlantedLeaks(t, msg)
	if res.StructuredContent != nil {
		raw, _ := json.Marshal(res.StructuredContent)
		t.Fatalf("deny must not return project StructuredContent: %s", raw)
	}
	if tracker.hits("99") != 1 {
		t.Fatalf("denied id hits=%d want 1 (identity only; no content fetch)", tracker.hits("99"))
	}
}

func TestGetProject_CallToolAuthzAllowIdentityThenContent(t *testing.T) {
	tracker := &getProjectRouteTracker{}
	d := getProjectDeps(t, plantedGetProjectHandler(t, tracker, safeWebURL), &config.Config{
		AllowedProjectIDs: []string{"42"},
	})

	res, err := callGetProject(t, d, "42")
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s", toolErrorText(t, res))
	}
	raw, _ := json.Marshal(res.StructuredContent)
	assertNoPlantedLeaks(t, string(raw))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	assertGetProjectTypedValues(t, m, 42, "group/project", true)
	if tracker.hits("42") < 2 {
		t.Fatalf("allowed id hits=%d want ≥2 (identity distinguished from post-authz content)", tracker.hits("42"))
	}
}

func TestGetProject_CallToolBackendErrorSafeMessage(t *testing.T) {
	const secret = "SECRET_BACKEND_TOKEN_do_not_leak"
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !isProjectIdentityPath(r.URL.Path) {
			t.Fatalf("path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"`+secret+`"}`)
	})
	d := getProjectDeps(t, h, &config.Config{})
	res, err := callGetProject(t, d, "42")
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("want IsError")
	}
	msg := toolErrorText(t, res)
	if !strings.Contains(msg, readmeta.CodeHTTPError) {
		t.Fatalf("want http_error, got %q", msg)
	}
	if strings.Contains(msg, secret) {
		t.Fatalf("raw backend secret leaked: %q", msg)
	}
}

func TestGetProject_CallToolMalformedProjectSafeError(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":0,"name":"x","runners_token":"`+plantedRunnerToken+`"}`)
	})
	d := getProjectDeps(t, h, &config.Config{})
	res, err := callGetProject(t, d, "1")
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("want IsError for unusable project id")
	}
	msg := toolErrorText(t, res)
	if !strings.Contains(msg, readmeta.CodeHTTPError) {
		t.Fatalf("want http_error, got %q", msg)
	}
	assertNoPlantedLeaks(t, msg)
}

func TestGetProject_RegisteredOutputSchemaRoundtrip(t *testing.T) {
	// 020 pattern: ListTools OutputSchema → temporary mcp.AddTool(OutputSchema) →
	// CallTool applySchema validates StructuredContent (no direct jsonschema-go import).
	tracker := &getProjectRouteTracker{}
	d := getProjectDeps(t, plantedGetProjectHandler(t, tracker, safeWebURL), &config.Config{})
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterProjects(srv, d)
	cs := testutil.MCPConnect(t, srv)

	listed, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var outSchema any
	for _, tool := range listed.Tools {
		if tool != nil && tool.Name == "get_project" {
			outSchema = tool.OutputSchema
			break
		}
	}
	if outSchema == nil {
		t.Fatal("get_project OutputSchema not registered")
	}
	schemaJSON, err := json.Marshal(outSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schemaMap map[string]any
	if err := json.Unmarshal(schemaJSON, &schemaMap); err != nil {
		t.Fatal(err)
	}
	if req, ok := schemaMap["required"].([]any); ok {
		for _, r := range req {
			if r == "web_url" || r == "namespace" {
				t.Fatalf("optional fields must not be required: %s", schemaJSON)
			}
		}
	}
	if strings.Contains(string(schemaJSON), "runners_token") || strings.Contains(string(schemaJSON), "import_url") {
		t.Fatalf("schema must not include SDK secret fields: %s", schemaJSON)
	}

	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_project",
		Arguments: map[string]any{"project_id": "42"},
	})
	if err != nil {
		t.Fatalf("CallTool schema/protocol: %v", err)
	}
	if res.IsError {
		t.Fatalf("%s", toolErrorText(t, res))
	}
	if res.StructuredContent == nil {
		t.Fatal("StructuredContent required")
	}

	scRaw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var scMap map[string]any
	if err := json.Unmarshal(scRaw, &scMap); err != nil {
		t.Fatal(err)
	}
	assertExactGetProjectKeys(t, scMap)
	assertGetProjectTypedValues(t, scMap, 42, "group/project", true)
	assertNoPlantedLeaks(t, string(scRaw))

	// Decoded text form must match the same projection (not helper-only).
	txt := toolResultText(t, res)
	assertNoPlantedLeaks(t, txt)
	var textMap map[string]any
	if err := json.Unmarshal([]byte(txt), &textMap); err != nil {
		t.Fatalf("tool text must be JSON projection: %v (%q)", err, txt)
	}
	assertExactGetProjectKeys(t, textMap)
	assertGetProjectTypedValues(t, textMap, 42, "group/project", true)

	// Re-validate StructuredContent through registered OutputSchema (020 applySchema path).
	validateAgainstRegisteredSchema(t, outSchema, scMap)

	// Omissions: unsafe web_url + nil namespace must validate and stay absent.
	omitH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
  "id": 5, "name": "p", "path": "p", "path_with_namespace": "g/p",
  "default_branch": "main", "visibility": "public",
  "web_url": "https://gitlab.example.com/g/p?",
  "archived": false, "namespace": null,
  "runners_token": "`+plantedRunnerToken+`"
}`)
	})
	omitDeps := getProjectDeps(t, omitH, &config.Config{})
	omitRes, err := callGetProject(t, omitDeps, "5")
	if err != nil {
		t.Fatal(err)
	}
	if omitRes.IsError {
		t.Fatalf("%s", toolErrorText(t, omitRes))
	}
	omitRaw, _ := json.Marshal(omitRes.StructuredContent)
	var omitMap map[string]any
	_ = json.Unmarshal(omitRaw, &omitMap)
	assertExactGetProjectKeys(t, omitMap)
	assertGetProjectTypedValues(t, omitMap, 5, "g/p", false)
	if _, ok := omitMap["namespace"]; ok {
		t.Fatalf("namespace must be omitted: %s", omitRaw)
	}
	assertNoPlantedLeaks(t, string(omitRaw))
	assertNoPlantedLeaks(t, toolResultText(t, omitRes))
	validateAgainstRegisteredSchema(t, outSchema, omitMap)
}

// validateAgainstRegisteredSchema uses the 020 temporary-tool applySchema path
// (vendored MCP SDK) — no new module dependency.
func validateAgainstRegisteredSchema(t *testing.T, outSchema any, payload map[string]any) {
	t.Helper()
	toolName := "get_project_schema_validate"
	valSrv := mcp.NewServer(&mcp.Implementation{Name: "schema-val", Version: "t"}, nil)
	captured := payload
	mcp.AddTool(valSrv, &mcp.Tool{
		Name:         toolName,
		Description:  "temporary validator for get_project outputSchema",
		OutputSchema: outSchema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
		return nil, captured, nil
	})
	valCS := testutil.MCPConnect(t, valSrv)
	res, err := valCS.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      toolName,
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("registered OutputSchema applySchema failed: %v payload=%v", err, payload)
	}
	if res.IsError {
		t.Fatalf("schema validate IsError: %s", toolErrorText(t, res))
	}
	if res.StructuredContent == nil {
		t.Fatal("schema validate missing StructuredContent")
	}
}
