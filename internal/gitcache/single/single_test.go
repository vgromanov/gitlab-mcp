package single_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/single"
)

func TestDoSerializesAndBusy(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := single.Do(func() error {
			close(started)
			<-release
			return nil
		})
		if err != nil {
			t.Errorf("holder: %v", err)
		}
	}()
	<-started
	if err := single.Do(func() error { return nil }); !errors.Is(err, single.ErrBusy) {
		t.Fatalf("busy: %v", err)
	}
	close(release)
	wg.Wait()
	if err := single.Do(func() error { return errors.New("inner") }); err == nil || err.Error() != "inner" {
		t.Fatalf("inner err: %v", err)
	}
	// ensure lock released promptly
	done := make(chan struct{})
	go func() {
		_ = single.Do(func() error { close(done); return nil })
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("lock stuck")
	}
}
