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
	out, spans := redactRangeEdges(head, 0, len(head), "", true, false, false)
	if strings.Contains(out, "lpat-") || strings.Contains(out, strings.Repeat("a", 20)) || len(spans) != 1 {
		t.Fatalf("head %q spans %d", out, len(spans))
	}
	tail := []byte("glpat-bbbb")
	out, spans = redactRangeEdges(tail, 0, len(tail), "", false, true, false)
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

func TestRedact_longAuthorizationAndPrivateToken(t *testing.T) {
	authVal := strings.Repeat("S", 600)
	privVal := strings.Repeat("P", 600)
	src := []byte("Authorization: Bearer " + authVal + "\nPRIVATE-TOKEN: " + privVal + "\nnext\n")
	out, n := redactBytes(src, nil)
	s := string(out)
	if strings.Contains(s, authVal) || strings.Contains(s, authVal[512:]) || strings.Contains(s, privVal) || strings.Contains(s, privVal[512:]) {
		t.Fatalf("leaked long credential %q", s)
	}
	if n < 2 || !strings.Contains(s, "next") {
		t.Fatalf("n %d out %q", n, s)
	}
}

func TestSelectPrefix_redactionCountFollowsReturnedWindow(t *testing.T) {
	secret := "glpat-" + strings.Repeat("b", 16)
	dropped := selectPrefix([]byte("ok\n"+secret), 0, 0, len("ok"), jobTraceHardLine, "", true)
	if dropped.redactions != 0 || strings.Contains(dropped.text, redactPlaceholder) || strings.Contains(dropped.text, "glpat-") {
		t.Fatalf("trimmed count %d text %q", dropped.redactions, dropped.text)
	}
	capped := selectPrefix([]byte(strings.Repeat("a", 40)+secret), 0, 0, 1<<20, 16, "", true)
	if capped.redactions != 0 || strings.Contains(capped.text, redactPlaceholder) || strings.Contains(capped.text, "glpat-") {
		t.Fatalf("line cap count %d text %q", capped.redactions, capped.text)
	}
}

func TestSelectPrefix_scanCutWithholdsShortCredential(t *testing.T) {
	data := []byte("aaaa\nBearer abc")
	p := selectPrefix(data, 0, 0, 1<<20, jobTraceHardLine, "", false)
	if strings.Contains(p.text, "Bearer") || strings.Contains(p.text, "abc") {
		t.Fatalf("prefix cut %q", p.text)
	}
	errData := []byte("ERROR Bearer abc")
	ep := selectError(errData, 0, "", 1<<20, jobTraceHardLine, "", false)
	if strings.Contains(ep.text, "Bearer") || strings.Contains(ep.text, "abc") {
		t.Fatalf("error cut %q", ep.text)
	}
}

func TestRedact_quotedAuthorizationAndBearer(t *testing.T) {
	cases := []string{
		`Authorization: Bearer "secret" next`,
		`Authorization: Basic "dXNlcjpwYXNz" next`,
		"Authorization: Bearer 'secret'\nnext",
		`Proxy-Authorization: Bearer "secret"`,
		`pre Bearer "secret" post`,
		"Authorization: Bearer \"unclosed\nnext",
	}
	for _, src := range cases {
		out, n := redactBytes([]byte(src), nil)
		s := string(out)
		for _, leak := range []string{"secret", "dXNlcjpwYXNz", "unclosed"} {
			if strings.Contains(s, leak) {
				t.Fatalf("leaked %q in %q -> %q n=%d", leak, src, s, n)
			}
		}
		if n < 1 {
			t.Fatalf("n %d src %q out %q", n, src, s)
		}
	}
	unquoted, n := redactBytes([]byte("Authorization: Bearer supersecret\n"), nil)
	if n < 1 || strings.Contains(string(unquoted), "supersecret") {
		t.Fatalf("unquoted n %d out %q", n, unquoted)
	}
}

func TestRedact_shortCompleteBearer(t *testing.T) {
	body := []byte("pre\nBearer abc\npost\n")
	out, n := redactBytes(body, nil)
	s := string(out)
	if strings.Contains(s, "abc") || strings.Contains(s, "Bearer") || n < 1 {
		t.Fatalf("n %d out %q", n, s)
	}
	if !strings.Contains(s, "pre") || !strings.Contains(s, "post") {
		t.Fatalf("out %q", s)
	}
	// A scan that stops inside the value is not a finished short token.
	cut := selectPrefix([]byte("pre\nBearer abc"), 0, 0, 1<<20, jobTraceHardLine, "", false)
	if strings.Contains(cut.text, "abc") || strings.Contains(cut.text, "Bearer") {
		t.Fatalf("cut %q", cut.text)
	}
	eof := selectPrefix([]byte("Bearer abc"), 0, 0, 1<<20, jobTraceHardLine, "", true)
	if strings.Contains(eof.text, "abc") || strings.Contains(eof.text, "Bearer") || eof.redactions < 1 {
		t.Fatalf("eof %q count %d", eof.text, eof.redactions)
	}
}

func TestRedact_longUserinfo(t *testing.T) {
	pass := strings.Repeat("p", 300)
	user := strings.Repeat("u", 300)
	body := "https://user:" + pass + "@host/x\nhttps://" + user + ":pw@host/y\nhttps://example.com/x\n"
	out, n := redactBytes([]byte(body), nil)
	s := string(out)
	if strings.Contains(s, pass) || strings.Contains(s, user) || strings.Contains(s, ":pw@") {
		t.Fatalf("leaked %q", s)
	}
	if !strings.Contains(s, "host/x") || !strings.Contains(s, "example.com/x") || n < 2 {
		t.Fatalf("n %d out %q", n, s)
	}
}

func TestRedact_passwordOnlyUserinfo(t *testing.T) {
	body := "see https://:secret@example.com/path and https://example.com/x\n"
	out, n := redactBytes([]byte(body), nil)
	s := string(out)
	if strings.Contains(s, "secret") || n < 1 {
		t.Fatalf("n %d out %q", n, s)
	}
	if !strings.Contains(s, "example.com/path") || !strings.Contains(s, "example.com/x") || !strings.Contains(s, "https://") {
		t.Fatalf("out %q", s)
	}
}

func TestRedact_userinfoWithoutPassword(t *testing.T) {
	body := "https://opaque-token@example.com/path\nhttps://secret:@example.com/x\nhttps://example.com/ok\n"
	out, n := redactBytes([]byte(body), nil)
	s := string(out)
	if strings.Contains(s, "opaque-token") || strings.Contains(s, "secret") || n < 2 {
		t.Fatalf("n %d out %q", n, s)
	}
	if !strings.Contains(s, "example.com/path") || !strings.Contains(s, "example.com/ok") {
		t.Fatalf("out %q", s)
	}
}

func TestSelectPrefix_manyGlpatSpansStayLinear(t *testing.T) {
	unit := []byte("glpat-" + strings.Repeat("a", 16) + ":")
	raw := bytes.Repeat(unit, (1<<20)/len(unit))
	start := time.Now()
	p := selectPrefix(raw, 0, 0, len(raw), 64<<10, "", true)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("line-cap remap took %s", elapsed)
	}
	if !p.lineCapped || strings.Contains(p.text, "glpat-") || strings.Contains(p.text, strings.Repeat("a", 16)) {
		t.Fatalf("capped %v text %q", p.lineCapped, p.text[:min(80, len(p.text))])
	}
}

func TestRedactRangeEdges_openCredentialBeyondLookbehind(t *testing.T) {
	secret := strings.Repeat("s", 600)
	src := []byte(secret + " next")
	out, _ := redactRangeEdges(src, 520, 560, "", true, false, false)
	if strings.Contains(out, "s") {
		t.Fatalf("suffix %q", out)
	}
	kept := []byte(strings.Repeat("s", 100) + " hello")
	out2, _ := redactRangeEdges(kept, len(kept)-5, len(kept), "", true, false, false)
	if out2 != "hello" {
		t.Fatalf("kept %q", out2)
	}
	plain, _ := redactRangeEdges(src, 520, 560, "", false, false, false)
	if !strings.Contains(plain, "s") {
		t.Fatalf("anchored window redacted %q", plain)
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
