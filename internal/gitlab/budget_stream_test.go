package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func TestPresenceHelpers(t *testing.T) {
	p, err := AllowFailurePresence(json.RawMessage(`{"allow_failure":true}`))
	if err != nil || p != readmeta.PresenceTrue {
		t.Fatalf("got %s err=%v", p, err)
	}
	p, _ = ApprovedPresence(json.RawMessage(`{}`))
	if p != readmeta.PresenceAbsent {
		t.Fatal(p)
	}
	p, err = UserHasApprovedPresence(json.RawMessage(`{"user_has_approved":false}`))
	if err != nil || p != readmeta.PresenceFalse {
		t.Fatalf("user_has_approved=%s err=%v", p, err)
	}
	p, err = DiffCollapsedPresence(json.RawMessage(`{"collapsed":true}`))
	if err != nil || p != readmeta.PresenceTrue {
		t.Fatalf("collapsed=%s err=%v", p, err)
	}
	p, err = DiffTooLargePresence(json.RawMessage(`{"too_large":null}`))
	if err != nil || p != readmeta.PresenceNull {
		t.Fatalf("too_large=%s err=%v", p, err)
	}
}

func TestBudget_CancelReleasesDeadline(t *testing.T) {
	b := DefaultBudget()
	b.MaxElapsed = time.Minute
	ctx := WithBudget(context.Background(), b)
	b.Cancel()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("budget.Cancel must cancel WithBudget-derived context (release deadline timer)")
	}
}

func TestBudget_RemainingBytes(t *testing.T) {
	if RemainingBytes := (*Budget)(nil).RemainingBytes(); RemainingBytes != -1 {
		t.Fatalf("nil RemainingBytes=%d", RemainingBytes)
	}
	b := &Budget{MaxBytes: 100, bytesRead: 40}
	if got := b.RemainingBytes(); got != 60 {
		t.Fatalf("RemainingBytes=%d", got)
	}
	b.bytesRead = 150
	if got := b.RemainingBytes(); got != 0 {
		t.Fatalf("over RemainingBytes=%d", got)
	}
	b.MaxBytes = 0
	if got := b.RemainingBytes(); got != -1 {
		t.Fatalf("unlimited RemainingBytes=%d", got)
	}
}

// TestBudget_CapLimitsConcurrentWithRemainingStatsCharge exercises CapLimits
// writers against RemainingBytes/Stats/reserveBytes/chargeRequest under -race.
func TestBudget_CapLimitsConcurrentWithRemainingStatsCharge(t *testing.T) {
	b := &Budget{
		MaxItems:    1000,
		MaxBytes:    8 << 20,
		MaxRequests: 1000,
		MaxElapsed:  time.Minute,
		start:       time.Now(),
	}
	ctx := WithBudget(context.Background(), b)
	defer b.Cancel()

	const goroutines = 8
	const iters = 200
	done := make(chan struct{}, goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer func() { done <- struct{}{} }()
			for i := 0; i < iters; i++ {
				switch g % 4 {
				case 0:
					// Tighten toward batch local maxima (never relax).
					b.CapLimits(20, 1<<20, 16)
					b.CapLimits(20, 8<<20, 16)
				case 1:
					_ = b.RemainingBytes()
					_, _, _ = b.Stats()
					_, _, _, _ = b.LimitsSnapshot()
				case 2:
					_, _ = b.reserveBytes(64)
					b.releaseBytes(32)
				default:
					_ = b.chargeRequest()
					_ = b.AddItem()
					if BudgetFromContext(ctx) != b {
						t.Errorf("budget identity lost")
					}
				}
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
	items, bytes, reqs, _ := b.LimitsSnapshot()
	if items != 20 {
		t.Fatalf("MaxItems want 20 after CapLimits, got %d", items)
	}
	if bytes != 1<<20 {
		t.Fatalf("MaxBytes want 1MiB (tightest CapLimits), got %d", bytes)
	}
	if reqs != 16 {
		t.Fatalf("MaxRequests want 16, got %d", reqs)
	}
	rem := b.RemainingBytes()
	if rem < 0 || rem > 1<<20 {
		t.Fatalf("RemainingBytes=%d out of range after tighten", rem)
	}
}

func TestBudgetInterceptor_errorBodyCap(t *testing.T) {
	big := strings.Repeat("x", 64*1024)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, big)
	}))
	t.Cleanup(ts.Close)

	b := &Budget{MaxBytes: 1024, MaxRequests: 8, MaxElapsed: time.Minute, start: time.Now()}
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithBudget(context.Background(), b)
	req, err := c.NewRequest(http.MethodGet, "projects/1", nil, []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Do(req, &map[string]any{})
	if err == nil {
		t.Fatal("expected error")
	}
	_, bytesRead, _ := b.Stats()
	if bytesRead > 1024 {
		t.Fatalf("bytesRead=%d over reserved cap", bytesRead)
	}
}

func TestBudgetInterceptor_countsRetries(t *testing.T) {
	var trips atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		trips.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"err"}`))
	}))
	t.Cleanup(ts.Close)
	b := DefaultBudget()
	b.MaxRequests = 100
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithCustomRetryMax(2),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithBudget(context.Background(), b)
	req, err := c.NewRequest(http.MethodGet, "projects/1", nil, []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Do(req, &map[string]any{})
	reqs, _, _ := b.Stats()
	if reqs < 2 {
		t.Fatalf("expected retries charged, requests=%d trips=%d", reqs, trips.Load())
	}
}

func TestBudget_concurrentReadersAtomicCap(t *testing.T) {
	b := &Budget{MaxBytes: 1000, start: time.Now(), MaxElapsed: time.Minute}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = b.reserveBytes(30)
			}
		}()
	}
	wg.Wait()
	_, bytesRead, _ := b.Stats()
	if bytesRead > 1000 {
		t.Fatalf("concurrent reserve overread: bytesRead=%d", bytesRead)
	}
	if bytesRead != 1000 {
		// May be exactly 1000 if fully reserved
		t.Logf("bytesRead=%d", bytesRead)
	}
	if bytesRead > b.MaxBytes {
		t.Fatal("exceeded MaxBytes")
	}
}

func TestBudget_elapsedCancelsInFlightBody(t *testing.T) {
	started := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `{"id":`)
		if flusher != nil {
			flusher.Flush()
		}
		close(started)
		time.Sleep(2 * time.Second)
		_, _ = io.WriteString(w, `1}`)
	}))
	t.Cleanup(ts.Close)

	b := &Budget{MaxBytes: 1 << 20, MaxRequests: 8, MaxElapsed: 80 * time.Millisecond, start: time.Now()}
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithBudget(context.Background(), b)
	req, err := c.NewRequest(http.MethodGet, "projects/1", nil, []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, e := c.Do(req, &map[string]any{})
		done <- e
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server did not start body")
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrBudgetElapsed) && !errors.Is(err, context.DeadlineExceeded) {
			// Accept wrapped elapsed
			if err == nil || (!strings.Contains(err.Error(), "budget_elapsed") && !errors.Is(err, ErrBudgetElapsed)) {
				t.Fatalf("want elapsed cancel, got %v", err)
			}
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("elapsed budget did not cancel in-flight read")
	}
}

func TestStreamJSONArray_retainsItemsOnBudget(t *testing.T) {
	item := `{"old_path":"a.go","new_path":"a.go","diff":"x","collapsed":false,"too_large":false}`
	body := "[" + item + "," + item + "," + item + "," + item + "]"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Next-Page", "")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)

	b := &Budget{MaxBytes: int64(len(item) + 20), MaxItems: 100, MaxRequests: 8, MaxElapsed: time.Minute, start: time.Now()}
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithBudget(context.Background(), b)
	var got []json.RawMessage
	_, err = StreamJSONArray(ctx, c, http.MethodGet, "projects/1/merge_requests/1/diffs", nil, func(raw json.RawMessage) error {
		got = append(got, append(json.RawMessage(nil), raw...))
		return nil
	})
	if len(got) == 0 {
		t.Fatalf("expected retained items, err=%v", err)
	}
	if len(got) >= 4 && err == nil {
		t.Fatalf("expected mid-stream stop, got all %d items err=%v", len(got), err)
	}
}

func TestStreamJSONArray_preservesBudgetItemsOverClosedPipe(t *testing.T) {
	item := `{"old_path":"a.go","new_path":"a.go","diff":"x"}`
	body := "[" + item + "," + item + "," + item + "]"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(ts.Close)
	b := &Budget{MaxBytes: 1 << 20, MaxItems: 1, MaxRequests: 8, MaxElapsed: time.Minute, start: time.Now()}
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithBudget(context.Background(), b)
	var n int
	_, err = StreamJSONArray(ctx, c, http.MethodGet, "projects/1/merge_requests/1/diffs", nil, func(json.RawMessage) error {
		n++
		return b.AddItem()
	})
	if !errors.Is(err, ErrBudgetItems) {
		t.Fatalf("want ErrBudgetItems, got %v (items=%d)", err, n)
	}
	if n < 1 {
		t.Fatal("expected at least one retained item before stop")
	}
}

func TestStreamJSONArray_rejectsTruncatedAndTrailing(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"truncated", `[{"a":1}`},
		{"trailing", `[{"a":1}] trailing`},
		{"not_array", `{"a":1}`},
		{"empty_body", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(ts.Close)
			c, err := gitlab.NewClient("t",
				gitlab.WithBaseURL(ts.URL+"/api/v4"),
				gitlab.WithoutRetries(),
				gitlab.WithInterceptor(BudgetInterceptor()),
			)
			if err != nil {
				t.Fatal(err)
			}
			b := DefaultBudget()
			ctx := WithBudget(context.Background(), b)
			_, err = StreamJSONArray(ctx, c, http.MethodGet, "projects/1/merge_requests/1/diffs", nil, func(json.RawMessage) error {
				return nil
			})
			if err == nil {
				t.Fatal("expected error for invalid stream")
			}
		})
	}
}

func TestStreamJSONArray_emptyArrayOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Next-Page", "")
		_, _ = io.WriteString(w, `[]`)
	}))
	t.Cleanup(ts.Close)
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithBudget(context.Background(), DefaultBudget())
	var n int
	_, err = StreamJSONArray(ctx, c, http.MethodGet, "projects/1/merge_requests/1/diffs", nil, func(json.RawMessage) error {
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("items=%d", n)
	}
}

func TestStreamJSONArray_cancelJoins(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `[`)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(2 * time.Second)
		_, _ = io.WriteString(w, `{"old_path":"a"}]`)
	}))
	t.Cleanup(ts.Close)
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := DefaultBudget()
	ctx = WithBudget(ctx, b)
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = StreamJSONArray(ctx, c, http.MethodGet, "projects/1/merge_requests/1/diffs", nil, func(json.RawMessage) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected cancel-related stop")
	}
}

func TestStreamJSONArray_elapsedMapsToBudgetElapsed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, `[{"old_path":"a","new_path":"a","diff":"","collapsed":false,"too_large":false}`)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(3 * time.Second)
		_, _ = io.WriteString(w, `]`)
	}))
	t.Cleanup(ts.Close)
	c, err := gitlab.NewClient("t",
		gitlab.WithBaseURL(ts.URL+"/api/v4"),
		gitlab.WithoutRetries(),
		gitlab.WithInterceptor(BudgetInterceptor()),
	)
	if err != nil {
		t.Fatal(err)
	}
	b := &Budget{MaxItems: 100, MaxBytes: 1 << 20, MaxElapsed: 200 * time.Millisecond, MaxRequests: 16, start: time.Now()}
	ctx := WithBudget(context.Background(), b)
	_, err = StreamJSONArray(ctx, c, http.MethodGet, "projects/1/merge_requests/1/diffs", nil, func(json.RawMessage) error {
		return nil
	})
	if !errors.Is(err, ErrBudgetElapsed) {
		t.Fatalf("want ErrBudgetElapsed, got %v", err)
	}
}

func TestPreferStreamError_budgetBeatsFraming(t *testing.T) {
	ctx := WithBudget(context.Background(), DefaultBudget())
	got := preferStreamError(ctx, ErrBudgetElapsed, fmt.Errorf("stream array: expected ']': unexpected EOF"))
	if !errors.Is(got, ErrBudgetElapsed) {
		t.Fatalf("got %v", got)
	}
	got = preferStreamError(ctx, ErrBudgetBytes, fmt.Errorf("stream array element: unexpected EOF"))
	if !errors.Is(got, ErrBudgetBytes) {
		t.Fatalf("got %v", got)
	}
	// Typed onItem budget still wins over closed pipe.
	got = preferStreamError(ctx, io.ErrClosedPipe, ErrBudgetItems)
	if !errors.Is(got, ErrBudgetItems) {
		t.Fatalf("got %v", got)
	}
}
