package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const batchTipSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type batchProbe struct {
	rawHits     atomic.Int64
	commitHits  atomic.Int64
	projectHits atomic.Int64
	lastRawURL  atomic.Value
}

func batchDeps(t *testing.T, h http.Handler, cfg *config.Config) Deps {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	if cfg == nil {
		cfg = &config.Config{}
	}
	cfg.Token = "t"
	cfg.APIURL = ts.URL + "/api/v4"
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(cfg.APIURL),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(&http.Client{
			Transport:     igl.BudgetInterceptor()(http.DefaultTransport),
			CheckRedirect: igl.SafeCheckRedirectForTest,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: cfg, Client: cli}
}

func batchFixture(t *testing.T, probe *batchProbe, mutate func(w http.ResponseWriter, r *http.Request) bool) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mutate != nil && mutate(w, r) {
			return
		}
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/projects/42") || strings.HasSuffix(path, "/projects/99"):
			if probe != nil {
				probe.projectHits.Add(1)
			}
			id := "42"
			if strings.HasSuffix(path, "/99") {
				id = "99"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%s,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`, id))
		case strings.Contains(path, "/repository/commits/") && !strings.Contains(path, "/diff"):
			if probe != nil {
				probe.commitHits.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%q,"short_id":"aaaa","title":"t"}`, batchTipSHA))
		case strings.Contains(path, "/repository/files/") && strings.HasSuffix(path, "/raw"):
			if probe != nil {
				probe.rawHits.Add(1)
				probe.lastRawURL.Store(r.URL.String())
			}
			name := "a.txt"
			if strings.Contains(path, "missing") {
				http.NotFound(w, r)
				return
			}
			if strings.Contains(path, "bin") {
				body := []byte{0x00, 0x01, 0xff, 'x'}
				sum := sha256.Sum256(body)
				w.Header().Set("X-Gitlab-Blob-Id", strings.Repeat("11", 20))
				w.Header().Set("X-Gitlab-Content-Sha256", hex.EncodeToString(sum[:]))
				w.Header().Set("X-Gitlab-Size", fmt.Sprintf("%d", len(body)))
				if rng := r.Header.Get("Range"); rng != "" {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
					w.WriteHeader(http.StatusPartialContent)
				}
				_, _ = w.Write(body)
				return
			}
			body := "hello-" + name
			if strings.Contains(path, "b.txt") {
				body = "world-b"
			}
			sum := sha256.Sum256([]byte(body))
			w.Header().Set("X-Gitlab-Blob-Id", strings.Repeat("22", 20))
			w.Header().Set("X-Gitlab-Content-Sha256", hex.EncodeToString(sum[:]))
			w.Header().Set("X-Gitlab-Size", fmt.Sprintf("%d", len(body)))
			if rng := r.Header.Get("Range"); strings.HasPrefix(rng, "bytes=") {
				// Honor small ranges for a.txt
				w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-4/%d", len(body)))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.WriteString(w, body[:5])
				return
			}
			_, _ = io.WriteString(w, body)
		default:
			http.NotFound(w, r)
		}
	})
}

func callBatch(t *testing.T, d Deps, in batchGetFileContentsIn) (map[string]any, error) {
	t.Helper()
	_, out, err := batchGetFileContents(context.Background(), nil, in, d)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m, nil
}

func TestBatchGetFileContents_AC1_rejectFloatingAndOverCapPreBlob(t *testing.T) {
	probe := &batchProbe{}
	d := batchDeps(t, batchFixture(t, probe, nil), nil)
	_, err := callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: "main", Paths: []batchPathSpec{{Path: "a.txt"}},
	})
	if err == nil || !strings.Contains(err.Error(), "40-hex") {
		t.Fatalf("want reject floating ref, got %v", err)
	}
	if probe.rawHits.Load() != 0 || probe.commitHits.Load() != 0 {
		t.Fatal("must not transport before validation")
	}

	paths := make([]batchPathSpec, 21)
	for i := range paths {
		paths[i] = batchPathSpec{Path: fmt.Sprintf("f%d.txt", i)}
	}
	_, err = callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: batchTipSHA, Paths: paths,
	})
	if err == nil || !strings.Contains(err.Error(), "1..20") {
		t.Fatalf("want >20 reject, got %v", err)
	}
	if probe.rawHits.Load() != 0 {
		t.Fatal("over-cap must be pre-transport")
	}

	over := int64(batchHardMaxReturnedBytes + 1)
	_, err = callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: batchTipSHA, Paths: []batchPathSpec{{Path: "a.txt"}}, MaxBytes: &over,
	})
	if err == nil {
		t.Fatal("want max_bytes over hard cap reject")
	}
}

func TestBatchGetFileContents_AC2_mixedSiblingAndAuthzDeny(t *testing.T) {
	probe := &batchProbe{}
	d := batchDeps(t, batchFixture(t, probe, nil), &config.Config{AllowedProjectIDs: []string{"42"}})
	out, err := callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: batchTipSHA,
		Paths: []batchPathSpec{{Path: "a.txt"}, {Path: "missing.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := out["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items=%d", len(items))
	}
	ok := items[0].(map[string]any)
	bad := items[1].(map[string]any)
	if ok["error"] != nil || ok["content"] == nil {
		t.Fatalf("success sibling: %#v", ok)
	}
	if bad["error"] == nil {
		t.Fatal("missing path must carry item error")
	}

	probe2 := &batchProbe{}
	d2 := batchDeps(t, batchFixture(t, probe2, nil), &config.Config{AllowedProjectIDs: []string{"42"}})
	_, err = callBatch(t, d2, batchGetFileContentsIn{
		ProjectID: "99", CommitSHA: batchTipSHA, Paths: []batchPathSpec{{Path: "a.txt"}},
	})
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("want authz_denied, got %v", err)
	}
	if probe2.rawHits.Load() != 0 || probe2.commitHits.Load() != 0 {
		t.Fatal("denied repo must not fetch commit/blobs")
	}
}

func TestBatchGetFileContents_AC3_rangeHashEncoding(t *testing.T) {
	d := batchDeps(t, batchFixture(t, &batchProbe{}, nil), nil)
	end := int64(5)
	out, err := callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: batchTipSHA,
		Paths: []batchPathSpec{
			{Path: "a.txt", Range: &batchByteRange{StartByte: 0, EndByte: &end}},
			{Path: "bin.dat"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := out["items"].([]any)
	text := items[0].(map[string]any)
	bin := items[1].(map[string]any)
	if text["content_encoding"] != "utf-8" || text["window_complete"] != true {
		t.Fatalf("text item: %#v", text)
	}
	if bin["content_encoding"] != "base64" || bin["is_binary"] != true {
		t.Fatalf("bin item: %#v", bin)
	}
	if text["returned_range_hash"] == nil || text["full_content_digest"] == nil {
		t.Fatal("expected hashes")
	}
	sec := out["section"].(map[string]any)
	if sec["capability_version"] != capabilityBatchFilesV1 || sec["next_cursor"] != nil {
		t.Fatalf("section=%#v", sec)
	}
	if sec["pagination_exhausted"] != true {
		t.Fatal("fixed list must set pagination_exhausted")
	}
}

func TestBatchGetFileContents_AC4_cancelStopsRemaining(t *testing.T) {
	started := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/projects/42") {
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
			return
		}
		if strings.Contains(path, "/commits/") {
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%q}`, batchTipSHA))
			return
		}
		if strings.Contains(path, "/raw") {
			close(started)
			<-r.Context().Done()
			return
		}
		http.NotFound(w, r)
	})
	d := batchDeps(t, h, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := batchGetFileContents(ctx, nil, batchGetFileContentsIn{
			ProjectID: "42", CommitSHA: batchTipSHA,
			Paths: []batchPathSpec{{Path: "a.txt"}, {Path: "b.txt"}},
		}, d)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("raw not started")
	}
	cancel()
	// Handler returns after cancel; tool should complete with cancellation semantics.
	select {
	case <-errCh:
	case <-time.After(3 * time.Second):
		t.Fatal("batch did not finish after cancel")
	}
}

func TestBatchGetFileContents_AC5_legacyGetFileContentsUnchanged(t *testing.T) {
	var rawHits atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/projects/42") {
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
			return
		}
		if strings.Contains(r.URL.Path, "/raw") {
			rawHits.Add(1)
			_, _ = io.WriteString(w, "legacy-bytes")
			return
		}
		http.NotFound(w, r)
	})
	d := batchDeps(t, h, nil)
	_, out, err := getFileContents(context.Background(), nil, getFileContentsIn{
		ProjectID: "42", FilePath: "a.txt", Ref: "main",
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := Out(out).(map[string]any)
	if m["content"] != "legacy-bytes" || m["ref"] != "main" {
		t.Fatalf("%#v", m)
	}
	if rawHits.Load() != 1 {
		t.Fatal("legacy path must still use GetRawFile once")
	}
}

func TestBatchGetFileContents_SHAMismatchPreBlob(t *testing.T) {
	probe := &batchProbe{}
	d := batchDeps(t, batchFixture(t, probe, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.Contains(r.URL.Path, "/commits/") {
			probe.commitHits.Add(1)
			_, _ = io.WriteString(w, `{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)
			return true
		}
		return false
	}), nil)
	_, err := callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: batchTipSHA, Paths: []batchPathSpec{{Path: "a.txt"}},
	})
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("got %v", err)
	}
	if probe.rawHits.Load() != 0 {
		t.Fatal("SHA mismatch must not blob-fetch")
	}
}

func TestBatchGetFileContents_traversalAndDuplicateRejected(t *testing.T) {
	d := batchDeps(t, batchFixture(t, &batchProbe{}, nil), nil)
	_, err := callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: batchTipSHA,
		Paths: []batchPathSpec{{Path: "../x"}},
	})
	if err == nil {
		t.Fatal("traversal")
	}
	_, err = callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: batchTipSHA,
		Paths: []batchPathSpec{{Path: "a.txt"}, {Path: "a.txt"}},
	})
	if err == nil {
		t.Fatal("duplicate")
	}
}

func TestBatchGetFileContents_MCPRegistrationReadOnlyAndSchema(t *testing.T) {
	d := batchDeps(t, batchFixture(t, &batchProbe{}, nil), &config.Config{ToolProfile: "review_read", Token: "t"})
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterAll(srv, d)
	cs := testutil.MCPConnect(t, srv)
	listed, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	var found *mcp.Tool
	for _, tl := range listed.Tools {
		if tl.Name == "batch_get_file_contents" {
			found = tl
			break
		}
	}
	if found == nil {
		t.Fatal("batch_get_file_contents not registered in review_read")
	}
	if found.Annotations == nil || !found.Annotations.ReadOnlyHint {
		t.Fatalf("must be read-only annotated: %#v", found.Annotations)
	}
	// Daily must not include it — confirm daily unchanged length.
	d2 := batchDeps(t, batchFixture(t, &batchProbe{}, nil), &config.Config{ToolProfile: "daily", Token: "t"})
	srv2 := mcp.NewServer(&mcp.Implementation{Name: "t2", Version: "t"}, nil)
	RegisterAll(srv2, d2)
	cs2 := testutil.MCPConnect(t, srv2)
	listed2, err := cs2.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed2.Tools) != len(DailyTools()) {
		t.Fatalf("daily count=%d want %d", len(listed2.Tools), len(DailyTools()))
	}
	for _, tl := range listed2.Tools {
		if tl.Name == "batch_get_file_contents" {
			t.Fatal("daily catalog must remain unchanged (no batch tool)")
		}
	}
}

func TestBatchGetFileContents_upstreamBudgetPreserved(t *testing.T) {
	probe := &batchProbe{}
	d := batchDeps(t, batchFixture(t, probe, nil), nil)
	b := igl.DefaultBudget()
	b.MaxRequests = 3 // tight: authz+commit+1 raw
	b.MaxItems = 50
	ctx := igl.WithBudget(context.Background(), b)
	_, out, err := batchGetFileContents(ctx, nil, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: batchTipSHA,
		Paths: []batchPathSpec{{Path: "a.txt"}, {Path: "b.txt"}},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if got := igl.BudgetFromContext(ctx); got != b {
		t.Fatalf("ensureBatchBudget must reuse upstream budget object: got %p want %p", got, b)
	}
	raw, _ := json.Marshal(out)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	items := m["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items=%d", len(items))
	}
	// Upstream MaxRequests=3: identity+commit+1 raw; 4th charge fails closed.
	// chargeRequest increments then rejects, so Stats.requests may be 4.
	reqs, _, _ := b.Stats()
	if reqs < 3 || reqs > 4 {
		t.Fatalf("requests=%d want 3..4 under tight upstream cap", reqs)
	}
	second := items[1].(map[string]any)
	if second["error"] == nil && second["visited"] != false {
		t.Fatalf("second path must be budget-stopped/unvisited, got %#v", second)
	}
	if probe.rawHits.Load() > 1 {
		t.Fatalf("rawHits=%d want ≤1 under MaxRequests=3", probe.rawHits.Load())
	}
}

func TestBatchGetFileContents_redirectCannotChangeImmutableRef(t *testing.T) {
	testBatchRedirectRejectsQuery(t, "ref=main")
}

func TestBatchGetFileContents_redirectRejectsMalformedExtraQuery(t *testing.T) {
	const sha = "1234567890abcdef1234567890abcdef12345678"
	testBatchRedirectRejectsQuery(t, "ref="+sha+"&unexpected=value;bad")
}

func testBatchRedirectRejectsQuery(t *testing.T, redirectQuery string) {
	t.Helper()
	const sha = "1234567890abcdef1234567890abcdef12345678"
	var pinned, floating atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/raw"):
			if r.URL.RawQuery == "ref="+sha {
				pinned.Add(1)
				u := *r.URL
				u.RawQuery = redirectQuery
				http.Redirect(w, r, u.String(), http.StatusFound)
				return
			}
			floating.Add(1)
			w.Header().Set("X-Gitlab-Blob-Id", strings.Repeat("1", 40))
			w.Header().Set("X-Gitlab-Content-Sha256", strings.Repeat("2", 64))
			w.Header().Set("X-Gitlab-Size", "7")
			_, _ = io.WriteString(w, "MOVING!")
		case strings.Contains(r.URL.Path, "/repository/commits/"):
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%q}`, sha))
		case strings.HasSuffix(r.URL.Path, "/projects/42"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})

	assertRejectedNoLeak := func(t *testing.T, item map[string]any) {
		t.Helper()
		if item["content"] == "MOVING!" || item["error"] == nil {
			t.Fatalf("must reject: %#v", item)
		}
		errObj := item["error"].(map[string]any)
		if errObj["code"] != readmeta.CodeInconsistent {
			t.Fatalf("code=%v want inconsistent", errObj["code"])
		}
		for _, k := range []string{"blob_id", "full_content_digest", "observed_total_size", "returned_range_hash", "observed_start_byte", "observed_end_byte"} {
			if item[k] != nil {
				t.Fatalf("unattested %s must be unknown: %#v", k, item)
			}
		}
		if item["full_content_complete"] == true || item["window_complete"] == true {
			t.Fatalf("cannot attest completeness: %#v", item)
		}
	}

	// Production-shaped client: CheckRedirect refuses changed-ref hops.
	d := batchDeps(t, h, nil)
	out, err := callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: sha, Paths: []batchPathSpec{{Path: "f.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Load() != 1 {
		t.Fatalf("pinned=%d", pinned.Load())
	}
	if floating.Load() != 0 {
		t.Fatalf("production CheckRedirect must not follow unauthorized hop, floating=%d", floating.Load())
	}
	item := out["items"].([]any)[0].(map[string]any)
	assertRejectedNoLeak(t, item)
	sec := out["section"].(map[string]any)
	if sec["consistency"] == "consistent" || sec["content_complete"] == true || sec["content_complete"] == "true" {
		t.Fatalf("section must not attest complete/consistent: %#v", sec)
	}

	// Coordinator-shaped client: default CheckRedirect follows; post-validate must drop.
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("synthetic",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	pinned.Store(0)
	floating.Store(0)
	out2, err := callBatch(t, Deps{Client: cli, Config: &config.Config{Token: "synthetic"}}, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: sha, Paths: []batchPathSpec{{Path: "f.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if floating.Load() != 1 {
		t.Fatalf("follow client must observe floating once, floating=%d", floating.Load())
	}
	assertRejectedNoLeak(t, out2["items"].([]any)[0].(map[string]any))
}

func TestBatchGetFileContents_nestedPinnedPathSucceeds(t *testing.T) {
	const sha = "1234567890abcdef1234567890abcdef12345678"
	var raw atomic.Int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/raw"):
			raw.Add(1)
			if r.URL.Query().Get("ref") != sha {
				http.Error(w, "wrong ref", 400)
				return
			}
			// Coordinator-style decoded Path check plus PathEscape wire form.
			if !strings.HasSuffix(r.URL.Path, "/files/dir/a.txt/raw") &&
				!strings.Contains(r.URL.EscapedPath(), "/files/dir%2Fa%2Etxt/raw") {
				http.Error(w, "wrong bound identity", 400)
				return
			}
			w.Header().Set("X-Gitlab-Size", "7")
			_, _ = io.WriteString(w, "PINNED!")
		case strings.Contains(r.URL.Path, "/repository/commits/"):
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%q}`, sha))
		case strings.HasSuffix(r.URL.Path, "/projects/42"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		default:
			http.NotFound(w, r)
		}
	})

	// Coordinator-shaped client (no custom CheckRedirect).
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("synthetic",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(igl.BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	out, err := callBatch(t, Deps{Client: cli, Config: &config.Config{Token: "synthetic"}}, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: sha, Paths: []batchPathSpec{{Path: "dir/a.txt"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw.Load() != 1 {
		t.Fatalf("raw=%d", raw.Load())
	}
	item := out["items"].([]any)[0].(map[string]any)
	if item["error"] != nil || item["content"] != "PINNED!" || item["full_content_complete"] != true {
		t.Fatalf("valid nested immutable path must succeed: %#v", item)
	}
}

func TestBatchGetFileContents_F1_retained206WindowEqualsBytes(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/projects/42"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		case strings.Contains(r.URL.Path, "/commits/"):
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%q}`, batchTipSHA))
		case strings.HasSuffix(r.URL.Path, "/raw"):
			w.Header().Set("Content-Range", "bytes 10-1009/2000")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, strings.Repeat("A", 1000))
		default:
			http.NotFound(w, r)
		}
	})
	d := batchDeps(t, h, nil)
	end := int64(1010)
	maxB := int64(100)
	out, err := callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: batchTipSHA, MaxBytes: &maxB,
		Paths: []batchPathSpec{{Path: "a.txt", Range: &batchByteRange{StartByte: 10, EndByte: &end}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	item := out["items"].([]any)[0].(map[string]any)
	if item["error"] != nil {
		t.Fatalf("local cap truncation is not framing error: %#v", item)
	}
	if item["observed_start_byte"] != float64(10) || item["observed_end_byte"] != float64(110) || item["bytes_returned"] != float64(100) {
		t.Fatalf("retained interval must be [10,110): %#v", item)
	}
	if item["window_complete"] != false || item["full_content_complete"] != false {
		t.Fatalf("incomplete: %#v", item)
	}
	if item["returned_range_hash"] == nil {
		t.Fatal("hash must cover retained bytes")
	}
	sum := sha256.Sum256([]byte(strings.Repeat("A", 100)))
	if item["returned_range_hash"] != hex.EncodeToString(sum[:]) {
		t.Fatalf("hash mismatch: %v", item["returned_range_hash"])
	}
}

func TestBatchGetFileContents_F1_siblingConsumesAggregateThenTruncates(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/projects/42"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
		case strings.Contains(r.URL.Path, "/commits/"):
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%q}`, batchTipSHA))
		case strings.Contains(r.URL.Path, "/files/") && strings.HasSuffix(r.URL.Path, "/raw"):
			if strings.Contains(r.URL.EscapedPath(), "a%2Etxt") || strings.HasSuffix(r.URL.Path, "/a.txt/raw") {
				body := strings.Repeat("X", 80)
				w.Header().Set("X-Gitlab-Size", fmt.Sprintf("%d", len(body)))
				_, _ = io.WriteString(w, body)
				return
			}
			w.Header().Set("Content-Range", "bytes 0-499/500")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, strings.Repeat("Y", 500))
		default:
			http.NotFound(w, r)
		}
	})
	d := batchDeps(t, h, nil)
	maxB := int64(100)
	end := int64(500)
	out, err := callBatch(t, d, batchGetFileContentsIn{
		ProjectID: "42", CommitSHA: batchTipSHA, MaxBytes: &maxB,
		Paths: []batchPathSpec{
			{Path: "a.txt"},
			{Path: "b.txt", Range: &batchByteRange{StartByte: 0, EndByte: &end}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	items := out["items"].([]any)
	first := items[0].(map[string]any)
	second := items[1].(map[string]any)
	if first["bytes_returned"] != float64(80) {
		t.Fatalf("first sibling bytes=%v", first["bytes_returned"])
	}
	// Remaining aggregate return budget is 20.
	if second["bytes_returned"] != float64(20) || second["observed_end_byte"] != float64(20) {
		t.Fatalf("second retained must be 20 bytes [0,20): %#v", second)
	}
	if second["window_complete"] != false || second["error"] != nil {
		t.Fatalf("second truncated without framing error: %#v", second)
	}
}

func TestBatchGetFileContents_F3_utf8ValidityIndependentOfNULBinary(t *testing.T) {
	cases := []struct {
		name    string
		body    []byte
		utf8ok  bool
		binary  bool
		enc     string
		content string // exact for small cases; empty to skip
	}{
		{name: "plain", body: []byte("hello"), utf8ok: true, binary: false, enc: "utf-8", content: "hello"},
		{name: "nul_valid_utf8", body: []byte{'a', 0, 'b'}, utf8ok: true, binary: true, enc: "base64", content: "YQBi"},
		{name: "invalid_non_nul", body: []byte{0xff, 0xfe}, utf8ok: false, binary: true, enc: "base64"},
		{name: "empty", body: []byte{}, utf8ok: true, binary: false, enc: "utf-8", content: ""},
		{name: "multibyte_split", body: []byte{0xe2, 0x82}, utf8ok: false, binary: true, enc: "base64"}, // split €
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/projects/42"):
					_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group"}}`)
				case strings.Contains(r.URL.Path, "/commits/"):
					_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%q}`, batchTipSHA))
				case strings.HasSuffix(r.URL.Path, "/raw"):
					w.Header().Set("X-Gitlab-Size", fmt.Sprintf("%d", len(tc.body)))
					_, _ = w.Write(tc.body)
				default:
					http.NotFound(w, r)
				}
			})
			d := batchDeps(t, h, nil)
			out, err := callBatch(t, d, batchGetFileContentsIn{
				ProjectID: "42", CommitSHA: batchTipSHA, Paths: []batchPathSpec{{Path: "x.bin"}},
			})
			if err != nil {
				t.Fatal(err)
			}
			item := out["items"].([]any)[0].(map[string]any)
			if item["utf8_valid"] != tc.utf8ok || item["is_binary"] != tc.binary || item["content_encoding"] != tc.enc {
				t.Fatalf("got utf8=%v bin=%v enc=%v want utf8=%v bin=%v enc=%v item=%#v",
					item["utf8_valid"], item["is_binary"], item["content_encoding"], tc.utf8ok, tc.binary, tc.enc, item)
			}
			if tc.name == "empty" {
				if item["content"] != nil && item["content"] != "" {
					t.Fatalf("empty content must be omitted/empty, got %#v", item["content"])
				}
			} else if tc.content != "" && item["content"] != tc.content {
				t.Fatalf("content=%q want %q", item["content"], tc.content)
			}
		})
	}
}
