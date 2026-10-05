package tools

import (
	"bytes"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	redactPlaceholder = "[REDACTED]"
	// tracePatternMax bounds how far a boundary check looks backward for a
	// keyword. Credential values run through the next delimiter; a fixed cap
	// would copy the rest of a longer secret.
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
		text, n := redactBytesAt(r.hold, r.token, true)
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
	spans := matchSpans(r.hold, r.token, false)
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
	text, n := redactBytesAt(r.hold[:cut], r.token, false)
	r.out = append(r.out, text...)
	r.count += n
	r.hold = append([]byte(nil), r.hold[cut:]...)
}

// redactRange redacts src and returns only the window [from, to).
// Matches that begin in the lookbehind or end in the lookahead are still
// removed from the window so a secret split on the boundary cannot leak.
// The spans are placeholder byte ranges in the returned string.
func redactRange(src []byte, from, to int, token string) (string, [][2]int) {
	return redactRangeEdges(src, from, to, token, false, false, false)
}

// redactRangeEdges is redactRange plus optional boundary suppression.
// hideHead redacts an ambiguous prefix when bytes before from were not fetched.
// hideTail redacts an ambiguous suffix when bytes at and after to were not fetched.
// atEOF treats the end of src as a finished credential boundary, so a short
// complete Bearer value with no trailing delimiter still redacts.
func redactRangeEdges(src []byte, from, to int, token string, hideHead, hideTail, atEOF bool) (string, [][2]int) {
	if from < 0 {
		from = 0
	}
	if to > len(src) {
		to = len(src)
	}
	if from > to {
		from = to
	}
	spans := matchSpans(src, []byte(token), atEOF)
	if hideHead {
		// Bytes before the buffer were not fetched. A credential that begins
		// there is withheld only where it overlaps the returned window.
		if s, e, ok := ambiguousHead(src, 0, token); ok && s < to && e > from {
			spans = append(spans, [2]int{s, e})
		}
		// Authorization and PRIVATE-TOKEN values have no length cap, so a
		// 512-byte lookbehind can start inside the secret. When nothing before
		// the window delimited that value, withhold the leading token.
		if s, e, ok := leadingUnanchoredToken(src, from, to); ok {
			spans = append(spans, [2]int{s, e})
		}
		// A quoted credential can contain spaces. Those spaces must not end
		// the withheld span when the opening quote sits in lookbehind or
		// before the fetched buffer.
		if s, e, ok := quotedHeadContinuation(src, from, to); ok {
			spans = append(spans, [2]int{s, e})
		}
	}
	if hideTail {
		// The buffer is not a proven end of the trace. Suppress a credential
		// that is still open at the last fetched byte when it overlaps the
		// window, including when a short lookahead does not finish it.
		if s, e, ok := ambiguousTail(src, len(src), token); ok && s < to && e > from {
			if e > to {
				e = to
			}
			if e > s {
				spans = append(spans, [2]int{s, e})
			}
		}
	}
	spans = mergeSpans(spans)
	return applyWindow(src, from, to, spans)
}

func applyWindow(src []byte, from, to int, spans [][2]int) (string, [][2]int) {
	var b bytes.Buffer
	var out [][2]int
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
		if e <= s {
			continue
		}
		b.Write(src[i:s])
		ps := b.Len()
		b.WriteString(redactPlaceholder)
		out = append(out, [2]int{ps, b.Len()})
		i = e
	}
	b.Write(src[i:to])
	return b.String(), out
}

func mergeSpans(spans [][2]int) [][2]int {
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i][0] == spans[j][0] {
			return spans[i][1] < spans[j][1]
		}
		return spans[i][0] < spans[j][0]
	})
	out := [][2]int{spans[0]}
	for _, sp := range spans[1:] {
		last := &out[len(out)-1]
		if sp[0] <= last[1] {
			if sp[1] > last[1] {
				last[1] = sp[1]
			}
			continue
		}
		out = append(out, sp)
	}
	return out
}

// ambiguousHead reports a credential that starts before from and continues
// into the window, when those leading bytes were not fetched.
func ambiguousHead(src []byte, from int, token string) (int, int, bool) {
	if from < 0 || from >= len(src) {
		return 0, 0, false
	}
	best := 0
	data := src[from:]
	if n := missingPrefixMatch(data, "glpat-", token); n > best {
		best = n
	}
	for _, prefix := range []string{"proxy-authorization", "authorization", "bearer", "private-token", "://"} {
		if n := missingPrefixMatch(data, prefix, token); n > best {
			best = n
		}
	}
	if n := missingTokenHead(data, []byte(token)); n > best {
		best = n
	}
	if best <= 0 {
		return 0, 0, false
	}
	return from, from + best, true
}

// leadingUnanchoredToken withholds a credential continuation when the fetched
// buffer does not start at the source and no delimiter appears before the
// window. The marker then sits before the bytes we have, so prefix matching
// cannot see it. The returned span is clipped to the window.
func leadingUnanchoredToken(src []byte, from, to int) (int, int, bool) {
	if from < 0 {
		from = 0
	}
	if to > len(src) {
		to = len(src)
	}
	if from >= to {
		return 0, 0, false
	}
	limit := from
	if limit > len(src) {
		limit = len(src)
	}
	for i := 0; i < limit; i++ {
		if isTraceDelim(src[i]) {
			return 0, 0, false
		}
	}
	end := from
	for end < to && !isTraceDelim(src[end]) {
		end++
	}
	if end <= from {
		return 0, 0, false
	}
	return from, end, true
}

func isTraceDelim(c byte) bool {
	return c <= ' ' || c == '"' || c == '\''
}

// quotedHeadContinuation withholds a quoted Authorization/Bearer value that
// began before the returned window. Spaces inside the quotes are not
// delimiters. When the opening quote is in lookbehind, quote state is
// tracked from the current line. When the opening quote sits before src,
// the first quote in the window that is followed by a delimiter or the
// window end is treated as the closer. If that closer is only in lookahead
// or is missing, the whole window is withheld.
func quotedHeadContinuation(src []byte, from, to int) (int, int, bool) {
	if from < 0 {
		from = 0
	}
	if to > len(src) {
		to = len(src)
	}
	if from >= to {
		return 0, 0, false
	}
	line := 0
	for i := 0; i < from; i++ {
		if src[i] == '\n' || src[i] == '\r' {
			line = i + 1
		}
	}
	quote := byte(0)
	for i := line; i < from; i++ {
		c := src[i]
		if quote != 0 {
			if c == '\\' && i+1 < from && src[i+1] != '\n' && src[i+1] != '\r' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			quote = c
		}
	}
	if quote == 0 {
		if line != 0 {
			return 0, 0, false
		}
		for i := from; i < to; i++ {
			if src[i] == '\n' || src[i] == '\r' {
				return 0, 0, false
			}
			if src[i] == '\\' && i+1 < to && src[i+1] != '\n' && src[i+1] != '\r' {
				i++
				continue
			}
			if src[i] == '"' || src[i] == '\'' {
				if i+1 >= to || src[i+1] <= ' ' {
					return from, i + 1, true
				}
				return 0, 0, false
			}
		}
		return from, to, true
	}
	end := from
	for end < to {
		if src[end] == '\n' || src[end] == '\r' {
			break
		}
		if src[end] == '\\' && end+1 < to && src[end+1] != '\n' && src[end+1] != '\r' {
			end += 2
			continue
		}
		if src[end] == quote {
			end++
			break
		}
		end++
	}
	if end <= from {
		return 0, 0, false
	}
	return from, end, true
}

func missingPrefixMatch(data []byte, prefix, token string) int {
	pb := []byte(prefix)
	best := 0
	limit := len(data)
	if limit > tracePatternMax+len(prefix) {
		limit = tracePatternMax + len(prefix)
	}
	for cut := 1; cut < len(pb); cut++ {
		if !hasFoldPrefix(data, prefix[cut:]) {
			continue
		}
		synthetic := make([]byte, 0, cut+limit)
		synthetic = append(synthetic, pb[:cut]...)
		synthetic = append(synthetic, data[:limit]...)
		_, e, ok := longestMatchAt(synthetic, 0, []byte(token), false)
		if !ok || e <= cut {
			continue
		}
		n := e - cut
		if n > len(data) {
			n = len(data)
		}
		if n > best {
			best = n
		}
	}
	return best
}

func missingTokenHead(data, token []byte) int {
	if len(token) < 8 || len(data) == 0 {
		return 0
	}
	best := 0
	maxCut := len(token)
	if maxCut > traceTokenHold {
		maxCut = traceTokenHold
	}
	for cut := 1; cut < maxCut; cut++ {
		rest := token[cut:]
		if len(data) >= len(rest) && bytes.HasPrefix(data, rest) {
			if len(rest) > best {
				best = len(rest)
			}
			continue
		}
		if len(data) < len(rest) && len(data) >= 8 && bytes.HasPrefix(rest, data) {
			if len(data) > best {
				best = len(data)
			}
		}
	}
	return best
}

// ambiguousTail reports a credential that starts in the window and would
// continue past to, when those trailing bytes were not fetched.
func ambiguousTail(src []byte, to int, token string) (int, int, bool) {
	if to <= 0 || to > len(src) {
		return 0, 0, false
	}
	start := -1
	consider := func(s int) {
		if s >= 0 && s < to && (start < 0 || s < start) {
			start = s
		}
	}
	consider(incompleteGlpatEnd(src, to))
	consider(incompleteBearerEnd(src, to))
	consider(incompleteUserinfoEnd(src, to))
	consider(danglingKeywordEnd(src, to))
	consider(tokenPrefixEnd(src, to, []byte(token)))
	if start < 0 {
		return 0, 0, false
	}
	return start, to, true
}

func incompleteGlpatEnd(src []byte, to int) int {
	back := to - (len("glpat-") + glpatMax)
	if back < 0 {
		back = 0
	}
	rel := bytes.LastIndex(src[back:to], []byte("glpat-"))
	if rel >= 0 {
		abs := back + rel
		j := abs + len("glpat-")
		n := 0
		for j < to && isGlpatChar(src[j]) && n < glpatMax {
			j++
			n++
		}
		if j == to && n < glpatMin {
			return abs
		}
	}
	return danglingWord(src, to, "glpat-", 4)
}

func incompleteBearerEnd(src []byte, to int) int {
	back := to - (len("bearer") + tracePatternMax)
	if back < 0 {
		back = 0
	}
	rel := lastIndexFold(src[back:to], "bearer")
	if rel < 0 {
		return -1
	}
	abs := back + rel
	if abs > 0 && isWordByte(src[abs-1]) {
		return -1
	}
	j := abs + len("bearer")
	if j >= to || !isSpaceByte(src[j]) {
		return -1
	}
	j = skipSpace(src, j)
	n := 0
	for j < to && isTokenChar(src[j]) {
		j++
		n++
	}
	if j == to && n < 8 {
		return abs
	}
	return -1
}

func incompleteUserinfoEnd(src []byte, to int) int {
	if to <= 0 {
		return -1
	}
	if to > len(src) {
		to = len(src)
	}
	rel := bytes.LastIndex(src[:to], []byte("://"))
	if rel < 0 {
		return -1
	}
	abs := rel
	if _, _, ok := parseUserinfo(src[:to], abs); ok {
		return -1
	}
	// A userinfo prefix that reaches the cut is ambiguous, including a
	// username-only form that has not yet seen @.
	j := abs + 3
	if j >= to {
		return -1
	}
	for j < to {
		c := src[j]
		if c <= ' ' || c == '/' {
			return -1
		}
		if c == '@' {
			return -1
		}
		j++
	}
	if j > abs+3 {
		return abs
	}
	return -1
}

func danglingKeywordEnd(src []byte, to int) int {
	best := -1
	for _, kw := range []string{"proxy-authorization", "authorization", "private-token", "bearer", "glpat-", "://"} {
		min := 4
		if len(kw) < min {
			min = len(kw) - 1
			if min < 1 {
				min = 1
			}
		}
		if s := danglingWord(src, to, kw, min); s >= 0 && (best < 0 || s < best) {
			best = s
		}
	}
	return best
}

func danglingWord(src []byte, to int, keyword string, min int) int {
	if min < 1 {
		min = 1
	}
	max := len(keyword) - 1
	if max > to {
		max = to
	}
	for n := max; n >= min; n-- {
		if hasFoldPrefix(src[to-n:to], keyword[:n]) {
			return to - n
		}
	}
	return -1
}

func tokenPrefixEnd(src []byte, to int, token []byte) int {
	if len(token) < 8 {
		return -1
	}
	max := len(token) - 1
	if max > to {
		max = to
	}
	for n := max; n >= 8; n-- {
		if bytes.Equal(src[to-n:to], token[:n]) {
			return to - n
		}
	}
	return -1
}

func lastIndexFold(b []byte, needle string) int {
	if needle == "" || len(b) < len(needle) {
		return -1
	}
	for i := len(b) - len(needle); i >= 0; i-- {
		if hasFoldPrefix(b[i:], needle) {
			return i
		}
	}
	return -1
}

// normalizeUTF8Spans replaces invalid UTF-8 with U+FFFD and shifts spans.
func normalizeUTF8Spans(s string, spans [][2]int) (string, [][2]int) {
	if utf8.ValidString(s) {
		return s, spans
	}
	mapping := make([]int, len(s)+1)
	var b strings.Builder
	b.Grow(len(s))
	ni := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			mapping[i] = ni
			b.WriteRune('\uFFFD')
			ni += len(string(rune('\uFFFD')))
			i++
			continue
		}
		if size <= 0 {
			size = 1
		}
		for k := 0; k < size && i+k < len(s); k++ {
			mapping[i+k] = ni + k
		}
		b.WriteString(s[i : i+size])
		ni += size
		i += size
	}
	mapping[len(s)] = ni
	out := make([][2]int, len(spans))
	for i, sp := range spans {
		a, b0 := sp[0], sp[1]
		if a < 0 {
			a = 0
		}
		if b0 > len(s) {
			b0 = len(s)
		}
		if a > len(s) {
			a = len(s)
		}
		out[i] = [2]int{mapping[a], mapping[b0]}
	}
	return b.String(), out
}

func countOverlapping(spans [][2]int, from, to int) int {
	n := 0
	for _, sp := range spans {
		if sp[1] > from && sp[0] < to {
			n++
		}
	}
	return n
}

func redactBytes(src, token []byte) ([]byte, int) {
	return redactBytesAt(src, token, true)
}

func redactBytesAt(src, token []byte, atEOF bool) ([]byte, int) {
	if len(src) == 0 {
		return nil, 0
	}
	spans := matchSpans(src, token, atEOF)
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

func matchSpans(src, token []byte, atEOF bool) [][2]int {
	cands := candidateStarts(src, token)
	var spans [][2]int
	prevEnd := 0
	for _, abs := range cands {
		if abs < prevEnd {
			continue
		}
		s, e, ok := longestMatchAt(src, abs, token, atEOF)
		if !ok || e <= s {
			continue
		}
		spans = append(spans, [2]int{s, e})
		prevEnd = e
	}
	return spans
}

// candidateStarts lists pattern anchors in one pass per needle. Failed matches
// advance to the next anchor instead of rescanning the remainder.
func candidateStarts(src, token []byte) []int {
	var pos []int
	lower := asciiLowerCopy(src)
	pos = addIndexes(pos, lower, "authorization")
	pos = addIndexes(pos, lower, "proxy-authorization")
	pos = addIndexes(pos, lower, "bearer")
	pos = addIndexes(pos, lower, "private-token")
	pos = addIndexes(pos, src, "glpat-")
	pos = addIndexes(pos, src, "://")
	if len(token) > 0 {
		pos = addIndexes(pos, src, string(token))
	}
	if len(pos) == 0 {
		return nil
	}
	sort.Ints(pos)
	out := pos[:0]
	last := -1
	for _, p := range pos {
		if p == last {
			continue
		}
		out = append(out, p)
		last = p
	}
	return out
}

func addIndexes(dst []int, hay []byte, needle string) []int {
	if needle == "" || len(hay) < len(needle) {
		return dst
	}
	nb := []byte(needle)
	from := 0
	for from < len(hay) {
		i := bytes.Index(hay[from:], nb)
		if i < 0 {
			return dst
		}
		dst = append(dst, from+i)
		from += i + 1
	}
	return dst
}

func asciiLowerCopy(src []byte) []byte {
	dst := make([]byte, len(src))
	for i, c := range src {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst[i] = c
	}
	return dst
}

func longestMatchAt(src []byte, i int, token []byte, atEOF bool) (int, int, bool) {
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
	try(parseBearer(src, i, atEOF))
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
	j = consumeAuthCredential(src, j, false)
	if j == start {
		return 0, 0, false
	}
	return i, j, true
}

func parseBearer(src []byte, i int, atEOF bool) (int, int, bool) {
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
	j = consumeAuthCredential(src, j, true)
	if j == start {
		return 0, 0, false
	}
	// A delimiter, or a proven EOF, finishes the value. Bearer abc is still
	// a secret. A short value that runs off a truncated buffer is a scan cut;
	// hideTail withholds that unless this read is the end of the trace.
	if j == len(src) && j-start < 8 && !atEOF {
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
	for j < len(src) && src[j] > ' ' {
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
	passStart := -1
	for j < len(src) {
		c := src[j]
		if c <= ' ' || c == '/' {
			return 0, 0, false
		}
		if c == '@' {
			break
		}
		if c == ':' && passStart < 0 {
			passStart = j + 1
		}
		j++
	}
	if j >= len(src) || src[j] != '@' {
		return 0, 0, false
	}
	if j == userStart {
		return 0, 0, false
	}
	if passStart < 0 {
		return i + 3, j + 1, true
	}
	if passStart-1 == userStart && passStart == j {
		return 0, 0, false
	}
	return i + 3, j + 1, true
}

func parseExactToken(src []byte, i int, token []byte) (int, int, bool) {
	if len(token) == 0 || !bytes.HasPrefix(src[i:], token) {
		return 0, 0, false
	}
	return i, i + len(token), true
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

// consumeAuthCredential reads one Authorization/Bearer value. A quoted value
// runs through its closing delimiter (the quotes stay in the span). An
// escaped quote (`\"` or `\'`) is not a closer. An unclosed quote runs to the
// newline or the end of src. Unquoted Authorization values still stop at
// whitespace or a quote; standalone Bearer tokens keep the token-character set.
func consumeAuthCredential(src []byte, j int, tokenChars bool) int {
	if j >= len(src) {
		return j
	}
	if src[j] == '"' || src[j] == '\'' {
		quote := src[j]
		j++
		for j < len(src) {
			if src[j] == '\n' || src[j] == '\r' {
				return j
			}
			if src[j] == '\\' && j+1 < len(src) && src[j+1] != '\n' && src[j+1] != '\r' {
				j += 2
				continue
			}
			if src[j] == quote {
				return j + 1
			}
			j++
		}
		return j
	}
	if tokenChars {
		for j < len(src) && isTokenChar(src[j]) {
			j++
		}
		return j
	}
	for j < len(src) && src[j] > ' ' && src[j] != '"' && src[j] != '\'' {
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
