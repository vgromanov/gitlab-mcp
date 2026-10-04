package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestRepairF1_nativeStringSchema(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	serve := func(body string) http.Handler {
		return serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				_, _ = io.WriteString(w, body)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
	}
	call := func(t *testing.T, body string) (map[string]any, error) {
		t.Helper()
		d := diffDeps(t, serve(body))
		b := igl.DefaultBudget()
		b.MaxItems = 500
		b.MaxRequests = 64
		return callDiffWindow(t, d, igl.WithBudget(context.Background(), b), map[string]any{
			"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20,
		})
	}
	t.Run("one", func(t *testing.T) {
		out, err := call(t, versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("a.txt", "p")))
		if err != nil {
			t.Fatal(err)
		}
		if sectionMap(out)["content_complete"] != readmeta.ContentCompleteTrue || sectionMap(out)["manifest_coverage"] != readmeta.CoverageFull {
			t.Fatalf("string real_size one did not prove: %#v", sectionMap(out))
		}
	})
	t.Run("empty", func(t *testing.T) {
		out, err := call(t, versionObject(1, 5001, head, base, start, "collected", "0", "[]"))
		if err != nil {
			t.Fatal(err)
		}
		sec := sectionMap(out)
		files, _ := asMap(t, sec["counts"])["files"].(float64)
		if sec["content_complete"] != readmeta.ContentCompleteTrue || files != 0 {
			t.Fatalf("string real_size empty did not prove: %#v", sec)
		}
	})
	t.Run("missing", func(t *testing.T) {
		body := fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","diffs":[]}`, head, base, start)
		out, err := call(t, body)
		if err != nil {
			t.Fatal(err)
		}
		if sectionMap(out)["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatal("missing real_size proved")
		}
	})
	t.Run("null", func(t *testing.T) {
		body := fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","real_size":null,"diffs":[]}`, head, base, start)
		out, err := call(t, body)
		if err != nil {
			t.Fatal(err)
		}
		if sectionMap(out)["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatal("null real_size proved")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		out, err := call(t, versionObject(1, 5001, head, base, start, "collected", "nope", "[]"))
		if err != nil {
			t.Fatal(err)
		}
		if sectionMap(out)["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatal("malformed real_size proved")
		}
	})
	t.Run("capped", func(t *testing.T) {
		out, err := call(t, versionObject(1, 5001, head, base, start, "collected", "100+", oneDiff("a.txt", "p")))
		if err != nil {
			t.Fatal(err)
		}
		if sectionMap(out)["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatal("capped real_size proved")
		}
	})
}

func TestRepairF1_numericCompatibilityControl(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	body := fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","real_size":1,"diffs":%s}`, head, base, start, oneDiff("a.txt", "p"))
	d := diffDeps(t, serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	}))
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
	if err != nil {
		t.Fatal(err)
	}
	if sectionMap(out)["content_complete"] != readmeta.ContentCompleteTrue {
		t.Fatalf("numeric control did not prove: %#v", sectionMap(out))
	}
}

func TestRepairF2_allowAllResolvesMR(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("a.txt", "p")))
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := newReviewDeps(t, h)
	d.Config.AllowedProjectIDs = nil
	d.Config.AllowedGroupIDs = nil
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
	if err != nil {
		t.Fatalf("allow-all valid version: %v paths=%v", err, log.paths)
	}
	if sectionMap(out)["content_complete"] != readmeta.ContentCompleteTrue || log.count("/merge_requests/") == 0 {
		t.Fatalf("allow-all section=%#v paths=%v", sectionMap(out), log.paths)
	}
}

func TestRepairF2_legacyHelperSkipsMR(t *testing.T) {
	log := &pathLog{}
	d := newReviewDeps(t, serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	d.Config.AllowedProjectIDs = nil
	d.Config.AllowedGroupIDs = nil
	owner, mr, err := loadAuthorizedMR(context.Background(), d, "42", 1)
	if err != nil || mr != nil || owner.ID != 42 {
		t.Fatalf("legacy owner=%+v mr=%v err=%v", owner, mr != nil, err)
	}
	if log.count("/merge_requests/") != 0 || log.count("/projects/") != 0 {
		t.Fatalf("legacy helper called the provider: %v", log.paths)
	}
}

func TestRepairF3_productionRedirectHasNoDestination(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	versionBody := fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","real_size":1,"diffs":%s}`, head, base, start, oneDiff("a.txt", "p"))
	mrBody := `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`
	for _, tc := range []string{"path", "query", "project", "origin"} {
		t.Run(tc, func(t *testing.T) {
			var dest atomic.Int32
			var srv *httptest.Server
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dest.Add(1)
				_, _ = io.WriteString(w, versionBody)
			}))
			t.Cleanup(other.Close)
			srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/user"):
					_, _ = io.WriteString(w, `{"id":7}`)
				case strings.Contains(r.URL.Path, "/projects/99/"):
					dest.Add(1)
					_, _ = io.WriteString(w, versionBody)
				case strings.Contains(r.URL.Path, "/versions/1/evil"):
					dest.Add(1)
					_, _ = io.WriteString(w, versionBody)
				case strings.Contains(r.URL.Path, "/versions/1"):
					if r.URL.RawQuery == "injected=1" {
						dest.Add(1)
						_, _ = io.WriteString(w, versionBody)
						return
					}
					loc := srv.URL + "/api/v4/projects/42/merge_requests/1/versions/1/evil"
					switch tc {
					case "query":
						loc = srv.URL + r.URL.Path + "?injected=1"
					case "project":
						loc = srv.URL + "/api/v4/projects/99/merge_requests/1/versions/1"
					case "origin":
						loc = other.URL + r.URL.RequestURI()
					}
					http.Redirect(w, r, loc, http.StatusFound)
				case strings.Contains(r.URL.Path, "/merge_requests/"):
					_, _ = io.WriteString(w, mrBody)
				case strings.Contains(r.URL.Path, "/projects/"):
					fmt.Fprintf(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group"}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(srv.Close)
			d := reviewDepsAt(t, srv)
			_, _ = callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
			if dest.Load() != 0 {
				t.Fatalf("destination content calls=%d", dest.Load())
			}
		})
	}
}

func reviewDepsAt(t *testing.T, srv *httptest.Server) Deps {
	t.Helper()
	d := newReviewDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "wrong server", http.StatusBadGateway)
	}))
	d.Config.APIURL = srv.URL + "/api/v4"
	d.Config.AllowedProjectIDs = []string{"42"}
	cli, err := igl.NewClient(d.Config, igl.WithRetryWaitMinMax(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	d.Client = cli
	return d
}

func TestRepairF4_identityBeforeContent(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	body := fmt.Sprintf(`{"id":1,"merge_request_id":9000,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","real_size":1,"diffs":%s}`, head, base, start, oneDiff("a.txt", "p"))
	cases := []struct {
		name  string
		mr    string
		allow []string
	}{
		{"iid", `{"id":9000,"iid":9,"project_id":42,"source_project_id":42,"target_project_id":42}`, []string{"42"}},
		{"owner", `{"id":9000,"iid":1,"project_id":99,"source_project_id":99,"target_project_id":99}`, []string{"42", "99"}},
		{"id", `{"id":0,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`, []string{"42"}},
		{"source", `{"id":5001,"iid":1,"project_id":42,"source_project_id":0,"target_project_id":42}`, []string{"42"}},
		{"target", `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":99}`, []string{"42", "99"}},
		{"absent-iid", `{"id":5001,"project_id":42,"source_project_id":42,"target_project_id":42}`, []string{"42"}},
		{"absent-owner", `{"id":5001,"iid":1,"source_project_id":42,"target_project_id":42}`, []string{"42"}},
		{"absent-id", `{"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`, []string{"42"}},
		{"absent-source", `{"id":5001,"iid":1,"project_id":42,"target_project_id":42}`, []string{"42"}},
		{"nil-meta", `null`, []string{"42"}},
		{"malformed-meta", `not-json`, []string{"42"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &pathLog{}
			h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/versions"):
					_, _ = io.WriteString(w, body)
				case strings.Contains(r.URL.Path, "/merge_requests/"):
					_, _ = io.WriteString(w, tc.mr)
				default:
					http.NotFound(w, r)
				}
			})
			d := diffDeps(t, h)
			d.Config.AllowedProjectIDs = tc.allow
			_, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
			if err == nil {
				t.Fatal("mismatch succeeded")
			}
			if tc.name != "nil-meta" && tc.name != "malformed-meta" && !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
				t.Fatalf("err=%v", err)
			}
			if log.count("/versions") != 0 {
				t.Fatalf("content calls=%v", log.paths)
			}
		})
	}
}

func repairReviewHandler(body string, fail func(r *http.Request) bool) http.HandlerFunc {
	head, base, start := shaN(1), shaN(101), shaN(201)
	return func(w http.ResponseWriter, r *http.Request) {
		if fail != nil && fail(r) {
			http.Error(w, "late", http.StatusInternalServerError)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/user"):
			_, _ = io.WriteString(w, `{"id":7}`)
		case strings.Contains(r.URL.Path, "/approval_state"):
			_, _ = io.WriteString(w, `{"rules":[]}`)
		case strings.Contains(r.URL.Path, "/repository/branches/"):
			name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			sha := head
			if name == "main" {
				sha = shaN(50)
			}
			fmt.Fprintf(w, `{"name":%q,"commit":{"id":%q}}`, name, sha)
		case strings.Contains(r.URL.Path, "/versions/"):
			_, _ = io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/versions"):
			w.Header().Set("X-Next-Page", "")
			fmt.Fprintf(w, `[{"id":9,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, head, base, start)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			h := head
			fmt.Fprintf(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42,"source_branch":"feature-1","target_branch":"main","sha":%q,"diff_refs":{"head_sha":%q,"base_sha":%q,"start_sha":%q}}`, h, h, base, start)
		case strings.Contains(r.URL.Path, "/projects/"):
			fmt.Fprintf(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	}
}

func sectionStillProved(sec readmeta.Section) bool {
	return sec.ContentComplete == readmeta.ContentCompleteTrue || sec.Consistency == readmeta.ConsistencyConsistent || sec.ManifestCoverage == readmeta.CoverageFull || sec.NextCursor != nil
}

func manifestClaimLeft(item reviewContextItemOut) bool {
	if sectionStillProved(item.Sections["diff_manifest"]) || item.diffManifestDigest != "" || item.ReviewClean || item.ContextRef != nil {
		return true
	}
	if item.DiffManifest != nil && (item.DiffManifest.Digest != nil || sectionStillProved(item.DiffManifest.Section)) {
		return true
	}
	return false
}

func TestRepairF5_lateFailuresDropManifest(t *testing.T) {
	head, base, start := shaN(1), shaN(101), shaN(201)
	body := versionObject(9, 5001, head, base, start, "collected", "1", oneDiff("m.txt", "patch"))
	assertDrop := func(t *testing.T, item reviewContextItemOut) {
		t.Helper()
		if item.ReviewClean || item.ContextRef != nil || manifestClaimLeft(item) {
			t.Fatalf("claim left clean=%v ref=%v digest=%q sec=%+v", item.ReviewClean, item.ContextRef != nil, item.diffManifestDigest, item.Sections["diff_manifest"])
		}
		if item.Approvals == nil {
			t.Fatal("approval sibling dropped")
		}
	}
	t.Run("bracket phase", func(t *testing.T) {
		d := newReviewDeps(t, repairReviewHandler(body, nil))
		ctx := withReviewPhaseHook(igl.WithBudget(context.Background(), reviewBudget(128)), func(phase string) error {
			if phase == "bracket" {
				return context.Canceled
			}
			return nil
		})
		out, err := callReviewDirect(t, d, ctx, []reviewContextItemIn{metaItem("42", 1, "metadata", "approvals", "diff_manifest")})
		if err != nil {
			t.Fatal(err)
		}
		assertDrop(t, out.Items[0])
	})
	t.Run("second detail", func(t *testing.T) {
		d := newReviewDeps(t, repairReviewHandler(body, nil))
		var n int
		ctx := withReviewPhaseHook(igl.WithBudget(context.Background(), reviewBudget(128)), func(phase string) error {
			if phase != "detail" {
				return nil
			}
			n++
			if n >= 2 {
				return context.Canceled
			}
			return nil
		})
		out, err := callReviewDirect(t, d, ctx, []reviewContextItemIn{metaItem("42", 1, "metadata", "approvals", "diff_manifest")})
		if err != nil {
			t.Fatal(err)
		}
		assertDrop(t, out.Items[0])
	})
	t.Run("second branch", func(t *testing.T) {
		var branches int
		d := newReviewDeps(t, repairReviewHandler(body, func(r *http.Request) bool {
			if !strings.Contains(r.URL.Path, "/repository/branches/") {
				return false
			}
			branches++
			return branches >= 3
		}))
		out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), reviewBudget(128)), []reviewContextItemIn{metaItem("42", 1, "metadata", "approvals", "diff_manifest")})
		if err != nil {
			t.Fatal(err)
		}
		assertDrop(t, out.Items[0])
	})
	t.Run("second version", func(t *testing.T) {
		var lists int
		d := newReviewDeps(t, repairReviewHandler(body, func(r *http.Request) bool {
			if !strings.Contains(r.URL.Path, "/versions") || strings.Contains(r.URL.Path, "/versions/") {
				return false
			}
			lists++
			return lists >= 2
		}))
		out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), reviewBudget(128)), []reviewContextItemIn{metaItem("42", 1, "metadata", "approvals", "diff_manifest")})
		if err != nil {
			t.Fatal(err)
		}
		assertDrop(t, out.Items[0])
	})
	preMint := func(t *testing.T, name, want string, hook func(*igl.Budget)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			d := newReviewDeps(t, repairReviewHandler(body, nil))
			b := reviewBudget(128)
			ctx := withReviewMintHook(igl.WithBudget(context.Background(), b), func() { hook(b) })
			out, err := callReviewDirect(t, d, ctx, []reviewContextItemIn{metaItem("42", 1, "metadata", "approvals", "diff_manifest")})
			if err != nil {
				t.Fatal(err)
			}
			item := out.Items[0]
			if item.Cause != want || manifestClaimLeft(item) || item.Metadata == nil {
				t.Fatalf("cause=%s want=%s digest=%q nested=%v sec=%+v", item.Cause, want, item.diffManifestDigest, item.DiffManifest != nil && item.DiffManifest.Digest != nil, item.Sections["diff_manifest"])
			}
		})
	}
	preMint(t, "pre-mint requests", readmeta.CodeBudgetRequests, func(b *igl.Budget) { b.CapLimits(0, 0, 1) })
	preMint(t, "pre-mint bytes", readmeta.CodeBudgetBytes, func(b *igl.Budget) { b.CapLimits(0, 1, 0) })
	preMint(t, "pre-mint items", readmeta.CodeBudgetItems, func(b *igl.Budget) { b.CapLimits(1, 0, 0) })
	t.Run("pre-mint elapsed", func(t *testing.T) {
		d := newReviewDeps(t, repairReviewHandler(body, nil))
		b := reviewBudget(128)
		b.MaxElapsed = 2 * time.Second
		ctx := withReviewMintHook(igl.WithBudget(context.Background(), b), func() {
			time.Sleep(3 * time.Second)
		})
		out, err := callReviewDirect(t, d, ctx, []reviewContextItemIn{metaItem("42", 1, "metadata", "approvals", "diff_manifest")})
		if err != nil {
			t.Fatal(err)
		}
		item := out.Items[0]
		if item.Cause != readmeta.CodeBudgetElapsed || manifestClaimLeft(item) || item.Metadata == nil {
			t.Fatalf("cause=%s digest=%q nested=%v sec=%+v", item.Cause, item.diffManifestDigest, item.DiffManifest != nil && item.DiffManifest.Digest != nil, item.Sections["diff_manifest"])
		}
	})
	t.Run("pre-mint cancel", func(t *testing.T) {
		d := newReviewDeps(t, repairReviewHandler(body, nil))
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx := withReviewMintHook(igl.WithBudget(parent, reviewBudget(128)), cancel)
		out, err := callReviewDirect(t, d, ctx, []reviewContextItemIn{metaItem("42", 1, "metadata", "approvals", "diff_manifest")})
		if err != nil {
			t.Fatal(err)
		}
		item := out.Items[0]
		if item.Cause != readmeta.CodeCancelled || manifestClaimLeft(item) {
			t.Fatalf("cancel cause=%s digest=%q sec=%+v nested=%+v", item.Cause, item.diffManifestDigest, item.Sections["diff_manifest"], item.DiffManifest)
		}
		if parent.Err() == nil {
			t.Fatal("parent cancel did not fire")
		}
	})
	t.Run("sibling item retained", func(t *testing.T) {
		d := newReviewDeps(t, repairReviewHandler(body, nil))
		var brackets int
		ctx := withReviewPhaseHook(igl.WithBudget(context.Background(), reviewBudget(128)), func(phase string) error {
			if phase != "bracket" {
				return nil
			}
			brackets++
			if brackets >= 2 {
				return context.Canceled
			}
			return nil
		})
		out, err := callReviewDirect(t, d, ctx, []reviewContextItemIn{
			metaItem("42", 1, "metadata"),
			metaItem("42", 1, "metadata", "diff_manifest"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Items) != 2 || !out.Items[0].ReviewClean || out.Items[0].Metadata == nil {
			t.Fatalf("first sibling=%+v", out.Items[0])
		}
		if manifestClaimLeft(out.Items[1]) || out.Items[1].ContextRef != nil {
			t.Fatalf("second claim left sec=%+v", out.Items[1].Sections["diff_manifest"])
		}
	})
}

func TestRepairF6_itemBudgetBeforeRetention(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	listHandler := func(log *pathLog) http.Handler {
		return serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/"):
				_, _ = io.WriteString(w, versionObject(7, 5001, head, base, start, "collected", "1", oneDiff("a.txt", "p")))
			case strings.Contains(r.URL.Path, "/versions"):
				w.Header().Set("X-Next-Page", "")
				fmt.Fprintf(w, `[{"id":7,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q},{"id":8,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q},{"id":9,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, head, base, start, shaN(8), base, start, shaN(9), base, start)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
	}
	t.Run("unique list sufficient budget", func(t *testing.T) {
		log := &pathLog{}
		out, err := callDiffWindow(t, diffDeps(t, listHandler(log)), nil, map[string]any{
			"project_id": "42", "merge_request_iid": 1, "base_sha": base, "start_sha": start, "head_sha": head, "per_page": 20, "max_items": 30,
		})
		if err != nil {
			t.Fatal(err)
		}
		if log.count("/versions/") == 0 || sectionMap(out)["content_complete"] != readmeta.ContentCompleteTrue {
			t.Fatalf("unique list did not prove body=%d section=%#v", log.count("/versions/"), sectionMap(out))
		}
	})
	t.Run("version rows", func(t *testing.T) {
		log := &pathLog{}
		_, err := callDiffWindow(t, diffDeps(t, listHandler(log)), nil, map[string]any{
			"project_id": "42", "merge_request_iid": 1, "base_sha": base, "start_sha": start, "head_sha": head, "per_page": 20, "max_items": 1,
		})
		if err == nil || !strings.Contains(err.Error(), igl.ErrBudgetItems.Error()) {
			t.Fatalf("err=%v", err)
		}
		if log.count("/versions/") != 0 {
			t.Fatalf("version body after owned list stop: %v", log.paths)
		}
	})
	t.Run("compare entries", func(t *testing.T) {
		from, to := shaN(4), shaN(5)
		log := &pathLog{}
		h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/repository/commits/"):
				sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				fmt.Fprintf(w, `{"id":%q}`, sha)
			case strings.Contains(r.URL.Path, "/repository/compare"):
				fmt.Fprintf(w, `{"commit":{"id":%q},"diffs":[{"old_path":"a","new_path":"a","diff":"1"},{"old_path":"b","new_path":"b","diff":"2"},{"old_path":"c","new_path":"c","diff":"3"}]}`, to)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{
			"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true, "per_page": 20, "max_items": 1,
		})
		if err == nil || !strings.Contains(err.Error(), igl.ErrBudgetItems.Error()) {
			entries := 0
			if out != nil {
				if got, ok := out["entries"].([]any); ok {
					entries = len(got)
				}
			}
			t.Fatalf("err=%v entries=%d paths=%v", err, entries, log.paths)
		}
	})
}

func TestRepairF7_groupPolicyTypedErrors(t *testing.T) {
	project := func(id, ns int64) string {
		return fmt.Sprintf(`{"id":%d,"path_with_namespace":"g/p","namespace":{"id":%d,"kind":"group"}}`, id, ns)
	}
	handler := func(group99 func(http.ResponseWriter, *http.Request)) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/user"):
				_, _ = io.WriteString(w, `{"id":7}`)
			case strings.Contains(r.URL.Path, "/groups/10"):
				_, _ = io.WriteString(w, `{"id":10,"full_path":"acme"}`)
			case strings.Contains(r.URL.Path, "/groups/99"):
				group99(w, r)
			case strings.Contains(r.URL.Path, "/projects/77"):
				_, _ = io.WriteString(w, project(77, 99))
			case strings.Contains(r.URL.Path, "/projects/42"):
				_, _ = io.WriteString(w, project(42, 99))
			default:
				http.NotFound(w, r)
			}
		})
	}
	deps := func(t *testing.T, h http.Handler) Deps {
		t.Helper()
		d := newReviewDeps(t, h)
		d.Config.AllowedProjectIDs = nil
		d.Config.AllowedGroupIDs = []string{"10"}
		return d
	}
	expectTyped := func(t *testing.T, err error, want error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("got %v want %v", err, want)
		}
	}
	t.Run("allowlist request", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":10}`)
		}))
		b := igl.DefaultBudget()
		b.MaxRequests = 1
		_, err := AuthorizeCanonicalProject(igl.WithBudget(context.Background(), b), d, "42")
		expectTyped(t, err, igl.ErrBudgetRequests)
	})
	t.Run("owner request", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":10}`)
		}))
		b := igl.DefaultBudget()
		b.MaxRequests = 2
		_, err := AuthorizeCanonicalProject(igl.WithBudget(context.Background(), b), d, "42")
		expectTyped(t, err, igl.ErrBudgetRequests)
	})
	t.Run("fork request", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":10}`)
		}))
		b := igl.DefaultBudget()
		b.MaxRequests = 2
		mr := &gitlab.MergeRequest{BasicMergeRequest: gitlab.BasicMergeRequest{ID: 5001, IID: 1, ProjectID: 42, SourceProjectID: 77}}
		err := requireProvenMRForkProjects(igl.WithBudget(context.Background(), b), d, CanonicalProject{ID: 42, NamespaceID: 10, NamespaceKind: "group"}, mr)
		expectTyped(t, err, igl.ErrBudgetRequests)
	})
	t.Run("owner bytes", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":99,"full_path":"`+strings.Repeat("g", 4000)+`","parent_id":10}`)
		}))
		b := igl.DefaultBudget()
		b.MaxBytes = 900
		b.MaxRequests = 16
		_, err := AuthorizeCanonicalProject(igl.WithBudget(context.Background(), b), d, "42")
		expectTyped(t, err, igl.ErrBudgetBytes)
	})
	t.Run("fork bytes", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":99,"full_path":"`+strings.Repeat("g", 4000)+`","parent_id":10}`)
		}))
		b := igl.DefaultBudget()
		b.MaxBytes = 900
		b.MaxRequests = 16
		mr := &gitlab.MergeRequest{BasicMergeRequest: gitlab.BasicMergeRequest{ID: 5001, IID: 1, ProjectID: 42, SourceProjectID: 77}}
		err := requireProvenMRForkProjects(igl.WithBudget(context.Background(), b), d, CanonicalProject{ID: 42}, mr)
		expectTyped(t, err, igl.ErrBudgetBytes)
	})
	t.Run("owner items", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":10}`)
		}))
		b := igl.DefaultBudget()
		b.MaxItems = 1
		if err := b.AddItem(); err != nil {
			t.Fatal(err)
		}
		_, err := AuthorizeCanonicalProject(igl.WithBudget(context.Background(), b), d, "42")
		expectTyped(t, err, igl.ErrBudgetItems)
	})
	t.Run("fork items", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":10}`)
		}))
		b := igl.DefaultBudget()
		b.MaxItems = 1
		if err := b.AddItem(); err != nil {
			t.Fatal(err)
		}
		mr := &gitlab.MergeRequest{BasicMergeRequest: gitlab.BasicMergeRequest{ID: 5001, IID: 1, ProjectID: 42, SourceProjectID: 77}}
		err := requireProvenMRForkProjects(igl.WithBudget(context.Background(), b), d, CanonicalProject{ID: 42}, mr)
		expectTyped(t, err, igl.ErrBudgetItems)
	})
	t.Run("owner parent deadline", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
			case <-timer.C:
			}
		}))
		parent, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := AuthorizeCanonicalProject(parent, d, "42")
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("owner max elapsed", func(t *testing.T) {
		parent := context.Background()
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
			case <-timer.C:
			}
		}))
		b := igl.DefaultBudget()
		b.MaxElapsed = 80 * time.Millisecond
		b.MaxRequests = 16
		_, err := AuthorizeCanonicalProject(igl.WithBudget(parent, b), d, "42")
		if !errors.Is(err, igl.ErrBudgetElapsed) || parent.Err() != nil {
			t.Fatalf("got %v parent=%v", err, parent.Err())
		}
	})
	t.Run("allowlist bytes", func(t *testing.T) {
		var ancestry int
		d := deps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/user"):
				_, _ = io.WriteString(w, `{"id":7}`)
			case strings.Contains(r.URL.Path, "/groups/10"):
				_, _ = io.WriteString(w, `{"id":10,"full_path":"`+strings.Repeat("g", 4000)+`"}`)
			case strings.Contains(r.URL.Path, "/groups/99"):
				ancestry++
				_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":10}`)
			case strings.Contains(r.URL.Path, "/projects/42"):
				_, _ = io.WriteString(w, project(42, 99))
			default:
				http.NotFound(w, r)
			}
		}))
		b := igl.DefaultBudget()
		b.MaxBytes = 900
		b.MaxRequests = 16
		_, err := AuthorizeCanonicalProject(igl.WithBudget(context.Background(), b), d, "42")
		expectTyped(t, err, igl.ErrBudgetBytes)
		if ancestry != 0 {
			t.Fatalf("allowlist byte stop reached ancestry %d", ancestry)
		}
	})
	t.Run("allowlist items", func(t *testing.T) {
		var ancestry int
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			ancestry++
			_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":10}`)
		}))
		b := igl.DefaultBudget()
		b.MaxItems = 1
		if err := b.AddItem(); err != nil {
			t.Fatal(err)
		}
		_, err := AuthorizeCanonicalProject(igl.WithBudget(context.Background(), b), d, "42")
		expectTyped(t, err, igl.ErrBudgetItems)
		if ancestry != 0 {
			t.Fatalf("allowlist item stop reached ancestry %d", ancestry)
		}
	})
	t.Run("ancestry items", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":99,"full_path":"other","parent_id":10}`)
		}))
		b := igl.DefaultBudget()
		b.MaxItems = 1
		if err := b.AddItem(); err != nil {
			t.Fatal(err)
		}
		_, err := groupAncestryContains(igl.WithBudget(context.Background(), b), d, 99, map[int64]struct{}{10: {}})
		expectTyped(t, err, igl.ErrBudgetItems)
	})
	t.Run("allowlist parent deadline", func(t *testing.T) {
		d := deps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/user"):
				_, _ = io.WriteString(w, `{"id":7}`)
			case strings.Contains(r.URL.Path, "/groups/10"):
				timer := time.NewTimer(2 * time.Second)
				defer timer.Stop()
				select {
				case <-r.Context().Done():
				case <-timer.C:
				}
			case strings.Contains(r.URL.Path, "/projects/42"):
				_, _ = io.WriteString(w, project(42, 99))
			default:
				http.NotFound(w, r)
			}
		}))
		parent, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		_, err := AuthorizeCanonicalProject(parent, d, "42")
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("allowlist max elapsed", func(t *testing.T) {
		parent := context.Background()
		d := deps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/user"):
				_, _ = io.WriteString(w, `{"id":7}`)
			case strings.Contains(r.URL.Path, "/groups/10"):
				timer := time.NewTimer(2 * time.Second)
				defer timer.Stop()
				select {
				case <-r.Context().Done():
				case <-timer.C:
				}
			case strings.Contains(r.URL.Path, "/projects/42"):
				_, _ = io.WriteString(w, project(42, 99))
			default:
				http.NotFound(w, r)
			}
		}))
		b := igl.DefaultBudget()
		b.MaxElapsed = 80 * time.Millisecond
		b.MaxRequests = 16
		_, err := AuthorizeCanonicalProject(igl.WithBudget(parent, b), d, "42")
		if !errors.Is(err, igl.ErrBudgetElapsed) || parent.Err() != nil {
			t.Fatalf("got %v parent=%v", err, parent.Err())
		}
	})
	t.Run("allowlist cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		d := deps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/user"):
				_, _ = io.WriteString(w, `{"id":7}`)
			case strings.Contains(r.URL.Path, "/groups/10"):
				cancel()
				<-r.Context().Done()
			case strings.Contains(r.URL.Path, "/projects/42"):
				_, _ = io.WriteString(w, project(42, 99))
			default:
				http.NotFound(w, r)
			}
		}))
		_, err := AuthorizeCanonicalProject(ctx, d, "42")
		expectTyped(t, err, context.Canceled)
	})
	t.Run("fork elapsed", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.URL.Path, "/groups/99") && !strings.Contains(r.URL.Path, "/projects/77") {
				_, _ = io.WriteString(w, `{"id":10}`)
				return
			}
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
			case <-timer.C:
			}
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		mr := &gitlab.MergeRequest{BasicMergeRequest: gitlab.BasicMergeRequest{ID: 5001, IID: 1, ProjectID: 42, SourceProjectID: 77}}
		err := requireProvenMRForkProjects(ctx, d, CanonicalProject{ID: 42}, mr)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("fork max elapsed", func(t *testing.T) {
		parent := context.Background()
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			timer := time.NewTimer(2 * time.Second)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
			case <-timer.C:
			}
		}))
		b := igl.DefaultBudget()
		b.MaxElapsed = 80 * time.Millisecond
		b.MaxRequests = 16
		mr := &gitlab.MergeRequest{BasicMergeRequest: gitlab.BasicMergeRequest{ID: 5001, IID: 1, ProjectID: 42, SourceProjectID: 77}}
		err := requireProvenMRForkProjects(igl.WithBudget(parent, b), d, CanonicalProject{ID: 42}, mr)
		if !errors.Is(err, igl.ErrBudgetElapsed) || parent.Err() != nil {
			t.Fatalf("got %v parent=%v", err, parent.Err())
		}
	})
	t.Run("owner cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			cancel()
			<-r.Context().Done()
		}))
		_, err := AuthorizeCanonicalProject(ctx, d, "42")
		expectTyped(t, err, context.Canceled)
	})
	t.Run("fork cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/projects/77") || strings.Contains(r.URL.Path, "/groups/") {
				cancel()
				<-r.Context().Done()
				return
			}
		}))
		mr := &gitlab.MergeRequest{BasicMergeRequest: gitlab.BasicMergeRequest{ID: 5001, IID: 1, ProjectID: 42, SourceProjectID: 77}}
		err := requireProvenMRForkProjects(ctx, d, CanonicalProject{ID: 42}, mr)
		expectTyped(t, err, context.Canceled)
	})
	t.Run("ordinary unavailable stays identity", func(t *testing.T) {
		d := deps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/projects/42") {
				fmt.Fprintf(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":99,"kind":"group"}}`)
				return
			}
			http.NotFound(w, r)
		}))
		_, err := AuthorizeCanonicalProject(context.Background(), d, "42")
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) || errors.Is(err, igl.ErrBudgetRequests) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("non-group ancestry stays denied", func(t *testing.T) {
		d := deps(t, handler(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		_, err := AuthorizeCanonicalProject(context.Background(), d, "42")
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) || errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
}

func diffPaths(entries []any) []string {
	var out []string
	for _, raw := range entries {
		m, _ := raw.(map[string]any)
		if p, _ := m["new_path"].(string); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func assertResumeEmitsNothing(t *testing.T, out map[string]any) {
	t.Helper()
	sec := sectionMap(out)
	entries := asSlice(t, out["entries"])
	if len(entries) != 0 || out["digest"] != nil || sec["next_cursor"] != nil || sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("resume left entries=%v digest=%v cursor=%v complete=%v", diffPaths(entries), out["digest"], sec["next_cursor"], sec["content_complete"])
	}
}

func TestRepairR1_incompleteResumeAndInitialBound(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	two := `[{"old_path":"a.txt","new_path":"a.txt","diff":"p"},{"old_path":"b.txt","new_path":"b.txt","diff":"p"}]`
	changed := `[{"old_path":"a.txt","new_path":"a.txt","diff":"p"},{"old_path":"new-unverified.txt","new_path":"new-unverified.txt","diff":"p"}]`
	mr := `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`
	serveSwitch := func(after int, next string) http.Handler {
		var n int
		return serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				n++
				body := versionObject(1, 5001, head, base, start, "collected", "2", two)
				if n > after {
					body = next
				}
				_, _ = io.WriteString(w, body)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, mr)
			default:
				http.NotFound(w, r)
			}
		})
	}
	firstCursor := func(t *testing.T, h http.Handler) (Deps, string) {
		t.Helper()
		d := diffDeps(t, h)
		out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 1})
		if err != nil {
			t.Fatal(err)
		}
		cur, _ := sectionMap(out)["next_cursor"].(string)
		if cur == "" || out["digest"] == nil || len(asSlice(t, out["entries"])) != 1 {
			t.Fatalf("first page did not mint a window: %#v", out)
		}
		return d, cur
	}
	cases := []struct {
		name string
		body string
	}{
		{"capped count", versionObject(1, 5001, head, base, start, "collected", "100+", changed)},
		{"missing count", fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","diffs":%s}`, head, base, start, changed)},
		{"mismatched count", versionObject(1, 5001, head, base, start, "collected", "9", changed)},
		{"missing state", fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"real_size":"2","diffs":%s}`, head, base, start, changed)},
		{"non-collected", versionObject(1, 5001, head, base, start, "overflow", "2", changed)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, cur := firstCursor(t, serveSwitch(2, tc.body))
			out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 1, "cursor": cur})
			if err != nil {
				t.Fatal(err)
			}
			assertResumeEmitsNothing(t, out)
		})
	}
	t.Run("stable resume still proves the next page", func(t *testing.T) {
		d, cur := firstCursor(t, serveSwitch(99, two))
		out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 1, "cursor": cur})
		if err != nil {
			t.Fatal(err)
		}
		sec := sectionMap(out)
		paths := diffPaths(asSlice(t, out["entries"]))
		if len(paths) != 1 || paths[0] != "b.txt" || out["digest"] == nil || sec["next_cursor"] != nil || sec["content_complete"] != readmeta.ContentCompleteTrue {
			t.Fatalf("positive resume paths=%v digest=%v sec=%#v", paths, out["digest"], sec)
		}
	})
	t.Run("initial partial respects per_page", func(t *testing.T) {
		var diffs []string
		for i := 0; i < 60; i++ {
			diffs = append(diffs, fmt.Sprintf(`{"old_path":"p%02d","new_path":"p%02d","diff":"x"}`, i, i))
		}
		body := versionObject(1, 5001, head, base, start, "collected", "61", "["+strings.Join(diffs, ",")+"]")
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				_, _ = io.WriteString(w, body)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, mr)
			default:
				http.NotFound(w, r)
			}
		})
		out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
		if err != nil {
			t.Fatal(err)
		}
		sec := sectionMap(out)
		entries := asSlice(t, out["entries"])
		if len(entries) > 20 || out["digest"] != nil || sec["next_cursor"] != nil || sec["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatalf("initial partial entries=%d digest=%v cursor=%v complete=%v", len(entries), out["digest"], sec["next_cursor"], sec["content_complete"])
		}
	})
}

func TestRepairR2_earlierParentDeadlineAfterBudget(t *testing.T) {
	groupDeps := func(t *testing.T, h http.Handler) Deps {
		t.Helper()
		d := newReviewDeps(t, h)
		d.Config.AllowedProjectIDs = nil
		d.Config.AllowedGroupIDs = []string{"10"}
		return d
	}
	t.Run("preflight after both elapsed", func(t *testing.T) {
		parent, ctx, b := func() (context.Context, context.Context, *igl.Budget) {
			parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			t.Cleanup(cancel)
			b := igl.DefaultBudget()
			b.MaxElapsed = 80 * time.Millisecond
			b.MaxRequests = 16
			return parent, igl.WithBudget(parent, b), b
		}()
		time.Sleep(100 * time.Millisecond)
		d := groupDeps(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("preflight reached HTTP")
		}))
		_, err := getGroupSafe(ctx, d, "10")
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) || !b.ElapsedExceeded() || parent.Err() == nil {
			t.Fatalf("got %v parent=%v elapsed=%v", err, parent.Err(), b.ElapsedExceeded())
		}
	})
	t.Run("delayed transport classification", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		b := igl.DefaultBudget()
		b.MaxElapsed = 80 * time.Millisecond
		ctx := igl.WithBudget(parent, b)
		time.Sleep(100 * time.Millisecond)
		err := groupContextCause(ctx, igl.ErrBudgetElapsed)
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("in-flight request observed after both", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		b := igl.DefaultBudget()
		b.MaxElapsed = 80 * time.Millisecond
		b.MaxRequests = 16
		ctx := igl.WithBudget(parent, b)
		d := groupDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			timer := time.NewTimer(150 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
				<-timer.C
			case <-timer.C:
			}
			http.Error(w, "late", http.StatusGatewayTimeout)
		}))
		_, err := getGroupSafe(ctx, d, "10")
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) || parent.Err() == nil {
			t.Fatalf("got %v parent=%v", err, parent.Err())
		}
	})
	t.Run("genuine max elapsed keeps live parent", func(t *testing.T) {
		parent := context.Background()
		b := igl.DefaultBudget()
		b.MaxElapsed = 80 * time.Millisecond
		b.MaxRequests = 16
		ctx := igl.WithBudget(parent, b)
		d := groupDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			timer := time.NewTimer(200 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-r.Context().Done():
			case <-timer.C:
			}
		}))
		_, err := getGroupSafe(ctx, d, "10")
		if !errors.Is(err, igl.ErrBudgetElapsed) || parent.Err() != nil {
			t.Fatalf("got %v parent=%v", err, parent.Err())
		}
	})
}

func jsonProvider(req *http.Request, code int, body string) (*http.Response, error) {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode:    code,
		Status:        fmt.Sprintf("%d", code),
		Header:        h,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

// holdAfterBoth sleeps past an earlier 20ms parent deadline and an 80ms budget,
// then returns a plain error. BudgetInterceptor normalizes that late parent
// deadline to ErrBudgetElapsed. The sleep ignores cancel, so this is not an
// in-flight return at the 20ms deadline.
func holdAfterBoth(match func(string) bool, fast func(*http.Request) (*http.Response, error)) http.RoundTripper {
	return roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if match(req.URL.Path) {
			time.Sleep(120 * time.Millisecond)
			return nil, errors.New("delayed provider")
		}
		return fast(req)
	})
}

func authClient(t *testing.T, next http.RoundTripper) Deps {
	t.Helper()
	d := newReviewDeps(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("real server reached")
	}))
	cli, err := gitlab.NewClient("fixture-only",
		gitlab.WithBaseURL(d.Config.APIURL),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(&http.Client{Transport: next, Timeout: 2 * time.Second}),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	d.Client = cli
	d.Config.AllowedProjectIDs = []string{"42"}
	return d
}

func earlierParentBudget(t *testing.T) (context.Context, context.Context) {
	t.Helper()
	parent, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	t.Cleanup(cancel)
	b := igl.DefaultBudget()
	b.MaxElapsed = 80 * time.Millisecond
	b.MaxRequests = 16
	return parent, igl.WithBudget(parent, b)
}

func project42(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/projects/42") {
		return jsonProvider(req, http.StatusOK, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group"}}`)
	}
	return jsonProvider(req, http.StatusNotFound, `{}`)
}

func TestRepairAuth_ownerMRForkEarlierDeadline(t *testing.T) {
	mrSame := `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`
	mrFork := `{"id":5001,"iid":1,"project_id":42,"source_project_id":99,"target_project_id":42}`
	wantDeadline := func(t *testing.T, err error, parent context.Context) {
		t.Helper()
		if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) || parent.Err() == nil {
			t.Fatalf("got %v parent=%v", err, parent.Err())
		}
	}
	t.Run("owner project after both", func(t *testing.T) {
		parent, ctx := earlierParentBudget(t)
		d := authClient(t, holdAfterBoth(func(path string) bool {
			return strings.Contains(path, "/projects/42") && !strings.Contains(path, "/merge_requests/")
		}, project42))
		_, err := getProjectSafe(ctx, d, "42")
		wantDeadline(t, err, parent)
	})
	t.Run("legacy MR after both", func(t *testing.T) {
		parent, ctx := earlierParentBudget(t)
		d := authClient(t, holdAfterBoth(func(path string) bool {
			return strings.Contains(path, "/merge_requests/")
		}, project42))
		_, _, err := loadAuthorizedMR(ctx, d, "42", 1)
		wantDeadline(t, err, parent)
	})
	t.Run("diff identity MR after both", func(t *testing.T) {
		parent, ctx := earlierParentBudget(t)
		d := authClient(t, holdAfterBoth(func(path string) bool {
			return strings.Contains(path, "/merge_requests/")
		}, func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "/merge_requests/") {
				return jsonProvider(req, http.StatusOK, mrSame)
			}
			return project42(req)
		}))
		_, _, err := loadDiffIdentity(ctx, d, "42", 1)
		wantDeadline(t, err, parent)
	})
	t.Run("fork source project after both", func(t *testing.T) {
		parent, ctx := earlierParentBudget(t)
		d := authClient(t, holdAfterBoth(func(path string) bool {
			return strings.Contains(path, "/projects/99")
		}, func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "/merge_requests/") {
				return jsonProvider(req, http.StatusOK, mrFork)
			}
			return project42(req)
		}))
		_, _, err := loadDiffIdentity(ctx, d, "42", 1)
		wantDeadline(t, err, parent)
	})
	t.Run("genuine max elapsed keeps live parent", func(t *testing.T) {
		parent := context.Background()
		b := igl.DefaultBudget()
		b.MaxElapsed = 80 * time.Millisecond
		b.MaxRequests = 16
		ctx := igl.WithBudget(parent, b)
		d := authClient(t, holdAfterBoth(func(path string) bool {
			return strings.Contains(path, "/projects/42") && !strings.Contains(path, "/merge_requests/")
		}, project42))
		_, err := getProjectSafe(ctx, d, "42")
		if !errors.Is(err, igl.ErrBudgetElapsed) || parent.Err() != nil {
			t.Fatalf("got %v parent=%v", err, parent.Err())
		}
	})
	t.Run("requests bytes items cancel and ordinary stay", func(t *testing.T) {
		b := igl.DefaultBudget()
		b.MaxRequests = 1
		b.MaxElapsed = 30 * time.Second
		ctx := igl.WithBudget(context.Background(), b)
		d := authClient(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return project42(req)
		}))
		if _, err := getProjectSafe(ctx, d, "42"); err != nil {
			t.Fatal(err)
		}
		_, err := getProjectSafe(ctx, d, "42")
		if !errors.Is(err, igl.ErrBudgetRequests) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("requests: %v", err)
		}
		for _, sentinel := range []error{igl.ErrBudgetBytes, igl.ErrBudgetItems} {
			d := authClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, sentinel
			}))
			_, err := getProjectSafe(igl.WithBudget(context.Background(), igl.DefaultBudget()), d, "42")
			if !errors.Is(err, sentinel) || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%v: %v", sentinel, err)
			}
		}
		parent, cancel := context.WithCancel(context.Background())
		cancel()
		d = authClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("cancel reached provider")
			return nil, errors.New("unreachable")
		}))
		_, err = getProjectSafe(igl.WithBudget(parent, igl.DefaultBudget()), d, "42")
		if !errors.Is(err, context.Canceled) || errors.Is(err, igl.ErrBudgetElapsed) {
			t.Fatalf("cancel: %v", err)
		}
		d = authClient(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return jsonProvider(req, http.StatusNotFound, `{}`)
		}))
		_, err = getProjectSafe(context.Background(), d, "42")
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) || errors.Is(err, igl.ErrBudgetElapsed) {
			t.Fatalf("ordinary: %v", err)
		}
	})
}

func sufficientManifestBudget() *igl.Budget {
	b := igl.DefaultBudget()
	// Default MaxItems is 100. A proved 101-path body charges each path, and
	// review context also charges the review item. This borrowed cap is only
	// large enough for that charge; it does not change the product default.
	b.MaxItems = 128
	return b
}

func TestRepairContext101_sharedDigest(t *testing.T) {
	head, base, start := shaN(1), shaN(101), shaN(201)
	var diffs []string
	for i := 0; i < 101; i++ {
		diffs = append(diffs, fmt.Sprintf(`{"old_path":"p%03d","new_path":"p%03d","diff":"x"}`, i, i))
	}
	body := versionObject(9, 5001, head, base, start, "collected", "101", "["+strings.Join(diffs, ",")+"]")
	d := newReviewDeps(t, repairReviewHandler(body, nil))
	d.Config.AllowedProjectIDs = []string{"42"}
	out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), sufficientManifestBudget()), []reviewContextItemIn{metaItem("42", 1, "diff_manifest")})
	if err != nil {
		t.Fatal(err)
	}
	item := out.Items[0]
	sec := item.Sections["diff_manifest"]
	if item.Cause != "" || !item.ReviewClean || item.ContextRef == nil || item.DiffManifest == nil || item.diffManifestDigest == "" {
		t.Fatalf("cause=%s clean=%v ref=%v manifest=%v digest=%q", item.Cause, item.ReviewClean, item.ContextRef != nil, item.DiffManifest != nil, item.diffManifestDigest)
	}
	if len(item.DiffManifest.Entries) != 101 || sec.NextCursor != nil || sec.ContentComplete != readmeta.ContentCompleteTrue || item.DiffManifest.Digest == nil || *item.DiffManifest.Digest != item.diffManifestDigest {
		t.Fatalf("entries=%d cursor=%v complete=%s digest=%v private=%s", len(item.DiffManifest.Entries), sec.NextCursor, sec.ContentComplete, item.DiffManifest.Digest, item.diffManifestDigest)
	}
	payload, err := cursor.Decode(d.Config.CursorKey, *item.ContextRef, d.now())
	if err != nil {
		t.Fatal(err)
	}
	if payload.ContextRef == nil || payload.ContextRef.Evidence["diff_manifest"] != cursor.DiffManifestEvidenceV1 || payload.ContextRef.Digests["diff_manifest"] != item.diffManifestDigest {
		t.Fatalf("evidence=%v digests=%v", payload.ContextRef.Evidence, payload.ContextRef.Digests)
	}
	id := int64(9)
	per := 50
	_, raw, err := getMergeRequestDiffWindow(igl.WithBudget(context.Background(), sufficientManifestBudget()), nil, diffWindowIn{
		ProjectID: "42", MergeRequestIID: 1, DiffVersionID: &id, PerPage: &per,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	window, ok := raw.(diffWindowOut)
	if !ok || window.Digest == nil || *window.Digest != item.diffManifestDigest || len(window.Entries) != 50 {
		t.Fatalf("direct ok=%v digest=%v entries=%d", ok, window.Digest, len(window.Entries))
	}
}

func TestRepairManifest_renameDeletePatchDigest(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	flags := `{"old_path":"a.txt","new_path":"b.txt","a_mode":"100644","b_mode":"100755","renamed_file":true,"new_file":false,"deleted_file":false},{"old_path":"c.txt","new_path":"c.txt","deleted_file":true,"renamed_file":false}`
	first := versionObject(1, 5001, head, base, start, "collected", "2", "["+strings.ReplaceAll(flags, "}", `,"diff":"patch-a","patch":"patch-a"}`)+"]")
	second := versionObject(1, 5001, head, base, start, "collected", "2", "["+strings.ReplaceAll(flags, "}", `,"diff":"other-patch","patch":"other-patch"}`)+"]")
	mr := `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42}`
	serve := func(body string) http.Handler {
		return serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				_, _ = io.WriteString(w, body)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, mr)
			default:
				http.NotFound(w, r)
			}
		})
	}
	read := func(body string) map[string]any {
		t.Helper()
		out, err := callDiffWindow(t, diffDeps(t, serve(body)), nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 2})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	left := read(first)
	right := read(second)
	if left["digest"] == nil || left["digest"] != right["digest"] {
		t.Fatalf("patch changed digest %v %v", left["digest"], right["digest"])
	}
	entries := asSlice(t, left["entries"])
	if len(entries) != 2 {
		t.Fatalf("entries %#v", entries)
	}
	renamed, _ := entries[0].(map[string]any)
	deleted, _ := entries[1].(map[string]any)
	if renamed["old_path"] != "a.txt" || renamed["new_path"] != "b.txt" || renamed["a_mode"] != "100644" || renamed["b_mode"] != "100755" || renamed["renamed_file"] != true || renamed["diff"] != nil || renamed["patch"] != nil {
		t.Fatalf("rename %#v", renamed)
	}
	if deleted["old_path"] != "c.txt" || deleted["new_path"] != "c.txt" || deleted["deleted_file"] != true || deleted["a_mode"] != nil || deleted["b_mode"] != nil || deleted["diff"] != nil {
		t.Fatalf("delete %#v", deleted)
	}
	if sectionMap(left)["content_complete"] != readmeta.ContentCompleteTrue {
		t.Fatalf("section %#v", sectionMap(left))
	}
}
