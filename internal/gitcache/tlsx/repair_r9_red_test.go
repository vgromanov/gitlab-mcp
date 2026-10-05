//go:build unix

package tlsx

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestR9_CALoadRejectsFIFOWithoutBlocking proves bounded descriptor-validated
// CA open: a FIFO must fail closed quickly, not block inside os.ReadFile.
func TestR9_CALoadRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "ca.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Build(Input{ServerName: "example.com", CAPath: fifo})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO CA path accepted (R9)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CA load blocked on FIFO without finite/nonblocking open (R9)")
	}
}
