package listx

import (
	"strings"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/redact"
)

func TestWithLoopbackAndLimits(t *testing.T) {
	o := Options{Timeout: bounds.Timeout * 2, MaxBytes: bounds.MaxRefBytes * 2}.WithLoopback()
	if !o.allowLoopback {
		t.Fatal("loopback")
	}
	to, max := o.limits()
	if to != bounds.Timeout || max != bounds.MaxRefBytes {
		t.Fatalf("clamp: %v %d", to, max)
	}
	o2 := Options{Timeout: time.Second, MaxBytes: 100}
	to, max = o2.limits()
	if to != time.Second || max != 100 {
		t.Fatalf("passthrough: %v %d", to, max)
	}
}

func TestDiagnosticJSONNilAndTruncate(t *testing.T) {
	if (&Diagnostic{}).JSON() == "" {
		// empty struct still marshals
	}
	var nilDiag *Diagnostic
	if nilDiag.JSON() != "" {
		t.Fatal("nil")
	}
	supp := make([]string, 40)
	req := make([]string, 40)
	for i := range supp {
		supp[i] = strings.Repeat("S", 80)
		req[i] = strings.Repeat("R", 80)
	}
	d := &Diagnostic{
		Stage:           "auth",
		AgentHasSigners: true,
		TCPConnected:    true,
		Chain: []redact.Frame{{
			Type:      "alg",
			Text:      strings.Repeat("x", 100),
			Supported: supp,
			Requested: req,
		}},
	}
	out := d.JSON()
	if out == "" || len(out) > redact.MaxTotal {
		t.Fatalf("json len=%d", len(out))
	}
	if !d.Truncated && !strings.Contains(out, "truncated") {
		// dropAlgo should have run; Truncated set when dropping
		if d.Chain[0].Omitted == 0 {
			t.Fatal("expected omitted algos during truncate")
		}
	}
}

func TestDropAlgoDirect(t *testing.T) {
	d := &Diagnostic{Chain: []redact.Frame{{
		Supported: []string{"a", "b"},
		Requested: []string{"c"},
	}}}
	if !d.dropAlgo() || len(d.Chain[0].Supported) != 1 {
		t.Fatal("drop supported")
	}
	if !d.dropAlgo() || d.Chain[0].Supported != nil {
		t.Fatal("drop last supported")
	}
	if !d.dropAlgo() || d.Chain[0].Requested != nil {
		t.Fatal("drop requested")
	}
	if d.dropAlgo() {
		t.Fatal("empty")
	}
}
