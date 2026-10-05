package tools

import (
	"bytes"
	"unicode/utf8"
)

const (
	redactPlaceholder = "[REDACTED]"
	// tracePatternMax caps a single credential match so a pathological line
	// cannot force an unbounded hold.
	tracePatternMax = 512
	traceTokenHold  = 4096
	glpatMin        = 16
	glpatMax        = 128
)

// traceRedactor redacts credential patterns across Write chunks.
// It is best-effort pattern redaction, not proof that every secret is gone.
type traceRedactor struct {
	token []byte
	hold  []byte
	out   []byte
	count int
	holdN int
}

func newTraceRedactor(token string) *traceRedactor {
	n := 600
	if len(token) > n {
		n = len(token)
	}
	if n > traceTokenHold {
		n = traceTokenHold
	}
	return &traceRedactor{token: []byte(token), holdN: n}
}

// Write appends raw trace bytes. Secrets split across calls are redacted
// once Flush is called, as long as the secret is at most holdN bytes.
func (r *traceRedactor) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	r.hold = append(r.hold, p...)
	r.drain(false)
}

// Flush emits the remaining hold.
func (r *traceRedactor) Flush() (string, int) {
	r.drain(true)
	return string(r.out), r.count
}

func (r *traceRedactor) drain(final bool) {
	if final {
		text, n := redactBytes(r.hold, r.token)
		r.out = append(r.out, text...)
		r.count += n
		r.hold = nil
		return
	}
	keep := r.holdN - 1
	if keep < 1 {
		keep = 1
	}
	if len(r.hold) <= keep {
		return
	}
	cut := len(r.hold) - keep
	spans := matchSpans(r.hold, r.token)
	for _, sp := range spans {
		if sp[1] > cut && sp[0] < cut {
			cut = sp[0]
		}
	}
	for cut > 0 && !utf8.RuneStart(r.hold[cut]) {
		cut--
	}
	if cut <= 0 {
		return
	}
	text, n := redactBytes(r.hold[:cut], r.token)
	r.out = append(r.out, text...)
	r.count += n
	r.hold = append([]byte(nil), r.hold[cut:]...)
}

// redactRange redacts src and returns only the window [from, to).
// Matches that begin in the lookbehind or end in the lookahead are still
// removed from the window so a secret split on the boundary cannot leak.
func redactRange(src []byte, from, to int, token string) (string, int) {
	if from < 0 {
		from = 0
	}
	if to > len(src) {
		to = len(src)
	}
	if from > to {
		from = to
	}
	spans := matchSpans(src, []byte(token))
	var b bytes.Buffer
	count := 0
	i := from
	for _, sp := range spans {
		if sp[1] <= from || sp[0] >= to {
			continue
		}
		s := sp[0]
		if s < from {
			s = from
		}
		e := sp[1]
		if e > to {
			e = to
		}
		b.Write(src[i:s])
		b.WriteString(redactPlaceholder)
		i = e
		count++
	}
	b.Write(src[i:to])
	return b.String(), count
}

func redactBytes(src, token []byte) ([]byte, int) {
	if len(src) == 0 {
		return nil, 0
	}
	spans := matchSpans(src, token)
	if len(spans) == 0 {
		return append([]byte(nil), src...), 0
	}
	var b bytes.Buffer
	i := 0
	for _, sp := range spans {
		if sp[0] > i {
			b.Write(src[i:sp[0]])
		}
		b.WriteString(redactPlaceholder)
		i = sp[1]
	}
	b.Write(src[i:])
	return b.Bytes(), len(spans)
}

func matchSpans(src, token []byte) [][2]int {
	var spans [][2]int
	from := 0
	for from < len(src) {
		s, e, ok := earliestMatch(src, from, token)
		if !ok {
			break
		}
		if e <= s {
			from = s + 1
			continue
		}
		spans = append(spans, [2]int{s, e})
		from = e
	}
	return spans
}

func earliestMatch(src []byte, from int, token []byte) (int, int, bool) {
	for from < len(src) {
		rest := src[from:]
		best := -1
		consider := func(rel int) {
			if rel < 0 {
				return
			}
			if best < 0 || rel < best {
				best = rel
			}
		}
		consider(indexFold(rest, "authorization"))
		consider(indexFold(rest, "proxy-authorization"))
		consider(indexFold(rest, "bearer"))
		consider(indexFold(rest, "private-token"))
		consider(bytes.Index(rest, []byte("glpat-")))
		consider(bytes.Index(rest, []byte("://")))
		if len(token) >= 8 && len(token) <= traceTokenHold {
			consider(bytes.Index(rest, token))
		} else if len(token) > traceTokenHold {
			consider(bytes.Index(rest, token))
		}
		if best < 0 {
			return 0, 0, false
		}
		abs := from + best
		s, e, ok := longestMatchAt(src, abs, token)
		if ok {
			return s, e, true
		}
		from = abs + 1
	}
	return 0, 0, false
}

func longestMatchAt(src []byte, i int, token []byte) (int, int, bool) {
	bestS, bestE := 0, 0
	found := false
	try := func(s, e int, ok bool) {
		if !ok || e <= bestE {
			return
		}
		bestS, bestE = s, e
		found = true
	}
	try(parseAuthHeader(src, i))
	try(parseBearer(src, i))
	try(parsePrivateToken(src, i))
	try(parseGlpat(src, i))
	try(parseUserinfo(src, i))
	try(parseExactToken(src, i, token))
	return bestS, bestE, found
}

func parseAuthHeader(src []byte, i int) (int, int, bool) {
	n := 0
	switch {
	case hasFoldPrefix(src[i:], "proxy-authorization"):
		n = len("proxy-authorization")
	case hasFoldPrefix(src[i:], "authorization"):
		n = len("authorization")
	default:
		return 0, 0, false
	}
	j := i + n
	j = skipSpace(src, j)
	if j >= len(src) || src[j] != ':' {
		return 0, 0, false
	}
	j++
	j = skipSpace(src, j)
	switch {
	case hasFoldPrefix(src[j:], "bearer"):
		j += len("bearer")
	case hasFoldPrefix(src[j:], "basic"):
		j += len("basic")
	default:
		return 0, 0, false
	}
	j = skipSpace(src, j)
	start := j
	for j < len(src) && j-start < tracePatternMax && src[j] > ' ' && src[j] != '"' {
		j++
	}
	if j == start {
		return 0, 0, false
	}
	return i, j, true
}

func parseBearer(src []byte, i int) (int, int, bool) {
	if !hasFoldPrefix(src[i:], "bearer") {
		return 0, 0, false
	}
	if i > 0 && isWordByte(src[i-1]) {
		return 0, 0, false
	}
	j := i + len("bearer")
	if j >= len(src) || !isSpaceByte(src[j]) {
		return 0, 0, false
	}
	j = skipSpace(src, j)
	start := j
	for j < len(src) && j-start < tracePatternMax && isTokenChar(src[j]) {
		j++
	}
	if j-start < 8 {
		return 0, 0, false
	}
	return i, j, true
}

func parsePrivateToken(src []byte, i int) (int, int, bool) {
	if !hasFoldPrefix(src[i:], "private-token") {
		return 0, 0, false
	}
	j := i + len("private-token")
	j = skipSpace(src, j)
	if j >= len(src) || (src[j] != ':' && src[j] != '=') {
		return 0, 0, false
	}
	j++
	j = skipSpace(src, j)
	start := j
	for j < len(src) && j-start < tracePatternMax && src[j] > ' ' {
		j++
	}
	if j == start {
		return 0, 0, false
	}
	return i, j, true
}

func parseGlpat(src []byte, i int) (int, int, bool) {
	const prefix = "glpat-"
	if !bytes.HasPrefix(src[i:], []byte(prefix)) {
		return 0, 0, false
	}
	j := i + len(prefix)
	start := j
	for j < len(src) && j-start < glpatMax && isGlpatChar(src[j]) {
		j++
	}
	if j-start < glpatMin {
		return 0, 0, false
	}
	return i, j, true
}

func parseUserinfo(src []byte, i int) (int, int, bool) {
	if !bytes.HasPrefix(src[i:], []byte("://")) {
		return 0, 0, false
	}
	j := i + 3
	userStart := j
	for j < len(src) && j-userStart < 256 && src[j] != ':' && src[j] != '@' && src[j] != '/' && src[j] > ' ' {
		j++
	}
	if j == userStart || j >= len(src) || src[j] != ':' {
		return 0, 0, false
	}
	j++
	passStart := j
	for j < len(src) && j-passStart < 256 && src[j] != '@' && src[j] != '/' && src[j] > ' ' {
		j++
	}
	if j == passStart || j >= len(src) || src[j] != '@' {
		return 0, 0, false
	}
	return i + 3, j + 1, true
}

func parseExactToken(src []byte, i int, token []byte) (int, int, bool) {
	if len(token) < 8 || !bytes.HasPrefix(src[i:], token) {
		return 0, 0, false
	}
	return i, i + len(token), true
}

func indexFold(b []byte, needle string) int {
	if needle == "" || len(b) < len(needle) {
		return -1
	}
	nb := []byte(needle)
	for i := 0; i+len(nb) <= len(b); i++ {
		if hasFoldPrefix(b[i:], needle) {
			return i
		}
	}
	return -1
}

func hasFoldPrefix(b []byte, prefix string) bool {
	if len(b) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		c := b[i]
		p := prefix[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if p >= 'A' && p <= 'Z' {
			p += 'a' - 'A'
		}
		if c != p {
			return false
		}
	}
	return true
}

func skipSpace(src []byte, j int) int {
	for j < len(src) && isSpaceByte(src[j]) {
		j++
	}
	return j
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t'
}

func isWordByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}

func isTokenChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		c == '-' || c == '.' || c == '_' || c == '~' || c == '+' || c == '/' || c == '='
}

func isGlpatChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
}

// trimOutputTail keeps the end of s, on a UTF-8 boundary.
func trimOutputTail(s string, max int) (string, bool) {
	if max < 0 {
		max = 0
	}
	if len(s) <= max {
		return s, false
	}
	cut := len(s) - max
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	if cut > len(s) {
		cut = len(s)
	}
	return s[cut:], true
}

// trimOutputBytes cuts s to at most max bytes without splitting a UTF-8 rune.
func trimOutputBytes(s string, max int) (string, bool) {
	if max < 0 {
		max = 0
	}
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
