package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	diffModeManifest = "manifest"
	diffModeContent  = "content"

	capabilityDiffContentV1    = "readmeta.diff_content.v1"
	diffContentWindowHashScope = "diff_content.window_text.v1"
	diffContentReturnHashScope = "diff_content.returned_windows_concat.v1"

	diffContentMaxCandidateBytes = 1 << 20
	diffContentMaxSelectedBytes  = 1 << 20

	diffFileStatusText        = "text"
	diffFileStatusBinary      = "binary"
	diffFileStatusSubmodule   = "submodule"
	diffFileStatusModeOnly    = "mode_only"
	diffFileStatusCollapsed   = "collapsed"
	diffFileStatusTooLarge    = "too_large"
	diffFileStatusAbsent      = "absent"
	diffFileStatusUnsupported = "unsupported"
	diffFileStatusMalformed   = "malformed"
	diffFileStatusUnavailable = "unavailable"
	diffFileStatusMetadata    = "metadata"

	diffSelectorMatched     = "matched"
	diffSelectorAbsent      = "absent"
	diffSelectorAmbiguous   = "ambiguous"
	diffSelectorCoalesced   = "coalesced"
	diffSelectorUnobserved  = "unobserved"
	diffSelectorUnavailable = "unavailable"

	diffLineKindContext  = "context"
	diffLineKindAddition = "addition"
	diffLineKindDeletion = "deletion"
	diffLineKindMarker   = "marker"
)

type diffContentOpts struct {
	Paths           []string
	ContextLines    int
	MaxLines        int
	MaxContentBytes int
}

type diffContentHash struct {
	Algorithm string `json:"algorithm"`
	Scope     string `json:"scope"`
	Value     string `json:"value"`
}

type diffContentSelectionOut struct {
	ProjectID       string  `json:"project_id"`
	MergeRequestIID int64   `json:"merge_request_iid"`
	Kind            string  `json:"kind"`
	VersionID       *int64  `json:"version_id"`
	BaseSHA         *string `json:"base_sha"`
	StartSHA        *string `json:"start_sha"`
	HeadSHA         *string `json:"head_sha"`
	FromSHA         *string `json:"from_sha"`
	ToSHA           *string `json:"to_sha"`
	Straight        *bool   `json:"straight"`
}

type diffContentLine struct {
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	OldLine   *int   `json:"old_line"`
	NewLine   *int   `json:"new_line"`
	NoNewline bool   `json:"no_newline,omitempty"`
}

type diffContentWindow struct {
	Header     string            `json:"header"`
	OldStart   int               `json:"old_start"`
	OldCount   int               `json:"old_count"`
	NewStart   int               `json:"new_start"`
	NewCount   int               `json:"new_count"`
	Text       string            `json:"text"`
	Lines      []diffContentLine `json:"lines"`
	WindowHash diffContentHash   `json:"window_hash"`
	Truncated  bool              `json:"truncated"`
	// parserCoords retains immutable parser-assigned old/new line numbers
	// parallel to Lines. Unexported so it is not part of the serialized MCP
	// window API; proofs are provider-constructed and never rebuilt from input.
	parserCoords []diffParserLineCoords
}

// diffParserLineCoords is the parser-derived coordinate pair for one emitted body line.
type diffParserLineCoords struct {
	oldLine *int
	newLine *int
}

type diffContentFile struct {
	OldPath       *string             `json:"old_path"`
	NewPath       *string             `json:"new_path"`
	AMode         *string             `json:"a_mode"`
	BMode         *string             `json:"b_mode"`
	NewFile       *bool               `json:"new_file"`
	RenamedFile   *bool               `json:"renamed_file"`
	DeletedFile   *bool               `json:"deleted_file"`
	GeneratedFile *bool               `json:"generated_file"`
	Collapsed     *bool               `json:"collapsed"`
	TooLarge      *bool               `json:"too_large"`
	Binary        *bool               `json:"binary"`
	Submodule     *bool               `json:"submodule"`
	Status        string              `json:"status"`
	Windows       []diffContentWindow `json:"windows,omitempty"`
	Limitations   []string            `json:"limitations,omitempty"`
}

type diffSelectorOutcome struct {
	Path    string  `json:"path"`
	Status  string  `json:"status"`
	OldPath *string `json:"old_path,omitempty"`
	NewPath *string `json:"new_path,omitempty"`
}

type diffContentOut struct {
	Section             readmeta.Section        `json:"section"`
	Selection           diffContentSelectionOut `json:"selection"`
	Files               []diffContentFile       `json:"files"`
	Selectors           []diffSelectorOutcome   `json:"selectors"`
	ReturnedContentHash *diffContentHash        `json:"returned_content_hash"`
	FullPatchHash       *diffContentHash        `json:"full_patch_hash"`
	Provenance          *diffCacheProvenance    `json:"provenance,omitempty"`
}

type retainedDiffFile struct {
	entry      diffManifestEntry
	patch      string
	patchOK    bool
	overCap    bool
	dupKey     bool
	wrongType  bool
	pathBad    bool
	sourceHash string
}

type diffParsedLine struct {
	kind      string
	text      string
	raw       string
	oldLine   *int
	newLine   *int
	noNewline bool
	changed   bool
}

type diffParsedHunk struct {
	header   string
	oldStart int
	oldCount int
	newStart int
	newCount int
	lines    []diffParsedLine
}

type diffParsedPatch struct {
	hunks []diffParsedHunk
	ok    bool
	empty bool
}

type contentEmitBudget struct {
	maxLines int
	maxBytes int
	lines    int
	bytes    int
}

func (b *contentEmitBudget) can(lineCount, byteCount int) bool {
	return b.lines+lineCount <= b.maxLines && b.bytes+byteCount <= b.maxBytes
}

func (b *contentEmitBudget) add(lineCount, byteCount int) {
	b.lines += lineCount
	b.bytes += byteCount
}

func normalizeDiffWindowMode(in diffWindowIn) (string, diffContentOpts, error) {
	if in.Cursor != nil && strings.TrimSpace(*in.Cursor) == "" {
		return "", diffContentOpts{}, fmt.Errorf("cursor must not be blank")
	}
	mode := diffModeManifest
	if in.Mode != nil {
		m := strings.TrimSpace(*in.Mode)
		if m == "" {
			return "", diffContentOpts{}, fmt.Errorf("mode must be manifest or content")
		}
		switch m {
		case diffModeManifest:
			mode = diffModeManifest
		case diffModeContent:
			mode = diffModeContent
		default:
			return "", diffContentOpts{}, fmt.Errorf("mode must be manifest or content")
		}
	}
	hasContentFields := in.Paths != nil || in.ContextLines != nil || in.MaxLines != nil || in.MaxContentBytes != nil
	if mode == diffModeManifest {
		if hasContentFields {
			return "", diffContentOpts{}, fmt.Errorf("content fields are only valid when mode=content")
		}
		return mode, diffContentOpts{}, nil
	}
	if in.Cursor != nil && strings.TrimSpace(*in.Cursor) != "" {
		return "", diffContentOpts{}, fmt.Errorf("content mode does not support cursor")
	}
	opts, err := normalizeDiffContentOpts(in)
	if err != nil {
		return "", diffContentOpts{}, err
	}
	return mode, opts, nil
}

func normalizeDiffContentOpts(in diffWindowIn) (diffContentOpts, error) {
	per := 20
	if in.PerPage != nil {
		per = *in.PerPage
	}
	if per < 1 || per > 50 {
		return diffContentOpts{}, fmt.Errorf("per_page must be 1..50")
	}
	if len(in.Paths) < 1 || len(in.Paths) > per {
		return diffContentOpts{}, fmt.Errorf("paths must contain 1..per_page distinct exact repository-relative strings")
	}
	seen := make(map[string]struct{}, len(in.Paths))
	paths := make([]string, 0, len(in.Paths))
	for _, p := range in.Paths {
		if err := validateDiffContentPath(p); err != nil {
			return diffContentOpts{}, err
		}
		if _, ok := seen[p]; ok {
			return diffContentOpts{}, fmt.Errorf("paths must be distinct")
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	ctxLines := 3
	if in.ContextLines != nil {
		ctxLines = *in.ContextLines
	}
	if ctxLines < 0 || ctxLines > 20 {
		return diffContentOpts{}, fmt.Errorf("context_lines must be 0..20")
	}
	maxLines := 1000
	if in.MaxLines != nil {
		maxLines = *in.MaxLines
	}
	if maxLines < 1 || maxLines > 10000 {
		return diffContentOpts{}, fmt.Errorf("max_lines must be 1..10000")
	}
	maxBytes := 262144
	if in.MaxContentBytes != nil {
		maxBytes = *in.MaxContentBytes
	}
	if maxBytes < 1 || maxBytes > 1048576 {
		return diffContentOpts{}, fmt.Errorf("max_content_bytes must be 1..1048576")
	}
	return diffContentOpts{Paths: paths, ContextLines: ctxLines, MaxLines: maxLines, MaxContentBytes: maxBytes}, nil
}

func validateDiffContentPath(p string) error {
	if p == "" {
		return fmt.Errorf("paths must not be empty")
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("paths must not contain NUL")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("paths must be repository-relative")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("paths must not contain dot segments")
		}
	}
	return nil
}

func newDiffContentSection(retrievedAt string) readmeta.Section {
	return readmeta.Section{
		RetrievedAt:         retrievedAt,
		Source:              readmeta.SourceGitLabREST,
		Provider:            readmeta.ProviderGitLab,
		CapabilityVersion:   capabilityDiffContentV1,
		Limitations:         []readmeta.Limitation{},
		ContentComplete:     readmeta.ContentCompleteUnknown,
		Consistency:         readmeta.ConsistencyUnknown,
		ManifestCoverage:    readmeta.CoverageUnknown,
		PatchCoverage:       readmeta.CoverageUnknown,
		Counts:              readmeta.Counts{},
		PaginationExhausted: true,
		NextCursor:          nil,
	}
}

func selectionOutFrom(q diffQuery, proved provedManifest, versionID int64) diffContentSelectionOut {
	out := diffContentSelectionOut{
		ProjectID:       q.Project,
		MergeRequestIID: q.IID,
		Kind:            q.Selection.Mode,
	}
	switch q.Selection.Mode {
	case diffModeVersion:
		id := versionID
		if id < 1 {
			id = q.Selection.VersionID
		}
		out.VersionID = &id
		if proved.Head != "" {
			h, b, s := proved.Head, proved.Base, proved.Start
			out.HeadSHA, out.BaseSHA, out.StartSHA = &h, &b, &s
		}
	case diffModeTuple:
		b, s, h := q.Selection.Base, q.Selection.Start, q.Selection.Head
		out.BaseSHA, out.StartSHA, out.HeadSHA = &b, &s, &h
		if versionID > 0 {
			id := versionID
			out.VersionID = &id
		}
		if proved.Head != "" {
			h2, b2, s2 := proved.Head, proved.Base, proved.Start
			out.HeadSHA, out.BaseSHA, out.StartSHA = &h2, &b2, &s2
		}
	case diffModeIncremental:
		f, t := q.Selection.From, q.Selection.To
		st := true
		out.FromSHA, out.ToSHA, out.Straight = &f, &t, &st
	}
	return out
}

func hashDiffContentText(scope, text string) diffContentHash {
	sum := sha256.Sum256([]byte(text))
	return diffContentHash{Algorithm: "sha256", Scope: scope, Value: hex.EncodeToString(sum[:])}
}

func sourcePatchDigest(patch string) string {
	sum := sha256.Sum256([]byte(patch))
	return hex.EncodeToString(sum[:])
}

type diffSourceLine struct {
	text  string
	raw   string
	hadNL bool
}

func splitDiffSourceLines(patch string) []diffSourceLine {
	var out []diffSourceLine
	for len(patch) > 0 {
		idx := strings.IndexByte(patch, '\n')
		if idx < 0 {
			out = append(out, diffSourceLine{text: strings.TrimSuffix(patch, "\r"), raw: patch, hadNL: false})
			break
		}
		raw := patch[:idx+1]
		text := patch[:idx]
		if strings.HasSuffix(text, "\r") {
			text = text[:len(text)-1]
		}
		out = append(out, diffSourceLine{text: text, raw: raw, hadNL: true})
		patch = patch[idx+1:]
	}
	return out
}

func parseHunkHeader(header string) (oldStart, oldCount, newStart, newCount int, ok bool) {
	raw := strings.TrimRight(header, "\r\n")
	if !strings.HasPrefix(raw, "@@") {
		return 0, 0, 0, 0, false
	}
	rest := strings.TrimPrefix(raw, "@@")
	parts := strings.SplitN(rest, "@@", 2)
	if len(parts) < 2 {
		return 0, 0, 0, 0, false
	}
	ranges := strings.Fields(strings.TrimSpace(parts[0]))
	if len(ranges) != 2 {
		return 0, 0, 0, 0, false
	}
	oStart, oCount, ook := parseDiffRange(ranges[0], '-')
	nStart, nCount, nok := parseDiffRange(ranges[1], '+')
	if !ook || !nok {
		return 0, 0, 0, 0, false
	}
	return oStart, oCount, nStart, nCount, true
}

func parseDiffRange(tok string, sign byte) (start, count int, ok bool) {
	if len(tok) < 2 || tok[0] != sign {
		return 0, 0, false
	}
	body := tok[1:]
	var s, c int
	if i := strings.IndexByte(body, ','); i >= 0 {
		var err1, err2 error
		s, err1 = strconv.Atoi(body[:i])
		c, err2 = strconv.Atoi(body[i+1:])
		if err1 != nil || err2 != nil || s < 0 || c < 0 {
			return 0, 0, false
		}
	} else {
		var err error
		s, err = strconv.Atoi(body)
		if err != nil || s < 0 {
			return 0, 0, false
		}
		c = 1
	}
	if s == 0 && c > 0 {
		return 0, 0, false
	}
	if c > 0 && s > math.MaxInt-c+1 {
		return 0, 0, false
	}
	return s, c, true
}

func checkedInc(pos int) (int, bool) {
	if pos == math.MaxInt {
		return 0, false
	}
	return pos + 1, true
}

func isDiffFramingLine(line string) bool {
	switch {
	case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "diff "),
		strings.HasPrefix(line, "index "), strings.HasPrefix(line, "old mode "), strings.HasPrefix(line, "new mode "),
		strings.HasPrefix(line, "similarity index "), strings.HasPrefix(line, "rename from "), strings.HasPrefix(line, "rename to "),
		strings.HasPrefix(line, "new file mode "), strings.HasPrefix(line, "deleted file mode "),
		line == "GIT binary patch", strings.HasPrefix(line, "Binary files "):
		return true
	default:
		return false
	}
}

func checkedAdd(a, b int) (int, bool) {
	if b < 0 {
		return 0, false
	}
	if a > math.MaxInt-b {
		return 0, false
	}
	return a + b, true
}

func advanceHunkBoundary(start, count int, haveEnd bool, lastNext int) (int, bool, bool) {
	// Normalize both positive and zero-count hunks to a next-line coordinate:
	//   positive [start, start+count) → next = start+count; require start >= lastNext
	//   zero-count boundary-after start → next = start+1; require next >= lastNext
	// Zero at MaxInt fails closed (no wrap). Equal next is allowed (same insertion point).
	var next int
	if count == 0 {
		n, ok := checkedAdd(start, 1)
		if !ok {
			return 0, false, false
		}
		next = n
		if haveEnd && next < lastNext {
			return 0, false, false
		}
		return next, true, true
	}
	end, ok := checkedAdd(start, count)
	if !ok {
		return 0, false, false
	}
	if haveEnd && start < lastNext {
		return 0, false, false
	}
	return end, true, true
}

func parseUnifiedDiff(patch string) diffParsedPatch {
	if patch == "" {
		return diffParsedPatch{empty: true, ok: true}
	}
	if !utf8.ValidString(patch) {
		return diffParsedPatch{ok: false}
	}
	lines := splitDiffSourceLines(patch)
	var hunks []diffParsedHunk
	var lastOldEnd, lastNewEnd int
	haveOldEnd, haveNewEnd := false, false
	i := 0
	for i < len(lines) {
		line := lines[i].text
		if isDiffFramingLine(line) {
			i++
			continue
		}
		if !strings.HasPrefix(line, "@@") {
			return diffParsedPatch{ok: false}
		}
		oldStart, oldCount, newStart, newCount, ok := parseHunkHeader(line)
		if !ok {
			return diffParsedPatch{ok: false}
		}
		var okBound bool
		lastOldEnd, haveOldEnd, okBound = advanceHunkBoundary(oldStart, oldCount, haveOldEnd, lastOldEnd)
		if !okBound {
			return diffParsedPatch{ok: false}
		}
		lastNewEnd, haveNewEnd, okBound = advanceHunkBoundary(newStart, newCount, haveNewEnd, lastNewEnd)
		if !okBound {
			return diffParsedPatch{ok: false}
		}
		h := diffParsedHunk{header: lines[i].raw, oldStart: oldStart, oldCount: oldCount, newStart: newStart, newCount: newCount}
		i++
		oldPos, newPos := oldStart, newStart
		oldSeen, newSeen := 0, 0
		sawMarker := false
		for i < len(lines) {
			cur := lines[i]
			body := cur.text
			if strings.HasPrefix(body, "@@") {
				break
			}
			if body == `\ No newline at end of file` {
				if len(h.lines) == 0 || h.lines[len(h.lines)-1].kind == diffLineKindMarker {
					return diffParsedPatch{ok: false}
				}
				if sawMarker {
					return diffParsedPatch{ok: false}
				}
				h.lines[len(h.lines)-1].noNewline = true
				h.lines = append(h.lines, diffParsedLine{kind: diffLineKindMarker, text: body, raw: cur.raw})
				sawMarker = true
				i++
				continue
			}
			sawMarker = false
			var pl diffParsedLine
			pl.raw = cur.raw
			switch {
			case strings.HasPrefix(body, "+"):
				n := newPos
				pl.kind = diffLineKindAddition
				pl.text = body
				pl.newLine = &n
				pl.changed = true
				var okInc bool
				newPos, okInc = checkedInc(newPos)
				if !okInc {
					return diffParsedPatch{ok: false}
				}
				newSeen++
			case strings.HasPrefix(body, "-"):
				o := oldPos
				pl.kind = diffLineKindDeletion
				pl.text = body
				pl.oldLine = &o
				pl.changed = true
				var okInc bool
				oldPos, okInc = checkedInc(oldPos)
				if !okInc {
					return diffParsedPatch{ok: false}
				}
				oldSeen++
			case strings.HasPrefix(body, " "):
				o, n := oldPos, newPos
				pl.kind = diffLineKindContext
				pl.text = body
				pl.oldLine = &o
				pl.newLine = &n
				var okOld, okNew bool
				oldPos, okOld = checkedInc(oldPos)
				newPos, okNew = checkedInc(newPos)
				if !okOld || !okNew {
					return diffParsedPatch{ok: false}
				}
				oldSeen++
				newSeen++
			default:
				return diffParsedPatch{ok: false}
			}
			h.lines = append(h.lines, pl)
			i++
		}
		if oldCount > 0 && oldSeen != oldCount {
			return diffParsedPatch{ok: false}
		}
		if newCount > 0 && newSeen != newCount {
			return diffParsedPatch{ok: false}
		}
		if oldCount == 0 && oldSeen != 0 {
			return diffParsedPatch{ok: false}
		}
		if newCount == 0 && newSeen != 0 {
			return diffParsedPatch{ok: false}
		}
		hunks = append(hunks, h)
	}
	return diffParsedPatch{hunks: hunks, ok: true, empty: len(hunks) == 0}
}

type lineRange struct{ start, end int }

func changedRanges(lines []diffParsedLine, contextLines int) []lineRange {
	var changed []int
	for i, ln := range lines {
		if ln.changed {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	var raw []lineRange
	runStart, prev := changed[0], changed[0]
	for _, idx := range changed[1:] {
		if idx == prev+1 || (idx == prev+2 && lines[prev+1].kind == diffLineKindMarker) {
			prev = idx
			continue
		}
		raw = append(raw, expandRange(lines, runStart, prev, contextLines))
		runStart, prev = idx, idx
	}
	raw = append(raw, expandRange(lines, runStart, prev, contextLines))
	return mergeRanges(raw)
}

func expandRange(lines []diffParsedLine, start, end, contextLines int) lineRange {
	// Expand by source-line context (markers do not consume context budget).
	s := start
	need := contextLines
	for s > 0 && need > 0 {
		s--
		if lines[s].kind == diffLineKindMarker {
			continue
		}
		need--
	}
	for s > 0 && lines[s-1].kind == diffLineKindMarker {
		s--
	}
	e := end
	need = contextLines
	for e+1 < len(lines) && need > 0 {
		next := lines[e+1]
		if next.kind == diffLineKindMarker {
			e++
			continue
		}
		e++
		need--
	}
	for e+1 < len(lines) && lines[e+1].kind == diffLineKindMarker {
		e++
	}
	if s > e {
		return lineRange{start: start, end: end}
	}
	return lineRange{start: s, end: e}
}

func mergeRanges(in []lineRange) []lineRange {
	if len(in) == 0 {
		return nil
	}
	out := []lineRange{in[0]}
	for _, rg := range in[1:] {
		last := &out[len(out)-1]
		if rg.start <= last.end+1 {
			if rg.end > last.end {
				last.end = rg.end
			}
			continue
		}
		out = append(out, rg)
	}
	return out
}

func buildWindow(hunk diffParsedHunk, rg lineRange, budget *contentEmitBudget) (diffContentWindow, bool, bool) {
	headerBytes := len(hunk.header)
	if !budget.can(1, headerBytes) {
		return diffContentWindow{}, false, true
	}
	var b strings.Builder
	b.WriteString(hunk.header)
	budget.add(1, headerBytes)

	var lines []diffContentLine
	var parserCoords []diffParserLineCoords
	truncated := false
	for i := rg.start; i <= rg.end; {
		ln := hunk.lines[i]
		if ln.kind == diffLineKindMarker {
			return diffContentWindow{}, false, false // orphan/detached marker in range
		}
		unit := ln.raw
		unitLines := 1
		j := i + 1
		for j <= rg.end && j < len(hunk.lines) && hunk.lines[j].kind == diffLineKindMarker {
			unit += hunk.lines[j].raw
			unitLines++
			j++
		}
		if !budget.can(unitLines, len(unit)) {
			truncated = true
			break
		}
		budget.add(unitLines, len(unit))
		b.WriteString(unit)
		out := diffContentLine{Kind: ln.kind, Text: ln.text, OldLine: cloneIntPtr(ln.oldLine), NewLine: cloneIntPtr(ln.newLine), NoNewline: ln.noNewline || j > i+1}
		lines = append(lines, out)
		// Retain a separate immutable copy of parser offsets for proof binding.
		parserCoords = append(parserCoords, diffParserLineCoords{
			oldLine: cloneIntPtr(ln.oldLine),
			newLine: cloneIntPtr(ln.newLine),
		})
		i = j
	}
	if len(lines) == 0 {
		return diffContentWindow{}, false, truncated
	}
	text := b.String()
	return diffContentWindow{
		Header: hunk.header, OldStart: hunk.oldStart, OldCount: hunk.oldCount, NewStart: hunk.newStart, NewCount: hunk.newCount,
		Text: text, Lines: lines, WindowHash: hashDiffContentText(diffContentWindowHashScope, text), Truncated: truncated,
		parserCoords: parserCoords,
	}, true, truncated
}

func cloneIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	x := *v
	return &x
}

func selectDiffWindows(parsed diffParsedPatch, contextLines int, budget *contentEmitBudget) ([]diffContentWindow, bool, bool, bool) {
	if !parsed.ok {
		return nil, false, false, false
	}
	var windows []diffContentWindow
	truncated := false
	omittedContext := false
	for _, hunk := range parsed.hunks {
		ranges := changedRanges(hunk.lines, contextLines)
		included := map[int]bool{}
		for _, rg := range ranges {
			for i := rg.start; i <= rg.end; i++ {
				included[i] = true
			}
		}
		for i, ln := range hunk.lines {
			if ln.kind == diffLineKindContext && !included[i] {
				omittedContext = true
				break
			}
		}
		for _, rg := range ranges {
			w, ok, crop := buildWindow(hunk, rg, budget)
			if !ok {
				if crop {
					return windows, true, true, omittedContext
				}
				// Non-crop rejection (e.g. detached/orphan marker in range): fail the
				// file selection so callers clear all windows via malformed handling.
				// Do not silently skip and relabel lost changes as omitted_context.
				return nil, false, false, false
			}
			windows = append(windows, w)
			if crop {
				return windows, true, true, omittedContext
			}
		}
	}
	return windows, true, truncated, omittedContext
}

func boolVal(v *bool) bool { return v != nil && *v }

func isGitlinkMode(v *string) bool { return v != nil && *v == "160000" }

func patchHasBinaryFraming(patch string) bool {
	for _, ln := range splitDiffSourceLines(patch) {
		if strings.HasPrefix(ln.text, "@@") {
			return false
		}
		if ln.text == "GIT binary patch" || strings.HasPrefix(ln.text, "Binary files ") {
			return true
		}
	}
	return false
}

func classifyDiffFile(entry diffManifestEntry, patch string, patchOK, overCap, dupKey, pathBad, wrongType bool, parsed diffParsedPatch) (string, []string) {
	if pathBad || dupKey || wrongType {
		return diffFileStatusMalformed, []string{"malformed entry"}
	}
	if boolVal(entry.Binary) || patchHasBinaryFraming(patch) {
		return diffFileStatusBinary, []string{"binary"}
	}
	if boolVal(entry.Submodule) || isGitlinkMode(entry.AMode) || isGitlinkMode(entry.BMode) {
		return diffFileStatusSubmodule, []string{"submodule"}
	}
	if boolVal(entry.Collapsed) {
		return diffFileStatusCollapsed, []string{"collapsed"}
	}
	if boolVal(entry.TooLarge) || overCap {
		return diffFileStatusTooLarge, []string{"too_large"}
	}
	if !patchOK {
		return diffFileStatusUnavailable, []string{"patch unavailable"}
	}
	if !parsed.ok {
		return diffFileStatusMalformed, []string{"malformed patch"}
	}
	if parsed.empty {
		if boolVal(entry.RenamedFile) || (pathStr(entry.AMode) != "" && pathStr(entry.BMode) != "" && pathStr(entry.AMode) != pathStr(entry.BMode)) {
			return diffFileStatusModeOnly, []string{"metadata only"}
		}
		if patch == "" {
			return diffFileStatusMetadata, []string{"empty patch"}
		}
		return diffFileStatusUnsupported, []string{"no hunks"}
	}
	return diffFileStatusText, nil
}

func matchSelectors(paths []string, files []retainedDiffFile, completeManifest bool) (outcomes []diffSelectorOutcome, selected []int) {
	type hit struct {
		fileIdx int
	}
	bySelector := make(map[string][]hit, len(paths))
	for i, f := range files {
		oldP, newP := pathStr(f.entry.OldPath), pathStr(f.entry.NewPath)
		for _, sel := range paths {
			if sel == oldP || sel == newP {
				bySelector[sel] = append(bySelector[sel], hit{fileIdx: i})
			}
		}
	}
	seenFile := map[int]struct{}{}
	for _, sel := range paths {
		hits := bySelector[sel]
		switch len(hits) {
		case 0:
			st := diffSelectorUnobserved
			if completeManifest {
				st = diffSelectorAbsent
			}
			outcomes = append(outcomes, diffSelectorOutcome{Path: sel, Status: st})
		case 1:
			idx := hits[0].fileIdx
			if _, ok := seenFile[idx]; ok {
				outcomes = append(outcomes, diffSelectorOutcome{
					Path: sel, Status: diffSelectorCoalesced,
					OldPath: files[idx].entry.OldPath, NewPath: files[idx].entry.NewPath,
				})
				break
			}
			seenFile[idx] = struct{}{}
			selected = append(selected, idx)
			outcomes = append(outcomes, diffSelectorOutcome{
				Path: sel, Status: diffSelectorMatched,
				OldPath: files[idx].entry.OldPath, NewPath: files[idx].entry.NewPath,
			})
		default:
			outcomes = append(outcomes, diffSelectorOutcome{Path: sel, Status: diffSelectorAmbiguous})
		}
	}
	return outcomes, selected
}

func buildDiffContentFiles(files []retainedDiffFile, selected []int, opts diffContentOpts) ([]diffContentFile, *diffContentHash, bool, bool) {
	budget := &contentEmitBudget{maxLines: opts.MaxLines, maxBytes: opts.MaxContentBytes}
	out := make([]diffContentFile, 0, len(selected))
	var concat strings.Builder
	cropped := false
	knownOmit := false
	for _, idx := range selected {
		f := files[idx]
		parsed := parseUnifiedDiff(f.patch)
		status, lims := classifyDiffFile(f.entry, f.patch, f.patchOK, f.overCap, f.dupKey, f.pathBad, f.wrongType, parsed)
		item := diffContentFile{
			OldPath: f.entry.OldPath, NewPath: f.entry.NewPath, AMode: f.entry.AMode, BMode: f.entry.BMode,
			NewFile: f.entry.NewFile, RenamedFile: f.entry.RenamedFile, DeletedFile: f.entry.DeletedFile,
			GeneratedFile: f.entry.GeneratedFile, Collapsed: f.entry.Collapsed, TooLarge: f.entry.TooLarge,
			Binary: f.entry.Binary, Submodule: f.entry.Submodule, Status: status, Limitations: lims,
		}
		if status == diffFileStatusText {
			wins, ok, trunc, omitted := selectDiffWindows(parsed, opts.ContextLines, budget)
			if !ok {
				item.Status = diffFileStatusMalformed
				item.Limitations = append(item.Limitations, "malformed patch")
				knownOmit = true
			} else {
				item.Windows = wins
				for _, w := range wins {
					concat.WriteString(w.Text)
				}
				if trunc {
					cropped = true
					item.Limitations = append(item.Limitations, "cropped")
				}
				if omitted {
					knownOmit = true
					item.Limitations = append(item.Limitations, "omitted_context")
					for i := range item.Windows {
						item.Windows[i].Truncated = true
					}
				}
			}
		} else {
			knownOmit = true
		}
		out = append(out, item)
	}
	var retHash *diffContentHash
	if concat.Len() > 0 {
		h := hashDiffContentText(diffContentReturnHashScope, concat.String())
		retHash = &h
	}
	return out, retHash, cropped, knownOmit
}

func absentAll(paths []string) []diffSelectorOutcome {
	out := make([]diffSelectorOutcome, 0, len(paths))
	for _, p := range paths {
		out = append(out, diffSelectorOutcome{Path: p, Status: diffSelectorAbsent})
	}
	return out
}

func unobservedAll(paths []string) []diffSelectorOutcome {
	out := make([]diffSelectorOutcome, 0, len(paths))
	for _, p := range paths {
		out = append(out, diffSelectorOutcome{Path: p, Status: diffSelectorUnobserved})
	}
	return out
}

func retainedFromStream(st objectStream) []retainedDiffFile {
	out := make([]retainedDiffFile, 0, len(st.entries))
	for i, e := range st.entries {
		rf := retainedDiffFile{entry: e, pathBad: st.pathBad}
		if i < len(st.patches) {
			p := st.patches[i]
			rf.dupKey = p.dupKey
			rf.overCap = p.overCap
			rf.wrongType = p.wrongType
			if p.retained {
				rf.patch = p.text
				rf.patchOK = true
				rf.sourceHash = sourcePatchDigest(p.text)
			}
		}
		out = append(out, rf)
	}
	return out
}

func readBoundedDiffContent(ctx context.Context, d Deps, q diffQuery, opts diffContentOpts) (diffContentOut, error) {
	sec := newDiffContentSection(newDiffSection(q.Now).RetrievedAt)
	switch q.Selection.Mode {
	case diffModeVersion:
		return readVersionContent(ctx, d, q, sec, opts, q.Selection.VersionID)
	case diffModeTuple:
		id, err := selectTupleVersion(ctx, d, q)
		if err != nil {
			if passthroughTypedProviderErr(err) {
				return diffContentOut{}, err
			}
			sec.AddLimitation(readmeta.CodeProviderPageAmbiguous, "version list")
			sec.ContentComplete = readmeta.ContentCompleteUnknown
			return emptyContent(sec, q, opts), nil
		}
		if id == 0 {
			sec.AddLimitation(readmeta.CodePartial, "version tuple not unique")
			sec.ContentComplete = readmeta.ContentCompleteFalse
			return emptyContent(sec, q, opts), nil
		}
		return readVersionContent(ctx, d, q, sec, opts, id)
	default:
		return readIncrementalContent(ctx, d, q, sec, opts)
	}
}

func emptyContent(sec readmeta.Section, q diffQuery, opts diffContentOpts) diffContentOut {
	sec.PaginationExhausted = false
	return diffContentOut{
		Section: sec, Selection: selectionOutFrom(q, provedManifest{}, 0),
		Files: []diffContentFile{}, Selectors: unobservedAll(opts.Paths), FullPatchHash: nil,
	}
}

func readVersionContent(ctx context.Context, d Deps, q diffQuery, sec readmeta.Section, opts diffContentOpts, versionID int64) (diffContentOut, error) {
	proved, files, status, err := proveVersionContent(ctx, d, q, versionID, opts.Paths)
	if err != nil {
		return diffContentOut{}, err
	}
	sel := selectionOutFrom(q, proved, versionID)
	if status != "" || !proved.Full {
		if out, ok := recoverCacheContent(ctx, d, q, sec, proved, opts, versionID); ok {
			return out, nil
		}
		if err := ctx.Err(); err != nil {
			return diffContentOut{}, err
		}
		if status == "" {
			status = readmeta.CodePartial
		}
		sec = stampDiffFailure(sec, status)
		sec.CapabilityVersion = capabilityDiffContentV1
		if proved.Head != "" {
			h := proved.Head
			sec.HeadSHA = &h
		}
		return diffContentOut{Section: sec, Selection: sel, Files: []diffContentFile{}, Selectors: unobservedAll(opts.Paths)}, nil
	}
	outcomes, selected := matchSelectors(opts.Paths, files, true)
	built, retHash, cropped, knownOmit := buildDiffContentFiles(files, selected, opts)
	sec.CapabilityVersion = capabilityDiffContentV1
	head := proved.Head
	sec.HeadSHA = &head
	sec.Consistency = readmeta.ConsistencyConsistent
	sec.ManifestCoverage = readmeta.CoverageUnknown
	sec.PatchCoverage = readmeta.CoveragePartial
	sec.PaginationExhausted = true
	sec.NextCursor = nil
	if cropped || knownOmit {
		sec.ContentComplete = readmeta.ContentCompleteFalse
		if cropped {
			sec.AddLimitation(readmeta.CodePartial, "content cropped")
		}
		if knownOmit {
			sec.AddLimitation(readmeta.CodePartial, "known omission")
		}
	} else {
		sec.ContentComplete = readmeta.ContentCompleteUnknown
	}
	for _, o := range outcomes {
		if o.Status == diffSelectorAbsent || o.Status == diffSelectorAmbiguous {
			sec.ContentComplete = readmeta.ContentCompleteFalse
			sec.AddLimitation(readmeta.CodePartial, "selector "+o.Status)
		}
	}
	for _, f := range built {
		switch f.Status {
		case diffFileStatusTooLarge:
			sec.AddLimitation(readmeta.CodeTooLarge, pathStr(f.NewPath))
			sec.ContentComplete = readmeta.ContentCompleteFalse
		case diffFileStatusCollapsed:
			sec.AddLimitation(readmeta.CodeCollapsed, pathStr(f.NewPath))
			sec.ContentComplete = readmeta.ContentCompleteFalse
		case diffFileStatusBinary, diffFileStatusSubmodule, diffFileStatusUnsupported, diffFileStatusMalformed, diffFileStatusUnavailable, diffFileStatusModeOnly, diffFileStatusMetadata:
			sec.AddLimitation(readmeta.CodePartial, f.Status)
			sec.ContentComplete = readmeta.ContentCompleteFalse
		}
	}
	items := len(built)
	sec.Counts.Items = &items
	filesCount := len(built)
	sec.Counts.Files = &filesCount
	return diffContentOut{
		Section: sec, Selection: sel, Files: built, Selectors: outcomes,
		ReturnedContentHash: retHash, FullPatchHash: nil,
	}, nil
}

func proveVersionContent(ctx context.Context, d Deps, q diffQuery, versionID int64, paths []string) (provedManifest, []retainedDiffFile, string, error) {
	var charged chargeCount
	zero := func() provedManifest { return provedManifest{Charged: charged.n} }
	collect := newPatchCollect(paths)
	first, err := getVersionBodyCollect(ctx, d, q.OwnerID, q.IID, versionID, &charged, collect)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return zero(), nil, "", err
		}
		if err == errDiffUnproved {
			return zero(), nil, readmeta.CodeUnsupported, nil
		}
		return zero(), nil, readmeta.CodeHTTPError, nil
	}
	parsed, status := parseVersionProof(first, versionID, q.MRID)
	if status != "" {
		return zero(), nil, status, nil
	}
	firstFiles := retainedFromStream(first)
	firstDigests := selectedSourceDigests(firstFiles, paths)

	collect2 := newPatchCollect(paths)
	second, err := getVersionBodyCollect(ctx, d, q.OwnerID, q.IID, versionID, nil, collect2)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return zero(), nil, "", err
		}
		return zero(), nil, readmeta.CodeHTTPError, nil
	}
	again, status := parseVersionProof(second, versionID, q.MRID)
	if status != "" || !sameParsedIdentity(parsed, again) || !requestedTupleMatches(q, parsed) {
		return zero(), nil, readmeta.CodeInconsistent, nil
	}
	secondFiles := retainedFromStream(second)
	secondDigests := selectedSourceDigests(secondFiles, paths)
	if !sameStringMap(firstDigests, secondDigests) {
		return zero(), nil, readmeta.CodeInconsistent, nil
	}
	proved := provedManifest{Head: parsed.head, Base: parsed.base, Start: parsed.start, Total: parsed.total, Charged: charged.n}
	if !parsed.complete {
		return proved, firstFiles, parsed.reason, nil
	}
	proved.Full = true
	return proved, firstFiles, "", nil
}

func selectedSourceDigests(files []retainedDiffFile, paths []string) map[string]string {
	_, selected := matchSelectors(paths, files, true)
	out := make(map[string]string, len(selected))
	for _, idx := range selected {
		f := files[idx]
		key := pathStr(f.entry.OldPath) + "\x00" + pathStr(f.entry.NewPath)
		if f.patchOK {
			out[key] = f.sourceHash
		} else if f.overCap {
			out[key] = "overcap"
		} else if f.dupKey || f.wrongType {
			out[key] = "dup"
		} else {
			out[key] = "absent"
		}
	}
	return out
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func getVersionBodyCollect(ctx context.Context, d Deps, projectID, iid, versionID int64, charged *chargeCount, collect *patchCollect) (objectStream, error) {
	path := fmt.Sprintf("projects/%s/merge_requests/%d/versions/%d", gitlab.PathEscape(strconv.FormatInt(projectID, 10)), iid, versionID)
	st, resp, err := streamDiffObject(ctx, d.Client, path, nil, func(diffManifestEntry) error {
		if charged == nil {
			return nil
		}
		return charged.charge(ctx)
	}, collect)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return objectStream{}, err
		}
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		if code == http.StatusNotFound || code == http.StatusMethodNotAllowed || errors.Is(err, gitlab.ErrNotFound) {
			return objectStream{}, errDiffUnproved
		}
		return objectStream{}, err
	}
	return st, nil
}

func readIncrementalContent(ctx context.Context, d Deps, q diffQuery, sec readmeta.Section, opts diffContentOpts) (diffContentOut, error) {
	projectID, err := proveCommitProject(ctx, d, q)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return diffContentOut{}, err
		}
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.AddLimitation(readmeta.CodeUnsupported, "commit membership")
		return emptyContent(sec, q, opts), nil
	}
	path := fmt.Sprintf("projects/%s/repository/compare", gitlab.PathEscape(strconv.FormatInt(projectID, 10)))
	collect := newPatchCollect(opts.Paths)
	var charged chargeCount
	st, _, err := streamDiffObject(ctx, d.Client, path, &compareOpt{From: q.Selection.From, To: q.Selection.To, Straight: true}, func(diffManifestEntry) error {
		return charged.charge(ctx)
	}, collect)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return diffContentOut{}, err
		}
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.AddLimitation(readmeta.CodeHTTPError, "compare")
		return emptyContent(sec, q, opts), nil
	}
	sec.ContentComplete = readmeta.ContentCompleteFalse
	sec.ManifestCoverage = readmeta.CoverageUnknown
	sec.PatchCoverage = readmeta.CoveragePartial
	sec.AddLimitation(readmeta.CodePartial, "compare")
	sec.CapabilityVersion = capabilityDiffContentV1
	env := st.fields
	openTimeout, openTimeoutOK := compareTimeoutProofToken(env["compare_timeout"])
	if openTimeout == "true" {
		sec.AddLimitation(readmeta.CodePartial, "compare_timeout")
	}
	commitID, ok := parseCompareCommitID(env["commit"])
	if !ok || commitID != q.Selection.To {
		sec.Consistency = readmeta.ConsistencyInconsistent
		return emptyContent(sec, q, opts), nil
	}
	if err := proveCommit(ctx, d, projectID, q.Selection.From); err != nil {
		if passthroughTypedProviderErr(err) {
			return diffContentOut{}, err
		}
		sec.Consistency = readmeta.ConsistencyInconsistent
		return emptyContent(sec, q, opts), nil
	}
	if err := proveCommit(ctx, d, projectID, q.Selection.To); err != nil {
		if passthroughTypedProviderErr(err) {
			return diffContentOut{}, err
		}
		sec.Consistency = readmeta.ConsistencyInconsistent
		return emptyContent(sec, q, opts), nil
	}
	firstFiles := retainedFromStream(st)
	firstDigests := selectedSourceDigests(firstFiles, opts.Paths)
	firstMeta := selectedPathMeta(firstFiles, opts.Paths)

	collect2 := newPatchCollect(opts.Paths)
	st2, _, err := streamDiffObject(ctx, d.Client, path, &compareOpt{From: q.Selection.From, To: q.Selection.To, Straight: true}, nil, collect2)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return diffContentOut{}, err
		}
		sec.Consistency = readmeta.ConsistencyInconsistent
		sec.AddLimitation(readmeta.CodeHTTPError, "compare closing")
		return emptyContent(sec, q, opts), nil
	}
	closeTimeout, closeTimeoutOK := compareTimeoutProofToken(st2.fields["compare_timeout"])
	if openTimeout != closeTimeout || !openTimeoutOK || !closeTimeoutOK {
		sec.Consistency = readmeta.ConsistencyInconsistent
		sec.AddLimitation(readmeta.CodePartial, "compare_timeout")
		return emptyContent(sec, q, opts), nil
	}
	commitID2, ok2 := parseCompareCommitID(st2.fields["commit"])
	if !ok2 || commitID2 != q.Selection.To {
		sec.Consistency = readmeta.ConsistencyInconsistent
		return emptyContent(sec, q, opts), nil
	}
	secondFiles := retainedFromStream(st2)
	if !sameStringMap(firstDigests, selectedSourceDigests(secondFiles, opts.Paths)) || !sameStringMap(firstMeta, selectedPathMeta(secondFiles, opts.Paths)) {
		sec.Consistency = readmeta.ConsistencyInconsistent
		return emptyContent(sec, q, opts), nil
	}
	outcomes, selected := matchSelectors(opts.Paths, firstFiles, false)
	built, retHash, cropped, knownOmit := buildDiffContentFiles(firstFiles, selected, opts)
	if cropped || knownOmit {
		sec.AddLimitation(readmeta.CodePartial, "content cropped or omitted")
	}
	sec.Consistency = readmeta.ConsistencyConsistent
	items := len(built)
	sec.Counts.Items = &items
	if out, ok := recoverCacheContent(ctx, d, q, sec, provedManifest{Charged: charged.n}, opts, 0); ok {
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return diffContentOut{}, err
	}
	return diffContentOut{
		Section: sec, Selection: selectionOutFrom(q, provedManifest{}, 0),
		Files: built, Selectors: outcomes, ReturnedContentHash: retHash, FullPatchHash: nil,
	}, nil
}

// compareTimeoutProofToken normalizes compare_timeout JSON for symmetric opening/closing proof.
// trustable is false for true and malformed values (either side invalidates trusted content).
func compareTimeoutProofToken(raw json.RawMessage) (token string, trustable bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "absent", true
	}
	if string(trimmed) == "null" {
		return "null", true
	}
	var b bool
	if json.Unmarshal(trimmed, &b) != nil {
		return "malformed", false
	}
	if b {
		return "true", false
	}
	return "false", true
}

func parseCompareCommitID(raw json.RawMessage) (string, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "", false
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return "", false
	}
	seen := map[string]struct{}{}
	var id string
	var haveID bool
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, ok := keyTok.(string)
		if !ok {
			return "", false
		}
		if _, dup := seen[key]; dup {
			return "", false
		}
		seen[key] = struct{}{}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return "", false
		}
		if key == "id" {
			if haveID {
				return "", false
			}
			id = reviewJSONString(val)
			haveID = true
		}
	}
	end, err := dec.Token()
	if err != nil {
		return "", false
	}
	if d, ok := end.(json.Delim); !ok || d != '}' {
		return "", false
	}
	if !haveID || id == "" {
		return "", false
	}
	return id, true
}

func selectedPathMeta(files []retainedDiffFile, paths []string) map[string]string {
	_, selected := matchSelectors(paths, files, false)
	out := make(map[string]string, len(selected))
	for _, idx := range selected {
		f := files[idx]
		key := pathStr(f.entry.OldPath) + "\x00" + pathStr(f.entry.NewPath)
		out[key] = pathStr(f.entry.AMode) + "\x00" + pathStr(f.entry.BMode) + "\x00" + flagKey(f.entry)
	}
	return out
}
