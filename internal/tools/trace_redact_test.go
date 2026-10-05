package tools

import (
	"bytes"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestRedactChunks_splitSecret(t *testing.T) {
	secret := "glpat-" + strings.Repeat("a", 20)
	r := newTraceRedactor("")
	r.Write([]byte("pre-" + secret[:10]))
	r.Write([]byte(secret[10:] + "\npost"))
	out, n := r.Flush()
	if strings.Contains(out, secret) || strings.Contains(out, "glpat-") || n != 1 {
		t.Fatalf("out %q count %d", out, n)
	}
	if !strings.Contains(out, redactPlaceholder) || !strings.HasPrefix(out, "pre-") || !strings.HasSuffix(out, "\npost") {
		t.Fatalf("out %q", out)
	}
}

func TestRedactRange_windowBoundary(t *testing.T) {
	secret := "glpat-" + strings.Repeat("b", 20)
	src := []byte("xxxx" + secret + "yyyy")
	// Window starts 4 bytes into the secret (after "xxxxglpa").
	from := 4 + 4
	out, spans := redactRange(src, from, len(src)-4, "")
	if strings.Contains(out, secret) || strings.Contains(out, "t-"+strings.Repeat("b", 10)) || len(spans) != 1 {
		t.Fatalf("out %q count %d", out, len(spans))
	}
}

func TestRedact_patternsAndToken(t *testing.T) {
	src := []byte("Authorization: Bearer supersecret\nPRIVATE-TOKEN: tok-value\nhttps://user:pass@host/x\nraw glpat-" + strings.Repeat("c", 16) + "\n")
	out, n := redactBytes(src, []byte("supersecret"))
	s := string(out)
	for _, leak := range []string{"supersecret", "tok-value", "user:pass", "glpat-"} {
		if strings.Contains(s, leak) {
			t.Fatalf("leaked %s in %q", leak, s)
		}
	}
	if n < 4 {
		t.Fatalf("count %d %q", n, s)
	}
}

func TestCapTraceLines_pathologicalLine(t *testing.T) {
	sentinel := "SENTINEL-PAST-CAP"
	line := strings.Repeat("a", 100) + sentinel
	out, capped := capTraceLines(line+"\nnext\n", 32)
	if !capped || strings.Contains(out, sentinel) || !strings.Contains(out, "…[line_capped]") || !strings.Contains(out, "next") {
		t.Fatalf("capped %v out %q", capped, out)
	}
}

func TestRedactRangeEdges_splitBoundaries(t *testing.T) {
	head := []byte("lpat-" + strings.Repeat("a", 20))
	out, spans := redactRangeEdges(head, 0, len(head), "", true, false)
	if strings.Contains(out, "lpat-") || strings.Contains(out, strings.Repeat("a", 20)) || len(spans) != 1 {
		t.Fatalf("head %q spans %d", out, len(spans))
	}
	tail := []byte("glpat-bbbb")
	out, spans = redactRangeEdges(tail, 0, len(tail), "", false, true)
	if strings.Contains(out, "glpat-") || strings.Contains(out, "bbbb") || len(spans) != 1 {
		t.Fatalf("tail %q spans %d", out, len(spans))
	}
}

func TestRedact_authorizationSpamLinear(t *testing.T) {
	src := bytes.Repeat([]byte("authorization "), (140<<10)/len("authorization "))
	start := time.Now()
	out, n := redactBytes(src, nil)
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("redact took %s", elapsed)
	}
	if n != 0 || bytes.Contains(out, []byte("Authorization:")) {
		t.Fatalf("n %d", n)
	}
}

func TestTrimOutputBytes_runeBoundary(t *testing.T) {
	s := "ok" + string(rune(0x1F600)) + "tail"
	cut, trimmed := trimOutputBytes(s, len("ok")+1)
	if !trimmed || !utf8.ValidString(cut) || strings.Contains(cut, "tail") {
		t.Fatalf("cut %q trimmed %v", cut, trimmed)
	}
	if strings.Contains(cut, string(rune(0x1F600))) {
		t.Fatalf("split rune kept %q", cut)
	}
}
