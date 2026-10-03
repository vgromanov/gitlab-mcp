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

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	capabilityListCommitsV1 = "readmeta.list_commits.v1"
	cursorModePerPageCap    = 50
	cursorHardItemCap       = 100 // never retain/process more than invocation budget items
	errCursorKeyMissing     = "GITLAB_MCP_CURSOR_KEY is required for cursor mode; configure a raw secret of at least 32 bytes"
	codeResyncRequired      = cursor.ResyncRequired
)

func listCommitsCursor(ctx context.Context, in listCommitsIn, d Deps) (*mcp.CallToolResult, any, error) {
	if d.Config == nil || !d.Config.CursorSigningEnabled() {
		return nil, nil, errors.New(errCursorKeyMissing)
	}
	cursorTok := strings.TrimSpace(in.Cursor)
	if cursorTok != "" && in.Page > 1 {
		return nil, nil, fmt.Errorf("ambiguous pagination: do not pass page>1 with cursor; resume uses the signed cursor page state")
	}
	if cursorTok == "" && in.Page > 1 {
		return nil, nil, fmt.Errorf("cursor mode starts at page 1; omit page or use page=1 on the initial use_cursor request")
	}

	budget := igl.DefaultBudget()
	ctx = igl.WithBudget(ctx, budget)
	defer budget.Cancel()

	now := d.now()
	section := newListCommitsSection(now)

	if cursorTok != "" {
		return resumeListCommitsCursor(ctx, in, d, budget, section, now, cursorTok)
	}
	return initialListCommitsCursor(ctx, in, d, budget, section, now)
}

func initialListCommitsCursor(ctx context.Context, in listCommitsIn, d Deps, budget *igl.Budget, section readmeta.Section, now time.Time) (*mcp.CallToolResult, any, error) {
	actorID, err := resolveCursorActor(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	pid, err := resolveCursorProjectCanonical(ctx, d, in.ProjectID)
	if err != nil {
		return nil, nil, err
	}

	filters, tipSHA, err := buildInitialCommitFilters(in, now, d)
	if err != nil {
		return nil, nil, err
	}
	// Pin moving branch/HEAD → immutable tip (separate from original Filters.RefName selection).
	pinRef := strings.TrimSpace(in.RefName)
	if pinRef == "" {
		pinRef = "HEAD"
	}
	tip, _, err := d.Client.Commits.GetCommit(pid, pinRef, nil, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, fmt.Errorf("%s: pin ref", readmeta.CodeHTTPError)
	}
	var ok bool
	tipSHA, ok = readmeta.ObservedHeadSHA(tipSHAFromCommit(tip))
	if !ok {
		return nil, nil, fmt.Errorf("%s: pinned ref is not a valid immutable commit SHA", codeResyncRequired)
	}

	expiresAt := now.Add(d.Config.CursorTTL()).UTC().Format(time.RFC3339)
	refs := []string{tipSHA}
	page, result := streamCommitsPage(ctx, d, budget, pid, tipSHA, filters, 1)
	return emitListCommitsCursor(section, page, result, d, actorID, pid, refs, filters.Until, expiresAt, filters, 1)
}

func resumeListCommitsCursor(ctx context.Context, in listCommitsIn, d Deps, budget *igl.Budget, section readmeta.Section, now time.Time, tok string) (*mcp.CallToolResult, any, error) {
	payload, err := cursor.Decode(d.Config.CursorKey, tok, now)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: cursor validation failed", codeResyncRequired)
	}
	if payload.Tool != cursor.ToolListCommits || payload.Section != cursor.SectionListCommits {
		return nil, nil, fmt.Errorf("%s: tool/section mismatch", codeResyncRequired)
	}

	// Policy fingerprint + credential-safe instance before any authz/content work.
	// Allowlist edits that also revoke the old project must still surface resync_required
	// (binding mismatch), not authz_denied from reauthorization.
	instance, err := cursorInstance(d.Config)
	if err != nil {
		return nil, nil, err
	}
	policyFP := d.Config.PolicyFingerprint()
	if payload.Instance != instance || payload.PolicyFP != policyFP {
		return nil, nil, fmt.Errorf("%s: binding mismatch", codeResyncRequired)
	}

	actorID, err := resolveCursorActor(ctx, d)
	if err != nil {
		return nil, nil, err
	}
	// Same-policy revocation (fingerprint unchanged) fails closed via authz_denied here.
	pid, err := resolveCursorProjectCanonical(ctx, d, in.ProjectID)
	if err != nil {
		return nil, nil, err
	}

	// Independently normalize request selection; strict repeat of bound originals.
	reqFilters, err := normalizeCommitSelection(in)
	if err != nil {
		return nil, nil, err
	}
	if err := selectionMatchesBound(reqFilters, payload.Filters, payload.UpperBound); err != nil {
		return nil, nil, err
	}
	expected := payload.Filters // Until/Order/Selection already bound
	expected.RefName = reqFilters.RefName
	expected.Path = reqFilters.Path
	expected.Since = reqFilters.Since
	expected.CallerUntil = reqFilters.CallerUntil
	expected.PerPage = reqFilters.PerPage

	scope := cursor.Scope{Kind: cursor.ScopeProject, ProjectID: pid}
	if err := cursor.MatchBinding(payload, instance, actorID, policyFP, cursor.ToolListCommits, cursor.SectionListCommits, scope, expected, payload.ImmutableRefs, payload.UpperBound); err != nil {
		return nil, nil, fmt.Errorf("%s: binding mismatch", codeResyncRequired)
	}
	if payload.Scope.Kind != cursor.ScopeProject || payload.Scope.ProjectID != pid {
		return nil, nil, fmt.Errorf("%s: project scope mismatch", codeResyncRequired)
	}

	tipSHA := cursor.TipRef(payload.ImmutableRefs)
	if _, ok := readmeta.ObservedHeadSHA(tipSHA); !ok {
		return nil, nil, fmt.Errorf("%s: pinned ref binding", codeResyncRequired)
	}

	// Guard: re-fetch signed previous page before requesting next. Charges budget; never returned.
	prevPage := payload.PageState.Page
	guardPage, guardRes := streamCommitsPage(ctx, d, budget, pid, tipSHA, payload.Filters, prevPage)
	if guardRes.stopErr != nil || guardRes.boundaryBroken || guardRes.partial {
		return nil, nil, fmt.Errorf("%s: previous-page guard failed", codeResyncRequired)
	}
	shas, ok := pageCommitSHAs(guardPage)
	if !ok {
		return nil, nil, fmt.Errorf("%s: previous-page boundary drift", codeResyncRequired)
	}
	digest := cursor.SequenceDigest(shas)
	last := ""
	if len(shas) > 0 {
		last = shas[len(shas)-1]
	}
	if digest != payload.PageState.SequenceDigest || last != payload.PageState.LastSHA || len(shas) != payload.PageState.ItemsOnPage {
		return nil, nil, fmt.Errorf("%s: previous-page boundary drift", codeResyncRequired)
	}
	if payload.PageState.ProviderNextPage <= 0 {
		section.PaginationExhausted = true
		section.ContentComplete = readmeta.ContentCompleteUnknown
		section.Consistency = readmeta.ConsistencyUnknown
		section.NextCursor = nil
		section.AddLimitation(codeResyncRequired, "no further page in signed cursor state")
		return nil, Out(map[string]any{"commits": []*gitlab.Commit{}, "section": section}), nil
	}

	nextPage := int(payload.PageState.ProviderNextPage)
	page, result := streamCommitsPage(ctx, d, budget, pid, tipSHA, payload.Filters, nextPage)
	return emitListCommitsCursor(section, page, result, d, actorID, pid, payload.ImmutableRefs, payload.UpperBound, payload.ExpiresAt, payload.Filters, nextPage)
}

type streamPageResult struct {
	resp           *gitlab.Response
	partial        bool
	boundaryBroken bool
	stopErr        error
}

func streamCommitsPage(ctx context.Context, d Deps, budget *igl.Budget, pid, tipSHA string, filters cursor.Filters, page int) ([]*gitlab.Commit, streamPageResult) {
	opt := &gitlab.ListCommitsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(filters.PerPage)},
		RefName:     gitlab.Ptr(tipSHA), // always pin query to immutable SHA
	}
	if filters.Path != "" {
		opt.Path = gitlab.Ptr(filters.Path)
	}
	if filters.Since != "" {
		if t, err := parseCommitTime(filters.Since); err == nil {
			opt.Since = &t
		}
	}
	if filters.Until != "" {
		if t, err := parseCommitTime(filters.Until); err == nil {
			opt.Until = &t
		}
	}
	path := fmt.Sprintf("projects/%s/repository/commits", gitlab.PathEscape(pid))
	commits := make([]*gitlab.Commit, 0, filters.PerPage)
	seen := map[string]struct{}{}
	var result streamPageResult

	resp, err := igl.StreamJSONArray(ctx, d.Client, http.MethodGet, path, opt, func(raw json.RawMessage) error {
		if len(commits) >= cursorHardItemCap {
			result.partial = true
			result.stopErr = igl.ErrBudgetItems
			return igl.ErrBudgetItems
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			result.boundaryBroken = true
			result.partial = true
			result.stopErr = fmt.Errorf("%s: nil commit element", codeResyncRequired)
			return result.stopErr
		}
		var c gitlab.Commit
		if err := json.Unmarshal(trimmed, &c); err != nil {
			result.boundaryBroken = true
			result.partial = true
			result.stopErr = fmt.Errorf("%s: malformed commit element", codeResyncRequired)
			return result.stopErr
		}
		sha, ok := readmeta.ObservedHeadSHA(c.ID)
		if !ok {
			result.boundaryBroken = true
			result.partial = true
			result.stopErr = fmt.Errorf("%s: invalid commit SHA", codeResyncRequired)
			return result.stopErr
		}
		if _, dup := seen[sha]; dup {
			result.boundaryBroken = true
			result.partial = true
			result.stopErr = fmt.Errorf("%s: duplicate commit SHA", codeResyncRequired)
			return result.stopErr
		}
		// Charge before retain/append (single charge site; guard and content share this path).
		if err := budget.AddItem(); err != nil {
			result.partial = true
			result.stopErr = err
			return err
		}
		seen[sha] = struct{}{}
		cp := c
		commits = append(commits, &cp)
		return nil
	})
	result.resp = resp
	if err != nil && result.stopErr == nil {
		// Upstream/stream failure: retain any fully decoded items; mark partial.
		result.partial = true
		result.stopErr = fmt.Errorf("%s: list commits", readmeta.CodeHTTPError)
		if errors.Is(err, igl.ErrBudgetItems) || errors.Is(err, igl.ErrBudgetBytes) ||
			errors.Is(err, igl.ErrBudgetElapsed) || errors.Is(err, igl.ErrBudgetRequests) ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			result.stopErr = err
		}
	}
	if result.stopErr != nil {
		result.partial = true
	}
	return commits, result
}

func emitListCommitsCursor(
	section readmeta.Section,
	commits []*gitlab.Commit,
	result streamPageResult,
	d Deps,
	actorID int64,
	pid string,
	immutableRefs []string,
	upperBound, expiresAt string,
	filters cursor.Filters,
	page int,
) (*mcp.CallToolResult, any, error) {
	if section.HeadSHA == nil {
		if sha, ok := readmeta.ObservedHeadSHA(cursor.TipRef(immutableRefs)); ok {
			section.HeadSHA = &sha
		}
	}
	var hdr http.Header
	var sdkNext int64
	if result.resp != nil {
		if result.resp.Response != nil {
			hdr = result.resp.Response.Header
		}
		sdkNext = result.resp.NextPage
	}
	obs := readmeta.ObservePaging(hdr, sdkNext)
	section.ApplyPaging(obs)
	section.NextCursor = nil

	section.Consistency = readmeta.ConsistencyUnknown
	section.ContentComplete = readmeta.ContentCompleteUnknown
	section.ManifestCoverage = readmeta.CoverageUnknown
	section.PatchCoverage = readmeta.CoverageUnknown
	n := len(commits)
	section.Counts.Items = &n

	shas, shasOK := pageCommitSHAs(commits)
	if !shasOK {
		result.boundaryBroken = true
		result.partial = true
	}
	digest := cursor.SequenceDigest(shas)
	last := ""
	if len(shas) > 0 {
		last = shas[len(shas)-1]
	}

	// Any partial stream/budget/decode/boundary failure: retain items, never mint cursor,
	// never claim exhaustion, never skip unfinished page items.
	if result.partial || result.boundaryBroken || result.stopErr != nil {
		section.PaginationExhausted = false
		section.NextCursor = nil
		section.ContentComplete = readmeta.ContentCompleteFalse
		section.AddLimitation(codeForBudget(result.stopErr), "partial page retained; continuation not safe")
		section.AddLimitation(codeResyncRequired, "safe continuation boundary not proven")
		return nil, Out(map[string]any{"commits": commits, "section": section}), nil
	}

	cont, contOK := safeProviderContinuation(page, obs, sdkNext)
	if !contOK {
		section.PaginationExhausted = false
		section.NextCursor = nil
		section.ContentComplete = readmeta.ContentCompleteUnknown
		section.AddLimitation(readmeta.CodeUnknownCount, cont.reason)
		section.AddLimitation(codeResyncRequired, "unsafe or unproven provider next page")
		return nil, Out(map[string]any{"commits": commits, "section": section}), nil
	}
	if cont.exhausted {
		section.PaginationExhausted = true
		section.NextCursor = nil
		section.ContentComplete = readmeta.ContentCompleteUnknown
		return nil, Out(map[string]any{"commits": commits, "section": section}), nil
	}

	instance, err := cursorInstance(d.Config)
	if err != nil {
		section.NextCursor = nil
		section.PaginationExhausted = false
		section.AddLimitation(codeResyncRequired, "instance binding unavailable")
		return nil, Out(map[string]any{"commits": commits, "section": section}), nil
	}
	nextPayload := cursor.Payload{
		SchemaVersion: cursor.SchemaV1,
		Instance:      instance,
		ActorID:       actorID,
		PolicyFP:      d.Config.PolicyFingerprint(),
		Tool:          cursor.ToolListCommits,
		Section:       cursor.SectionListCommits,
		Scope:         cursor.Scope{Kind: cursor.ScopeProject, ProjectID: pid},
		Filters:       filters,
		ImmutableRefs: append([]string{}, immutableRefs...),
		UpperBound:    upperBound,
		ExpiresAt:     expiresAt,
		PageState: cursor.PageState{
			Page:             page,
			PerPage:          filters.PerPage,
			SequenceDigest:   digest,
			LastSHA:          last,
			ItemsOnPage:      len(shas),
			ProviderNextPage: cont.next,
		},
	}
	tok, err := cursor.Encode(d.Config.CursorKey, nextPayload)
	if err != nil {
		section.NextCursor = nil
		section.PaginationExhausted = false
		section.AddLimitation(codeResyncRequired, "failed to sign next cursor")
		return nil, Out(map[string]any{"commits": commits, "section": section}), nil
	}
	section.NextCursor = &tok
	section.PaginationExhausted = false
	return nil, Out(map[string]any{"commits": commits, "section": section}), nil
}

type providerContinuation struct {
	next      int64
	exhausted bool
	reason    string
}

// safeProviderContinuation accepts only explicit, non-jumping, non-repeating next pages.
// Never infers exhaustion from sdkNext<=0 when the header is unknown/invalid/contradictory.
func safeProviderContinuation(page int, obs readmeta.PagingObservation, sdkNext int64) (providerContinuation, bool) {
	if page < 1 {
		return providerContinuation{reason: "invalid current page"}, false
	}
	if !obs.PagingKnown || !obs.HeaderPresent {
		return providerContinuation{reason: "paging metadata unavailable"}, false
	}
	headerNext, err := parseNextPageHeader(obs.HeaderValue)
	if err != nil {
		return providerContinuation{reason: "malformed X-Next-Page"}, false
	}
	if headerNext != sdkNext {
		return providerContinuation{reason: "contradictory X-Next-Page vs SDK next"}, false
	}
	if headerNext == 0 {
		if obs.ExhaustedObserved {
			return providerContinuation{exhausted: true}, true
		}
		return providerContinuation{reason: "unproven exhaustion"}, false
	}
	if headerNext != int64(page)+1 {
		return providerContinuation{reason: "repeating or jumping X-Next-Page"}, false
	}
	return providerContinuation{next: headerNext}, true
}

func parseNextPageHeader(v string) (int64, error) {
	if v == "" || v == "0" {
		return 0, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad next page")
	}
	return n, nil
}

func buildInitialCommitFilters(in listCommitsIn, now time.Time, d Deps) (cursor.Filters, string, error) {
	sel, err := normalizeCommitSelection(in)
	if err != nil {
		return cursor.Filters{}, "", err
	}
	upper, err := discoveryUpperBound(sel.CallerUntil, now)
	if err != nil {
		return cursor.Filters{}, "", err
	}
	upperStr := upper.UTC().Format(time.RFC3339)
	return cursor.Filters{
		RefName:     sel.RefName,
		Path:        sel.Path,
		Since:       sel.Since,
		CallerUntil: sel.CallerUntil,
		Until:       upperStr,
		Order:       "provider_default",
		Selection:   "list_commits",
		PerPage:     sel.PerPage,
	}, "", nil
}

type commitSelection struct {
	RefName     string
	Path        string
	Since       string
	CallerUntil string
	PerPage     int
}

// normalizeCommitSelection independently normalizes request selection fields.
// Resume must strictly repeat these normalized originals (documented in docs/cursors.md).
func normalizeCommitSelection(in listCommitsIn) (commitSelection, error) {
	_, perPage := in.ListOpts()
	if perPage > cursorModePerPageCap {
		perPage = cursorModePerPageCap
	}
	sel := commitSelection{
		RefName: strings.TrimSpace(in.RefName),
		Path:    strings.TrimSpace(in.Path),
		PerPage: perPage,
	}
	if strings.TrimSpace(in.Since) != "" {
		t, err := parseCommitTime(in.Since)
		if err != nil {
			// Static projection — never wrap parseCommitTime (echoes raw input).
			return commitSelection{}, errors.New("invalid since")
		}
		sel.Since = t.UTC().Format(time.RFC3339)
	}
	if strings.TrimSpace(in.Until) != "" {
		t, err := parseCommitTime(in.Until)
		if err != nil {
			return commitSelection{}, errors.New("invalid until")
		}
		sel.CallerUntil = t.UTC().Format(time.RFC3339)
	}
	return sel, nil
}

func discoveryUpperBound(callerUntil string, now time.Time) (time.Time, error) {
	upper := now.UTC()
	if callerUntil == "" {
		return upper, nil
	}
	t, err := time.Parse(time.RFC3339, callerUntil)
	if err != nil {
		// callerUntil is normally normalized RFC3339; keep static if malformed.
		return time.Time{}, errors.New("invalid until")
	}
	if t.UTC().Before(upper) {
		return t.UTC(), nil
	}
	return upper, nil
}

func selectionMatchesBound(req commitSelection, bound cursor.Filters, upperBound string) error {
	if req.RefName != bound.RefName || req.Path != bound.Path || req.Since != bound.Since {
		return fmt.Errorf("%s: filter mismatch", codeResyncRequired)
	}
	if req.CallerUntil != bound.CallerUntil {
		return fmt.Errorf("%s: filter mismatch", codeResyncRequired)
	}
	if req.PerPage != bound.PerPage {
		return fmt.Errorf("%s: filter mismatch", codeResyncRequired)
	}
	if bound.Until != upperBound {
		return fmt.Errorf("%s: upper bound mismatch", codeResyncRequired)
	}
	return nil
}

func resolveCursorActor(ctx context.Context, d Deps) (int64, error) {
	u, _, err := d.Client.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil || u == nil || u.ID < 1 {
		return 0, fmt.Errorf("%s: authenticated actor", readmeta.CodeIdentityUnresolved)
	}
	return u.ID, nil
}

// resolveCursorProjectCanonical always resolves a numeric project identity via
// AuthorizeCanonicalProject (works with empty allowlists). Lookup RoundTrips are
// budgeted through ctx. Errors stay allowlisted codes — never echo Config/secrets
// or raw upstream bodies. Does not alter legacy resolveProjectAuthz behavior.
func resolveCursorProjectCanonical(ctx context.Context, d Deps, projectID string) (string, error) {
	c, err := AuthorizeCanonicalProject(ctx, d, projectID)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.HasPrefix(msg, readmeta.CodeAuthzDenied):
			return "", fmt.Errorf("%s: project not authorized", readmeta.CodeAuthzDenied)
		case strings.HasPrefix(msg, readmeta.CodeIdentityUnresolved):
			return "", fmt.Errorf("%s: project identity", readmeta.CodeIdentityUnresolved)
		default:
			return "", fmt.Errorf("%s: project identity", readmeta.CodeIdentityUnresolved)
		}
	}
	if c.ID < 1 {
		return "", fmt.Errorf("%s: project identity", readmeta.CodeIdentityUnresolved)
	}
	return strconv.FormatInt(c.ID, 10), nil
}

func tipSHAFromCommit(c *gitlab.Commit) string {
	if c == nil {
		return ""
	}
	return c.ID
}

// pageCommitSHAs returns provider-ordered valid 40-hex SHAs, or false on nil/invalid/duplicate.
func pageCommitSHAs(commits []*gitlab.Commit) ([]string, bool) {
	out := make([]string, 0, len(commits))
	seen := map[string]struct{}{}
	for _, c := range commits {
		if c == nil {
			return nil, false
		}
		sha, ok := readmeta.ObservedHeadSHA(c.ID)
		if !ok {
			return nil, false
		}
		if _, dup := seen[sha]; dup {
			return nil, false
		}
		seen[sha] = struct{}{}
		out = append(out, sha)
	}
	return out, true
}

// cursorInstance returns the credential-safe canonical instance binding.
// Never echoes cfg.APIURL (may contain userinfo/query secrets).
func cursorInstance(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", fmt.Errorf("%s: instance", codeResyncRequired)
	}
	inst, err := cursor.CanonicalInstance(cfg.APIURL)
	if err != nil {
		return "", fmt.Errorf("%s: instance", codeResyncRequired)
	}
	return inst, nil
}

func newListCommitsSection(now time.Time) readmeta.Section {
	return readmeta.Section{
		RetrievedAt:       now.UTC().Format(time.RFC3339),
		Source:            readmeta.SourceGitLabREST,
		Provider:          readmeta.ProviderGitLab,
		CapabilityVersion: capabilityListCommitsV1,
		HeadSHA:           nil,
		Limitations:       []readmeta.Limitation{},
		Counts:            readmeta.Counts{},
		ContentComplete:   readmeta.ContentCompleteUnknown,
		Consistency:       readmeta.ConsistencyUnknown,
		ManifestCoverage:  readmeta.CoverageUnknown,
		PatchCoverage:     readmeta.CoverageUnknown,
	}
}

func codeForBudget(err error) string {
	if err == nil {
		return readmeta.CodePartial
	}
	switch {
	case errors.Is(err, igl.ErrBudgetItems):
		return readmeta.CodeBudgetItems
	case errors.Is(err, igl.ErrBudgetBytes):
		return readmeta.CodeBudgetBytes
	case errors.Is(err, igl.ErrBudgetElapsed):
		return readmeta.CodeBudgetElapsed
	case errors.Is(err, igl.ErrBudgetRequests):
		return readmeta.CodeBudgetRequests
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return readmeta.CodeCancelled
	default:
		if strings.HasPrefix(err.Error(), codeResyncRequired) {
			return codeResyncRequired
		}
		return readmeta.CodePartial
	}
}
