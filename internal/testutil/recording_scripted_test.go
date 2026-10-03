package testutil

import (
	"context"
	"errors"
	"io"
	"net/http"
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
	cli, _ := NewGitLabClient(t, rec)

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
	if barrier.Count() < 1 {
		t.Fatalf("barrier count=%d", barrier.Count())
	}
	cancel()
	wg.Wait()
	if callErr == nil {
		t.Fatal("expected error after drop/cancel")
	}
	if rec.Attempts() < 1 {
		t.Fatalf("expected at least one attempt, got %d", rec.Attempts())
	}
	if rec.MethodCount(http.MethodGet) < 1 {
		t.Fatalf("expected GET attempts, got %v", rec.Snapshot())
	}
}

func TestScriptAcceptedThenHang_cancelAfterAccept(t *testing.T) {
	barrier := NewAcceptedBarrier()
	rec := &RecordingHandler{Next: ScriptAcceptedThenHang(barrier)}
	cli, _ := NewGitLabClient(t, rec)

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
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected cancel error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("call did not return after cancel")
	}
	if rec.MethodCount(http.MethodGet) < 1 {
		t.Fatalf("methods=%v", rec.Snapshot())
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
