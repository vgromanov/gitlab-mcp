package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// legacyMRDiffPage is one bounded inspected page of MR diffs with honesty metadata.
// Pagination.next_page and section.next_cursor are informational unsigned page hints
// (RVG-120 style); they are not an authenticated cursor codec and these getters do
// not accept a cursor input.
type legacyMRDiffPage struct {
	Diffs      []*MRDiffItem
	Pagination listMergeRequestDiffsPagination
	Section    readmeta.Section
	MR         *gitlab.MergeRequest
}

type legacyMRDiffFetchOpts struct {
	Owner           CanonicalProject
	MergeRequestIID int64
	PerPage         int64 // existing ceilings: 100 (diffs) or 200 (conflicts/file_diff)
	TruncateLines   int
	// FilterFiles enables path filtering (get_merge_request_file_diff). When true,
	// an empty/nil WantFiles list returns no diffs (legacy behavior) — it does NOT
	// mean "all files". Unmatched requested paths are unobserved, never absent.
	FilterFiles bool
	WantFiles   []string
}

// patchPresence distinguishes explicit empty patch content from absent/null/malformed.
type patchPresence int

const (
	patchAbsent patchPresence = iota
	patchNull
	patchEmptyString // explicit JSON ""
	patchNonEmpty
	patchWrongType
)

// ensureLegacyInvocationBudget attaches one DefaultBudget before authorization when
// the caller has not already supplied a contextual budget. Caller-supplied budgets
// are reused as-is (MaxItems not raised) so synthetic tight-cap tests remain meaningful.
// The returned release cancels owned deadline timers; it is a no-op when reusing.
func ensureLegacyInvocationBudget(ctx context.Context, perPage int64) (context.Context, func()) {
	if igl.BudgetFromContext(ctx) != nil {
		return ctx, func() {}
	}
	budget := igl.DefaultBudget()
	if perPage > 0 && int(perPage) > budget.MaxItems {
		budget.MaxItems = int(perPage)
	}
	ctx = igl.WithBudget(ctx, budget)
	return ctx, budget.Cancel
}

// fetchLegacyMRDiffPage streams a single first page of MR diffs with presence-aware
// section metadata. Caller must already have authorized owner/forks via
// authorizeMROwnerAndForks and should attach the invocation budget before that authz.
// This helper re-fetches MR metadata and re-applies requireProvenMRForkProjects under
// active policy before streaming content. It reuses any contextual budget and does
// not allocate/cancel a second timer when one is already present.
func fetchLegacyMRDiffPage(ctx context.Context, d Deps, opts legacyMRDiffFetchOpts) (legacyMRDiffPage, error) {
	out := legacyMRDiffPage{
		Diffs: make([]*MRDiffItem, 0), // empty backend page → JSON [] (SDK semantics)
	}
	if opts.MergeRequestIID < 1 {
		return out, fmt.Errorf("merge_request_iid must be >= 1")
	}
	perPage := opts.PerPage
	if perPage <= 0 {
		perPage = 100
	}

	budget := igl.BudgetFromContext(ctx)
	ownsBudget := false
	if budget == nil {
		// Fallback only — getters normally attach before authz.
		budget = igl.DefaultBudget()
		if int(perPage) > budget.MaxItems {
			budget.MaxItems = int(perPage)
		}
		ctx = igl.WithBudget(ctx, budget)
		ownsBudget = true
	}
	if ownsBudget {
		defer budget.Cancel()
	}

	pid := projectAPIID(opts.Owner)
	ownerID := opts.Owner.ID
	if ownerID <= 0 {
		if id, ok := parseStrictPositiveID(pid); ok {
			ownerID = id
		}
	}

	// Fresh MR metadata — do not assume the authorizeMROwnerAndForks snapshot still applies.
	mr, _, err := d.Client.MergeRequests.GetMergeRequest(pid, opts.MergeRequestIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return out, fmt.Errorf("%s: merge request metadata", readmeta.CodeHTTPError)
	}
	out.MR = mr

	if policyActive(d.Config) {
		if err := requireProvenMRForkProjects(ctx, d, opts.Owner, mr); err != nil {
			return out, err
		}
	} else if mr != nil {
		// Empty-policy compatibility (same pattern as list_merge_request_diffs).
		var extra []string
		if mr.SourceProjectID > 0 && mr.SourceProjectID != opts.Owner.ID {
			extra = append(extra, strconv.FormatInt(mr.SourceProjectID, 10))
		}
		if mr.ProjectID > 0 && mr.ProjectID != opts.Owner.ID {
			extra = append(extra, strconv.FormatInt(mr.ProjectID, 10))
		}
		if _, err := AuthorizeAdditionalProjects(ctx, d, extra...); err != nil {
			return out, err
		}
	}

	section := readmeta.NewMRDiffsSection(time.Now())
	headBefore := ""
	if mr != nil {
		if sha, ok := readmeta.ObservedHeadSHA(mr.DiffRefs.HeadSha); ok {
			headBefore = sha
			section.HeadSHA = &sha
		}
	}

	pathID := pid
	if ownerID > 0 {
		pathID = strconv.FormatInt(ownerID, 10)
	}
	apiPath := fmt.Sprintf("projects/%s/merge_requests/%d/diffs", gitlab.PathEscape(pathID), opts.MergeRequestIID)
	listOpt := &gitlab.ListMergeRequestDiffsOptions{
		ListOptions: gitlab.ListOptions{Page: 1, PerPage: perPage},
	}

	var sawCollapsed, sawTooLarge, sawOverflow bool
	var presenceUnknown, entryGap bool
	var streamStop error
	var truncated bool

	resp, err := igl.StreamJSONArray(ctx, d.Client, http.MethodGet, apiPath, listOpt, func(raw json.RawMessage) error {
		if err := budget.AddItem(); err != nil {
			streamStop = err
			return err
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			out.Diffs = append(out.Diffs, nil)
			entryGap = true
			section.AddLimitation(readmeta.CodePartial, "null diff entry retained")
			section.ContentComplete = readmeta.ContentCompleteUnknown
			return nil
		}
		var item MRDiffItem
		if err := json.Unmarshal(trimmed, &item); err != nil {
			out.Diffs = append(out.Diffs, nil)
			entryGap = true
			section.AddLimitation(readmeta.CodePartial, "malformed diff entry retained as null")
			section.ContentComplete = readmeta.ContentCompleteUnknown
			return nil
		}

		collapsedP, cerr := igl.DiffCollapsedPresence(trimmed)
		if cerr != nil {
			presenceUnknown = true
			section.AddLimitation(readmeta.CodePartial, "collapsed presence decode failed")
			section.ContentComplete = readmeta.ContentCompleteUnknown
		} else {
			item.Collapsed = readmeta.BoolFromPresence(collapsedP)
			switch collapsedP {
			case readmeta.PresenceTrue:
				sawCollapsed = true
			case readmeta.PresenceAbsent, readmeta.PresenceNull:
				presenceUnknown = true
				section.ContentComplete = readmeta.ContentCompleteUnknown
			}
		}
		tooLargeP, terr := igl.DiffTooLargePresence(trimmed)
		if terr != nil {
			presenceUnknown = true
			section.AddLimitation(readmeta.CodePartial, "too_large presence decode failed")
			section.ContentComplete = readmeta.ContentCompleteUnknown
		} else {
			item.TooLarge = readmeta.BoolFromPresence(tooLargeP)
			switch tooLargeP {
			case readmeta.PresenceTrue:
				sawTooLarge = true
			case readmeta.PresenceAbsent, readmeta.PresenceNull:
				presenceUnknown = true
				section.ContentComplete = readmeta.ContentCompleteUnknown
			}
		}

		// Optional server overflow flag (not projected onto MRDiffItem; list adapter unchanged).
		// Absent → ignore. True → incomplete. Null/wrong-type → unknown.
		overflowP, oerr := readmeta.DecodeBoolPresence(trimmed, "overflow")
		if oerr != nil {
			presenceUnknown = true
			section.AddLimitation(readmeta.CodePartial, "overflow presence decode failed")
			section.ContentComplete = readmeta.ContentCompleteUnknown
		} else {
			switch overflowP {
			case readmeta.PresenceTrue:
				sawOverflow = true
			case readmeta.PresenceNull:
				presenceUnknown = true
				section.ContentComplete = readmeta.ContentCompleteUnknown
			}
		}

		// Patch content presence: JSON unmarshal collapses absent/null to "" — track raw type.
		pp, perr := decodeDiffPatchPresence(trimmed)
		if perr != nil {
			presenceUnknown = true
			section.AddLimitation(readmeta.CodePartial, "diff patch presence decode failed")
			section.ContentComplete = readmeta.ContentCompleteUnknown
		} else {
			switch pp {
			case patchAbsent, patchNull:
				presenceUnknown = true
				section.AddLimitation(readmeta.CodePartial, "diff patch field omitted or null; content unknown")
				section.ContentComplete = readmeta.ContentCompleteUnknown
				// Preserve SDK item shape: Diff stays "" projection.
			case patchWrongType:
				presenceUnknown = true
				section.AddLimitation(readmeta.CodePartial, "diff patch field non-string; content unknown")
				section.ContentComplete = readmeta.ContentCompleteUnknown
				item.Diff = ""
			case patchEmptyString, patchNonEmpty:
				// Explicit known empty or non-empty string — OK for completeness if other flags allow.
			}
		}

		if opts.TruncateLines > 0 && pp == patchNonEmpty && item.Diff != "" {
			before := item.Diff
			item.Diff = TruncateLines(item.Diff, opts.TruncateLines)
			if item.Diff != before {
				truncated = true
			}
		}
		out.Diffs = append(out.Diffs, &item)
		return nil
	})

	sdkNext := int64(0)
	var hdr http.Header
	if resp != nil {
		sdkNext = resp.NextPage
		if resp.Response != nil {
			hdr = resp.Response.Header
		}
	}
	obs := readmeta.ObservePaging(hdr, sdkNext)
	section.ApplyPaging(obs)
	section.PaginationExhausted = obs.ExhaustedObserved

	n := len(out.Diffs)
	section.Counts.Items = &n
	_, bytesRead, _ := budget.Stats()
	bytesCopy := bytesRead
	section.Counts.Bytes = &bytesCopy
	section.Counts.Files = nil

	if sawCollapsed {
		section.AddLimitation(readmeta.CodeCollapsed, "one or more diffs collapsed")
	}
	if sawTooLarge {
		section.AddLimitation(readmeta.CodeTooLarge, "one or more diffs too_large")
	}
	if sawOverflow {
		section.AddLimitation(readmeta.CodePartial, "one or more diffs marked overflow by server")
	}
	if truncated {
		section.AddLimitation(readmeta.CodePartial, "local truncate_lines applied; patch content incomplete")
	}

	section.Consistency = readmeta.ConsistencyUnknown
	if headBefore != "" {
		mrAfter, _, herr := d.Client.MergeRequests.GetMergeRequest(pid, opts.MergeRequestIID, nil, gitlab.WithContext(ctx))
		if herr == nil && mrAfter != nil {
			if after, ok := readmeta.ObservedHeadSHA(mrAfter.DiffRefs.HeadSha); ok {
				if after == headBefore {
					section.Consistency = readmeta.ConsistencyConsistent
				} else {
					section.Consistency = readmeta.ConsistencyInconsistent
					section.AddLimitation(readmeta.CodeInconsistent, "head_sha drifted during read")
				}
			}
		}
	}

	unknownLatched := presenceUnknown || entryGap
	page := 1 // legacy getters inspect page 1 only

	switch {
	case errors.Is(err, context.Canceled) || errors.Is(streamStop, context.Canceled):
		section.AddLimitation(readmeta.CodeCancelled, "request cancelled")
		setContentComplete(&section, readmeta.ContentCompleteFalse, unknownLatched)
		section.Consistency = readmeta.ConsistencyUnknown
		section.ManifestCoverage = readmeta.CoveragePartial
		section.PatchCoverage = readmeta.CoveragePartial
	case errors.Is(err, igl.ErrBudgetBytes) || errors.Is(streamStop, igl.ErrBudgetBytes):
		section.AddLimitation(readmeta.CodeBudgetBytes, "response body budget exhausted")
		setContentComplete(&section, readmeta.ContentCompleteFalse, unknownLatched)
		section.ManifestCoverage = readmeta.CoveragePartial
		section.PatchCoverage = readmeta.CoveragePartial
	case errors.Is(err, igl.ErrBudgetItems) || errors.Is(streamStop, igl.ErrBudgetItems):
		section.AddLimitation(readmeta.CodeBudgetItems, "item budget exhausted")
		setContentComplete(&section, readmeta.ContentCompleteFalse, unknownLatched)
		section.ManifestCoverage = readmeta.CoveragePartial
		section.PatchCoverage = readmeta.CoveragePartial
	case errors.Is(err, igl.ErrBudgetRequests) || errors.Is(streamStop, igl.ErrBudgetRequests):
		section.AddLimitation(readmeta.CodeBudgetRequests, "request budget exhausted")
		setContentComplete(&section, readmeta.ContentCompleteFalse, unknownLatched)
		section.ManifestCoverage = readmeta.CoveragePartial
		section.PatchCoverage = readmeta.CoveragePartial
	case errors.Is(err, igl.ErrBudgetElapsed) || errors.Is(streamStop, igl.ErrBudgetElapsed) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(streamStop, context.DeadlineExceeded):
		section.AddLimitation(readmeta.CodeBudgetElapsed, "elapsed budget exhausted")
		setContentComplete(&section, readmeta.ContentCompleteFalse, unknownLatched)
		section.ManifestCoverage = readmeta.CoveragePartial
		section.PatchCoverage = readmeta.CoveragePartial
	case err != nil && len(out.Diffs) == 0:
		return out, fmt.Errorf("%s: backend request failed", readmeta.CodeHTTPError)
	case err != nil:
		if isStreamFramingError(err) {
			section.AddLimitation(readmeta.CodePartial, "incomplete JSON stream; partial results retained")
		} else {
			section.AddLimitation(readmeta.CodeHTTPError, "backend error after partial decode")
			section.AddLimitation(readmeta.CodePartial, "partial results retained")
		}
		setContentComplete(&section, readmeta.ContentCompleteFalse, unknownLatched)
		section.ManifestCoverage = readmeta.CoveragePartial
		section.PatchCoverage = readmeta.CoveragePartial
		section.Consistency = readmeta.ConsistencyUnknown
	default:
		// Fold server overflow into the collapsed slot for honesty math (list adapter unchanged).
		applySuccessHonesty(&section, page, obs, sawCollapsed || sawOverflow, sawTooLarge, presenceUnknown, entryGap)
		if truncated {
			setContentComplete(&section, readmeta.ContentCompleteFalse, unknownLatched)
			if section.PatchCoverage == readmeta.CoverageFull {
				section.PatchCoverage = readmeta.CoveragePartial
			}
		}
	}

	if opts.FilterFiles {
		out.Diffs, section = filterLegacyDiffsByWant(out.Diffs, opts.WantFiles, section, obs)
	}

	out.Pagination = listMergeRequestDiffsPagination{NextPage: sdkNext}
	out.Section = section
	return out, nil
}

// decodeDiffPatchPresence inspects raw JSON for the "diff" patch field type/presence.
// Explicit "" is known-empty; absent/null/wrong-type must not be treated as known empty.
func decodeDiffPatchPresence(raw json.RawMessage) (patchPresence, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return patchAbsent, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return 0, fmt.Errorf("diff presence: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return 0, fmt.Errorf("diff presence: expected object")
	}
	found := false
	var val json.RawMessage
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, fmt.Errorf("diff presence: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return 0, fmt.Errorf("diff presence: expected key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return 0, fmt.Errorf("diff presence: %w", err)
		}
		if key == "diff" {
			found = true
			val = append(json.RawMessage(nil), v...)
		}
	}
	closeTok, err := dec.Token()
	if err != nil {
		return 0, fmt.Errorf("diff presence: %w", err)
	}
	closeDelim, ok := closeTok.(json.Delim)
	if !ok || closeDelim != '}' {
		return 0, fmt.Errorf("diff presence: expected end of object")
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return 0, fmt.Errorf("diff presence: trailing input")
		}
		return 0, fmt.Errorf("diff presence: %w", err)
	}
	if !found {
		return patchAbsent, nil
	}
	s := bytes.TrimSpace(val)
	if bytes.Equal(s, []byte("null")) {
		return patchNull, nil
	}
	if len(s) >= 2 && s[0] == '"' {
		var str string
		if err := json.Unmarshal(s, &str); err != nil {
			return patchWrongType, nil
		}
		if str == "" {
			return patchEmptyString, nil
		}
		return patchNonEmpty, nil
	}
	return patchWrongType, nil
}

// filterLegacyDiffsByWant keeps matching diffs and marks requested paths missing from
// the inspected page as unobserved (never conclusive absence), especially on partial pages.
func filterLegacyDiffsByWant(diffs []*MRDiffItem, wantFiles []string, section readmeta.Section, obs readmeta.PagingObservation) ([]*MRDiffItem, readmeta.Section) {
	want := map[string]struct{}{}
	for _, f := range wantFiles {
		if f == "" {
			continue
		}
		want[f] = struct{}{}
	}
	seen := map[string]struct{}{}
	var filtered []*MRDiffItem // nil when empty → JSON null (legacy base shape)
	for _, df := range diffs {
		if df == nil {
			continue
		}
		_, wantNew := want[df.NewPath]
		_, wantOld := want[df.OldPath]
		if wantNew || (df.OldPath != "" && wantOld) {
			filtered = append(filtered, df)
			if wantNew {
				seen[df.NewPath] = struct{}{}
			}
			if wantOld {
				seen[df.OldPath] = struct{}{}
			}
		}
	}
	var missing []string
	for f := range want {
		if _, ok := seen[f]; !ok {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		ordered := make([]string, 0, len(missing))
		for _, f := range wantFiles {
			for _, m := range missing {
				if f == m {
					ordered = append(ordered, f)
					break
				}
			}
		}
		section.AddLimitation(readmeta.CodePartial,
			"requested file(s) not observed on inspected page (unobserved, not proven absent): "+strings.Join(ordered, ", "))
		if !obs.ExhaustedObserved || !obs.PagingKnown || obs.SDKNextPage > 0 {
			setContentComplete(&section, readmeta.ContentCompleteFalse, true)
			section.ManifestCoverage = readmeta.CoveragePartial
			if section.PatchCoverage == readmeta.CoverageFull {
				section.PatchCoverage = readmeta.CoveragePartial
			}
		} else {
			setContentComplete(&section, readmeta.ContentCompleteUnknown, true)
			if section.ManifestCoverage == readmeta.CoverageFull {
				section.ManifestCoverage = readmeta.CoveragePartial
			}
		}
	}
	n := len(filtered)
	section.Counts.Items = &n
	return filtered, section
}

func scanConflictFiles(diffs []*MRDiffItem) []string {
	var conflictFiles []string // nil when empty → JSON null (legacy base shape)
	for _, df := range diffs {
		if df == nil {
			continue
		}
		if strings.Contains(df.Diff, "<<<<<<<") || strings.Contains(df.Diff, ">>>>>>>") {
			conflictFiles = append(conflictFiles, df.NewPath)
		}
	}
	return conflictFiles
}

// annotateConflictScanCoverage records that conflict_files is a heuristic page scan,
// independent of authoritative has_conflicts / detailed_merge_status.
func annotateConflictScanCoverage(section *readmeta.Section) {
	section.AddLimitation(readmeta.CodePartial,
		"conflict_files is a heuristic marker scan of the inspected diffs page only; does not override has_conflicts or detailed_merge_status")
}
