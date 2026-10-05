package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func decodeTrace(t *testing.T, raw any) map[string]any {
	t.Helper()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func traceWindow(t *testing.T, raw any) map[string]any {
	t.Helper()
	m := decodeTrace(t, raw)
	w, _ := m["window"].(map[string]any)
	if w == nil {
		t.Fatalf("window %#v", m["window"])
	}
	return w
}

func traceSection(t *testing.T, raw any) map[string]any {
	t.Helper()
	m := decodeTrace(t, raw)
	s, _ := m["section"].(map[string]any)
	if s == nil {
		t.Fatalf("section %#v", m["section"])
	}
	return s
}

func TestGetPipelineJobOutput_prefixRedactsSplitSecret(t *testing.T) {
	secret := "glpat-" + strings.Repeat("d", 20)
	var traceHits atomic.Int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			traceHits.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "alpha\n"+secret[:8])
			_, _ = io.WriteString(w, secret[8:]+"\nomega\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	d.Config.Token = "configured-token-value"
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 3, TruncateLines: 10,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeTrace(t, raw)
	text, _ := m["trace"].(string)
	if strings.Contains(text, secret) || strings.Contains(text, "glpat-") {
		t.Fatalf("secret leaked %q", text)
	}
	if !strings.Contains(text, "alpha") || !strings.Contains(text, redactPlaceholder) {
		t.Fatalf("trace %q", text)
	}
	w := traceWindow(t, raw)
	if w["redaction_count"].(float64) < 1 {
		t.Fatalf("window %#v", w)
	}
	if w["total_known"] != true {
		t.Fatalf("total %#v", w)
	}
	sec := traceSection(t, raw)
	if sec["capability_version"] != jobTraceCapability || sec["head_sha"] != nil {
		t.Fatalf("section %#v", sec)
	}
	if sec["content_complete"] != readmeta.ContentCompleteTrue {
		t.Fatalf("complete %#v", sec["content_complete"])
	}
	if traceHits.Load() != 1 {
		t.Fatalf("hits %d", traceHits.Load())
	}
}

func TestGetPipelineJobOutput_authzBeforeTrace(t *testing.T) {
	var order []string
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/trace") {
			t.Errorf("trace fetched on deny: %s", r.URL.Path)
			return
		}
		id := echoNumericID(r.URL.Path, 99)
		_, _ = io.WriteString(w, `{"id":`+itoa(id)+`,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group","full_path":"g","parent_id":0}}`)
	}))
	d.Config.AllowedProjectIDs = []string{"42"}
	_, _, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{ProjectID: "99", JobID: 1}, d)
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeAuthzDenied) {
		t.Fatalf("err %v", err)
	}
	for _, p := range order {
		if strings.Contains(p, "/trace") {
			t.Fatalf("trace path %v", order)
		}
	}
}

func TestGetPipelineJobOutput_tailFittingAndUnproven(t *testing.T) {
	body := "one\ntwo\nTHREE\n"
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		if r.Header.Get("Range") == "" {
			t.Errorf("tail should request Range")
		}
		w.Header().Set("Content-Range", "bytes 0-13/14")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, body)
	}))
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 4, Selector: "tail", MaxLines: 1,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, raw)["trace"].(string)
	if text != "THREE\n" && text != "THREE" {
		t.Fatalf("tail %q", text)
	}
	w := traceWindow(t, raw)
	if w["tail_proven"] != true {
		t.Fatalf("window %#v", w)
	}

	var served atomic.Int64
	d2 := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "IGNORED")
			return
		}
		// Bounded fallback. Stop once the client closes.
		fl, _ := w.(http.Flusher)
		buf := make([]byte, 1024)
		for i := 0; i < 5_000; i++ {
			n, err := w.Write(buf)
			served.Add(int64(n))
			if fl != nil {
				fl.Flush()
			}
			if err != nil {
				return
			}
		}
	}))
	_, raw, err = getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 4, Selector: "tail", MaxScanBytes: 4096,
	}, d2)
	if err != nil {
		t.Fatal(err)
	}
	text = decodeTrace(t, raw)["trace"].(string)
	w = traceWindow(t, raw)
	if text != "" || w["tail_proven"] != false {
		t.Fatalf("fabricated tail %q window %#v", text, w)
	}
	sec := traceSection(t, raw)
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("complete %#v", sec)
	}
	if served.Load() > 128<<10 {
		t.Fatalf("served %d", served.Load())
	}
}

func TestGetPipelineJobOutput_errorRegionAndRange(t *testing.T) {
	body := "ok\nfine\nERROR: boom\nmore\nlast\n"
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 5, Selector: "error",
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, raw)["trace"].(string)
	if !strings.Contains(text, "ERROR: boom") || !strings.Contains(text, "fine") {
		t.Fatalf("region %q", text)
	}
	if traceWindow(t, raw)["error_region_proven"] != true {
		t.Fatalf("%#v", traceWindow(t, raw))
	}

	start := int64(0)
	end := int64(2)
	d2 := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		if r.Header.Get("Range") == "" {
			t.Errorf("missing Range")
		}
		w.Header().Set("Content-Range", "bytes 0-1/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "ab")
	}))
	_, raw, err = getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 5, Selector: "range", StartByte: &start, EndByte: &end,
	}, d2)
	if err != nil {
		t.Fatal(err)
	}
	if decodeTrace(t, raw)["trace"] != "ab" {
		t.Fatalf("%#v", decodeTrace(t, raw))
	}
	w := traceWindow(t, raw)
	if w["total_known"] != true || w["output_bytes"].(float64) != 2 {
		t.Fatalf("%#v", w)
	}
	if w["source_end_exclusive"].(float64) == w["output_bytes"].(float64) && w["redaction_count"].(float64) != 0 {
		t.Fatal("expected distinct semantics")
	}

	start = 50_000
	end = 50_100
	var served atomic.Int64
	d3 := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		// Ignore Range and stream a body longer than the scan cap.
		fl, _ := w.(http.Flusher)
		buf := bytes.Repeat([]byte("Z"), 1024)
		for i := 0; i < 200; i++ {
			n, werr := w.Write(buf)
			served.Add(int64(n))
			if fl != nil {
				fl.Flush()
			}
			if werr != nil {
				return
			}
		}
	}))
	_, raw, err = getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 5, Selector: "range", StartByte: &start, EndByte: &end, MaxScanBytes: 4096,
	}, d3)
	if err != nil {
		t.Fatal(err)
	}
	if decodeTrace(t, raw)["trace"] != "" {
		t.Fatalf("ignored range returned %q", decodeTrace(t, raw)["trace"])
	}
	sec := traceSection(t, raw)
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("complete %v", sec["content_complete"])
	}
	if served.Load() > 128<<10 {
		t.Fatalf("served %d", served.Load())
	}
}

func TestGetPipelineJobOutput_outputShorterThanSourceWhenRedacted(t *testing.T) {
	token := "configured-token-value"
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, "xx "+token+" yy")
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	d.Config = &config.Config{Token: token}
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{ProjectID: "42", JobID: 1}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, raw)["trace"].(string)
	if strings.Contains(text, token) {
		t.Fatal(text)
	}
	w := traceWindow(t, raw)
	span := w["source_end_exclusive"].(float64) - w["source_start"].(float64)
	if w["output_bytes"].(float64) == span || w["redaction_count"].(float64) < 1 {
		t.Fatalf("window %#v text %q", w, text)
	}
}

// shortContentLengthRT returns a 200 trace whose body EOFs before Content-Length.
// The header is not enforced by net/http, which is the case finalizeJobTrace must reject.
type shortContentLengthRT struct{}

func (shortContentLengthRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/trace") {
		h := make(http.Header)
		h.Set("Content-Type", "text/plain")
		h.Set("Content-Length", "4096")
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        h,
			Body:          io.NopCloser(bytes.NewReader([]byte("ok\n"))),
			ContentLength: -1,
			Request:       req,
		}, nil
	}
	body := []byte(`{"id":42}`)
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        http.Header{"Content-Type": []string{"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}, nil
}

func TestGetPipelineJobOutput_shortContentLengthIsNotComplete(t *testing.T) {
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL("https://example.test/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(&http.Client{Transport: igl.BudgetInterceptor()(shortContentLengthRT{})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "t"}, Client: cli}
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 9, Selector: "error", MaxScanBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeTrace(t, raw)
	if m["trace"] != "" {
		t.Fatalf("trace %q", m["trace"])
	}
	w := traceWindow(t, raw)
	if w["error_region_proven"] != false || w["total_known"] != false || w["total_bytes"] != nil {
		t.Fatalf("window %#v", w)
	}
	sec := traceSection(t, raw)
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("complete %#v", sec)
	}
}
