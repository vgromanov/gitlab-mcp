package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// MRDiffItem preserves the SDK MergeRequestDiff JSON projection shape.
type MRDiffItem struct {
	OldPath       string `json:"old_path"`
	NewPath       string `json:"new_path"`
	AMode         string `json:"a_mode"`
	BMode         string `json:"b_mode"`
	Diff          string `json:"diff"`
	NewFile       bool   `json:"new_file"`
	RenamedFile   bool   `json:"renamed_file"`
	DeletedFile   bool   `json:"deleted_file"`
	GeneratedFile bool   `json:"generated_file"`
	Collapsed     bool   `json:"collapsed"`
	TooLarge      bool   `json:"too_large"`
}

// listMergeRequestDiffsPagination preserves legacy pagination.next_page.
type listMergeRequestDiffsPagination struct {
	NextPage int64 `json:"next_page"`
}

// listMergeRequestDiffsOut is the registered structured output (outputSchema).
// Diffs uses []*MRDiffItem to match SDK []*MergeRequestDiff: empty → JSON [], null elements preserved.
type listMergeRequestDiffsOut struct {
	Diffs      []*MRDiffItem                   `json:"diffs"`
	Pagination listMergeRequestDiffsPagination `json:"pagination"`
	Section    readmeta.Section                `json:"section"`
}

func listMergeRequestDiffs(ctx context.Context, _ *mcp.CallToolRequest, in listMergeRequestDiffsIn, d Deps) (*mcp.CallToolResult, listMergeRequestDiffsOut, error) {
	budget := igl.DefaultBudget()
	ctx = igl.WithBudget(ctx, budget)
	// Release the WithBudget deadline timer on every return path (authz/error/success).
	defer budget.Cancel()

	owner, err := AuthorizeCanonicalProject(ctx, d, in.ProjectID)
	if err != nil {
		return nil, listMergeRequestDiffsOut{}, err
	}
	if in.MergeRequestIID < 1 {
		return nil, listMergeRequestDiffsOut{}, fmt.Errorf("merge_request_iid must be >= 1")
	}

	// Owner authorized — discover fork/downstream via MR metadata, then authz those before content.
	mr, _, err := d.Client.MergeRequests.GetMergeRequest(owner.ID, in.MergeRequestIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, listMergeRequestDiffsOut{}, fmt.Errorf("%s: merge request metadata", readmeta.CodeHTTPError)
	}
	if policyActive(d.Config) {
		if err := requireProvenMRForkProjects(ctx, d, owner, mr); err != nil {
			return nil, listMergeRequestDiffsOut{}, err
		}
	} else if mr != nil {
		// Empty-policy compatibility: optional additional resolve when source differs (no fail-closed).
		var extra []string
		if mr.SourceProjectID > 0 && mr.SourceProjectID != owner.ID {
			extra = append(extra, strconv.FormatInt(mr.SourceProjectID, 10))
		}
		if mr.ProjectID > 0 && mr.ProjectID != owner.ID {
			extra = append(extra, strconv.FormatInt(mr.ProjectID, 10))
		}
		if _, err := AuthorizeAdditionalProjects(ctx, d, extra...); err != nil {
			return nil, listMergeRequestDiffsOut{}, err
		}
	}

	page, perPage := in.ListOpts()
	opt := &gitlab.ListMergeRequestDiffsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		Unidiff:     in.Unidiff,
	}
	path := fmt.Sprintf("projects/%s/merge_requests/%d/diffs", gitlab.PathEscape(strconv.FormatInt(owner.ID, 10)), in.MergeRequestIID)

	section := readmeta.NewMRDiffsSection(time.Now())
	headBefore := ""
	if mr != nil {
		if sha, ok := readmeta.ObservedHeadSHA(mr.DiffRefs.HeadSha); ok {
			headBefore = sha
			section.HeadSHA = &sha
		}
		// Invalid/non-40-hex observation stays null; consistency remains unknown.
	}

	// Non-nil empty slice so valid backend [] projects as JSON [] (not null), matching SDK.
	diffs := make([]*MRDiffItem, 0)
	var sawCollapsed, sawTooLarge bool
	var presenceUnknown bool // collapsed/too_large absent|null|decode-fail
	var entryGap bool        // null/malformed array elements retained as nil
	var streamStop error

	resp, err := igl.StreamJSONArray(ctx, d.Client, http.MethodGet, path, opt, func(raw json.RawMessage) error {
		if err := budget.AddItem(); err != nil {
			streamStop = err
			return err
		}
		trimmed := bytes.TrimSpace(raw)
		// Legacy []*MergeRequestDiff preserves JSON null elements as nil pointers.
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			diffs = append(diffs, nil)
			entryGap = true
			section.AddLimitation(readmeta.CodePartial, "null diff entry retained")
			section.ContentComplete = readmeta.ContentCompleteUnknown
			return nil
		}
		var item MRDiffItem
		if err := json.Unmarshal(trimmed, &item); err != nil {
			diffs = append(diffs, nil)
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
		diffs = append(diffs, &item)
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
	section.PaginationExhausted = obs.ExhaustedObserved // reaffirm locked rule

	// Counts.Items = retained element count (including null entries; excluding rejected over-budget).
	n := len(diffs)
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

	// Content-head bracketing: only with valid 40-hex observations; never fabricate optimistic consistent.
	section.Consistency = readmeta.ConsistencyUnknown
	if headBefore != "" {
		mrAfter, _, herr := d.Client.MergeRequests.GetMergeRequest(owner.ID, in.MergeRequestIID, nil, gitlab.WithContext(ctx))
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
	case err != nil && len(diffs) == 0:
		// Fatal with nothing retained — safe fixed code/message only (no raw SDK/API body).
		return nil, listMergeRequestDiffsOut{}, fmt.Errorf("%s: backend request failed", readmeta.CodeHTTPError)
	case err != nil:
		// Stream/JSON integrity failures are partial, not HTTP backend failures.
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
		applySuccessHonesty(&section, page, obs, sawCollapsed, sawTooLarge, presenceUnknown, entryGap)
	}

	out := listMergeRequestDiffsOut{
		Diffs:      diffs,
		Pagination: listMergeRequestDiffsPagination{NextPage: sdkNext},
		Section:    section,
	}
	return nil, out, nil
}

// setContentComplete applies claim without overwriting a presence/decode unknown latch into complete.
func setContentComplete(s *readmeta.Section, claim string, unknownLatched bool) {
	if unknownLatched && claim == readmeta.ContentCompleteTrue {
		s.ContentComplete = readmeta.ContentCompleteUnknown
		return
	}
	s.ContentComplete = claim
}

// applySuccessHonesty sets content_complete and independent coverages on the clean success path.
func applySuccessHonesty(s *readmeta.Section, page int, obs readmeta.PagingObservation, sawCollapsed, sawTooLarge, presenceUnknown, entryGap bool) {
	unknownLatched := presenceUnknown || entryGap

	// Head drift rejects any completeness claim of a stable read.
	if s.Consistency == readmeta.ConsistencyInconsistent {
		setContentComplete(s, readmeta.ContentCompleteFalse, unknownLatched)
		s.ManifestCoverage = readmeta.CoveragePartial
		s.PatchCoverage = readmeta.CoveragePartial
		return
	}

	wholeMRKnown := obs.PagingKnown && obs.ExhaustedObserved && page <= 1
	partialPage := obs.PagingKnown && (!obs.ExhaustedObserved || page > 1)

	// Patch coverage: needs known non-collapsed/non-overflow presence on an observed page set.
	switch {
	case presenceUnknown || entryGap || !obs.PagingKnown:
		s.PatchCoverage = readmeta.CoverageUnknown
	case sawCollapsed || sawTooLarge || partialPage:
		s.PatchCoverage = readmeta.CoveragePartial
	case wholeMRKnown:
		s.PatchCoverage = readmeta.CoverageFull
	default:
		s.PatchCoverage = readmeta.CoverageUnknown
	}

	// Manifest coverage: independent of collapsed/too_large (paths still listed).
	// Last page alone with Page>1 or unknown paging cannot prove a complete MR manifest.
	// Null/malformed retained entries leave path slots unknown ⇒ not full.
	switch {
	case !obs.PagingKnown:
		s.ManifestCoverage = readmeta.CoverageUnknown
	case partialPage || entryGap:
		s.ManifestCoverage = readmeta.CoveragePartial
	case wholeMRKnown:
		s.ManifestCoverage = readmeta.CoverageFull
	default:
		s.ManifestCoverage = readmeta.CoverageUnknown
	}

	// content_complete: exhaustion alone never proves true; missing/null presence stays unknown.
	switch {
	case sawCollapsed || sawTooLarge:
		setContentComplete(s, readmeta.ContentCompleteFalse, unknownLatched)
	case presenceUnknown || entryGap || !obs.PagingKnown:
		setContentComplete(s, readmeta.ContentCompleteUnknown, unknownLatched)
	case !obs.ExhaustedObserved || page > 1:
		setContentComplete(s, readmeta.ContentCompleteFalse, unknownLatched)
	case wholeMRKnown && s.PatchCoverage == readmeta.CoverageFull && s.ManifestCoverage == readmeta.CoverageFull:
		setContentComplete(s, readmeta.ContentCompleteTrue, unknownLatched)
	default:
		setContentComplete(s, readmeta.ContentCompleteUnknown, unknownLatched)
	}
}

func isStreamFramingError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "stream array:") || strings.Contains(msg, "stream array element:")
}
