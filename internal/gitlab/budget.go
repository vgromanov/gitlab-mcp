package gitlab

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// Default reference-adapter budgets (locked plan).
const (
	DefaultMaxItems    = 100
	DefaultMaxBytes    = int64(8 << 20) // 8 MiB
	DefaultMaxElapsed  = 30 * time.Second
	DefaultMaxRequests = 16
)

var (
	ErrBudgetBytes    = errors.New("budget_bytes")
	ErrBudgetRequests = errors.New("budget_requests")
	ErrBudgetElapsed  = errors.New("budget_elapsed")
	ErrBudgetItems    = errors.New("budget_items")
)

type budgetKey struct{}

// Budget tracks invocation limits across all RoundTrips and body reads.
type Budget struct {
	MaxItems    int
	MaxBytes    int64
	MaxElapsed  time.Duration
	MaxRequests int

	mu        sync.Mutex
	start     time.Time
	bytesRead int64
	requests  int
	items     int
	cancel    context.CancelFunc
}

// DefaultBudget returns locked reference-adapter defaults.
func DefaultBudget() *Budget {
	return &Budget{
		MaxItems:    DefaultMaxItems,
		MaxBytes:    DefaultMaxBytes,
		MaxElapsed:  DefaultMaxElapsed,
		MaxRequests: DefaultMaxRequests,
		start:       time.Now(),
	}
}

// WithBudget attaches a budget to ctx, starts the elapsed clock, and derives a
// deadline so MaxElapsed cancels in-flight RoundTrip/body reads (not only
// pre/post checks).
func WithBudget(ctx context.Context, b *Budget) context.Context {
	if b == nil {
		return ctx
	}
	b.mu.Lock()
	if b.start.IsZero() {
		b.start = time.Now()
	}
	start := b.start
	maxElapsed := b.MaxElapsed
	b.mu.Unlock()

	ctx = context.WithValue(ctx, budgetKey{}, b)
	if maxElapsed > 0 {
		dctx, cancel := context.WithDeadline(ctx, start.Add(maxElapsed))
		b.mu.Lock()
		b.cancel = cancel
		b.mu.Unlock()
		return dctx
	}
	return ctx
}

// Cancel aborts the budget-bound context (stops in-flight transport/body work).
func (b *Budget) Cancel() {
	if b == nil {
		return
	}
	b.mu.Lock()
	cancel := b.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// BudgetFromContext returns the budget if present.
func BudgetFromContext(ctx context.Context) *Budget {
	b, _ := ctx.Value(budgetKey{}).(*Budget)
	return b
}

func (b *Budget) chargeRequest() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.MaxElapsed > 0 && time.Since(b.start) > b.MaxElapsed {
		return ErrBudgetElapsed
	}
	b.requests++
	if b.MaxRequests > 0 && b.requests > b.MaxRequests {
		return ErrBudgetRequests
	}
	return nil
}

// reserveBytes atomically reserves up to want bytes against the remaining
// budget before any Read. Prevents concurrent-reader over-commit.
// Returns (allowed, err). allowed may be 0 with ErrBudgetBytes when exhausted.
func (b *Budget) reserveBytes(want int) (int, error) {
	if b == nil {
		return want, nil
	}
	if want <= 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.MaxElapsed > 0 && time.Since(b.start) > b.MaxElapsed {
		return 0, ErrBudgetElapsed
	}
	if b.MaxBytes <= 0 {
		return want, nil
	}
	left := b.MaxBytes - b.bytesRead
	if left <= 0 {
		return 0, ErrBudgetBytes
	}
	n := want
	if int64(n) > left {
		n = int(left)
	}
	b.bytesRead += int64(n)
	return n, nil
}

func (b *Budget) releaseBytes(n int64) {
	if b == nil || n <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bytesRead -= n
	if b.bytesRead < 0 {
		b.bytesRead = 0
	}
}

// RemainingBytes returns bytes left before cap (-1 if unlimited/nil).
// MaxBytes and bytesRead are observed under the budget mutex so concurrent
// CapLimits tighten cannot race a pre-lock limit read.
func (b *Budget) RemainingBytes() int64 {
	if b == nil {
		return -1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.MaxBytes <= 0 {
		return -1
	}
	left := b.MaxBytes - b.bytesRead
	if left < 0 {
		return 0
	}
	return left
}

// AddItem increments retained item count; returns ErrBudgetItems if already at max.
func (b *Budget) AddItem() error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.MaxItems > 0 && b.items >= b.MaxItems {
		return ErrBudgetItems
	}
	b.items++
	return nil
}

// Stats returns current counters.
func (b *Budget) Stats() (requests int, bytes int64, items int) {
	if b == nil {
		return 0, 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests, b.bytesRead, b.items
}

// CapLimits tightens MaxItems/MaxBytes/MaxRequests under the budget mutex.
// Each positive argument is applied only when it is stricter than the current
// cap (or when the current cap is unlimited/non-positive). Never relaxes.
// Does not alter MaxElapsed, start time, cancel func, or counters.
func (b *Budget) CapLimits(maxItems int, maxBytes int64, maxRequests int) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if maxItems > 0 && (b.MaxItems <= 0 || b.MaxItems > maxItems) {
		b.MaxItems = maxItems
	}
	if maxBytes > 0 && (b.MaxBytes <= 0 || b.MaxBytes > maxBytes) {
		b.MaxBytes = maxBytes
	}
	if maxRequests > 0 && (b.MaxRequests <= 0 || b.MaxRequests > maxRequests) {
		b.MaxRequests = maxRequests
	}
}

// SetMaxItems sets the retained-item cap for an owned tool budget.
func (b *Budget) SetMaxItems(maxItems int) {
	if b == nil || maxItems <= 0 {
		return
	}
	b.mu.Lock()
	b.MaxItems = maxItems
	b.mu.Unlock()
}

// SetMaxRequests sets the request cap for an owned tool budget. Unlike
// CapLimits, a positive value replaces the current cap even when that
// relaxes the limit (e.g. raising the default for a single tool call).
func (b *Budget) SetMaxRequests(maxRequests int) {
	if b == nil || maxRequests <= 0 {
		return
	}
	b.mu.Lock()
	b.MaxRequests = maxRequests
	b.mu.Unlock()
}

// ElapsedExceeded reports that the budget clock, not a parent deadline, has passed.
func (b *Budget) ElapsedExceeded() bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.MaxElapsed > 0 && !b.start.IsZero() && time.Since(b.start) > b.MaxElapsed
}

// OriginalDeadline is the deadline WithBudget derived from the clock start.
// It does not change the budget. A parent deadline can still be earlier.
func (b *Budget) OriginalDeadline() (time.Time, bool) {
	if b == nil {
		return time.Time{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.start.IsZero() || b.MaxElapsed <= 0 {
		return time.Time{}, false
	}
	return b.start.Add(b.MaxElapsed), true
}

// LimitsSnapshot returns current Max* caps under lock (for tests/diagnostics).
func (b *Budget) LimitsSnapshot() (maxItems int, maxBytes int64, maxRequests int, maxElapsed time.Duration) {
	if b == nil {
		return 0, 0, 0, 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.MaxItems, b.MaxBytes, b.MaxRequests, b.MaxElapsed
}

// BudgetInterceptor returns a client-go Interceptor that counts requests and
// caps body reads (success, error, and retry) so Do's deferred Discard cannot
// unbounded-drain past the invocation budget.
func BudgetInterceptor() func(http.RoundTripper) http.RoundTripper {
	return func(next http.RoundTripper) http.RoundTripper {
		if next == nil {
			next = http.DefaultTransport
		}
		return &budgetTransport{next: next}
	}
}

type budgetTransport struct {
	next http.RoundTripper
}

func (t *budgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := checkCacheBinding(req); err != nil {
		return nil, err
	}
	b := BudgetFromContext(req.Context())
	if b == nil {
		return t.next.RoundTrip(req)
	}
	if err := b.chargeRequest(); err != nil {
		return nil, err
	}
	if err := req.Context().Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrBudgetElapsed
		}
		return nil, err
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(req.Context().Err(), context.DeadlineExceeded) {
			return resp, ErrBudgetElapsed
		}
		return resp, err
	}
	if resp != nil && resp.Body != nil {
		resp.Body = &cappedBody{r: resp.Body, b: b, ctx: req.Context()}
	}
	return resp, nil
}

type cappedBody struct {
	r      io.ReadCloser
	b      *Budget
	ctx    context.Context
	mu     sync.Mutex
	closed bool
}

func (c *cappedBody) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, io.EOF
	}
	if c.ctx != nil {
		if err := c.ctx.Err(); err != nil {
			_ = c.r.Close()
			c.closed = true
			if errors.Is(err, context.DeadlineExceeded) {
				return 0, ErrBudgetElapsed
			}
			return 0, err
		}
	}
	allowed, rerr := c.b.reserveBytes(len(p))
	if rerr != nil {
		_ = c.r.Close()
		c.closed = true
		return 0, rerr
	}
	if allowed == 0 {
		_ = c.r.Close()
		c.closed = true
		return 0, ErrBudgetBytes
	}
	buf := p[:allowed]
	n, err := c.r.Read(buf)
	if int64(n) < int64(allowed) {
		c.b.releaseBytes(int64(allowed) - int64(n))
	}
	if err != nil && c.ctx != nil && errors.Is(c.ctx.Err(), context.DeadlineExceeded) {
		_ = c.r.Close()
		c.closed = true
		return n, ErrBudgetElapsed
	}
	return n, err
}

func (c *cappedBody) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.r.Close()
}
