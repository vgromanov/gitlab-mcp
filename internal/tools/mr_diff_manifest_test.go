package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

func diffDeps(t *testing.T, h http.Handler) Deps {
	t.Helper()
	d := newReviewDeps(t, h)
	d.Config.AllowedProjectIDs = []string{"42"}
	return d
}

func callDiffWindow(t *testing.T, d Deps, srvCtx context.Context, args map[string]any) (map[string]any, error) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "diff-window", Version: "t"}, nil)
	RegisterMergeRequests(srv, d)
	if srvCtx == nil {
		srvCtx = context.Background()
	}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(srvCtx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cli := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	cs, err := cli.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_merge_request_diff_window", Arguments: args})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, confToolErr(res)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(toolJSON(t, res)), &out); err != nil {
		t.Fatal(err)
	}
	return out, nil
}

func versionObject(id, mrID int64, head, base, start, state string, realSize string, diffs string) string {
	return fmt.Sprintf(`{"id":%d,"merge_request_id":%d,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":%q,"real_size":%q,"diffs":%s}`,
		id, mrID, head, base, start, state, realSize, diffs)
}

func oneDiff(path, patch string) string {
	return fmt.Sprintf(`[{"old_path":%q,"new_path":%q,"a_mode":"100644","b_mode":"100644","new_file":false,"renamed_file":false,"deleted_file":false,"diff":%q}]`, path, path, patch)
}

func serveDiffBase(log *pathLog, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.add(r.URL.Path, r.URL.RawQuery)
		switch {
		case strings.HasSuffix(r.URL.Path, "/user"):
			_, _ = io.WriteString(w, `{"id":7}`)
		case strings.Contains(r.URL.Path, "/merge_requests/") || strings.Contains(r.URL.Path, "/repository/") || strings.Contains(r.URL.Path, "/diffs"):
			next(w, r)
		case strings.Contains(r.URL.Path, "/projects/"):
			id := projectIDFromPath(r.URL.Path)
			fmt.Fprintf(w, `{"id":%d,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group"}}`, id)
		default:
			next(w, r)
		}
	})
}

func TestDiffWindow_historicalNotCurrentDiffs(t *testing.T) {
	log := &pathLog{}
	head, base, start := shaN(1), shaN(2), shaN(3)
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/diffs"):
			_, _ = io.WriteString(w, `{"diffs":[{"new_path":"current.txt","old_path":"current.txt","diff":"now"}]}`)
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("historical.txt", "OLD")))
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
	if err != nil {
		t.Fatal(err)
	}
	if log.count("/diffs") != 0 || log.count("/versions/1") < 2 {
		t.Fatalf("paths=%v", log.paths)
	}
	sec := sectionMap(out)
	if sec["content_complete"] != readmeta.ContentCompleteTrue || sec["patch_coverage"] != readmeta.CoverageUnknown || sec["manifest_coverage"] != readmeta.CoverageFull {
		t.Fatalf("section=%#v", sec)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 || asMap(t, entries[0])["new_path"] != "historical.txt" {
		t.Fatalf("entries=%#v", entries)
	}
	raw, _ := json.Marshal(entries[0])
	if strings.Contains(string(raw), "diff") || strings.Contains(string(raw), "patch") || strings.Contains(string(raw), "overflow") {
		t.Fatalf("entry leaked patch: %s", raw)
	}
}

func TestDiffWindow_tuplePagesAndFailures(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	row := func(id int64, h, b, s string) string {
		return fmt.Sprintf(`{"id":%d,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}`, id, h, b, s)
	}
	newLog := func(mode string) (*pathLog, Deps) {
		log := &pathLog{}
		h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/7"):
				_, _ = io.WriteString(w, versionObject(7, 5001, head, base, start, "collected", "1", oneDiff("tuple.txt", "P")))
			case strings.Contains(r.URL.Path, "/versions"):
				page := r.URL.Query().Get("page")
				switch mode {
				case "ok":
					if page == "2" {
						w.Header().Set("X-Next-Page", "")
						fmt.Fprintf(w, "[%s]", row(7, head, base, start))
						return
					}
					w.Header().Set("X-Next-Page", "2")
					fmt.Fprintf(w, "[%s]", row(1, shaN(9), base, start))
				case "leap":
					w.Header().Set("X-Next-Page", "3")
					fmt.Fprintf(w, "[%s]", row(7, head, base, start))
				case "dup":
					w.Header().Add("X-Next-Page", "2")
					w.Header().Add("X-Next-Page", "2")
					fmt.Fprintf(w, "[%s]", row(7, head, base, start))
				case "missing":
					fmt.Fprintf(w, "[%s]", row(7, head, base, start))
				case "later-dup":
					if page == "2" {
						w.Header().Set("X-Next-Page", "")
						fmt.Fprintf(w, "[%s]", row(8, head, base, start))
						return
					}
					w.Header().Set("X-Next-Page", "2")
					fmt.Fprintf(w, "[%s]", row(7, head, base, start))
				case "head-only":
					w.Header().Set("X-Next-Page", "")
					fmt.Fprintf(w, "[%s]", row(7, head, shaN(8), start))
				}
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		return log, diffDeps(t, h)
	}
	args := map[string]any{"project_id": "42", "merge_request_iid": 1, "base_sha": base, "start_sha": start, "head_sha": head, "per_page": 20}
	t.Run("page1-2-terminal", func(t *testing.T) {
		log, d := newLog("ok")
		out, err := callDiffWindow(t, d, nil, args)
		if err != nil {
			t.Fatal(err)
		}
		if log.count("/versions/7") != 2 || sectionMap(out)["content_complete"] != readmeta.ContentCompleteTrue {
			t.Fatalf("paths=%v section=%#v", log.paths, sectionMap(out))
		}
	})
	for _, mode := range []string{"leap", "dup", "missing", "later-dup", "head-only"} {
		t.Run(mode, func(t *testing.T) {
			log, d := newLog(mode)
			out, err := callDiffWindow(t, d, nil, args)
			if err != nil {
				t.Fatal(err)
			}
			if log.count("/versions/7") != 0 || sectionMap(out)["content_complete"] == readmeta.ContentCompleteTrue {
				t.Fatalf("%s paths=%v section=%#v", mode, log.paths, sectionMap(out))
			}
		})
	}
}

func TestDiffWindow_collected101AndCaps(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	var diffs []string
	for i := 0; i < 101; i++ {
		diffs = append(diffs, fmt.Sprintf(`{"old_path":"f-%03d","new_path":"f-%03d","diff":"p-%d"}`, i, i, i))
	}
	body := versionObject(1, 5001, head, base, start, "collected", "101", "["+strings.Join(diffs, ",")+"]")
	serve := func(log *pathLog) http.Handler {
		return serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				_, _ = io.WriteString(w, body)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
	}
	t.Run("sufficient budget windows", func(t *testing.T) {
		log := &pathLog{}
		d := diffDeps(t, serve(log))
		b := igl.DefaultBudget()
		b.MaxItems = 500
		b.MaxRequests = 64
		ctx := igl.WithBudget(context.Background(), b)
		out, err := callDiffWindow(t, d, ctx, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 50})
		if err != nil {
			t.Fatal(err)
		}
		sec := sectionMap(out)
		entries, _ := out["entries"].([]any)
		if sec["content_complete"] != readmeta.ContentCompleteFalse || sec["pagination_exhausted"] != false || len(entries) != 50 || sec["next_cursor"] == nil {
			t.Fatalf("page1 entries=%d section=%#v", len(entries), sec)
		}
		if sec["manifest_coverage"] != readmeta.CoverageFull || out["digest"] == nil || sec["patch_coverage"] != readmeta.CoverageUnknown {
			t.Fatalf("page1 proof separated from window: digest=%v section=%#v", out["digest"], sec)
		}
		files, _ := asMap(t, sec["counts"])["files"].(float64)
		if files != 101 {
			t.Fatalf("counts=%#v", sec["counts"])
		}
		cur, _ := sec["next_cursor"].(string)
		out2, err := callDiffWindow(t, d, ctx, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 50, "cursor": cur})
		if err != nil {
			t.Fatal(err)
		}
		if sectionMap(out2)["content_complete"] != readmeta.ContentCompleteFalse || sectionMap(out2)["next_cursor"] == nil {
			t.Fatalf("page2 still a partial window: %#v", sectionMap(out2))
		}
		cur2, _ := sectionMap(out2)["next_cursor"].(string)
		out3, err := callDiffWindow(t, d, ctx, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 50, "cursor": cur2})
		if err != nil {
			t.Fatal(err)
		}
		sec3 := sectionMap(out3)
		entries3, _ := out3["entries"].([]any)
		if sec3["pagination_exhausted"] != true || sec3["next_cursor"] != nil || len(entries3) != 1 || sec3["content_complete"] != readmeta.ContentCompleteTrue {
			t.Fatalf("last entries=%d section=%#v", len(entries3), sec3)
		}
	})
	t.Run("default item cap", func(t *testing.T) {
		log := &pathLog{}
		d := diffDeps(t, serve(log))
		_, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 50})
		if err == nil || !strings.Contains(err.Error(), readmeta.CodeBudgetItems) {
			t.Fatalf("default cap: %v", err)
		}
	})
}

func TestDiffWindow_malformedAndFlags(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing-diffs", fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":"collected","real_size":1}`, head, base, start), readmeta.ContentCompleteUnknown},
		{"null-diffs", versionObject(1, 5001, head, base, start, "collected", "1", "null"), readmeta.ContentCompleteUnknown},
		{"null-state", fmt.Sprintf(`{"id":1,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q,"state":null,"real_size":1,"diffs":%s}`, head, base, start, oneDiff("a", "p")), readmeta.ContentCompleteUnknown},
		{"plus-count", versionObject(1, 5001, head, base, start, "collected", "100+", oneDiff("a", "p")), readmeta.ContentCompleteUnknown},
		{"overflow", versionObject(1, 5001, head, base, start, "overflow", "1", oneDiff("a", "p")), readmeta.ContentCompleteFalse},
		{"without-files", versionObject(1, 5001, head, base, start, "without_files", "1", "[]"), readmeta.ContentCompleteFalse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			log := &pathLog{}
			h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.Contains(r.URL.Path, "/versions/1"):
					_, _ = io.WriteString(w, tc.body)
				case strings.Contains(r.URL.Path, "/merge_requests/"):
					_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
				default:
					http.NotFound(w, r)
				}
			})
			out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1})
			if err != nil {
				t.Fatal(err)
			}
			if sectionMap(out)["content_complete"] != tc.want || sectionMap(out)["next_cursor"] != nil {
				t.Fatalf("%s section=%#v", tc.name, sectionMap(out))
			}
		})
	}
	t.Run("unknown flags", func(t *testing.T) {
		raw := fmt.Sprintf(`[{"old_path":"a","new_path":"b","collapsed":null,"too_large":"x","generated_file":true,"diff":"secret"}]`)
		body := versionObject(1, 5001, head, base, start, "collected", "1", raw)
		log := &pathLog{}
		h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				_, _ = io.WriteString(w, body)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1})
		if err != nil {
			t.Fatal(err)
		}
		entry := asMap(t, asSlice(t, out["entries"])[0])
		if entry["collapsed"] != nil || entry["too_large"] != nil || entry["new_file"] != nil || entry["generated_file"] != true {
			t.Fatalf("flags=%#v", entry)
		}
		if _, ok := entry["overflow"]; ok {
			t.Fatal("invented overflow")
		}
		if _, ok := entry["diff"]; ok {
			t.Fatal("patch in output")
		}
	})
}

func TestDiffWindow_cursorMismatches(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	var generation int
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			generation++
			name := "a"
			if generation > 2 {
				name = "b"
			}
			var diffs []string
			for i := 0; i < 2; i++ {
				diffs = append(diffs, fmt.Sprintf(`{"old_path":"%s-%d","new_path":"%s-%d","diff":"p"}`, name, i, name, i))
			}
			_, _ = io.WriteString(w, versionObject(1, 5001, head, base, start, "collected", "2", "["+strings.Join(diffs, ",")+"]"))
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 1})
	if err != nil {
		t.Fatal(err)
	}
	cur, _ := sectionMap(out)["next_cursor"].(string)
	if cur == "" {
		t.Fatal("missing cursor")
	}
	before := log.snapshot()
	if _, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 2, "per_page": 1, "cursor": cur}); err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) || log.snapshot() != before {
		t.Fatalf("selection mismatch err=%v paths=%v", err, log.paths[before:])
	}
	payload, err := cursor.Decode(d.Config.CursorKey, cur, d.now())
	if err != nil {
		t.Fatal(err)
	}
	payload.ActorID = 9
	actorTok, err := cursor.Encode(d.Config.CursorKey, payload)
	if err != nil {
		t.Fatal(err)
	}
	alog := &pathLog{}
	ad := diffDeps(t, serveDiffBase(alog, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/user") {
			_, _ = io.WriteString(w, `{"id":7}`)
			return
		}
		http.NotFound(w, r)
	}))
	ad.Config.CursorKey = d.Config.CursorKey
	ad.Config.AllowedProjectIDs = append([]string(nil), d.Config.AllowedProjectIDs...)
	if _, err := callDiffWindow(t, ad, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 1, "cursor": actorTok}); err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) || alog.count("/versions/") != 0 {
		t.Fatalf("actor err=%v paths=%v", err, alog.paths)
	}
	payload.ActorID = 7
	payload.PolicyFP = "other-policy"
	polTok, err := cursor.Encode(d.Config.CursorKey, payload)
	if err != nil {
		t.Fatal(err)
	}
	plog := &pathLog{}
	if _, err := callDiffWindow(t, diffDeps(t, serveDiffBase(plog, func(http.ResponseWriter, *http.Request) {})), nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 1, "cursor": polTok}); err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) || plog.snapshot() != 0 {
		t.Fatalf("policy err=%v paths=%v", err, plog.paths)
	}
	payload.PolicyFP = d.Config.PolicyFingerprint()
	past := d.now().Add(-3 * time.Hour)
	payload.UpperBound = past.Format(time.RFC3339)
	payload.Filters.Until = payload.UpperBound
	payload.ExpiresAt = past.Add(cursor.DefaultTTL).Format(time.RFC3339)
	expTok, err := cursor.Encode(d.Config.CursorKey, payload)
	if err != nil {
		t.Fatal(err)
	}
	elog := &pathLog{}
	if _, err := callDiffWindow(t, diffDeps(t, serveDiffBase(elog, func(http.ResponseWriter, *http.Request) {})), nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 1, "cursor": expTok}); err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) || elog.snapshot() != 0 {
		t.Fatalf("expiry err=%v paths=%v", err, elog.paths)
	}
	shift, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 1, "cursor": cur})
	if err != nil {
		t.Fatal(err)
	}
	if len(asSlice(t, shift["entries"])) != 0 || sectionMap(shift)["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("sequence mismatch emitted %#v", shift)
	}
}

func TestDiffWindow_incrementalMembership(t *testing.T) {
	from, to := shaN(4), shaN(5)
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/diffs"):
			http.Error(w, "no", http.StatusOK)
		case strings.Contains(r.URL.Path, "/repository/commits/"):
			sha := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			fmt.Fprintf(w, `{"id":%q}`, sha)
		case strings.Contains(r.URL.Path, "/repository/compare"):
			fmt.Fprintf(w, `{"commit":{"id":%q},"diffs":[{"old_path":"c","new_path":"c","diff":"hidden"}]}`, to)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true, "per_page": 20})
	if err != nil {
		t.Fatal(err)
	}
	if log.count("/diffs") != 0 || log.count("/repository/compare") != 1 || sectionMap(out)["content_complete"] != readmeta.ContentCompleteFalse {
		t.Fatalf("paths=%v section=%#v", log.paths, sectionMap(out))
	}
	entry := asMap(t, asSlice(t, out["entries"])[0])
	if _, ok := entry["diff"]; ok {
		t.Fatal("compare patch leaked")
	}
}

func TestDiffWindow_crossProjectNoCompare(t *testing.T) {
	from, to := shaN(4), shaN(5)
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/projects/99/repository/commits/"+from):
			fmt.Fprintf(w, `{"id":%q,"project_id":99}`, from)
		case strings.Contains(r.URL.Path, "/projects/42/repository/commits/"+to):
			fmt.Fprintf(w, `{"id":%q}`, to)
		case strings.Contains(r.URL.Path, "/repository/commits/"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/repository/compare"):
			t.Errorf("compare called")
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":99}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	d.Config.AllowedProjectIDs = []string{"42", "99"}
	out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "from_sha": from, "to_sha": to, "straight": true})
	if err != nil {
		t.Fatal(err)
	}
	if log.count("/repository/compare") != 0 || sectionMap(out)["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("paths=%v section=%#v", log.paths, sectionMap(out))
	}
}

func TestDiffAuth_borrowedBudget(t *testing.T) {
	log := &pathLog{}
	borrowed := igl.DefaultBudget()
	borrowed.MaxRequests = 8
	ctx := igl.WithBudget(context.Background(), borrowed)
	var projects int
	inner := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		id := projectIDFromPath(r.URL.Path)
		fmt.Fprintf(w, `{"id":%d,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group"}}`, id)
	})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/projects/") && !strings.Contains(r.URL.Path, "/merge_requests/") {
			projects++
			if projects > 1 {
				borrowed.Cancel()
				<-r.Context().Done()
				return
			}
		}
		inner.ServeHTTP(w, r)
	})
	d := diffDeps(t, h)
	before, _, _ := borrowed.Stats()
	sib := igl.DefaultBudget()
	sibCtx := igl.WithBudget(context.Background(), sib)
	if _, err := getProjectSafe(ctx, d, "42"); err != nil {
		t.Fatalf("seed read: %v", err)
	}
	_, err := getProjectSafe(ctx, d, "42")
	if !errors.Is(err, context.Canceled) && !errors.Is(err, igl.ErrBudgetRequests) {
		t.Fatalf("auth err=%v", err)
	}
	after, _, _ := borrowed.Stats()
	if after < before {
		t.Fatal("counters reset")
	}
	if sibCtx.Err() != nil {
		t.Fatal("sibling budget cancelled")
	}
	parent, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	early := igl.DefaultBudget()
	earlyCtx := igl.WithBudget(parent, early)
	_, err = getProjectSafe(earlyCtx, d, "42")
	if !errors.Is(err, igl.ErrBudgetElapsed) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline err=%v", err)
	}
	if strings.Contains(err.Error(), readmeta.CodeIdentityUnresolved) {
		t.Fatal("deadline wrapped as identity")
	}
	dl, ok := earlyCtx.Deadline()
	if !ok || time.Until(dl) > time.Second {
		t.Fatal("earlier deadline was extended")
	}
}

func TestReviewContext_diffManifestProofs(t *testing.T) {
	head, base, start := shaN(1), shaN(101), shaN(201)
	body := versionObject(9, 5001, head, base, start, "collected", "1", oneDiff("m.txt", "patch"))
	handler := func(drift bool, cancelAfter int) http.HandlerFunc {
		var details int
		return func(w http.ResponseWriter, r *http.Request) {
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
				if r.URL.Query().Get("per_page") != "20" {
					http.Error(w, "per_page", http.StatusBadRequest)
					return
				}
				w.Header().Set("X-Next-Page", "")
				fmt.Fprintf(w, `[{"id":9,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, head, base, start)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				details++
				h := head
				if drift && details > 1 {
					h = shaN(77)
				}
				if cancelAfter > 0 && details >= cancelAfter {
					if b := igl.BudgetFromContext(r.Context()); b != nil {
						b.Cancel()
					}
					return
				}
				fmt.Fprintf(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42,"source_branch":"feature-1","target_branch":"main","sha":%q,"diff_refs":{"head_sha":%q,"base_sha":%q,"start_sha":%q}}`, h, h, base, start)
			case strings.Contains(r.URL.Path, "/projects/"):
				fmt.Fprintf(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group"}}`)
			default:
				http.NotFound(w, r)
			}
		}
	}
	t.Run("clean evidence", func(t *testing.T) {
		d := newReviewDeps(t, handler(false, 0))
		out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), reviewBudget(128)), []reviewContextItemIn{metaItem("42", 1, "metadata", "diff_manifest")})
		if err != nil {
			t.Fatal(err)
		}
		item := out.Items[0]
		if !item.ReviewClean || item.ContextRef == nil || item.Sections["diff_manifest"].NextCursor != nil || item.Sections["diff_manifest"].ContentComplete != readmeta.ContentCompleteTrue {
			t.Fatalf("clean=%v ref=%v sec=%+v", item.ReviewClean, item.ContextRef != nil, item.Sections["diff_manifest"])
		}
		if item.Sections["pipeline_graph"].CapabilityVersion != "" {
			t.Fatal("pipeline was filled")
		}
		decoded, err := cursor.Decode(d.Config.CursorKey, *item.ContextRef, d.now())
		if err != nil {
			t.Fatal(err)
		}
		if len(decoded.ContextRef.Evidence) != 1 || decoded.ContextRef.Evidence["diff_manifest"] != cursor.DiffManifestEvidenceV1 {
			t.Fatalf("evidence=%v", decoded.ContextRef.Evidence)
		}
	})
	t.Run("late bracket drop keeps approval", func(t *testing.T) {
		d := newReviewDeps(t, handler(true, 0))
		out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), reviewBudget(128)), []reviewContextItemIn{metaItem("42", 1, "metadata", "approvals", "diff_manifest")})
		if err != nil {
			t.Fatal(err)
		}
		item := out.Items[0]
		if item.ReviewClean || item.ContextRef != nil || item.Approvals == nil || item.Sections["diff_manifest"].ContentComplete == readmeta.ContentCompleteTrue {
			t.Fatalf("drop item=%+v", item)
		}
	})
	t.Run("mint hook drops manifest", func(t *testing.T) {
		d := newReviewDeps(t, handler(false, 0))
		parent, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx := withReviewMintHook(igl.WithBudget(parent, reviewBudget(128)), cancel)
		out, err := callReviewDirect(t, d, ctx, []reviewContextItemIn{metaItem("42", 1, "metadata", "diff_manifest")})
		if err != nil {
			t.Fatal(err)
		}
		item := out.Items[0]
		if item.ReviewClean || item.ContextRef != nil || item.Metadata == nil || item.Sections["diff_manifest"].ContentComplete == readmeta.ContentCompleteTrue {
			t.Fatalf("mint drop=%+v", item)
		}
	})
	t.Run("foreign dm1 does no http", func(t *testing.T) {
		log := &pathLog{}
		d := newReviewDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log.add(r.URL.Path, r.URL.RawQuery)
			http.NotFound(w, r)
		}))
		now := d.now()
		upper := now.Format(time.RFC3339)
		iid := int64(1)
		sum := strings.Repeat("ab", 32)
		tok, err := cursor.Encode(d.Config.CursorKey, cursor.Payload{
			SchemaVersion: cursor.SchemaV1, Instance: "https://gitlab.example/api/v4", ActorID: 7, PolicyFP: d.Config.PolicyFingerprint(),
			Tool: cursor.ToolDiffWindow, Section: cursor.SectionDiffManifest,
			Scope:   cursor.Scope{Kind: cursor.ScopeProject, ProjectID: "42", MergeRequestIID: &iid},
			Filters: cursor.Filters{PerPage: 1, Selection: "version:1", Until: upper}, ImmutableRefs: []string{shaN(1)},
			UpperBound: upper, ExpiresAt: now.Add(cursor.DefaultTTL).Format(time.RFC3339),
			PageState:  cursor.PageState{Page: 1, PerPage: 1, ItemsOnPage: 1, LastSHA: sum, SequenceDigest: sum, ProviderNextPage: 2},
			DiffWindow: &cursor.DiffWindowCont{V: cursor.DiffWindowSchemaDM1, Mode: diffModeVersion, VersionID: 1, Total: 2, Offset: 1, FullDigest: sum, PerPage: 1},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = callReviewContext(t, d, nil, map[string]any{"items": []any{itemArg("42", 1, []any{"diff_manifest"}, map[string]any{"cursors": []any{map[string]any{"section": "diff_manifest", "cursor": tok}}})}}, false)
		if err == nil || !strings.Contains(err.Error(), cursor.ResyncRequired) || log.snapshot() != 0 {
			t.Fatalf("foreign err=%v paths=%v", err, log.paths)
		}
	})
	t.Run("body bracket mismatch", func(t *testing.T) {
		wrong := shaN(99)
		body := versionObject(9, 5001, head, wrong, start, "collected", "1", oneDiff("m.txt", "patch"))
		d := newReviewDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/user"):
				_, _ = io.WriteString(w, `{"id":7}`)
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
				if r.URL.Query().Get("per_page") != "20" {
					http.Error(w, "per_page", http.StatusBadRequest)
					return
				}
				w.Header().Set("X-Next-Page", "")
				fmt.Fprintf(w, `[{"id":9,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, head, base, start)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				fmt.Fprintf(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42,"target_project_id":42,"source_branch":"feature-1","target_branch":"main","sha":%q,"diff_refs":{"head_sha":%q,"base_sha":%q,"start_sha":%q}}`, head, head, base, start)
			case strings.Contains(r.URL.Path, "/projects/"):
				fmt.Fprintf(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":1,"kind":"group"}}`)
			default:
				http.NotFound(w, r)
			}
		}))
		out, err := callReviewDirect(t, d, igl.WithBudget(context.Background(), reviewBudget(128)), []reviewContextItemIn{metaItem("42", 1, "metadata", "diff_manifest")})
		if err != nil {
			t.Fatal(err)
		}
		item := out.Items[0]
		if item.ReviewClean || item.Metadata == nil || item.Sections["diff_manifest"].ContentComplete == readmeta.ContentCompleteTrue {
			t.Fatalf("bracket body mismatch item=%+v", item)
		}
		if item.ContextRef != nil {
			decoded, err := cursor.Decode(d.Config.CursorKey, *item.ContextRef, d.now())
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range decoded.ContextRef.Complete {
				if name == "diff_manifest" {
					t.Fatal("mismatched body stayed complete")
				}
			}
			if _, ok := decoded.ContextRef.Evidence["diff_manifest"]; ok {
				t.Fatal("mismatched body minted evidence")
			}
		}
	})
}

func TestDiffWindow_identityAndRefs(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	t.Run("stable head changed base", func(t *testing.T) {
		var n int
		log := &pathLog{}
		h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				n++
				bsha := base
				if n%2 == 0 {
					bsha = shaN(9)
				}
				_, _ = io.WriteString(w, versionObject(1, 5001, head, bsha, start, "collected", "1", oneDiff("a.txt", "p")))
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
		if err != nil {
			t.Fatal(err)
		}
		if sectionMap(out)["content_complete"] == readmeta.ContentCompleteTrue || len(asSlice(t, out["entries"])) != 0 || out["digest"] != nil {
			t.Fatalf("closing identity drift accepted %#v", out)
		}
	})
	t.Run("tuple body mismatch", func(t *testing.T) {
		log := &pathLog{}
		h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/7"):
				_, _ = io.WriteString(w, versionObject(7, 5001, head, shaN(9), start, "collected", "1", oneDiff("tuple.txt", "p")))
			case strings.Contains(r.URL.Path, "/versions"):
				w.Header().Set("X-Next-Page", "")
				fmt.Fprintf(w, `[{"id":7,"merge_request_id":5001,"head_commit_sha":%q,"base_commit_sha":%q,"start_commit_sha":%q}]`, head, base, start)
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		out, err := callDiffWindow(t, diffDeps(t, h), nil, map[string]any{
			"project_id": "42", "merge_request_iid": 1, "base_sha": base, "start_sha": start, "head_sha": head, "per_page": 20,
		})
		if err != nil {
			t.Fatal(err)
		}
		if log.count("/versions/7") < 1 || sectionMap(out)["content_complete"] == readmeta.ContentCompleteTrue || len(asSlice(t, out["entries"])) != 0 {
			t.Fatalf("tuple body mismatch paths=%v out=%#v", log.paths, out)
		}
	})
	t.Run("resume refs stable digest", func(t *testing.T) {
		var n int
		h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.Contains(r.URL.Path, "/versions/1"):
				n++
				bsha := base
				if n > 2 {
					bsha = shaN(9)
				}
				diffs := `[{"old_path":"a","new_path":"a","diff":"p"},{"old_path":"b","new_path":"b","diff":"p"}]`
				_, _ = io.WriteString(w, versionObject(1, 5001, head, bsha, start, "collected", "2", diffs))
			case strings.Contains(r.URL.Path, "/merge_requests/"):
				_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
			default:
				http.NotFound(w, r)
			}
		})
		d := diffDeps(t, h)
		out, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 1})
		if err != nil {
			t.Fatal(err)
		}
		cur, _ := sectionMap(out)["next_cursor"].(string)
		if cur == "" || out["digest"] == nil {
			t.Fatal("expected a proved partial window")
		}
		decoded, err := cursor.Decode(d.Config.CursorKey, cur, d.now())
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(decoded.ImmutableRefs, ","), base) {
			t.Fatalf("refs=%v", decoded.ImmutableRefs)
		}
		shift, err := callDiffWindow(t, d, nil, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 1, "cursor": cur})
		if err != nil {
			t.Fatal(err)
		}
		if len(asSlice(t, shift["entries"])) != 0 || sectionMap(shift)["content_complete"] == readmeta.ContentCompleteTrue || sectionMap(shift)["next_cursor"] != nil {
			t.Fatalf("changed refs resumed %#v", shift)
		}
	})
}

func TestDiffWindow_streamStopOmitsPatch(t *testing.T) {
	const secret = "SECRET-PATCH-VALUE"
	cancelled := make(chan struct{})
	var once sync.Once
	log := &pathLog{}
	h := serveDiffBase(log, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			flusher, _ := w.(http.Flusher)
			_, _ = io.WriteString(w, `{"id":1,"merge_request_id":5001,"head_commit_sha":"`+shaN(1)+`","base_commit_sha":"`+shaN(2)+`","start_commit_sha":"`+shaN(3)+`","state":"collected","real_size":"2","diffs":[`)
			_, _ = io.WriteString(w, oneDiff("a.txt", secret)[1:len(oneDiff("a.txt", secret))-1])
			_, _ = io.WriteString(w, `,`)
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = io.WriteString(w, oneDiff("b.txt", secret)[1:len(oneDiff("b.txt", secret))-1])
			if flusher != nil {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				once.Do(func() { close(cancelled) })
			case <-time.After(3 * time.Second):
			}
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	b := igl.DefaultBudget()
	b.MaxItems = 1
	b.MaxRequests = 64
	ctx := igl.WithBudget(context.Background(), b)
	sibling, stopSibling := context.WithCancel(ctx)
	defer stopSibling()
	_, err := callDiffWindow(t, d, ctx, map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20})
	if err == nil || !strings.Contains(err.Error(), igl.ErrBudgetItems.Error()) {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("patch leaked into the error")
	}
	select {
	case <-ctx.Done():
		t.Fatal("borrowed budget context cancelled")
	default:
	}
	if sibling.Err() != nil {
		t.Fatal("sibling cancelled")
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("owned request context was not cancelled")
	}
	if _, _, items := b.Stats(); items != 1 {
		t.Fatalf("items=%d", items)
	}
}

func TestDiffWindow_borrowedElapsedKeepsBudget(t *testing.T) {
	head, base, start := shaN(1), shaN(2), shaN(3)
	body := versionObject(1, 5001, head, base, start, "collected", "1", oneDiff("a.txt", "p"))
	h := serveDiffBase(&pathLog{}, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/versions/1"):
			_, _ = io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/merge_requests/"):
			_, _ = io.WriteString(w, `{"id":5001,"iid":1,"project_id":42,"source_project_id":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	d := diffDeps(t, h)
	b := igl.DefaultBudget()
	b.MaxElapsed = time.Hour
	b.MaxItems = 50
	b.MaxRequests = 64
	if err := b.AddItem(); err != nil {
		t.Fatal(err)
	}
	ctx := igl.WithBudget(context.Background(), b)
	early, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	deadline, ok := early.Deadline()
	if !ok {
		t.Fatal("missing deadline")
	}
	sibling, stopSibling := context.WithCancel(early)
	defer stopSibling()
	args := map[string]any{"project_id": "42", "merge_request_iid": 1, "diff_version_id": 1, "per_page": 20, "max_elapsed_ms": int64(1)}
	_, err := callDiffWindow(t, d, early, args)
	if err == nil {
		t.Fatal("1ms child deadline succeeded")
	}
	select {
	case <-early.Done():
		t.Fatal("earlier parent deadline cancelled")
	default:
	}
	if sibling.Err() != nil {
		t.Fatal("sibling cancelled")
	}
	if igl.BudgetFromContext(early) != b || b.MaxElapsed != time.Hour {
		t.Fatal("borrowed budget pointer or MaxElapsed changed")
	}
	if _, _, items := b.Stats(); items < 1 {
		t.Fatal("counters reset")
	}
	got, ok := early.Deadline()
	if !ok || !got.Equal(deadline) {
		t.Fatal("earlier deadline changed")
	}
}

func asSlice(t *testing.T, v any) []any {
	t.Helper()
	s, _ := v.([]any)
	return s
}
