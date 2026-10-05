package tools

import (
	"bytes"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

const (
	jobTraceDefaultScan   = 1 << 20
	jobTraceHardScan      = 8 << 20
	jobTraceDefaultOutput = 256 << 10
	jobTraceHardOutput    = 1 << 20
	jobTraceDefaultLine   = 64 << 10
	jobTraceHardLine      = 256 << 10
	jobTraceLookbehind    = 512
	jobTraceCapability    = "readmeta.job_trace.v1"
)

// jobTraceWindow is the raw-source vs redacted-output metadata for one trace read.
// source_* are byte offsets in the original trace, before redaction.
// output_bytes is the length of the returned redacted text.
// total_bytes is null when total_known is false.
type jobTraceWindow struct {
	Selector           string `json:"selector"`
	SourceStart        *int64 `json:"source_start"`
	SourceEndExclusive *int64 `json:"source_end_exclusive"`
	TotalBytes         *int64 `json:"total_bytes"`
	TotalKnown         bool   `json:"total_known"`
	OutputBytes        int    `json:"output_bytes"`
	RedactionCount     int    `json:"redaction_count"`
	ScannedBytes       int64  `json:"scanned_bytes"`
	RangeHonored       bool   `json:"range_honored"`
	TailProven         bool   `json:"tail_proven"`
	ErrorRegionProven  bool   `json:"error_region_proven"`
	LineCapped         bool   `json:"line_capped"`
}

type traceQuery struct {
	selector   string
	scan       int
	output     int
	line       int
	lines      int
	errorMatch string
	start      *int64
	end        *int64
}

func normalizeTraceQuery(in getPipelineJobOutputIn) (traceQuery, error) {
	q := traceQuery{
		selector:   strings.TrimSpace(in.Selector),
		scan:       in.MaxScanBytes,
		output:     in.MaxBytes,
		line:       jobTraceDefaultLine,
		lines:      in.MaxLines,
		errorMatch: in.ErrorMatch,
		start:      in.StartByte,
		end:        in.EndByte,
	}
	if q.selector == "" {
		q.selector = "prefix"
	}
	switch q.selector {
	case "prefix", "tail", "error", "range":
	default:
		return q, fmt.Errorf("invalid job trace selector")
	}
	if in.TruncateLines < 0 || in.MaxLines < 0 || in.MaxBytes < 0 || in.MaxScanBytes < 0 {
		return q, fmt.Errorf("job trace bounds must be >= 0")
	}
	if q.selector == "prefix" && in.TruncateLines > 0 {
		q.lines = in.TruncateLines
	}
	if q.selector != "prefix" && in.TruncateLines > 0 {
		return q, fmt.Errorf("truncate_lines applies only to prefix selection")
	}
	if q.selector != "error" && in.ErrorMatch != "" {
		return q, fmt.Errorf("error_match applies only to error selection")
	}
	if q.selector != "range" && (in.StartByte != nil || in.EndByte != nil) {
		return q, fmt.Errorf("start_byte and end_byte apply only to range selection")
	}
	if q.selector == "range" {
		if in.StartByte == nil || *in.StartByte < 0 {
			return q, fmt.Errorf("range start_byte must be >= 0")
		}
		if in.EndByte != nil && *in.EndByte <= *in.StartByte {
			return q, fmt.Errorf("range end_byte must be > start_byte")
		}
	}
	if q.scan == 0 {
		q.scan = jobTraceDefaultScan
	}
	if q.scan > jobTraceHardScan {
		return q, fmt.Errorf("max_scan_bytes exceeds %d", jobTraceHardScan)
	}
	if q.output == 0 {
		q.output = jobTraceDefaultOutput
	}
	if q.output > jobTraceHardOutput {
		return q, fmt.Errorf("max_bytes exceeds %d", jobTraceHardOutput)
	}
	if q.line > jobTraceHardLine {
		q.line = jobTraceHardLine
	}
	return q, nil
}

type tracePiece struct {
	text       string
	redactions int
	start      *int64
	end        *int64
	lineCapped bool
	outCapped  bool
	proven     bool
	full       bool
}

func finishPiece(text string, spans [][2]int, start, end int64, known bool, maxOut, maxLine int, eof, whole, keepTail bool) tracePiece {
	text, spans = normalizeUTF8Spans(text, spans)
	capped, lineCapped, spans := capTraceLinesTracked(text, spans, maxLine)
	var trimmed string
	var outCapped bool
	keptFrom := 0
	keptTo := 0
	if keepTail {
		trimmed, outCapped = trimOutputTail(capped, maxOut)
		keptFrom = len(capped) - len(trimmed)
		keptTo = len(capped)
	} else {
		trimmed, outCapped = trimOutputBytes(capped, maxOut)
		keptTo = len(trimmed)
	}
	p := tracePiece{
		text:       trimmed,
		redactions: countOverlapping(spans, keptFrom, keptTo),
		lineCapped: lineCapped,
		outCapped:  outCapped,
	}
	if known {
		s, e := start, end
		p.start = &s
		p.end = &e
	}
	p.full = whole && eof && known && start == 0 && !lineCapped && !outCapped
	return p
}

func selectPrefix(data []byte, base int64, lines, maxOut, maxLine int, token string, eof bool) tracePiece {
	start := 0
	end := len(data)
	whole := true
	if lines > 0 {
		end = prefixEnd(data, lines)
		if end < len(data) {
			whole = false
		}
	}
	text, spans := redactRange(data, start, end, token)
	p := finishPiece(text, spans, base+int64(start), base+int64(end), true, maxOut, maxLine, eof, whole && end == len(data), false)
	p.full = p.full && whole
	return p
}

func selectTail(data []byte, base int64, lead, lines, maxOut, maxLine int, token string, eof, proven, hideHead bool) tracePiece {
	if !proven {
		return tracePiece{}
	}
	if lead < 0 {
		lead = 0
	}
	if lead > len(data) {
		lead = len(data)
	}
	start := lead
	if lines > 0 {
		start = lead + tailStart(data[lead:], lines)
	}
	text, spans := redactRangeEdges(data, start, len(data), token, hideHead, false)
	whole := start == 0 && lead == 0 && eof
	p := finishPiece(text, spans, base+int64(start), base+int64(len(data)), true, maxOut, maxLine, eof || proven, whole, true)
	p.proven = proven
	if p.outCapped || p.lineCapped {
		p.full = false
	}
	return p
}

func selectError(data []byte, base int64, match string, maxOut, maxLine int, token string, eof bool) tracePiece {
	s, e, ok := errorSpan(data, match)
	if !ok {
		if eof {
			start, end := base, base+int64(len(data))
			return tracePiece{start: &start, end: &end, full: true}
		}
		return tracePiece{}
	}
	lineClosed := e < len(data) || (e == len(data) && (len(data) == 0 || data[len(data)-1] == '\n' || eof))
	text, spans := redactRange(data, s, e, token)
	p := finishPiece(text, spans, base+int64(s), base+int64(e), true, maxOut, maxLine, eof, s == 0 && e == len(data), false)
	p.proven = lineClosed && !p.lineCapped && !p.outCapped
	return p
}

func selectRange(data []byte, observedStart, wantStart int64, wantEnd *int64, maxOut, maxLine int, token string, sizeKnown bool, total int64) tracePiece {
	if observedStart > wantStart {
		return tracePiece{}
	}
	start := int(wantStart - observedStart)
	if start > len(data) {
		return tracePiece{}
	}
	end := len(data)
	if wantEnd != nil {
		rel := int(*wantEnd - observedStart)
		if rel < start {
			return tracePiece{}
		}
		if rel < end {
			end = rel
		}
	}
	hideHead := start == 0 && observedStart > 0
	observedEnd := observedStart + int64(len(data))
	reachedEnd := sizeKnown && observedEnd == total
	hideTail := end == len(data) && !reachedEnd
	text, spans := redactRangeEdges(data, start, end, token, hideHead, hideTail)
	windowEnd := observedStart + int64(end)
	// A 206 body ends when the requested bytes end. That is the full trace
	// only when the observed window is exactly [0, total).
	coversTotal := sizeKnown && wantStart == 0 && windowEnd == total
	return finishPiece(text, spans, wantStart, windowEnd, true, maxOut, maxLine, coversTotal, coversTotal, false)
}

func prefixEnd(data []byte, lines int) int {
	if lines <= 0 {
		return len(data)
	}
	seen := 0
	for i, c := range data {
		if c == '\n' {
			seen++
			if seen == lines {
				return i
			}
		}
	}
	return len(data)
}

func tailStart(data []byte, lines int) int {
	if lines <= 0 || len(data) == 0 {
		return 0
	}
	end := len(data)
	if data[end-1] == '\n' {
		end--
	}
	n := 0
	for i := end - 1; i >= 0; i-- {
		if data[i] == '\n' {
			n++
			if n == lines {
				return i + 1
			}
		}
	}
	return 0
}

func errorSpan(data []byte, literal string) (int, int, bool) {
	idx := -1
	if literal != "" {
		idx = bytes.Index(data, []byte(literal))
	} else {
		for _, m := range [][]byte{[]byte("ERROR"), []byte("FATAL"), []byte("panic:"), []byte("Traceback")} {
			i := bytes.Index(data, m)
			if i >= 0 && (idx < 0 || i < idx) {
				idx = i
			}
		}
	}
	if idx < 0 {
		return 0, 0, false
	}
	lineStart := 0
	if prev := bytes.LastIndexByte(data[:idx], '\n'); prev >= 0 {
		lineStart = prev + 1
	}
	lineEnd := len(data)
	if rel := bytes.IndexByte(data[idx:], '\n'); rel >= 0 {
		lineEnd = idx + rel + 1
	}
	s := lineStart
	for n := 0; n < 2 && s > 0; n++ {
		prev := bytes.LastIndexByte(data[:s-1], '\n')
		if prev < 0 {
			s = 0
			break
		}
		s = prev + 1
	}
	e := lineEnd
	for n := 0; n < 2 && e < len(data); n++ {
		rel := bytes.IndexByte(data[e:], '\n')
		if rel < 0 {
			e = len(data)
			break
		}
		e = e + rel + 1
	}
	return s, e, true
}

func capTraceLinesTracked(s string, spans [][2]int, maxLine int) (string, bool, [][2]int) {
	if utf8.ValidString(s) && !lineExceeds(s, maxLine) {
		return s, false, spans
	}
	if maxLine <= 0 {
		maxLine = jobTraceDefaultLine
	}
	var b strings.Builder
	capped := false
	lineLen := 0
	var copied []struct{ src, dst, n int }
	dst := 0
	for i := 0; i < len(s); {
		if s[i] == '\n' {
			b.WriteByte('\n')
			copied = append(copied, struct{ src, dst, n int }{i, dst, 1})
			dst++
			lineLen = 0
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		if size <= 0 {
			size = 1
		}
		if lineLen >= maxLine || lineLen+size > maxLine {
			capped = true
			for i < len(s) && s[i] != '\n' {
				i++
			}
			marker := "…[line_capped]"
			b.WriteString(marker)
			dst += len(marker)
			continue
		}
		b.WriteString(s[i : i+size])
		copied = append(copied, struct{ src, dst, n int }{i, dst, size})
		dst += size
		lineLen += size
		i += size
	}
	if !capped {
		return b.String(), false, spans
	}
	return b.String(), true, remapSpans(spans, copied)
}

func lineExceeds(s string, maxLine int) bool {
	if maxLine <= 0 {
		maxLine = jobTraceDefaultLine
	}
	lineLen := 0
	for i := 0; i < len(s); {
		if s[i] == '\n' {
			lineLen = 0
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		if size <= 0 {
			size = 1
		}
		if lineLen >= maxLine || lineLen+size > maxLine {
			return true
		}
		lineLen += size
		i += size
	}
	return false
}

func remapSpans(spans [][2]int, copied []struct{ src, dst, n int }) [][2]int {
	if len(spans) == 0 || len(copied) == 0 {
		return nil
	}
	out := make([][2]int, 0, len(spans))
	for _, sp := range spans {
		dstS, dstE := -1, -1
		for _, c := range copied {
			a, b := c.src, c.src+c.n
			if b <= sp[0] || a >= sp[1] {
				continue
			}
			ovS, ovE := a, b
			if sp[0] > ovS {
				ovS = sp[0]
			}
			if sp[1] < ovE {
				ovE = sp[1]
			}
			d0 := c.dst + (ovS - c.src)
			d1 := c.dst + (ovE - c.src)
			if dstS < 0 || d0 < dstS {
				dstS = d0
			}
			if d1 > dstE {
				dstE = d1
			}
		}
		if dstS >= 0 && dstE > dstS {
			out = append(out, [2]int{dstS, dstE})
		}
	}
	return out
}

func capTraceLines(s string, maxLine int) (string, bool) {
	if maxLine <= 0 {
		maxLine = jobTraceDefaultLine
	}
	var b strings.Builder
	capped := false
	lineLen := 0
	for i := 0; i < len(s); {
		if s[i] == '\n' {
			b.WriteByte('\n')
			lineLen = 0
			i++
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		if size <= 0 {
			size = 1
		}
		if lineLen >= maxLine || lineLen+size > maxLine {
			capped = true
			for i < len(s) && s[i] != '\n' {
				i++
			}
			b.WriteString("…[line_capped]")
			continue
		}
		b.WriteString(s[i : i+size])
		lineLen += size
		i += size
	}
	return b.String(), capped
}

func newJobTraceSection(now time.Time) readmeta.Section {
	sec := readmeta.NewMRDiffsSection(now)
	sec.CapabilityVersion = jobTraceCapability
	sec.HeadSHA = nil
	sec.Consistency = readmeta.ConsistencyUnknown
	sec.PaginationExhausted = false
	sec.NextCursor = nil
	sec.ContentComplete = readmeta.ContentCompleteUnknown
	sec.ManifestCoverage = readmeta.CoverageUnknown
	sec.PatchCoverage = readmeta.CoverageUnknown
	return sec
}
