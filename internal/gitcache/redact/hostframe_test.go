package redact

import (
	"net"
	"testing"
)

func TestHostFrameCategories(t *testing.T) {
	for _, tc := range []struct {
		class string
		text  string
	}{
		{"missing", "knownhosts: key is unknown"},
		{"mismatch", "knownhosts: key mismatch"},
		{"revoked", "knownhosts: key is revoked"},
		{"other", "[redacted]"},
	} {
		f := HostFrame(tc.class)
		if f.Text != tc.text || f.Type == "" {
			t.Fatalf("%s: %#v", tc.class, f)
		}
	}
}

func TestNetTextOpError(t *testing.T) {
	op := &net.OpError{Op: "dial", Net: "tcp", Err: errTimeout{}}
	got := netText(op)
	if got != "dial: i/o timeout" {
		t.Fatalf("netText: %q", got)
	}
	if netText(nil) != "[redacted]" {
		t.Fatal("nil")
	}
}

type errTimeout struct{}

func (errTimeout) Error() string   { return "i/o timeout connecting to 10.1.2.3:7999" }
func (errTimeout) Timeout() bool   { return true }
func (errTimeout) Temporary() bool { return true }
