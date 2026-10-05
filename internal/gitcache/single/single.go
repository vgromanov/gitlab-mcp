// Package single allows one list or fetch at a time in this process.
package single

import (
	"errors"
	"sync"
)

// ErrBusy is returned when a second git operation starts before the first finishes.
var ErrBusy = errors.New("another git operation is in progress")

var mu sync.Mutex

// Do runs fn while holding the process lock. A concurrent call returns ErrBusy
// and does not run fn.
func Do(fn func() error) error {
	if !mu.TryLock() {
		return ErrBusy
	}
	defer mu.Unlock()
	return fn()
}
