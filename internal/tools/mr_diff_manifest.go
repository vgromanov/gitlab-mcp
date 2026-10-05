package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	diffModeVersion     = "full_version"
	diffModeTuple       = "full_tuple"
	diffModeIncremental = "incremental"
	capabilityDiffV1    = "readmeta.diff_manifest.v1"

	errCursorKeyMissingDiffWindow = "GITLAB_MCP_CURSOR_KEY is required for get_merge_request_diff_window; configure a raw secret of at least 32 bytes"
)

type diffWindowIn struct {
	ProjectID       string   `json:"project_id"`
	MergeRequestIID int64    `json:"merge_request_iid"`
	DiffVersionID   *int64   `json:"diff_version_id,omitempty"`
	BaseSHA         *string  `json:"base_sha,omitempty"`
	StartSHA        *string  `json:"start_sha,omitempty"`
	HeadSHA         *string  `json:"head_sha,omitempty"`
	FromSHA         *string  `json:"from_sha,omitempty"`
	ToSHA           *string  `json:"to_sha,omitempty"`
	Straight        *bool    `json:"straight,omitempty"`
	PerPage         *int     `json:"per_page,omitempty"`
	Cursor          *string  `json:"cursor,omitempty"`
	Mode            *string  `json:"mode,omitempty" jsonschema:"manifest (default) or content"`
	Paths           []string `json:"paths,omitempty" jsonschema:"content mode: 1..per_page distinct repository-relative paths"`
	ContextLines    *int     `json:"context_lines,omitempty" jsonschema:"content mode: 0..20, default 3"`
	MaxLines        *int     `json:"max_lines,omitempty" jsonschema:"content mode: 1..10000, default 1000"`
	MaxContentBytes *int     `json:"max_content_bytes,omitempty" jsonschema:"content mode: 1..1048576, default 262144"`
	MaxItems        *int     `json:"max_items,omitempty"`
	MaxBytes        *int64   `json:"max_bytes,omitempty"`
	MaxRequests     *int     `json:"max_requests,omitempty"`
	MaxElapsedMS    *int64   `json:"max_elapsed_ms,omitempty"`
}

type diffManifestEntry struct {
	OldPath       *string `json:"old_path"`
	NewPath       *string `json:"new_path"`
	AMode         *string `json:"a_mode"`
	BMode         *string `json:"b_mode"`
	NewFile       *bool   `json:"new_file"`
	RenamedFile   *bool   `json:"renamed_file"`
	DeletedFile   *bool   `json:"deleted_file"`
	GeneratedFile *bool   `json:"generated_file"`
	Collapsed     *bool   `json:"collapsed"`
	TooLarge      *bool   `json:"too_large"`
	Binary        *bool   `json:"binary"`
	Submodule     *bool   `json:"submodule"`
}

type diffWindowOut struct {
	Section    readmeta.Section     `json:"section"`
	Entries    []diffManifestEntry  `json:"entries"`
	Digest     *string              `json:"digest"`
	Provenance *diffCacheProvenance `json:"provenance,omitempty"`
}

type diffSelection struct {
	ProjectID string
	IID       int64
	Mode      string
	VersionID int64
	Base      string
	Start     string
	Head      string
	From      string
	To        string
	PerPage   int
}

type diffQuery struct {
	OwnerID         int64
	IID             int64
	MRID            int64
	SourceProjectID int64
	TargetProjectID int64
	Selection       diffSelection
	Full            bool
	Offset          int
	Now             time.Time
	Resume          *cursor.Payload
	ActorID         int64
	Instance        string
	PolicyFP        string
	Key             []byte
	Project         string
}

type diffCaps struct {
	items    int
	bytes    int64
	requests int
	elapsed  time.Duration
}

func getMergeRequestDiffWindow(ctx context.Context, _ *mcp.CallToolRequest, in diffWindowIn, d Deps) (*mcp.CallToolResult, any, error) {
	if d.Config == nil || len(d.Config.CursorKey) < cursor.MinKeyBytes {
		return nil, nil, fmt.Errorf("%s", errCursorKeyMissingDiffWindow)
	}
	mode, contentOpts, err := normalizeDiffWindowMode(in)
	if err != nil {
		return nil, nil, err
	}
	sel, err := normalizeDiffSelection(in)
	if err != nil {
		return nil, nil, err
	}
	var resume *cursor.Payload
	if in.Cursor != nil && strings.TrimSpace(*in.Cursor) != "" {
		if mode == diffModeContent {
			return nil, nil, fmt.Errorf("content mode does not support cursor")
		}
		decoded, decErr := cursor.Decode(d.Config.CursorKey, *in.Cursor, d.now())
		if decErr != nil {
			return nil, nil, fmt.Errorf("%s", cursor.ResyncRequired)
		}
		if bindErr := diffSelectionBinds(decoded, sel); bindErr != nil {
			return nil, nil, bindErr
		}
		instance, instErr := cursorInstance(d.Config)
		if instErr != nil || decoded.Instance != instance || decoded.PolicyFP != d.Config.PolicyFingerprint() {
			return nil, nil, fmt.Errorf("%s", cursor.ResyncRequired)
		}
		resume = &decoded
	}
	caps, err := diffCallerCaps(in)
	if err != nil {
		return nil, nil, err
	}
	ctx, budget, release := ensureDiffBudget(ctx, caps)
	defer release()
	ctx = igl.WithExactReadProvenance(ctx)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := diffBudgetPreflight(budget); err != nil {
		return nil, nil, err
	}
	actor, err := queueResolvePrincipal(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	if resume != nil && resume.ActorID != actor {
		return nil, nil, fmt.Errorf("%s", cursor.ResyncRequired)
	}
	instance, err := cursorInstance(d.Config)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: instance", cursor.ResyncRequired)
	}
	owner, mr, err := loadDiffIdentity(ctx, d, sel.ProjectID, sel.IID)
	if err != nil {
		return nil, nil, err
	}
	if mr == nil || mr.ID < 1 || mr.SourceProjectID < 1 {
		return nil, nil, identityErr("merge request metadata missing")
	}
	if resume != nil && resume.Scope.ProjectID != strconv.FormatInt(owner.ID, 10) {
		return nil, nil, fmt.Errorf("%s", cursor.ResyncRequired)
	}
	offset := 0
	if resume != nil {
		offset = resume.DiffWindow.Offset
	}
	q := diffQuery{
		OwnerID: owner.ID, IID: sel.IID, MRID: mr.ID,
		SourceProjectID: mr.SourceProjectID, TargetProjectID: mr.ProjectID,
		Selection: sel, Offset: offset, Now: d.now(), Resume: resume,
		ActorID: actor, Instance: instance, PolicyFP: d.Config.PolicyFingerprint(),
		Key: d.Config.CursorKey, Project: strconv.FormatInt(owner.ID, 10),
	}
	if mode == diffModeContent {
		out, err := readBoundedDiffContent(ctx, d, q, contentOpts)
		if err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	}
	out, err := readBoundedDiffManifest(ctx, d, q)
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

func normalizeDiffSelection(in diffWindowIn) (diffSelection, error) {
	if strings.TrimSpace(in.ProjectID) == "" || in.MergeRequestIID < 1 {
		return diffSelection{}, fmt.Errorf("project_id and merge_request_iid are required")
	}
	per := 20
	if in.PerPage != nil {
		per = *in.PerPage
	}
	if per < 1 || per > 50 {
		return diffSelection{}, fmt.Errorf("per_page must be 1..50")
	}
	versionSet := in.DiffVersionID != nil
	tupleSet := in.BaseSHA != nil || in.StartSHA != nil || in.HeadSHA != nil
	incSet := in.FromSHA != nil || in.ToSHA != nil || in.Straight != nil
	modes := 0
	if versionSet {
		modes++
	}
	if tupleSet {
		modes++
	}
	if incSet {
		modes++
	}
	if modes != 1 {
		return diffSelection{}, fmt.Errorf("diff selection modes are exclusive")
	}
	sel := diffSelection{ProjectID: strings.TrimSpace(in.ProjectID), IID: in.MergeRequestIID, PerPage: per}
	switch {
	case versionSet:
		if in.DiffVersionID == nil || *in.DiffVersionID < 1 {
			return diffSelection{}, fmt.Errorf("diff_version_id must be positive")
		}
		sel.Mode = diffModeVersion
		sel.VersionID = *in.DiffVersionID
	case tupleSet:
		base, bok := canonicalSHA(in.BaseSHA)
		start, sok := canonicalSHA(in.StartSHA)
		head, hok := canonicalSHA(in.HeadSHA)
		if !bok || !sok || !hok {
			return diffSelection{}, fmt.Errorf("base_sha, start_sha, and head_sha must be canonical 40-hex")
		}
		sel.Mode = diffModeTuple
		sel.Base, sel.Start, sel.Head = base, start, head
	default:
		from, fok := canonicalSHA(in.FromSHA)
		to, tok := canonicalSHA(in.ToSHA)
		if !fok || !tok || in.Straight == nil || !*in.Straight {
			return diffSelection{}, fmt.Errorf("incremental selection requires from_sha, to_sha, and straight true")
		}
		sel.Mode = diffModeIncremental
		sel.From, sel.To = from, to
	}
	return sel, nil
}

func canonicalSHA(v *string) (string, bool) {
	if v == nil {
		return "", false
	}
	sha, ok := readmeta.ObservedHeadSHA(*v)
	if !ok || sha != *v {
		return "", false
	}
	return sha, true
}

func diffCallerCaps(in diffWindowIn) (diffCaps, error) {
	var c diffCaps
	if in.MaxItems != nil {
		if *in.MaxItems < 1 {
			return c, fmt.Errorf("max_items must be positive")
		}
		c.items = *in.MaxItems
	}
	if in.MaxBytes != nil {
		if *in.MaxBytes < 1 {
			return c, fmt.Errorf("max_bytes must be positive")
		}
		c.bytes = *in.MaxBytes
	}
	if in.MaxRequests != nil {
		if *in.MaxRequests < 1 {
			return c, fmt.Errorf("max_requests must be positive")
		}
		c.requests = *in.MaxRequests
	}
	if in.MaxElapsedMS != nil {
		if *in.MaxElapsedMS < 1 {
			return c, fmt.Errorf("max_elapsed_ms must be positive")
		}
		c.elapsed = time.Duration(*in.MaxElapsedMS) * time.Millisecond
	}
	return c, nil
}

func ensureDiffBudget(ctx context.Context, caps diffCaps) (context.Context, *igl.Budget, func()) {
	b := igl.BudgetFromContext(ctx)
	if b == nil {
		b = igl.DefaultBudget()
		if caps.elapsed > 0 && (b.MaxElapsed <= 0 || caps.elapsed < b.MaxElapsed) {
			b.MaxElapsed = caps.elapsed
		}
		ctx = igl.WithBudget(ctx, b)
		b.CapLimits(caps.items, caps.bytes, caps.requests)
		return ctx, b, func() { b.Cancel() }
	}
	b.CapLimits(caps.items, caps.bytes, caps.requests)
	if caps.elapsed <= 0 {
		return ctx, b, func() {}
	}
	deadline := time.Now().Add(caps.elapsed)
	if existing, ok := ctx.Deadline(); ok && existing.Before(deadline) {
		deadline = existing
	}
	child, cancel := context.WithDeadline(ctx, deadline)
	return child, b, func() { cancel() }
}

func diffBudgetPreflight(b *igl.Budget) error {
	if b == nil {
		return nil
	}
	reqs, nbytes, items := b.Stats()
	if b.MaxRequests > 0 && reqs >= b.MaxRequests {
		return igl.ErrBudgetRequests
	}
	if b.MaxBytes > 0 && nbytes >= b.MaxBytes {
		return igl.ErrBudgetBytes
	}
	if b.MaxItems > 0 && items >= b.MaxItems {
		return igl.ErrBudgetItems
	}
	return nil
}

func diffSelectionBinds(p cursor.Payload, sel diffSelection) error {
	if p.Tool != cursor.ToolDiffWindow || p.Section != cursor.SectionDiffManifest || p.DiffWindow == nil {
		return fmt.Errorf("%s", cursor.ResyncRequired)
	}
	w := p.DiffWindow
	if p.Scope.MergeRequestIID == nil || *p.Scope.MergeRequestIID != sel.IID || w.PerPage != sel.PerPage || w.Mode != sel.Mode {
		return fmt.Errorf("%s", cursor.ResyncRequired)
	}
	switch sel.Mode {
	case diffModeVersion:
		if w.VersionID != sel.VersionID {
			return fmt.Errorf("%s", cursor.ResyncRequired)
		}
	case diffModeTuple:
		if w.BaseSHA != sel.Base || w.StartSHA != sel.Start || w.HeadSHA != sel.Head {
			return fmt.Errorf("%s", cursor.ResyncRequired)
		}
	case diffModeIncremental:
		if w.FromSHA != sel.From || w.ToSHA != sel.To || !w.Straight {
			return fmt.Errorf("%s", cursor.ResyncRequired)
		}
	default:
		return fmt.Errorf("%s", cursor.ResyncRequired)
	}
	if strings.TrimSpace(sel.ProjectID) != "" && p.Scope.ProjectID != "" && sel.ProjectID != p.Scope.ProjectID && !sameNumeric(sel.ProjectID, p.Scope.ProjectID) {
		if _, err := strconv.ParseInt(sel.ProjectID, 10, 64); err == nil {
			return fmt.Errorf("%s", cursor.ResyncRequired)
		}
	}
	return nil
}

func sameNumeric(a, b string) bool {
	ai, aerr := strconv.ParseInt(a, 10, 64)
	bi, berr := strconv.ParseInt(b, 10, 64)
	return aerr == nil && berr == nil && ai == bi
}

func readBoundedDiffManifest(ctx context.Context, d Deps, q diffQuery) (diffWindowOut, error) {
	sec := newDiffSection(q.Now)
	switch q.Selection.Mode {
	case diffModeVersion:
		return readVersionManifest(ctx, d, q, sec, q.Selection.VersionID, "")
	case diffModeTuple:
		id, err := selectTupleVersion(ctx, d, q)
		if err != nil {
			if passthroughTypedProviderErr(err) {
				return diffWindowOut{}, err
			}
			sec.AddLimitation(readmeta.CodeProviderPageAmbiguous, "version list")
			sec.ContentComplete = readmeta.ContentCompleteUnknown
			return diffWindowOut{Section: sec, Entries: []diffManifestEntry{}}, nil
		}
		if id == 0 {
			sec.AddLimitation(readmeta.CodePartial, "version tuple not unique")
			sec.ContentComplete = readmeta.ContentCompleteFalse
			return diffWindowOut{Section: sec, Entries: []diffManifestEntry{}}, nil
		}
		return readVersionManifest(ctx, d, q, sec, id, "")
	default:
		return readIncrementalManifest(ctx, d, q, sec)
	}
}

type versionPageOpt struct {
	Page    int `url:"page,omitempty"`
	PerPage int `url:"per_page,omitempty"`
}

type versionRow struct {
	ID    int64
	MRID  int64
	Head  string
	Base  string
	Start string
}

func selectTupleVersion(ctx context.Context, d Deps, q diffQuery) (int64, error) {
	rows, err := walkVersionList(ctx, d, q.OwnerID, q.IID)
	if err != nil {
		return 0, err
	}
	var selected int64
	matches := 0
	headOnly := false
	for _, row := range rows {
		if row.MRID != q.MRID {
			continue
		}
		full := row.Head == q.Selection.Head && row.Base == q.Selection.Base && row.Start == q.Selection.Start
		if full {
			matches++
			selected = row.ID
			continue
		}
		if row.Head == q.Selection.Head {
			headOnly = true
		}
	}
	if matches != 1 || headOnly {
		return 0, nil
	}
	return selected, nil
}

func walkVersionList(ctx context.Context, d Deps, projectID, iid int64) ([]versionRow, error) {
	seen := map[int]struct{}{}
	page := 1
	var rows []versionRow
	for guard := 0; guard < 20; guard++ {
		if _, ok := seen[page]; ok {
			return nil, fmt.Errorf("%s", readmeta.CodeProviderPageAmbiguous)
		}
		seen[page] = struct{}{}
		path := fmt.Sprintf("projects/%s/merge_requests/%d/versions", gitlab.PathEscape(strconv.FormatInt(projectID, 10)), iid)
		var rawRows []json.RawMessage
		resp, err := igl.StreamJSONArrayQueue(ctx, d.Client, http.MethodGet, path, &versionPageOpt{Page: page, PerPage: 100}, func(raw json.RawMessage) error {
			if b := igl.BudgetFromContext(ctx); b != nil {
				if err := b.AddItem(); err != nil {
					return err
				}
			}
			rawRows = append(rawRows, append(json.RawMessage(nil), raw...))
			return nil
		})
		if err != nil {
			if passthroughTypedProviderErr(err) {
				return nil, err
			}
			return nil, fmt.Errorf("%s", readmeta.CodeProviderPageAmbiguous)
		}
		for _, raw := range rawRows {
			var env map[string]json.RawMessage
			if json.Unmarshal(raw, &env) != nil {
				return nil, fmt.Errorf("%s", readmeta.CodeProviderPageAmbiguous)
			}
			id, mrID, head, base, start, ok := provedVersionRow(env)
			if !ok {
				return nil, fmt.Errorf("%s", readmeta.CodeProviderPageAmbiguous)
			}
			rows = append(rows, versionRow{ID: id, MRID: mrID, Head: head, Base: base, Start: start})
		}
		next, terminal, ok := strictNextPage(resp, page)
		if !ok {
			return nil, fmt.Errorf("%s", readmeta.CodeProviderPageAmbiguous)
		}
		if terminal {
			return rows, nil
		}
		page = next
	}
	return nil, fmt.Errorf("%s", readmeta.CodeProviderPageAmbiguous)
}

func strictNextPage(resp *gitlab.Response, current int) (next int, terminal bool, ok bool) {
	if resp == nil || resp.Response == nil {
		return 0, false, false
	}
	vals, present := queueNextPageValues(resp.Header)
	if !present || len(vals) != 1 {
		return 0, false, false
	}
	raw := vals[0]
	if strings.TrimSpace(raw) != raw {
		return 0, false, false
	}
	sdk := resp.NextPage
	if raw == "" || raw == "0" {
		return 0, true, sdk == 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 || strconv.FormatInt(n, 10) != raw || n != int64(current)+1 || sdk != n {
		return 0, false, false
	}
	return int(n), false, true
}

func readVersionManifest(ctx context.Context, d Deps, q diffQuery, sec readmeta.Section, versionID int64, _ string) (diffWindowOut, error) {
	proved, entries, status, err := proveVersion(ctx, d, q, versionID)
	if err != nil {
		return diffWindowOut{}, err
	}
	if !proved.Full {
		if out, ok := recoverCacheManifest(ctx, d, q, sec, proved); ok {
			return out, nil
		}
		return incompleteManifestWindow(q, sec, proved.Head, status, entries), nil
	}
	return finishManifestWindow(q, sec, proved, entries)
}

type provedManifest struct {
	Head  string
	Base  string
	Start string
	Total int
	Full  bool
}

func proveVersion(ctx context.Context, d Deps, q diffQuery, versionID int64) (provedManifest, []diffManifestEntry, string, error) {
	first, err := getVersionBody(ctx, d, q.OwnerID, q.IID, versionID, true)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return provedManifest{}, nil, "", err
		}
		if errors.Is(err, errDiffUnproved) {
			return provedManifest{}, nil, readmeta.CodeUnsupported, nil
		}
		return provedManifest{}, nil, readmeta.CodeHTTPError, nil
	}
	parsed, status := parseVersionProof(first, versionID, q.MRID)
	if status != "" {
		return provedManifest{}, nil, status, nil
	}
	second, err := getVersionBody(ctx, d, q.OwnerID, q.IID, versionID, false)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return provedManifest{}, nil, "", err
		}
		return provedManifest{}, nil, readmeta.CodeHTTPError, nil
	}
	again, status := parseVersionProof(second, versionID, q.MRID)
	if status != "" || !sameParsedIdentity(parsed, again) || !requestedTupleMatches(q, parsed) {
		return provedManifest{}, nil, readmeta.CodeInconsistent, nil
	}
	proved := provedManifest{Head: parsed.head, Base: parsed.base, Start: parsed.start, Total: parsed.total}
	if !parsed.complete {
		return proved, parsed.entries, parsed.reason, nil
	}
	proved.Full = true
	return proved, parsed.entries, "", nil
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameParsedIdentity(a, b parsedVersion) bool {
	return a.head == b.head && a.base == b.base && a.start == b.start && a.total == b.total && a.complete == b.complete && manifestDigest(a.entries) == manifestDigest(b.entries)
}

func requestedTupleMatches(q diffQuery, p parsedVersion) bool {
	sel := q.Selection
	if sel.Mode != diffModeTuple && sel.Head == "" && sel.Base == "" && sel.Start == "" {
		return true
	}
	return p.head == sel.Head && p.base == sel.Base && p.start == sel.Start
}

var errDiffUnproved = errors.New("diff unproved")

func getVersionBody(ctx context.Context, d Deps, projectID, iid, versionID int64, charge bool) (objectStream, error) {
	path := fmt.Sprintf("projects/%s/merge_requests/%d/versions/%d", gitlab.PathEscape(strconv.FormatInt(projectID, 10)), iid, versionID)
	st, resp, err := streamDiffObject(ctx, d.Client, path, nil, func(diffManifestEntry) error {
		if !charge {
			return nil
		}
		return igl.BudgetFromContext(ctx).AddItem()
	}, nil)
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

type parsedVersion struct {
	entries  []diffManifestEntry
	head     string
	base     string
	start    string
	total    int
	complete bool
	reason   string
}

func parseVersionProof(st objectStream, versionID, mrID int64) (parsedVersion, string) {
	env := st.fields
	id, idOK := jsonInt(env["id"])
	gotMR, mrOK := jsonInt(env["merge_request_id"])
	head, hok := readmeta.ObservedHeadSHA(reviewJSONString(env["head_commit_sha"]))
	base, bok := readmeta.ObservedHeadSHA(reviewJSONString(env["base_commit_sha"]))
	start, sok := readmeta.ObservedHeadSHA(reviewJSONString(env["start_commit_sha"]))
	if !idOK || !mrOK || !hok || !bok || !sok || id != versionID || gotMR != mrID {
		return parsedVersion{}, readmeta.CodeIdentityUnresolved
	}
	state, stateOK := jsonStringExact(env["state"])
	total, totalOK := jsonCanonicalInt(env["real_size"])
	if !st.diffsOK {
		return parsedVersion{head: head}, readmeta.CodeUnknownCount
	}
	entries := st.entries
	pv := parsedVersion{entries: entries, head: head, base: base, start: start, total: total}
	if !stateOK || !totalOK {
		pv.reason = readmeta.CodeUnknownCount
		return pv, ""
	}
	if state != "collected" {
		pv.reason = readmeta.CodePartial
		if state == "overflow" || state == "without_files" || state == "timeout" || strings.HasPrefix(state, "overflow_") {
			pv.reason = readmeta.CodePartial
		}
		return pv, ""
	}
	if st.pathBad || total != len(entries) {
		pv.reason = readmeta.CodePartial
		return pv, ""
	}
	pv.complete = true
	return pv, ""
}

func entryFromRaw(env map[string]json.RawMessage) (diffManifestEntry, bool) {
	old, oldOK := optionalString(env["old_path"])
	newPath, newOK := optionalString(env["new_path"])
	aMode, aOK := optionalString(env["a_mode"])
	bMode, bOK := optionalString(env["b_mode"])
	entry := diffManifestEntry{
		OldPath: old, NewPath: newPath, AMode: aMode, BMode: bMode,
		NewFile: triBool(env["new_file"]), RenamedFile: triBool(env["renamed_file"]), DeletedFile: triBool(env["deleted_file"]),
		GeneratedFile: triBool(env["generated_file"]), Collapsed: triBool(env["collapsed"]), TooLarge: triBool(env["too_large"]),
		Binary: triBool(env["binary"]), Submodule: triBool(env["submodule"]),
	}
	return entry, !oldOK || !newOK || !aOK || !bOK
}

func optionalString(raw json.RawMessage) (*string, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, true
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return nil, false
	}
	return &s, true
}

func triBool(raw json.RawMessage) *bool {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return nil
	}
	return &b
}

func pathStr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func manifestOrderLess(a, b diffManifestEntry) bool {
	if pathStr(a.OldPath) != pathStr(b.OldPath) {
		return pathStr(a.OldPath) < pathStr(b.OldPath)
	}
	if pathStr(a.NewPath) != pathStr(b.NewPath) {
		return pathStr(a.NewPath) < pathStr(b.NewPath)
	}
	return flagKey(a) < flagKey(b)
}

func sortManifestEntries(entries []diffManifestEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return manifestOrderLess(entries[i], entries[j])
	})
}

func flagKey(e diffManifestEntry) string {
	raw, _ := json.Marshal(e)
	return string(raw)
}

func jsonStringExact(raw json.RawMessage) (string, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}

func jsonCanonicalInt(raw json.RawMessage) (int, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return 0, false
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner, err := strconv.Unquote(s)
		if err != nil {
			return 0, false
		}
		return canonicalUncappedDecimal(inner)
	}
	return canonicalUncappedDecimal(s)
}

func canonicalUncappedDecimal(s string) (int, bool) {
	if s == "" || strings.ContainsAny(s, "eE.+- ") {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || strconv.Itoa(n) != s {
		return 0, false
	}
	return n, true
}

func manifestDigest(entries []diffManifestEntry) string {
	raw, _ := json.Marshal(struct {
		Schema  string              `json:"schema"`
		Entries []diffManifestEntry `json:"entries"`
	}{Schema: "diff_manifest.v1", Entries: entries})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func incompleteManifestWindow(q diffQuery, sec readmeta.Section, head, status string, entries []diffManifestEntry) diffWindowOut {
	if status == "" {
		status = readmeta.CodePartial
	}
	sec = stampDiffFailure(sec, status)
	if head != "" {
		copied := head
		sec.HeadSHA = &copied
	}
	if q.Resume != nil {
		return diffWindowOut{Section: sec, Entries: []diffManifestEntry{}}
	}
	shown := entries
	if !q.Full {
		shown = windowEntries(entries, 0, q.Selection.PerPage)
	}
	return diffWindowOut{Section: sec, Entries: shown}
}

func finishManifestWindow(q diffQuery, sec readmeta.Section, proved provedManifest, entries []diffManifestEntry) (diffWindowOut, error) {
	if !proved.Full {
		return incompleteManifestWindow(q, sec, proved.Head, readmeta.CodePartial, entries), nil
	}
	full := manifestDigest(entries)
	if q.Resume != nil {
		prefix := manifestDigest(prefixEntries(entries, q.Offset))
		refsSame := sameStringSlice(q.Resume.ImmutableRefs, diffRefs(q, proved))
		if !refsSame || q.Resume.DiffWindow.FullDigest != full || q.Resume.DiffWindow.Total != len(entries) || q.Resume.PageState.SequenceDigest != prefix {
			sec.ContentComplete = readmeta.ContentCompleteFalse
			sec.Consistency = readmeta.ConsistencyInconsistent
			sec.AddLimitation(readmeta.CodeInconsistent, "full sequence")
			return diffWindowOut{Section: sec, Entries: []diffManifestEntry{}}, nil
		}
	}
	offset := q.Offset
	per := q.Selection.PerPage
	if q.Full {
		offset = 0
		per = len(entries)
		if per == 0 {
			per = 1
		}
	}
	if offset < 0 || offset > len(entries) {
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.AddLimitation(readmeta.CodeInconsistent, "window offset")
		return diffWindowOut{Section: sec, Entries: []diffManifestEntry{}}, nil
	}
	end := offset + per
	if q.Full || end > len(entries) {
		end = len(entries)
	}
	window := append([]diffManifestEntry(nil), entries[offset:end]...)
	head := proved.Head
	total := len(entries)
	items := len(window)
	sec.HeadSHA = &head
	sec.Consistency = readmeta.ConsistencyConsistent
	sec.ManifestCoverage = readmeta.CoverageFull
	sec.PatchCoverage = readmeta.CoverageUnknown
	sec.Counts.Files = &total
	sec.Counts.Items = &items
	more := !q.Full && end < len(entries)
	sec.PaginationExhausted = !more
	if more {
		sec.ContentComplete = readmeta.ContentCompleteFalse
	} else {
		sec.ContentComplete = readmeta.ContentCompleteTrue
	}
	dig := full
	if more {
		tok, err := mintDiffCursor(q, proved, entries, end, full)
		if err != nil {
			sec.NextCursor = nil
			sec.ContentComplete = readmeta.ContentCompleteFalse
			sec.AddLimitation(readmeta.CodeCursorCapacity, "cursor")
			return diffWindowOut{Section: sec, Entries: window}, nil
		}
		sec.NextCursor = &tok
	}
	if q.Full {
		sec.NextCursor = nil
		sec.PaginationExhausted = true
	}
	return diffWindowOut{Section: sec, Entries: window, Digest: &dig}, nil
}

func prefixEntries(entries []diffManifestEntry, offset int) []diffManifestEntry {
	if offset < 0 {
		return nil
	}
	if offset > len(entries) {
		offset = len(entries)
	}
	return entries[:offset]
}

func windowEntries(entries []diffManifestEntry, offset, end int) []diffManifestEntry {
	if offset < 0 || end < offset || offset > len(entries) {
		return []diffManifestEntry{}
	}
	if end > len(entries) {
		end = len(entries)
	}
	return append([]diffManifestEntry(nil), entries[offset:end]...)
}

func mintDiffCursor(q diffQuery, proved provedManifest, entries []diffManifestEntry, end int, full string) (string, error) {
	if q.Key == nil || end >= len(entries) || end-q.Offset != q.Selection.PerPage {
		return "", fmt.Errorf("%s", readmeta.CodeCursorCapacity)
	}
	prefix := manifestDigest(entries[:end])
	now := q.Now.UTC()
	upper := now.Format(time.RFC3339)
	expires := now.Add(cursor.DefaultTTL).Format(time.RFC3339)
	if q.Resume != nil {
		upper = q.Resume.UpperBound
		expires = q.Resume.ExpiresAt
	}
	page := end / q.Selection.PerPage
	iid := q.IID
	w := &cursor.DiffWindowCont{
		V: diffWindowSchema(), Mode: q.Selection.Mode, VersionID: q.Selection.VersionID,
		BaseSHA: q.Selection.Base, StartSHA: q.Selection.Start, HeadSHA: q.Selection.Head,
		FromSHA: q.Selection.From, ToSHA: q.Selection.To, Straight: q.Selection.Mode == diffModeIncremental,
		Total: len(entries), Offset: end, FullDigest: full, PerPage: q.Selection.PerPage,
	}
	payload := cursor.Payload{
		SchemaVersion: cursor.SchemaV1, Instance: q.Instance, ActorID: q.ActorID, PolicyFP: q.PolicyFP,
		Tool: cursor.ToolDiffWindow, Section: cursor.SectionDiffManifest,
		Scope:         cursor.Scope{Kind: cursor.ScopeProject, ProjectID: q.Project, MergeRequestIID: &iid},
		Filters:       cursor.Filters{Selection: cursorSelection(w), Until: upper, PerPage: q.Selection.PerPage},
		ImmutableRefs: diffRefs(q, proved), UpperBound: upper, ExpiresAt: expires,
		PageState: cursor.PageState{
			Page: page, PerPage: q.Selection.PerPage, SequenceDigest: prefix, LastSHA: prefix,
			ItemsOnPage: q.Selection.PerPage, ProviderNextPage: int64(page) + 1,
		},
		DiffWindow: w,
	}
	if len(payload.ImmutableRefs) == 0 {
		return "", fmt.Errorf("%s", readmeta.CodeCursorCapacity)
	}
	return cursor.Encode(q.Key, payload)
}

func diffWindowSchema() string { return cursor.DiffWindowSchemaDM1 }

func cursorSelection(w *cursor.DiffWindowCont) string {
	switch w.Mode {
	case diffModeVersion:
		return fmt.Sprintf("version:%d", w.VersionID)
	case diffModeTuple:
		return "tuple:" + w.BaseSHA + ":" + w.StartSHA + ":" + w.HeadSHA
	default:
		return "inc:" + w.FromSHA + ":" + w.ToSHA
	}
}

func diffRefs(q diffQuery, proved provedManifest) []string {
	seeds := []string{q.Selection.Head, q.Selection.Base, q.Selection.Start}
	if q.Selection.Mode == diffModeVersion {
		seeds = []string{proved.Head, proved.Base, proved.Start}
	}
	if q.Selection.Mode == diffModeIncremental {
		seeds = []string{q.Selection.From, q.Selection.To}
	}
	seen := map[string]struct{}{}
	var out []string
	for _, s := range seeds {
		if !isFortyHex(s) {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

func isFortyHex(s string) bool {
	sha, ok := readmeta.ObservedHeadSHA(s)
	return ok && sha == s
}

func stampDiffFailure(sec readmeta.Section, code string) readmeta.Section {
	switch code {
	case readmeta.CodeUnsupported:
		sec.ContentComplete = readmeta.ContentCompleteUnknown
		sec.AddLimitation(readmeta.CodeUnsupported, "diff version")
	case readmeta.CodeUnknownCount:
		sec.ContentComplete = readmeta.ContentCompleteUnknown
		sec.AddLimitation(readmeta.CodeUnknownCount, "diff version")
	case readmeta.CodeIdentityUnresolved:
		sec.ContentComplete = readmeta.ContentCompleteUnknown
		sec.AddLimitation(readmeta.CodeIdentityUnresolved, "diff version")
	case readmeta.CodeInconsistent:
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.Consistency = readmeta.ConsistencyInconsistent
		sec.AddLimitation(readmeta.CodeInconsistent, "diff version")
	case readmeta.CodeHTTPError:
		sec.ContentComplete = readmeta.ContentCompleteUnknown
		sec.AddLimitation(readmeta.CodeHTTPError, "diff version")
	default:
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.AddLimitation(readmeta.CodePartial, "diff version")
	}
	sec.ManifestCoverage = readmeta.CoverageUnknown
	sec.PatchCoverage = readmeta.CoverageUnknown
	sec.NextCursor = nil
	sec.PaginationExhausted = false
	return sec
}

func newDiffSection(now time.Time) readmeta.Section {
	if now.IsZero() {
		now = time.Now()
	}
	return readmeta.Section{
		RetrievedAt:       now.UTC().Format(time.RFC3339),
		Source:            readmeta.SourceGitLabREST,
		Provider:          readmeta.ProviderGitLab,
		CapabilityVersion: capabilityDiffV1,
		Limitations:       []readmeta.Limitation{},
		ContentComplete:   readmeta.ContentCompleteUnknown,
		Consistency:       readmeta.ConsistencyUnknown,
		ManifestCoverage:  readmeta.CoverageUnknown,
		PatchCoverage:     readmeta.CoverageUnknown,
		Counts:            readmeta.Counts{},
	}
}

type compareOpt struct {
	From     string `url:"from"`
	To       string `url:"to"`
	Straight bool   `url:"straight"`
}

func readIncrementalManifest(ctx context.Context, d Deps, q diffQuery, sec readmeta.Section) (diffWindowOut, error) {
	projectID, err := proveCommitProject(ctx, d, q)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return diffWindowOut{}, err
		}
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.AddLimitation(readmeta.CodeUnsupported, "commit membership")
		return diffWindowOut{Section: sec, Entries: []diffManifestEntry{}}, nil
	}
	path := fmt.Sprintf("projects/%s/repository/compare", gitlab.PathEscape(strconv.FormatInt(projectID, 10)))
	st, _, err := streamDiffObject(ctx, d.Client, path, &compareOpt{From: q.Selection.From, To: q.Selection.To, Straight: true}, func(diffManifestEntry) error {
		if b := igl.BudgetFromContext(ctx); b != nil {
			return b.AddItem()
		}
		return nil
	}, nil)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return diffWindowOut{}, err
		}
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.AddLimitation(readmeta.CodeHTTPError, "compare")
		return diffWindowOut{Section: sec, Entries: []diffManifestEntry{}}, nil
	}
	sec.ContentComplete = readmeta.ContentCompleteFalse
	sec.ManifestCoverage = readmeta.CoveragePartial
	sec.PatchCoverage = readmeta.CoverageUnknown
	sec.AddLimitation(readmeta.CodePartial, "compare")
	env := st.fields
	if timeout := triBool(env["compare_timeout"]); timeout != nil && *timeout {
		sec.AddLimitation(readmeta.CodePartial, "compare_timeout")
	}
	var commit map[string]json.RawMessage
	if json.Unmarshal(env["commit"], &commit) != nil || reviewJSONString(commit["id"]) != q.Selection.To {
		sec.Consistency = readmeta.ConsistencyInconsistent
		return diffWindowOut{Section: sec, Entries: []diffManifestEntry{}}, nil
	}
	if err := proveCommit(ctx, d, projectID, q.Selection.From); err != nil {
		if passthroughTypedProviderErr(err) {
			return diffWindowOut{}, err
		}
		sec.Consistency = readmeta.ConsistencyInconsistent
		return diffWindowOut{Section: sec, Entries: []diffManifestEntry{}}, nil
	}
	if err := proveCommit(ctx, d, projectID, q.Selection.To); err != nil {
		if passthroughTypedProviderErr(err) {
			return diffWindowOut{}, err
		}
		sec.Consistency = readmeta.ConsistencyInconsistent
		return diffWindowOut{Section: sec, Entries: []diffManifestEntry{}}, nil
	}
	entries := st.entries
	if !st.diffsOK {
		entries = []diffManifestEntry{}
	}
	end := q.Selection.PerPage
	if end > len(entries) {
		end = len(entries)
	}
	if out, ok := recoverCacheManifest(ctx, d, q, sec, provedManifest{}); ok {
		return out, nil
	}
	return diffWindowOut{Section: sec, Entries: windowEntries(entries, q.Offset, end)}, nil
}

func proveCommitProject(ctx context.Context, d Deps, q diffQuery) (int64, error) {
	var ids []int64
	add := func(id int64) {
		if id < 1 {
			return
		}
		for _, have := range ids {
			if have == id {
				return
			}
		}
		ids = append(ids, id)
	}
	add(q.SourceProjectID)
	add(q.OwnerID)
	add(q.TargetProjectID)
	var last error
	for _, id := range ids {
		fromErr := proveCommit(ctx, d, id, q.Selection.From)
		if passthroughTypedProviderErr(fromErr) {
			return 0, fromErr
		}
		toErr := proveCommit(ctx, d, id, q.Selection.To)
		if passthroughTypedProviderErr(toErr) {
			return 0, toErr
		}
		if fromErr == nil && toErr == nil {
			return id, nil
		}
		last = errDiffUnproved
	}
	if last == nil {
		last = errDiffUnproved
	}
	return 0, last
}

func proveCommit(ctx context.Context, d Deps, projectID int64, sha string) error {
	path := fmt.Sprintf("projects/%s/repository/commits/%s", gitlab.PathEscape(strconv.FormatInt(projectID, 10)), gitlab.PathEscape(sha))
	st, resp, err := streamDiffObject(ctx, d.Client, path, nil, nil, nil)
	if err != nil {
		if passthroughTypedProviderErr(err) {
			return err
		}
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		if code == http.StatusNotFound || code == http.StatusForbidden || errors.Is(err, gitlab.ErrNotFound) {
			return errDiffUnproved
		}
		return err
	}
	env := st.fields
	if reviewJSONString(env["id"]) != sha {
		return errDiffUnproved
	}
	if raw, ok := env["project_id"]; ok && len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		got, ok := jsonInt(raw)
		if !ok || got != projectID {
			return errDiffUnproved
		}
	}
	return nil
}

type objectStream struct {
	fields  map[string]json.RawMessage
	entries []diffManifestEntry
	patches []streamedPatch
	diffsOK bool
	pathBad bool
}

type streamedPatch struct {
	text      string
	retained  bool
	overCap   bool
	dupKey    bool
	wrongType bool
	absent    bool
}

type patchCollect struct {
	want         map[string]struct{}
	maxCandidate int
	maxSelected  int
	selectedUsed int
}

func newPatchCollect(paths []string) *patchCollect {
	want := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		want[p] = struct{}{}
	}
	return &patchCollect{
		want:         want,
		maxCandidate: diffContentMaxCandidateBytes,
		maxSelected:  diffContentMaxSelectedBytes,
	}
}

func (c *patchCollect) wanted(entry diffManifestEntry) bool {
	if c == nil || len(c.want) == 0 {
		return false
	}
	_, okOld := c.want[pathStr(entry.OldPath)]
	_, okNew := c.want[pathStr(entry.NewPath)]
	return okOld || okNew
}

// streamDiffObject reads one JSON object with StreamJSONArrayQueue ownership:
// a derived child context and pipe are closed on every exit, including a
// callback stop, and the shared budget is never cancelled. Patch strings are
// discarded while scanning unless collect is non-nil and selects the entry.
// Bool flags stay raw JSON.
func streamDiffObject(ctx context.Context, client *gitlab.Client, path string, opt any, onEntry func(diffManifestEntry) error, collect *patchCollect) (objectStream, *gitlab.Response, error) {
	var zero objectStream
	if client == nil {
		return zero, nil, io.ErrUnexpectedEOF
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := client.NewRequest(http.MethodGet, path, opt, []gitlab.RequestOptionFunc{gitlab.WithContext(ctx)})
	if err != nil {
		return zero, nil, err
	}
	pr, pw := io.Pipe()
	var (
		decodeErr error
		result    objectStream
		wg        sync.WaitGroup
		stopOnce  sync.Once
	)
	stop := func(cause error) {
		stopOnce.Do(func() {
			decodeErr = cause
			cancel()
			_ = pr.CloseWithError(cause)
		})
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() { _ = pr.Close() }()
		dec := json.NewDecoder(pr)
		dec.UseNumber()
		parsed, err := decodeProviderObject(dec, onEntry, collect)
		if err != nil {
			stop(err)
			return
		}
		if _, err := dec.Token(); err != io.EOF {
			if err == nil {
				stop(fmt.Errorf("stream object: trailing input"))
			} else {
				stop(fmt.Errorf("stream object: trailing input: %w", err))
			}
			return
		}
		result = parsed
	}()
	resp, doErr := client.Do(req, pw)
	if doErr != nil {
		_ = pw.CloseWithError(doErr)
	} else {
		_ = pw.Close()
	}
	wg.Wait()
	if passthroughTypedProviderErr(decodeErr) {
		return zero, resp, decodeErr
	}
	if passthroughTypedProviderErr(doErr) {
		return zero, resp, doErr
	}
	if resp != nil && resp.StatusCode != http.StatusOK {
		return zero, resp, fmt.Errorf("%s: status %d", readmeta.CodeHTTPError, resp.StatusCode)
	}
	if decodeErr != nil {
		return zero, resp, decodeErr
	}
	return result, resp, doErr
}

func decodeProviderObject(dec *json.Decoder, onEntry func(diffManifestEntry) error, collect *patchCollect) (objectStream, error) {
	tok, err := dec.Token()
	if err != nil {
		return objectStream{}, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return objectStream{}, fmt.Errorf("stream object: expected '{'")
	}
	st := objectStream{fields: map[string]json.RawMessage{}}
	seen := map[string]struct{}{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return objectStream{}, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return objectStream{}, fmt.Errorf("stream object: key")
		}
		if _, dup := seen[key]; dup {
			return objectStream{}, fmt.Errorf("stream object: duplicate key %q", key)
		}
		seen[key] = struct{}{}
		if key == "diffs" {
			entries, patches, okDiff, bad, err := decodeDiffArray(dec, onEntry, collect)
			if err != nil {
				return objectStream{}, err
			}
			st.entries = entries
			st.patches = patches
			st.diffsOK = okDiff
			st.pathBad = bad
			continue
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return objectStream{}, err
		}
		st.fields[key] = raw
	}
	end, err := dec.Token()
	if err != nil {
		return objectStream{}, err
	}
	if d, ok := end.(json.Delim); !ok || d != '}' {
		return objectStream{}, fmt.Errorf("stream object: expected '}'")
	}
	return st, nil
}

func decodeDiffArray(dec *json.Decoder, onEntry func(diffManifestEntry) error, collect *patchCollect) ([]diffManifestEntry, []streamedPatch, bool, bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, false, false, err
	}
	if tok == nil {
		return nil, nil, false, false, nil
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '[' {
		if err := discardJSONValueAfterToken(dec, tok); err != nil {
			return nil, nil, false, false, err
		}
		return nil, nil, false, false, nil
	}
	var entries []diffManifestEntry
	var patches []streamedPatch
	pathBad := false
	seen := map[string]struct{}{}
	for dec.More() {
		entry, patch, bad, err := decodeEntry(dec, collect)
		if err != nil {
			return nil, nil, false, false, err
		}
		if bad || (pathStr(entry.OldPath) == "" && pathStr(entry.NewPath) == "") {
			pathBad = true
		}
		key := pathStr(entry.OldPath) + "\x00" + pathStr(entry.NewPath)
		if _, dup := seen[key]; dup {
			pathBad = true
		}
		seen[key] = struct{}{}
		if onEntry != nil {
			if err := onEntry(entry); err != nil {
				return nil, nil, false, false, err
			}
		}
		entries = append(entries, entry)
		patches = append(patches, patch)
	}
	end, err := dec.Token()
	if err != nil {
		return nil, nil, false, false, err
	}
	if d, ok := end.(json.Delim); !ok || d != ']' {
		return nil, nil, false, false, fmt.Errorf("stream object: expected ']'")
	}
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool {
		return manifestOrderLess(entries[order[i]], entries[order[j]])
	})
	sortedEntries := make([]diffManifestEntry, len(entries))
	sortedPatches := make([]streamedPatch, len(patches))
	for i, idx := range order {
		sortedEntries[i] = entries[idx]
		sortedPatches[i] = patches[idx]
	}
	return sortedEntries, sortedPatches, true, pathBad, nil
}

func decodeEntry(dec *json.Decoder, collect *patchCollect) (diffManifestEntry, streamedPatch, bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return diffManifestEntry{}, streamedPatch{}, false, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return diffManifestEntry{}, streamedPatch{}, false, fmt.Errorf("stream object: diff entry")
	}
	env := map[string]json.RawMessage{}
	var patch streamedPatch
	patch.absent = true
	sawDiff, sawPatch := false, false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return diffManifestEntry{}, streamedPatch{}, false, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return diffManifestEntry{}, streamedPatch{}, false, fmt.Errorf("stream object: entry key")
		}
		if key == "diff" || key == "patch" {
			if key == "diff" {
				if sawDiff || sawPatch {
					patch.dupKey = true
				}
				sawDiff = true
			} else {
				if sawPatch || sawDiff {
					patch.dupKey = true
				}
				sawPatch = true
			}
			if collect == nil || patch.dupKey {
				if err := discardJSONValue(dec); err != nil {
					return diffManifestEntry{}, streamedPatch{}, false, err
				}
				continue
			}
			var raw json.RawMessage
			if err := dec.Decode(&raw); err != nil {
				return diffManifestEntry{}, streamedPatch{}, false, err
			}
			trimmed := bytes.TrimSpace(raw)
			if len(trimmed) == 0 || string(trimmed) == "null" {
				patch.absent = true
				continue
			}
			var s string
			if json.Unmarshal(trimmed, &s) != nil {
				patch.wrongType = true
				patch.absent = false
				continue
			}
			patch.absent = false
			if len(s) > collect.maxCandidate {
				patch.overCap = true
			} else {
				patch.text = s
			}
			continue
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return diffManifestEntry{}, streamedPatch{}, false, err
		}
		if _, exists := env[key]; exists {
			patch.dupKey = true
		}
		env[key] = raw
	}
	end, err := dec.Token()
	if err != nil {
		return diffManifestEntry{}, streamedPatch{}, false, err
	}
	if d, ok := end.(json.Delim); !ok || d != '}' {
		return diffManifestEntry{}, streamedPatch{}, false, fmt.Errorf("stream object: entry end")
	}
	if sawDiff && sawPatch {
		patch.dupKey = true
		patch.text = ""
	}
	entry, bad := entryFromRaw(env)
	if collect != nil && !patch.dupKey && !patch.wrongType && !patch.absent && !patch.overCap && collect.wanted(entry) {
		if collect.selectedUsed+len(patch.text) > collect.maxSelected {
			patch.overCap = true
			patch.text = ""
		} else {
			collect.selectedUsed += len(patch.text)
			patch.retained = true
		}
	} else if !patch.retained {
		patch.text = ""
	}
	return entry, patch, bad, nil
}

func discardJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	return discardJSONValueAfterToken(dec, tok)
}

func discardJSONValueAfterToken(dec *json.Decoder, tok json.Token) error {
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if d != '{' && d != '[' {
		return nil
	}
	for dec.More() {
		if d == '{' {
			if _, err := dec.Token(); err != nil {
				return err
			}
		}
		if err := discardJSONValue(dec); err != nil {
			return err
		}
	}
	end, err := dec.Token()
	if err != nil {
		return err
	}
	if _, ok := end.(json.Delim); !ok {
		return fmt.Errorf("stream object: discard")
	}
	return nil
}
