package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

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
		w.Header().Set("Content-Range", "bytes 0-4/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abcde")
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

func TestGetPipelineJobOutput_newlineTerminatedLineCount(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, "one\n")
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 9,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeTrace(t, raw)
	if m["trace"] != "one\n" {
		t.Fatalf("trace %q", m["trace"])
	}
	counts, _ := traceSection(t, raw)["counts"].(map[string]any)
	if counts == nil || int(counts["items"].(float64)) != 1 {
		t.Fatalf("items %#v", counts)
	}

	d2 := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, "one\ntwo")
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	_, raw, err = getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 9,
	}, d2)
	if err != nil {
		t.Fatal(err)
	}
	counts, _ = traceSection(t, raw)["counts"].(map[string]any)
	if counts == nil || int(counts["items"].(float64)) != 2 {
		t.Fatalf("unterminated items %#v", counts)
	}
}

func TestGetPipelineJobOutput_maxLinesKeepsTerminator(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, "one\n")
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 9, MaxLines: 1,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeTrace(t, raw)
	if m["trace"] != "one\n" {
		t.Fatalf("trace %q", m["trace"])
	}
	sec := traceSection(t, raw)
	if sec["content_complete"] != readmeta.ContentCompleteTrue {
		t.Fatalf("complete %#v", sec)
	}
}

func TestGetPipelineJobOutput_shortConfiguredTokenIsRedacted(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, "pre secret post\n")
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	d.Config.Token = "secret"
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 9,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, raw)["trace"].(string)
	if strings.Contains(text, "secret") {
		t.Fatalf("leaked %q", text)
	}
	if !strings.Contains(text, "pre") || !strings.Contains(text, "post") {
		t.Fatalf("trace %q", text)
	}
}

type unexpected206RT struct{}

func (unexpected206RT) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/trace") {
		secret := "lpat-" + strings.Repeat("a", 20)
		h := make(http.Header)
		h.Set("Content-Type", "text/plain")
		h.Set("Content-Range", "bytes 4-28/100")
		return &http.Response{
			StatusCode:    http.StatusPartialContent,
			Header:        h,
			Body:          io.NopCloser(strings.NewReader(secret)),
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

func TestGetPipelineJobOutput_unexpected206DoesNotLeakGlpatSuffix(t *testing.T) {
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL("https://example.test/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(&http.Client{Transport: igl.BudgetInterceptor()(unexpected206RT{})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "fixture-pat"}, Client: cli}
	_, _, err = getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 9,
	}, d)
	if err == nil || !strings.Contains(err.Error(), readmeta.CodeHTTPError) {
		t.Fatalf("err %v", err)
	}
}

type overlongContentLengthRT struct{}

func (overlongContentLengthRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if strings.Contains(req.URL.Path, "/trace") {
		h := make(http.Header)
		h.Set("Content-Type", "text/plain")
		h.Set("Content-Length", "2")
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        h,
			Body:          io.NopCloser(bytes.NewReader([]byte("abcd"))),
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

func TestGetPipelineJobOutput_overlongContentLengthIsNotComplete(t *testing.T) {
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL("https://example.test/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(&http.Client{Transport: igl.BudgetInterceptor()(overlongContentLengthRT{})}),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "t"}, Client: cli}
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 9,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeTrace(t, raw)
	if m["trace"] != "abcd" {
		t.Fatalf("trace %q", m["trace"])
	}
	w := traceWindow(t, raw)
	if w["total_known"] != false || w["total_bytes"] != nil {
		t.Fatalf("window %#v", w)
	}
	if w["source_end_exclusive"] != float64(4) {
		t.Fatalf("source end %#v", w)
	}
	sec := traceSection(t, raw)
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("complete %#v", sec)
	}
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

func TestGetPipelineJobOutput_tailRedactsSplitGlpat(t *testing.T) {
	secret := "glpat-" + strings.Repeat("a", 20)
	body := []byte(secret)
	scan := len(body) - 1 // window starts at "lpat-..."
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		var n int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=-%d", &n); err != nil || n <= 0 {
			t.Errorf("range %q", r.Header.Get("Range"))
			n = scan
		}
		if n > len(body) {
			n = len(body)
		}
		start := len(body) - n
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(body)-1, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[start:])
	}))
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "tail", MaxScanBytes: scan, MaxBytes: scan,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, raw)["trace"].(string)
	if strings.Contains(text, "lpat-") || strings.Contains(text, secret) || strings.Contains(text, strings.Repeat("a", 20)) {
		t.Fatalf("unredacted tail %q", text)
	}
	if !strings.Contains(text, redactPlaceholder) {
		t.Fatalf("trace %q", text)
	}
}

func TestGetPipelineJobOutput_rangeRedactsTokenPastEnd(t *testing.T) {
	secret := "glpat-" + strings.Repeat("b", 20)
	body := []byte("pre " + secret + " post")
	start := int64(strings.Index(string(body), "glpat-"))
	end := start + int64(len("glpat-")+4) // token continues past end_byte
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		rng := r.Header.Get("Range")
		var from, to int
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &from, &to); err != nil {
			t.Fatalf("range %q", rng)
		}
		if from < 0 {
			from = 0
		}
		if to >= len(body) {
			to = len(body) - 1
		}
		if from > to {
			from = to
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[from : to+1])
	}))
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, EndByte: &end, MaxScanBytes: 1 << 20, MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, raw)["trace"].(string)
	if strings.Contains(text, "glpat-") || strings.Contains(text, strings.Repeat("b", 4)) {
		t.Fatalf("unredacted range %q", text)
	}
	if !strings.Contains(text, redactPlaceholder) {
		t.Fatalf("trace %q", text)
	}
}

func TestGetPipelineJobOutput_partial206IsNotFullTrace(t *testing.T) {
	start := int64(0)
	end := int64(2)
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-4/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "abcde")
	}))
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, EndByte: &end,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if decodeTrace(t, raw)["trace"] != "ab" {
		t.Fatalf("%#v", decodeTrace(t, raw))
	}
	sec := traceSection(t, raw)
	if sec["content_complete"] == readmeta.ContentCompleteTrue {
		t.Fatalf("partial 206 marked complete %#v", sec)
	}
}

func TestGetPipelineJobOutput_redactionCountFollowsTrim(t *testing.T) {
	secretA := "glpat-" + strings.Repeat("a", 16)
	secretB := "glpat-" + strings.Repeat("b", 16)
	body := secretA + "\n" + secretB + "\n"
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	keep := len(redactPlaceholder) + 1
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, MaxBytes: keep,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, raw)["trace"].(string)
	w := traceWindow(t, raw)
	if strings.Count(text, redactPlaceholder) != 1 {
		t.Fatalf("trace %q", text)
	}
	if int(w["redaction_count"].(float64)) != 1 {
		t.Fatalf("count %#v text %q", w["redaction_count"], text)
	}
}

func TestGetPipelineJobOutput_outputBytesMatchUTF8(t *testing.T) {
	rawBody := string([]byte{0xff, 0xff, 'a'})
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = w.Write([]byte(rawBody))
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, MaxBytes: 4,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeTrace(t, raw)
	text := m["trace"].(string)
	w := traceWindow(t, raw)
	enc, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	// JSON string contents are the quoted payload without the surrounding quotes.
	quoted := enc[1 : len(enc)-1]
	if int(w["output_bytes"].(float64)) != len(text) || len(text) > 4 || len(quoted) > 4 {
		t.Fatalf("text %q json %s window %#v", text, enc, w)
	}
}

func TestGetPipelineJobOutput_tailBeyondScanUsesMargin(t *testing.T) {
	scan := 64
	raw := bytes.Repeat([]byte("x"), 8000)
	secret := []byte("glpat-" + strings.Repeat("a", 20))
	tailAt := len(raw) - scan
	copy(raw[tailAt-1:], secret)
	copy(raw[len(raw)-5:], []byte(" END\n"))
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		var n int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=-%d", &n); err != nil || n <= scan {
			t.Errorf("suffix range %q", r.Header.Get("Range"))
			n = scan
		}
		if n > len(raw) {
			n = len(raw)
		}
		start := len(raw) - n
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(raw)-1, len(raw)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw[start:])
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "tail", MaxScanBytes: scan, MaxBytes: scan,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	win := traceWindow(t, out)
	if win["tail_proven"] != true {
		t.Fatalf("unproven %#v text %q", win, text)
	}
	if strings.Contains(text, "lpat-") || strings.Contains(text, string(secret)) || !strings.Contains(text, "END") {
		t.Fatalf("tail %q", text)
	}
}

func TestGetPipelineJobOutput_tailBoundaryWithheldWithoutLead(t *testing.T) {
	scan := 64
	raw := bytes.Repeat([]byte("x"), 8000)
	secret := []byte("glpat-" + strings.Repeat("a", 20))
	tailAt := len(raw) - scan
	copy(raw[tailAt-1:], secret)
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		start := len(raw) - scan
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(raw)-1, len(raw)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw[start:])
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "tail", MaxScanBytes: scan, MaxBytes: scan,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	if strings.Contains(text, "lpat-") || strings.Contains(text, string(secret)) || !strings.Contains(text, redactPlaceholder) {
		t.Fatalf("boundary tail %q", text)
	}
}

func TestGetPipelineJobOutput_rangeShortLookaheadWithheld(t *testing.T) {
	secret := "glpat-" + strings.Repeat("b", 20)
	body := []byte("pre " + secret + " post")
	start := int64(strings.Index(string(body), "glpat-"))
	end := start + int64(len("glpat-")+4)
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		var from, to int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			t.Fatalf("range %q", r.Header.Get("Range"))
		}
		if to < int(end) {
			t.Errorf("range stopped at end_byte: %q", r.Header.Get("Range"))
		}
		if from < 0 {
			from = 0
		}
		if to >= len(body) {
			to = len(body) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[from : to+1])
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, EndByte: &end, MaxScanBytes: int(end - start), MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	if strings.Contains(text, "glpat-") || strings.Contains(text, "bbbb") {
		t.Fatalf("range prefix %q", text)
	}
}

func TestGetPipelineJobOutput_prefixAndErrorCutOffCredential(t *testing.T) {
	head := "aaaa\nBearer abc"
	body := head + "DEFGHIJKLMNOP\nERROR later\n"
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, MaxScanBytes: len(head), MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	if strings.Contains(text, "Bearer") || strings.Contains(text, "abc") {
		t.Fatalf("prefix %q", text)
	}

	errBody := "ERROR Bearer abc" + strings.Repeat("Z", 40)
	d2 := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, errBody)
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	_, out, err = getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "error", MaxScanBytes: len("ERROR Bearer abc"), MaxBytes: 1 << 20,
	}, d2)
	if err != nil {
		t.Fatal(err)
	}
	text = decodeTrace(t, out)["trace"].(string)
	if strings.Contains(text, "Bearer") || strings.Contains(text, "abc") {
		t.Fatalf("error %q", text)
	}
}

func TestGetPipelineJobOutput_rangeInsideLongBearer(t *testing.T) {
	secret := strings.Repeat("s", 600)
	body := "Authorization: Bearer " + secret + "\nnext\n"
	marker := strings.Index(body, secret)
	start := int64(marker + 520)
	end := start + 40
	want := body[start:end]
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		var from, to int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			t.Fatalf("range %q", r.Header.Get("Range"))
		}
		if from >= int(start) || to < int(end) {
			t.Errorf("range %q did not keep context around %d-%d", r.Header.Get("Range"), start, end)
		}
		if from < 0 {
			from = 0
		}
		if to >= len(body) {
			to = len(body) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(body)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(body[from : to+1]))
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, EndByte: &end, MaxScanBytes: 4096, MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	if strings.Contains(text, want) || strings.Contains(text, secret[520:560]) {
		t.Fatalf("bearer suffix %q", text)
	}
}

func TestGetPipelineJobOutput_tailInsideLongBearer(t *testing.T) {
	secret := strings.Repeat("s", 600)
	raw := []byte("Authorization: Bearer " + secret + " END\n")
	scan := 24
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		var n int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=-%d", &n); err != nil || n <= scan {
			t.Errorf("suffix %q", r.Header.Get("Range"))
			n = scan
		}
		if n > len(raw) {
			n = len(raw)
		}
		start := len(raw) - n
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(raw)-1, len(raw)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw[start:])
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "tail", MaxScanBytes: scan, MaxBytes: scan,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	win := traceWindow(t, out)
	if win["tail_proven"] != true || strings.Contains(text, strings.Repeat("s", 8)) || !strings.Contains(text, "END") {
		t.Fatalf("tail %q window %#v", text, win)
	}
}

func TestGetPipelineJobOutput_presetScanBudgetStillProvesTail(t *testing.T) {
	scan := 64
	raw := bytes.Repeat([]byte("y"), 4000)
	copy(raw[len(raw)-5:], []byte(" END\n"))
	b := igl.DefaultBudget()
	b.MaxBytes = int64(scan) + 1
	ctx := igl.WithBudget(context.Background(), b)
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		var n int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=-%d", &n); err != nil || n <= scan {
			t.Errorf("suffix %q", r.Header.Get("Range"))
			n = scan
		}
		if n > len(raw) {
			n = len(raw)
		}
		start := len(raw) - n
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(raw)-1, len(raw)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw[start:])
	}))
	_, out, err := getPipelineJobOutput(ctx, nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "tail", MaxScanBytes: scan, MaxBytes: scan,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	win := traceWindow(t, out)
	if win["tail_proven"] != true || !strings.Contains(text, "END") {
		t.Fatalf("text %q window %#v", text, win)
	}
}

func TestGetPipelineJobOutput_shortCompleteBearer(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, "pre\nBearer abc\npost\n")
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	d.Config.Token = ""
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	if strings.Contains(text, "Bearer") || strings.Contains(text, "abc") || !strings.Contains(text, "pre") || !strings.Contains(text, "post") {
		t.Fatalf("trace %q", text)
	}
}

func TestGetPipelineJobOutput_unterminatedShortBearer(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, "Bearer abc")
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	w := traceWindow(t, out)
	if strings.Contains(text, "Bearer") || strings.Contains(text, "abc") || int(w["redaction_count"].(float64)) < 1 {
		t.Fatalf("trace %q window %#v", text, w)
	}
}

func TestGetPipelineJobOutput_tailLineCapKeepsEnd(t *testing.T) {
	raw := append(bytes.Repeat([]byte("a"), 70_000), []byte(" END-SENTINEL\n")...)
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(raw)-1, len(raw)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw)
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "tail", MaxLines: 1, MaxScanBytes: len(raw), MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	w := traceWindow(t, out)
	if w["tail_proven"] != true || !strings.Contains(text, "END-SENTINEL") {
		t.Fatalf("tail %q window %#v", text, w)
	}
}

func TestGetPipelineJobOutput_openRangeIncludesLookbehind(t *testing.T) {
	raw := bytes.Repeat([]byte(" "), 2000)
	copy(raw[1000:], []byte("MARKER"))
	start := int64(1000)
	var rng string
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		rng = r.Header.Get("Range")
		var from int
		if _, err := fmt.Sscanf(rng, "bytes=%d-", &from); err != nil {
			t.Fatalf("range %q", rng)
		}
		if from >= int(start) {
			t.Errorf("open range started at window: %q", rng)
		}
		if from < 0 {
			from = 0
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(raw)-1, len(raw)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw[from:])
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, MaxScanBytes: 100, MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	w := traceWindow(t, out)
	if !strings.Contains(text, "MARKER") || w["range_honored"] != true || w["source_start"].(float64) != float64(start) {
		t.Fatalf("trace %q window %#v range %q", text, w, rng)
	}
}

func TestGetPipelineJobOutput_boundedRangeCapsReturnedSpan(t *testing.T) {
	raw := bytes.Repeat([]byte(" "), 4000)
	copy(raw[1000:], []byte("MARKER"))
	start := int64(1000)
	end := int64(3000)
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		var from, to int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			t.Fatalf("range %q", r.Header.Get("Range"))
		}
		if from >= int(start) {
			t.Errorf("bounded range omitted lookbehind: %q", r.Header.Get("Range"))
		}
		if to < int(end)-1 {
			t.Errorf("bounded range omitted lookahead: %q", r.Header.Get("Range"))
		}
		if from < 0 {
			from = 0
		}
		if to >= len(raw) {
			to = len(raw) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(raw)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw[from : to+1])
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, EndByte: &end, MaxScanBytes: 100, MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	w := traceWindow(t, out)
	span := w["source_end_exclusive"].(float64) - w["source_start"].(float64)
	if !strings.Contains(text, "MARKER") || w["source_start"].(float64) != float64(start) || span != 100 {
		t.Fatalf("trace %q window %#v", text, w)
	}
}

func TestGetPipelineJobOutput_authzUsesBoundedContext(t *testing.T) {
	var sawDeadline atomic.Bool
	var authzBeforeTrace atomic.Bool
	var sawAuthz atomic.Bool
	rt := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if _, ok := req.Context().Deadline(); ok {
			sawDeadline.Store(true)
		}
		if strings.Contains(req.URL.Path, "/trace") {
			if sawAuthz.Load() {
				authzBeforeTrace.Store(true)
			}
			body := []byte("ok\n")
			return &http.Response{
				StatusCode:    http.StatusOK,
				Header:        http.Header{"Content-Type": []string{"text/plain"}},
				Body:          io.NopCloser(bytes.NewReader(body)),
				ContentLength: int64(len(body)),
				Request:       req,
			}, nil
		}
		sawAuthz.Store(true)
		body := []byte(`{"id":42,"path_with_namespace":"g/p","namespace":{"id":7,"kind":"group","full_path":"g","parent_id":0}}`)
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)),
			Request:       req,
		}, nil
	})
	cli, err := gitlab.NewClient("t",
		gitlab.WithBaseURL("https://example.test/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithHTTPClient(&http.Client{Transport: igl.BudgetInterceptor()(rt)}),
	)
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "fixture-pat", AllowedProjectIDs: []string{"42"}}, Client: cli}
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if decodeTrace(t, raw)["trace"] != "ok\n" {
		t.Fatalf("trace %#v", raw)
	}
	if !sawDeadline.Load() || !authzBeforeTrace.Load() || !sawAuthz.Load() {
		t.Fatalf("deadline=%v authzBeforeTrace=%v authz=%v", sawDeadline.Load(), authzBeforeTrace.Load(), sawAuthz.Load())
	}
}

func TestGetPipelineJobOutput_identityBodyDoesNotStealTailBudget(t *testing.T) {
	body := "one\nTAIL-END\n"
	fat := `{"id":42,"description":"` + strings.Repeat("x", 2048) + `","path_with_namespace":"g/p","namespace":{"id":7,"kind":"group","full_path":"g","parent_id":0}}`
	var traceHits atomic.Int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			traceHits.Add(1)
			w.Header().Set("Content-Range", "bytes 0-12/13")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fat)
	}))
	d.Config.Token = "fixture-pat"
	d.Config.AllowedProjectIDs = []string{"42"}
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "tail", MaxScanBytes: 64, MaxLines: 1,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, raw)["trace"].(string)
	w := traceWindow(t, raw)
	if traceHits.Load() != 1 || w["tail_proven"] != true || !strings.Contains(text, "TAIL-END") {
		t.Fatalf("trace %q window %#v hits %d", text, w, traceHits.Load())
	}
}

func TestGetPipelineJobOutput_groupAncestryLeavesTraceRequests(t *testing.T) {
	var traceHits atomic.Int32
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/trace"):
			traceHits.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "ok\n")
		case strings.Contains(r.URL.Path, "/projects/"):
			_, _ = io.WriteString(w, `{"id":42,"path_with_namespace":"root/mid/p","namespace":{"id":11,"kind":"group","full_path":"root/mid","parent_id":10}}`)
		case strings.Contains(r.URL.Path, "/groups/11"):
			_, _ = io.WriteString(w, `{"id":11,"full_path":"root/mid","parent_id":10}`)
		case strings.Contains(r.URL.Path, "/groups/10"):
			_, _ = io.WriteString(w, `{"id":10,"full_path":"root","parent_id":0}`)
		default:
			http.NotFound(w, r)
		}
	}))
	d.Config.Token = "fixture-pat"
	d.Config.AllowedGroupIDs = []string{"10"}
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if traceHits.Load() != 1 || decodeTrace(t, raw)["trace"] != "ok\n" {
		t.Fatalf("hits %d raw %#v", traceHits.Load(), raw)
	}
}

func TestGetPipelineJobOutput_errorStarTotalIsNotComplete(t *testing.T) {
	body := "ok\nfine\nnothing\n"
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		if r.Header.Get("Range") != "" {
			t.Errorf("error selector sent Range %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/*", len(body)-1))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, body)
	}))
	d.Config.Token = "fixture-pat"
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "error",
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	w := traceWindow(t, raw)
	sec := traceSection(t, raw)
	if w["total_known"] != false || w["error_region_proven"] != false {
		t.Fatalf("window %#v", w)
	}
	if sec["content_complete"] == readmeta.ContentCompleteTrue || sec["patch_coverage"] == readmeta.CoverageFull {
		t.Fatalf("complete %#v", sec)
	}
}

func TestGetPipelineJobOutput_zeroBased206PrefixAndErrorAreNotComplete(t *testing.T) {
	for _, sel := range []string{"", "error"} {
		sel := sel
		d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.URL.Path, "/trace") {
				_, _ = io.WriteString(w, `{"id":42}`)
				return
			}
			if r.Header.Get("Range") != "" {
				t.Errorf("%s sent Range %q", sel, r.Header.Get("Range"))
			}
			w.Header().Set("Content-Range", "bytes 0-1/5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "ab")
		}))
		d.Config.Token = "fixture-pat"
		in := getPipelineJobOutputIn{ProjectID: "42", JobID: 8}
		if sel != "" {
			in.Selector = sel
		}
		_, raw, err := getPipelineJobOutput(context.Background(), nil, in, d)
		if err != nil {
			t.Fatal(err)
		}
		sec := traceSection(t, raw)
		w := traceWindow(t, raw)
		if w["total_known"] != true || sec["content_complete"] == readmeta.ContentCompleteTrue || sec["patch_coverage"] == readmeta.CoverageFull {
			t.Fatalf("selector %q complete %#v window %#v", sel, sec, w)
		}
	}
}

func TestGetPipelineJobOutput_starTotal206WithholdsCutGlpat(t *testing.T) {
	body := "ok\nERROR: boom\nglpat-abc"
	for _, sel := range []string{"", "error"} {
		sel := sel
		d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.URL.Path, "/trace") {
				_, _ = io.WriteString(w, `{"id":42}`)
				return
			}
			if r.Header.Get("Range") != "" {
				t.Errorf("%s sent Range %q", sel, r.Header.Get("Range"))
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/*", len(body)-1))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, body)
		}))
		d.Config.Token = "fixture-pat"
		in := getPipelineJobOutputIn{ProjectID: "42", JobID: 8}
		if sel != "" {
			in.Selector = sel
		}
		_, raw, err := getPipelineJobOutput(context.Background(), nil, in, d)
		if err != nil {
			t.Fatal(err)
		}
		text, _ := decodeTrace(t, raw)["trace"].(string)
		if strings.Contains(text, "glpat-") {
			t.Fatalf("selector %q leaked cut glpat %q", sel, text)
		}
		sec := traceSection(t, raw)
		w := traceWindow(t, raw)
		if w["total_known"] != false || sec["content_complete"] == readmeta.ContentCompleteTrue {
			t.Fatalf("selector %q complete %#v window %#v", sel, sec, w)
		}
	}
}

func TestJobTraceObjectEOF_unknownSizeIsFalse(t *testing.T) {
	start := int64(0)
	end := int64(12)
	res := igl.JobTraceResult{
		EOF: true, SizeKnown: false, Data: []byte("pre\nglpat-abc"),
		ObservedStart: &start, ObservedEndExcl: &end,
	}
	if jobTraceObjectEOF(res) {
		t.Fatal("star-total 206 treated as object EOF")
	}
	res.SizeKnown = true
	res.Size = 12
	if !jobTraceObjectEOF(res) {
		t.Fatal("covering known-size 206 should be object EOF")
	}
	res.Size = 20
	if jobTraceObjectEOF(res) {
		t.Fatal("short known-size 206 treated as object EOF")
	}
}

func TestGetPipelineJobOutput_quotedAuthContinuationAtRangeCut(t *testing.T) {
	prefix := `Authorization: Bearer "`
	payload := strings.Repeat("A ", 400) + "SECRET-MARKER"
	raw := []byte(prefix + payload + "\"\n")
	start := int64(len(prefix) + 700)
	end := int64(len(raw))
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		var from, to int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			t.Fatalf("range %q", r.Header.Get("Range"))
		}
		if from < 0 {
			from = 0
		}
		if to >= len(raw) {
			to = len(raw) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(raw)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw[from : to+1])
	}))
	d.Config.Token = "fixture-pat"
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, EndByte: &end, MaxScanBytes: 256, MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	if strings.Contains(text, "SECRET") {
		t.Fatalf("quoted continuation leaked %q", text)
	}
}

func TestGetPipelineJobOutput_quotedAuthCloserAfterRangeEnd(t *testing.T) {
	prefix := `Authorization: Bearer "`
	payload := strings.Repeat("A ", 400) + "SECRET-MARKER" + strings.Repeat(" B", 40)
	raw := []byte(prefix + payload + "\"\n")
	start := int64(len(prefix) + 700)
	end := int64(len(prefix) + len(payload))
	if start-int64(jobTraceLookbehind) <= int64(len(prefix)) || end <= start {
		t.Fatalf("fixture start=%d end=%d prefix=%d", start, end, len(prefix))
	}
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		var from, to int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			t.Fatalf("range %q", r.Header.Get("Range"))
		}
		if from < 0 {
			from = 0
		}
		if to >= len(raw) {
			to = len(raw) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(raw)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw[from : to+1])
	}))
	d.Config.Token = "fixture-pat"
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, EndByte: &end, MaxScanBytes: 256, MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	if strings.Contains(text, "SECRET") {
		t.Fatalf("quoted continuation leaked %q", text)
	}
}

func TestGetPipelineJobOutput_rangeEndNearMaxIntDoesNotCollapse(t *testing.T) {
	start := int64(0)
	end := int64(math.MaxInt64 - 10)
	var rng string
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		rng = r.Header.Get("Range")
		_, _ = io.WriteString(w, "ok\n")
	}))
	_, _, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, EndByte: &end, MaxScanBytes: 64,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	var from, to int64
	if _, scanErr := fmt.Sscanf(rng, "bytes=%d-%d", &from, &to); scanErr != nil {
		t.Fatalf("range %q", rng)
	}
	if to <= from || to < end-1 {
		t.Fatalf("collapsed range %q", rng)
	}
}

func TestGetPipelineJobOutput_shortQuotedBearerAtScanBudget(t *testing.T) {
	payload := `pre Bearer "abc"`
	body := payload + strings.Repeat("Z", 400)
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	d.Config.Token = "fixture-pat"
	_, raw, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, MaxScanBytes: len(payload),
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, raw)["trace"].(string)
	if strings.Contains(text, "abc") || strings.Contains(text, "Bearer") {
		t.Fatalf("quoted bearer leaked %q", text)
	}
}

func TestGetPipelineJobOutput_boundedRangeShort206IsRejected(t *testing.T) {
	start := int64(0)
	end := int64(100)
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-49/1000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(bytes.Repeat([]byte("a"), 50))
	}))
	d.Config.Token = "fixture-pat"
	_, _, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, EndByte: &end, MaxScanBytes: 1 << 20,
	}, d)
	if err == nil {
		t.Fatal("short provider 206 accepted")
	}
}

func TestTraceEndWithMarginSaturates(t *testing.T) {
	if got := traceEndWithMargin(math.MaxInt64-10, 512); got != math.MaxInt64 {
		t.Fatalf("saturated %d", got)
	}
	if got := traceEndWithMargin(100, 512); got != 612 {
		t.Fatalf("plain %d", got)
	}
	if got := traceEndWithMargin(math.MaxInt64-512, 512); got != math.MaxInt64 {
		t.Fatalf("exact %d", got)
	}
}

func TestTraceBudgetBytesIncludesMarginAndPeek(t *testing.T) {
	scan, margin := 64, jobTraceLookbehind
	got := traceBudgetBytes(scan, margin)
	if got < int64(scan)+int64(margin)+1 {
		t.Fatalf("budget %d", got)
	}
}

func TestGetPipelineJobOutput_outputBytesMatchJSONReplacement(t *testing.T) {
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/trace") {
			_, _ = w.Write([]byte{0xff})
			return
		}
		_, _ = io.WriteString(w, `{"id":42}`)
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	m := decodeTrace(t, out)
	text := m["trace"].(string)
	w := traceWindow(t, out)
	sec := traceSection(t, out)
	counts, _ := sec["counts"].(map[string]any)
	if !utf8.ValidString(text) || len(text) != 3 || int(w["output_bytes"].(float64)) != len(text) {
		t.Fatalf("text %q window %#v", text, w)
	}
	if counts == nil || int(counts["bytes"].(float64)) != len(text) {
		t.Fatalf("counts %#v", counts)
	}
	_, capped, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, MaxBytes: 1,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	ctext := decodeTrace(t, capped)["trace"].(string)
	cw := traceWindow(t, capped)
	if len(ctext) != int(cw["output_bytes"].(float64)) || len(ctext) > 1 || !utf8.ValidString(ctext) {
		t.Fatalf("capped %q window %#v", ctext, cw)
	}
}

func TestGetPipelineJobOutput_quotedAuthCloserBeyondLookahead(t *testing.T) {
	prefix := `Authorization: Bearer "`
	payload := strings.Repeat("A ", 700) + "SECRET-MARKER " + strings.Repeat("B ", 600)
	raw := []byte(prefix + payload + "\"\n")
	start := int64(len(prefix) + 1400)
	end := start + int64(len("SECRET-MARKER"))
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		var from, to int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &from, &to); err != nil {
			t.Fatalf("range %q", r.Header.Get("Range"))
		}
		if from < 0 {
			from = 0
		}
		if to >= len(raw) {
			to = len(raw) - 1
		}
		if bytes.ContainsAny(raw[from:to+1], `"`) {
			t.Fatalf("fixture buffer %d-%d contains a quote", from, to)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, len(raw)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(raw[from : to+1])
	}))
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "range", StartByte: &start, EndByte: &end, MaxScanBytes: 256, MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	if strings.Contains(text, "SECRET") {
		t.Fatalf("quoted value beyond lookahead leaked %q", text)
	}
}

func TestGetPipelineJobOutput_cappedPrefixWithholdsShortTokenFragment(t *testing.T) {
	raw := []byte("hello secret-token and more\n")
	capAt := len("hello secre")
	d := authzDeps(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/trace") {
			_, _ = io.WriteString(w, `{"id":42}`)
			return
		}
		_, _ = w.Write(raw)
	}))
	d.Config.Token = "secret-token"
	_, out, err := getPipelineJobOutput(context.Background(), nil, getPipelineJobOutputIn{
		ProjectID: "42", JobID: 8, Selector: "prefix", MaxScanBytes: capAt, MaxBytes: 1 << 20,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	text := decodeTrace(t, out)["trace"].(string)
	if strings.Contains(text, "secre") || !strings.Contains(text, "hello") {
		t.Fatalf("token fragment at scan cut %q", text)
	}
}

func TestSafeTraceErr_budgetCodesAreDistinct(t *testing.T) {
	req := safeTraceErr(fmt.Errorf("wrap: %w", igl.ErrBudgetRequests))
	if req == nil || !strings.Contains(req.Error(), readmeta.CodeBudgetRequests) || strings.Contains(req.Error(), readmeta.CodeBudgetBytes) {
		t.Fatalf("requests %v", req)
	}
	by := safeTraceErr(fmt.Errorf("wrap: %w", igl.ErrBudgetBytes))
	if by == nil || !strings.Contains(by.Error(), readmeta.CodeBudgetBytes) || strings.Contains(by.Error(), readmeta.CodeBudgetRequests) {
		t.Fatalf("bytes %v", by)
	}
}
