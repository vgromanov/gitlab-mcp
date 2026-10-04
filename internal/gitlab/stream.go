package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// StreamJSONArray streams a JSON array response element-by-element via
// Client.Do into an io.Pipe writer, retaining each fully decoded element
// even if a later budget/decode stop occurs.
//
// Requires a complete JSON array ('[' … ']') with only trailing whitespace
// after the closing bracket. Empty/truncated/trailing-junk inputs return an
// error (caller may still have retained fully decoded items via onItem).
//
// When onItem/decode stops early, the budget context is cancelled and the
// pipe reader is closed so SDK deferred Discard cannot keep draining until
// byte-cap; typed onItem errors (e.g. ErrBudgetItems) are preserved over
// generic io.ErrClosedPipe from the writer side.
func StreamJSONArray(ctx context.Context, client *gitlab.Client, method, path string, opt any, onItem func(json.RawMessage) error) (*gitlab.Response, error) {
	return streamJSONArray(ctx, client, method, path, opt, onItem, true)
}

// StreamJSONArrayQueue is the queue-owned stream entry: callback/decode stop
// cancels only the derived child context and closes the pipe/reader. It never
// calls Budget.Cancel on an attached shared budget; transport still charges
// the same BudgetFromContext counters. Optional RequestOptionFunc values are
// applied first; the derived child WithContext is applied LAST so a caller
// gitlab.WithContext(parent) cannot override the cancellable child.
func StreamJSONArrayQueue(ctx context.Context, client *gitlab.Client, method, path string, opt any, onItem func(json.RawMessage) error, opts ...gitlab.RequestOptionFunc) (*gitlab.Response, error) {
	return streamJSONArray(ctx, client, method, path, opt, onItem, false, opts...)
}

func streamJSONArray(ctx context.Context, client *gitlab.Client, method, path string, opt any, onItem func(json.RawMessage) error, cancelBudget bool, opts ...gitlab.RequestOptionFunc) (*gitlab.Response, error) {
	if client == nil {
		return nil, io.ErrUnexpectedEOF
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Caller options first (bounds, headers, …); authoritative child context last.
	reqOpts := append([]gitlab.RequestOptionFunc{}, opts...)
	reqOpts = append(reqOpts, gitlab.WithContext(ctx))
	req, err := client.NewRequest(method, path, opt, reqOpts)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	var (
		decodeErr error
		wg        sync.WaitGroup
		stopOnce  sync.Once
	)
	stopUpstream := func(cause error) {
		stopOnce.Do(func() {
			decodeErr = mapBudgetContextErr(ctx, cause)
			cancel()
			if cancelBudget {
				if b := BudgetFromContext(ctx); b != nil {
					b.Cancel()
				}
			}
			// Closing the reader fails subsequent pipe writes (ErrClosedPipe),
			// stopping Client.Do Copy and limiting deferred Discard drain.
			_ = pr.CloseWithError(decodeErr)
		})
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			// Ensure reader is closed on success path too (stopUpstream may already have).
			_ = pr.Close()
		}()

		dec := json.NewDecoder(pr)
		dec.UseNumber()

		tok, err := dec.Token()
		if err != nil {
			stopUpstream(fmt.Errorf("stream array: expected '[': %w", err))
			return
		}
		d, ok := tok.(json.Delim)
		if !ok || d != '[' {
			stopUpstream(fmt.Errorf("stream array: expected '['"))
			return
		}

		for dec.More() {
			if err := ctx.Err(); err != nil {
				stopUpstream(err)
				return
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				stopUpstream(fmt.Errorf("stream array element: %w", err))
				return
			}
			if err := onItem(raw); err != nil {
				stopUpstream(err)
				return
			}
		}

		closeTok, err := dec.Token()
		if err != nil {
			stopUpstream(fmt.Errorf("stream array: expected ']': %w", err))
			return
		}
		closeDelim, ok := closeTok.(json.Delim)
		if !ok || closeDelim != ']' {
			stopUpstream(fmt.Errorf("stream array: expected ']'"))
			return
		}

		// Trailing input must be EOF (whitespace-only is fine for Decoder).
		if _, err := dec.Token(); err != io.EOF {
			if err == nil {
				stopUpstream(fmt.Errorf("stream array: trailing input after ']'"))
			} else {
				stopUpstream(fmt.Errorf("stream array: trailing input: %w", err))
			}
			return
		}
	}()

	resp, doErr := client.Do(req, pw)
	if doErr != nil {
		_ = pw.CloseWithError(doErr)
	} else {
		_ = pw.Close()
	}
	wg.Wait()

	return resp, preferStreamError(ctx, doErr, decodeErr)
}

// mapBudgetContextErr normalizes context deadline from WithBudget into ErrBudgetElapsed
// so adapters see a typed budget cause (not raw DeadlineExceeded → http_error).
func mapBudgetContextErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) && BudgetFromContext(ctx) != nil {
		return ErrBudgetElapsed
	}
	return err
}

func preferStreamError(ctx context.Context, doErr, decodeErr error) error {
	decodeErr = mapBudgetContextErr(ctx, decodeErr)
	doErr = mapBudgetContextErr(ctx, doErr)
	// When budget kills the upstream copy, the decode side often fails first with a
	// framing/partial-JSON error. Prefer the typed budget cause so adapters emit
	// budget_* limitations instead of generic partial/http_error.
	if isTypedBudgetErr(doErr) && (decodeErr == nil || isClosedPipe(decodeErr) || isStreamFramingErr(decodeErr)) {
		return doErr
	}
	if decodeErr != nil {
		if doErr == nil || isClosedPipe(doErr) || isTypedBudgetErr(decodeErr) {
			return decodeErr
		}
		return decodeErr
	}
	return doErr
}

func isTypedBudgetErr(err error) bool {
	return errors.Is(err, ErrBudgetBytes) ||
		errors.Is(err, ErrBudgetItems) ||
		errors.Is(err, ErrBudgetElapsed) ||
		errors.Is(err, ErrBudgetRequests)
}

func isStreamFramingErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "stream array:") || strings.Contains(msg, "stream array element:")
}

func isClosedPipe(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	return strings.Contains(err.Error(), "closed pipe")
}
