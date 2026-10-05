package listx

import (
	"errors"
	"testing"
)

func TestR11_SSHUploadClosePropagatesUpdateError(t *testing.T) {
	s := &sshUploadSession{updates: &failingUpdates{err: errors.New("hostkey update failed")}}
	if err := s.Close(); err == nil {
		t.Fatal("Close discarded host-key update failure (R11)")
	}
}

type failingUpdates struct{ err error }

func (f *failingUpdates) Wait()        {}
func (f *failingUpdates) Sync() error  { return nil }
func (f *failingUpdates) Error() error { return f.err }
