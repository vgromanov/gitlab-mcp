package testutil

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestRecordingHandler_methodAndAttempts(t *testing.T) {
	rec := &RecordingHandler{Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})}
	cli, _ := NewGitLabClient(t, rec)
	_, _, err := cli.Projects.ListProjects(&gitlab.ListProjectsOptions{}, gitlab.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Attempts() < 1 {
		t.Fatalf("attempts=%d", rec.Attempts())
	}
	if rec.MethodCount(http.MethodGet) < 1 {
		t.Fatalf("GET count=%d snapshot=%v", rec.MethodCount(http.MethodGet), rec.Snapshot())
	}
}

func TestScriptAcceptedThenDrop_barrierThenCancel(t *testing.T) {
	barrier := NewAcceptedBarrier()
	rec := &RecordingHandler{Next: ScriptAcceptedThenDrop(barrier)}
	ts := httptest.NewServer(rec)
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("test-token",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithCustomRetryMax(0),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	var callErr error
	go func() {
		defer wg.Done()
		_, _, callErr = cli.Projects.ListProjects(&gitlab.ListProjectsOptions{}, gitlab.WithContext(ctx))
	}()

	select {
	case <-barrier.Accepted():
	case <-time.After(5 * time.Second):
		t.Fatal("accepted barrier not signaled")
	}
	if barrier.Count() != 1 {
		t.Fatalf("barrier count=%d want 1", barrier.Count())
	}
	if got := rec.Attempts(); got != 1 {
		t.Fatalf("attempts at accept=%d want exact 1", got)
	}
	if got := rec.MethodCount(http.MethodGet); got != 1 {
		t.Fatalf("GET at accept=%d want exact 1 snapshot=%v", got, rec.Snapshot())
	}
	cancel()
	wg.Wait()
	if callErr == nil {
		t.Fatal("expected error after drop/cancel")
	}
	if got := rec.Attempts(); got != 1 {
		t.Fatalf("attempts after cancel=%d want exact 1 (retries disabled)", got)
	}
}

func TestScriptAcceptedThenHang_cancelAfterAccept(t *testing.T) {
	barrier := NewAcceptedBarrier()
	rec := &RecordingHandler{Next: ScriptAcceptedThenHang(barrier)}
	ts := httptest.NewServer(rec)
	t.Cleanup(ts.Close)
	cli, err := gitlab.NewClient("test-token",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithCustomRetryMax(0),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, _, err := cli.Projects.ListProjects(&gitlab.ListProjectsOptions{}, gitlab.WithContext(ctx))
		errCh <- err
	}()

	select {
	case <-barrier.Accepted():
	case <-time.After(5 * time.Second):
		t.Fatal("accepted not signaled")
	}
	if got := rec.Attempts(); got != 1 {
		t.Fatalf("attempts at accept=%d want 1", got)
	}
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected cancel error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return after cancel")
	}
	if got := rec.MethodCount(http.MethodGet); got != 1 {
		t.Fatalf("GET count=%d want 1 methods=%v", got, rec.Snapshot())
	}
}

func TestRecordingRoundTripper_exactCounts(t *testing.T) {
	fail := &FailIfCalledRoundTripper{}
	rec := &RecordingRoundTripper{Next: fail}
	req, _ := http.NewRequest(http.MethodPut, "http://127.0.0.1/api/v4/projects/1", nil)
	_, _ = rec.RoundTrip(req)
	if rec.Attempts() != 1 || rec.MethodCount(http.MethodPut) != 1 {
		t.Fatalf("attempts=%d PUT=%d", rec.Attempts(), rec.MethodCount(http.MethodPut))
	}
	if !fail.Called.Load() {
		t.Fatal("expected underlying call")
	}
}

func TestRecordingRoundTripper_nilRequestAndNilNext(t *testing.T) {
	rec := &RecordingRoundTripper{Next: &FailIfCalledRoundTripper{}}
	_, err := rec.RoundTrip(nil)
	if err == nil {
		t.Fatal("nil request must error")
	}
	if rec.Attempts() != 0 {
		t.Fatalf("nil request must not count attempts, got %d", rec.Attempts())
	}

	rec = &RecordingRoundTripper{}
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/api/v4/projects/1", nil)
	_, err = rec.RoundTrip(req)
	if err == nil {
		t.Fatal("nil Next must fail closed")
	}
	if rec.Attempts() != 0 {
		t.Fatalf("nil Next must not count attempts, got %d", rec.Attempts())
	}
}

func TestRetryMax_HTTP500_exactAttempts(t *testing.T) {
	const retryMax = 2
	rec := &RecordingHandler{Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})}
	ts := httptest.NewServer(rec)
	t.Cleanup(ts.Close)

	cli, err := gitlab.NewClient("fake-token-not-live",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithCustomRetryMax(retryMax),
		gitlab.WithCustomRetryWaitMinMax(0, 0),
		gitlab.WithCustomBackoff(func(min, max time.Duration, attemptNum int, resp *http.Response) time.Duration {
			return 0
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = cli.Projects.ListProjects(&gitlab.ListProjectsOptions{}, gitlab.WithContext(context.Background()))
	if err == nil {
		t.Fatal("expected 500 error")
	}
	want := int64(1 + retryMax)
	if got := rec.Attempts(); got != want {
		t.Fatalf("attempts=%d want %d (1+RetryMax)", got, want)
	}
	if got := rec.MethodCount(http.MethodGet); got != int(want) {
		t.Fatalf("GET=%d want %d", got, want)
	}
}

func TestFailIfCalledRoundTripper(t *testing.T) {
	f := &FailIfCalledRoundTripper{}
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/api/v4/projects/1", nil)
	_, err := f.RoundTrip(req)
	if !errors.Is(err, errRoundTripForbidden) {
		t.Fatalf("err=%v", err)
	}
	if !f.Called.Load() {
		t.Fatal("expected Called")
	}
}
