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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	"github.com/hashicorp/go-retryablehttp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	capabilityMRApprovalsV1 = "readmeta.mr_approvals.v1"
	endpointApprovalState   = "approval_state"
	endpointApprovals       = "approvals"

	rulesCapabilityFull    = "full"
	rulesCapabilityPartial = "partial"
	rulesCapabilityUnknown = "unknown"
)

// approvalBodyCapture wraps a response body so non-EOF read failures survive
// SDK CheckResponse (which discards io.ReadAll errors). Legitimate empty bodies
// that reach clean EOF leave readErr nil.
type approvalBodyCapture struct {
	io.ReadCloser
	mu      sync.Mutex
	readErr error
}

func (c *approvalBodyCapture) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		c.mu.Lock()
		if c.readErr == nil {
			c.readErr = err
		}
		c.mu.Unlock()
	}
	return n, err
}

func (c *approvalBodyCapture) capturedReadErr() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readErr
}

// approvalReadCheckRetry never retries. It wraps the response body before the
// SDK error-body ReadAll so truncated/chunked/custom read failures are retained.
// Original transport errors are passed through unchanged.
func approvalReadCheckRetry(cap **approvalBodyCapture) retryablehttp.CheckRetry {
	return func(_ context.Context, resp *http.Response, err error) (bool, error) {
		if resp != nil && resp.Body != nil {
			if _, ok := resp.Body.(*approvalBodyCapture); !ok {
				c := &approvalBodyCapture{ReadCloser: resp.Body}
				*cap = c
				resp.Body = c
			}
		}
		return false, err
	}
}

// requirePostReadStillValid rechecks cancellation/elapsed after an SDK read
// completes and before publishing normalized success. Exhausted request/byte
// budgets for a *completed* read do not invalidate that read (next-dispatch
// gating is separate via budgetAllowsNext).
func requirePostReadStillValid(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return mapTransportOrBudgetErr(ctx, err)
	}
	return nil
}

// mrApprovalRuleObs is one presence-aware rule observation.
type mrApprovalRuleObs struct {
	ID                   *int64  `json:"id"`
	Name                 *string `json:"name"`
	Approved             *bool   `json:"approved"`
	ApprovalsRequired    *int64  `json:"approvals_required"`
	ContainsHiddenGroups *bool   `json:"contains_hidden_groups"`
}

// mrApprovalReadResult is the normalized MCP structured output for both endpoints.
type mrApprovalReadResult struct {
	Endpoint          string              `json:"endpoint"`
	Approved          *bool               `json:"approved"`
	ActorApproved     *bool               `json:"actor_approved"`
	ActorCanApprove   *bool               `json:"actor_can_approve"`
	ApprovalsRequired *int64              `json:"approvals_required"`
	ApprovalsLeft     *int64              `json:"approvals_left"`
	Rules             []mrApprovalRuleObs `json:"rules"`
	RulesLeft         []mrApprovalRuleObs `json:"rules_left"`
	RulesCapability   string              `json:"rules_capability"`
	RulesComplete     string              `json:"rules_complete"`
	Section           readmeta.Section    `json:"section"`
}

func newApprovalReadResult(endpoint string, section readmeta.Section) mrApprovalReadResult {
	return mrApprovalReadResult{
		Endpoint:        endpoint,
		Rules:           nil,
		RulesLeft:       nil,
		RulesCapability: rulesCapabilityUnknown,
		RulesComplete:   readmeta.ContentCompleteUnknown,
		Section:         section,
	}
}

func newApprovalsSection(now time.Time) readmeta.Section {
	return readmeta.Section{
		RetrievedAt:         now.UTC().Format(time.RFC3339),
		Source:              readmeta.SourceGitLabREST,
		Provider:            readmeta.ProviderGitLab,
		CapabilityVersion:   capabilityMRApprovalsV1,
		HeadSHA:             nil, // approval endpoints never supply head_sha
		PaginationExhausted: true,
		ContentComplete:     readmeta.ContentCompleteUnknown,
		Consistency:         readmeta.ConsistencyUnknown,
		Limitations:         []readmeta.Limitation{},
		NextCursor:          nil,
		Counts:              readmeta.Counts{},
		ManifestCoverage:    readmeta.CoverageUnknown,
		PatchCoverage:       readmeta.CoverageUnknown,
	}
}

// ensureApprovalInvocationBudget reuses an upstream budget or attaches DefaultBudget.
func ensureApprovalInvocationBudget(ctx context.Context) (context.Context, *igl.Budget, func()) {
	if b := igl.BudgetFromContext(ctx); b != nil {
		return ctx, b, func() {}
	}
	b := igl.DefaultBudget()
	ctx = igl.WithBudget(ctx, b)
	return ctx, b, b.Cancel
}

// authorizeAndVerifyApprovalMR canonicalizes the configured owner in BOTH policy
// modes, verifies THIS MR's positive owner/source IDs and exact IID/owner match,
// then authorizes source/downstream before any approval content read.
func authorizeAndVerifyApprovalMR(ctx context.Context, d Deps, projectID string, mrIID int64) (CanonicalProject, error) {
	if mrIID < 1 {
		return CanonicalProject{}, fmt.Errorf("merge_request_iid must be >= 1")
	}
	// Always canonicalize via GetProject (inactive allow-all still resolves identity).
	owner, err := AuthorizeCanonicalProject(ctx, d, projectID)
	if err != nil {
		return CanonicalProject{}, err
	}
	if owner.ID <= 0 {
		return CanonicalProject{}, identityErr("unproven canonical owner")
	}

	pid := projectAPIID(owner)
	mr, _, err := d.Client.MergeRequests.GetMergeRequest(pid, mrIID, nil, gitlab.WithContext(ctx))
	if err != nil {
		return CanonicalProject{}, fmt.Errorf("%s: merge request metadata", readmeta.CodeHTTPError)
	}
	if mr == nil {
		return CanonicalProject{}, identityErr("merge request metadata missing")
	}
	if mr.IID != mrIID {
		return CanonicalProject{}, identityErr("merge request iid mismatch")
	}
	if mr.ProjectID <= 0 {
		return CanonicalProject{}, identityErr("unproven merge request owner project")
	}
	if mr.SourceProjectID <= 0 {
		return CanonicalProject{}, identityErr("unproven merge request source project")
	}
	if mr.ProjectID != owner.ID {
		return CanonicalProject{}, identityErr("merge request owner mismatch")
	}
	if err := requireProvenMRForkProjects(ctx, d, owner, mr); err != nil {
		return CanonicalProject{}, err
	}
	return owner, nil
}

func approvalSafeErr(code, kind string) error {
	return fmt.Errorf("%s: %s", code, kind)
}

func mapTransportOrBudgetErr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return approvalSafeErr(readmeta.CodeCancelled, "request cancelled")
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, igl.ErrBudgetElapsed) {
		return approvalSafeErr(readmeta.CodeBudgetElapsed, "elapsed budget exhausted")
	}
	if errors.Is(err, igl.ErrBudgetBytes) {
		return approvalSafeErr(readmeta.CodeBudgetBytes, "byte budget exhausted")
	}
	if errors.Is(err, igl.ErrBudgetRequests) {
		return approvalSafeErr(readmeta.CodeBudgetRequests, "request budget exhausted")
	}
	if errors.Is(err, igl.ErrBudgetItems) {
		return approvalSafeErr(readmeta.CodeBudgetItems, "item budget exhausted")
	}
	if ctx.Err() != nil {
		return mapTransportOrBudgetErr(ctx, ctx.Err())
	}
	return approvalSafeErr(readmeta.CodeHTTPError, "transport failure")
}

func approvalHTTPStatusErr(status int) error {
	switch {
	case status == http.StatusNotFound:
		return approvalSafeErr(readmeta.CodeUnsupported, "approval resource unavailable")
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return approvalSafeErr(readmeta.CodeInaccessible, "authorization denied")
	case status == http.StatusTooManyRequests:
		return approvalSafeErr(readmeta.CodeHTTPError, "rate limited")
	case status >= 500 && status <= 599:
		return approvalSafeErr(readmeta.CodeHTTPError, "upstream server error")
	case status == http.StatusMethodNotAllowed:
		return approvalSafeErr(readmeta.CodeUnsupported, "method not allowed")
	default:
		return approvalSafeErr(readmeta.CodeHTTPError, "upstream request failed")
	}
}

func budgetAllowsNext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return mapTransportOrBudgetErr(ctx, err)
	}
	b := igl.BudgetFromContext(ctx)
	if b == nil {
		return nil
	}
	reqs, bytes, _ := b.Stats()
	if b.MaxRequests > 0 && reqs >= b.MaxRequests {
		return approvalSafeErr(readmeta.CodeBudgetRequests, "request budget exhausted")
	}
	if b.MaxBytes > 0 && bytes >= b.MaxBytes {
		return approvalSafeErr(readmeta.CodeBudgetBytes, "byte budget exhausted")
	}
	if b.MaxElapsed > 0 {
		// Elapsed is enforced by ctx deadline + chargeRequest; recheck context.
		if err := ctx.Err(); err != nil {
			return mapTransportOrBudgetErr(ctx, err)
		}
	}
	return nil
}

type approvalRawRead struct {
	Status   int
	Body     []byte
	Response *http.Response
	Err      error // transport / budget / decode infrastructure
}

func readApprovalResource(ctx context.Context, d Deps, projectPath string, mrIID int64, resource string) approvalRawRead {
	path := fmt.Sprintf("projects/%s/merge_requests/%d/%s", gitlab.PathEscape(projectPath), mrIID, resource)
	var bodyCap *approvalBodyCapture
	req, err := d.Client.NewRequest(http.MethodGet, path, nil, []gitlab.RequestOptionFunc{
		gitlab.WithContext(ctx),
		gitlab.WithRequestRetry(approvalReadCheckRetry(&bodyCap)),
	})
	if err != nil {
		return approvalRawRead{Err: mapTransportOrBudgetErr(ctx, err)}
	}

	var buf bytes.Buffer
	resp, err := d.Client.Do(req, &buf)
	out := approvalRawRead{}
	if resp != nil && resp.Response != nil {
		out.Response = resp.Response
		out.Status = resp.StatusCode
	}
	// Body-read failures (truncated Content-Length, chunked mid-stream, custom
	// reader errors, budget-capped reads) must fail closed even when the SDK
	// status error discarded the io.ReadAll error. Clean EOF / empty bodies OK.
	if readErr := bodyCap.capturedReadErr(); readErr != nil {
		out.Err = mapTransportOrBudgetErr(ctx, readErr)
		return out
	}
	if err != nil {
		if errors.Is(err, gitlab.ErrNotFound) {
			out.Status = http.StatusNotFound
			if out.Response == nil && resp != nil {
				out.Response = resp.Response
			}
			return out
		}
		var er *gitlab.ErrorResponse
		if errors.As(err, &er) && er.Response != nil {
			out.Response = er.Response
			out.Status = er.Response.StatusCode
			out.Body = append([]byte(nil), er.Body...)
			return out
		}
		out.Err = mapTransportOrBudgetErr(ctx, err)
		return out
	}
	out.Body = append([]byte(nil), buf.Bytes()...)
	return out
}

func responseWasRedirected(resp *http.Response) bool {
	if resp == nil || resp.Request == nil {
		return true
	}
	// Go sets Request.Response on the request that followed a redirect response.
	if resp.Request.Response != nil {
		return true
	}
	return false
}

func tlsVerifiedTrust(resp *http.Response) bool {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil {
		return false
	}
	if !strings.EqualFold(resp.Request.URL.Scheme, "https") {
		return false
	}
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return false
	}
	// VerifiedChains empty ⇒ verification disabled/missing (e.g. InsecureSkipVerify).
	if len(resp.TLS.VerifiedChains) == 0 {
		return false
	}
	return true
}

func configuredAPIAuthority(d Deps) (*url.URL, error) {
	if d.Client == nil {
		return nil, approvalSafeErr(readmeta.CodeHTTPError, "client missing")
	}
	u := d.Client.BaseURL()
	if u == nil {
		return nil, approvalSafeErr(readmeta.CodeHTTPError, "api base missing")
	}
	if u.Scheme != "https" {
		return nil, approvalSafeErr(readmeta.CodeHTTPError, "api base not https")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" {
		return nil, approvalSafeErr(readmeta.CodeHTTPError, "api base ambiguous")
	}
	return u, nil
}

// expectedApprovalEscapedPath mirrors client-go NewRequest RawPath join:
// base.Path + "projects/{PathEscape(pid)}/merge_requests/{iid}/{resource}".
func expectedApprovalEscapedPath(base *url.URL, projectPath string, mrIID int64, resource string) string {
	rel := fmt.Sprintf("projects/%s/merge_requests/%d/%s", gitlab.PathEscape(projectPath), mrIID, resource)
	return base.Path + rel
}

// matchesConfiguredTarget requires the exact original GET against the configured
// HTTPS API base + escaped project + IID + resource. Suffix-only path matches,
// EqualFold methods, query/userinfo/fragment, or alternate prefixes fail closed.
func matchesConfiguredTarget(resp *http.Response, base *url.URL, projectPath string, mrIID int64, resource string) bool {
	if resp == nil || resp.Request == nil || resp.Request.URL == nil || base == nil {
		return false
	}
	if resp.Request.Method != http.MethodGet {
		return false
	}
	u := resp.Request.URL
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.Opaque != "" {
		return false
	}
	if u.Scheme != base.Scheme || u.Host != base.Host {
		return false
	}
	want := expectedApprovalEscapedPath(base, projectPath, mrIID, resource)
	got := u.EscapedPath()
	return got == want
}

// allowExcludesGET parses every Allow header field value; returns ok=false on any ambiguity.
func allowExcludesGET(h http.Header) bool {
	if h == nil {
		return false
	}
	vals := h.Values("Allow")
	if len(vals) == 0 {
		return false
	}
	sawMethod := false
	for _, raw := range vals {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return false
		}
		for _, part := range strings.Split(raw, ",") {
			tok := strings.TrimSpace(part)
			if tok == "" {
				return false
			}
			// Reject quoted tokens / separators other than bare method tokens.
			if strings.ContainsAny(tok, "\"'()<>@;:\\[]?={} \t") {
				return false
			}
			if tok != strings.ToUpper(tok) && tok != strings.ToLower(tok) && !isHTTPToken(tok) {
				return false
			}
			if !isHTTPToken(tok) {
				return false
			}
			sawMethod = true
			if strings.EqualFold(tok, http.MethodGet) {
				return false
			}
		}
	}
	return sawMethod
}

func isHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+.^_`|~-", rune(c)):
		default:
			return false
		}
	}
	return true
}

// qualifiesApprovalStateMethodFallback implements the USERAPPROVED bounded-405 predicate.
func qualifiesApprovalStateMethodFallback(d Deps, projectPath string, mrIID int64, read approvalRawRead) bool {
	if read.Err != nil || read.Status != http.StatusMethodNotAllowed || read.Response == nil {
		return false
	}
	base, err := configuredAPIAuthority(d)
	if err != nil {
		return false
	}
	if responseWasRedirected(read.Response) {
		return false
	}
	if !tlsVerifiedTrust(read.Response) {
		return false
	}
	if !matchesConfiguredTarget(read.Response, base, projectPath, mrIID, endpointApprovalState) {
		return false
	}
	if !allowExcludesGET(read.Response.Header) {
		return false
	}
	return true
}

func presenceBoolPtr(p readmeta.Presence) *bool {
	switch p {
	case readmeta.PresenceTrue:
		v := true
		return &v
	case readmeta.PresenceFalse:
		v := false
		return &v
	default:
		return nil
	}
}

func decodeOptionalInt64(raw json.RawMessage, field string) (*int64, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, fmt.Errorf("expected object")
	}
	var found json.RawMessage
	have := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("bad key")
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		if key == field {
			have = true
			found = append(json.RawMessage(nil), v...)
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing")
		}
		return nil, err
	}
	if !have {
		return nil, nil
	}
	s := string(bytes.TrimSpace(found))
	if s == "null" || s == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("field %q type", field)
	}
	return &n, nil
}

func validateJSONObjectRoot(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("empty")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return fmt.Errorf("non-object root")
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing junk")
		}
		return err
	}
	return nil
}

func validateKnownBoolField(envelope map[string]json.RawMessage, field string) error {
	raw, ok := envelope[field]
	if !ok {
		return nil
	}
	s := string(bytes.TrimSpace(raw))
	switch s {
	case "true", "false", "null":
		return nil
	default:
		return fmt.Errorf("field %q type", field)
	}
}

func validateKnownArrayField(envelope map[string]json.RawMessage, field string) (json.RawMessage, bool, error) {
	raw, ok := envelope[field]
	if !ok {
		return nil, false, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if bytes.Equal(trimmed, []byte("null")) {
		return trimmed, true, nil
	}
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, true, fmt.Errorf("field %q type", field)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(trimmed, &arr); err != nil {
		return nil, true, fmt.Errorf("field %q type", field)
	}
	return trimmed, true, nil
}

func chargeRetainedItems(ctx context.Context, n int) error {
	if n <= 0 {
		return nil
	}
	b := igl.BudgetFromContext(ctx)
	if b == nil {
		return nil
	}
	for i := 0; i < n; i++ {
		if err := b.AddItem(); err != nil {
			return approvalSafeErr(readmeta.CodeBudgetItems, "item budget exhausted")
		}
	}
	return nil
}

func decodeApprovalStateRaw(ctx context.Context, raw []byte, section *readmeta.Section) (mrApprovalReadResult, error) {
	out := newApprovalReadResult(endpointApprovalState, *section)
	if err := validateJSONObjectRoot(raw); err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approval_state payload")
	}

	// approval_state has no top-level overall approval observation — keep unknown.
	out.Approved = nil
	out.ActorApproved = nil
	out.ActorCanApprove = nil

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approval_state payload")
	}
	if err := validateKnownBoolField(envelope, "approval_rules_overwritten"); err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approval_state payload")
	}

	rulesRaw, hasRules, err := validateKnownArrayField(envelope, "rules")
	if err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approval_state rules")
	}
	if !hasRules {
		out.RulesCapability = rulesCapabilityUnknown
		out.RulesComplete = readmeta.ContentCompleteUnknown
		section.ContentComplete = readmeta.ContentCompleteUnknown
		section.AddLimitation(readmeta.CodeUnknownCount, "approval rules not present")
		out.Section = *section
		return out, nil
	}
	if bytes.Equal(rulesRaw, []byte("null")) {
		out.Rules = nil
		out.RulesCapability = rulesCapabilityUnknown
		out.RulesComplete = readmeta.ContentCompleteUnknown
		section.ContentComplete = readmeta.ContentCompleteUnknown
		section.AddLimitation(readmeta.CodePartial, "approval rules null")
		out.Section = *section
		return out, nil
	}
	var ruleObjs []json.RawMessage
	if err := json.Unmarshal(rulesRaw, &ruleObjs); err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approval_state rules")
	}
	out.Rules = make([]mrApprovalRuleObs, 0, len(ruleObjs))
	hidden := false
	for _, rr := range ruleObjs {
		obs, err := decodeRuleObs(rr)
		if err != nil {
			return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approval rule")
		}
		if err := chargeRetainedItems(ctx, 1); err != nil {
			return out, err
		}
		out.Rules = append(out.Rules, obs)
		if obs.ContainsHiddenGroups != nil && *obs.ContainsHiddenGroups {
			hidden = true
		}
	}
	n := len(out.Rules)
	section.Counts.Items = &n
	// Full rules field present on approval_state (including explicit empty array).
	out.RulesCapability = rulesCapabilityFull
	// Completeness stays unknown unless an explicit incompleteness signal exists.
	// Do not infer complete=true from approved booleans, empty rules, or missing hidden-groups.
	if hidden {
		out.RulesComplete = readmeta.ContentCompleteFalse
		section.ContentComplete = readmeta.ContentCompleteFalse
		section.AddLimitation(readmeta.CodePartial, "rules contain hidden groups")
	} else {
		out.RulesComplete = readmeta.ContentCompleteUnknown
		section.ContentComplete = readmeta.ContentCompleteUnknown
		section.AddLimitation(readmeta.CodeUnknownCount, "rule completeness not certified by approval_state")
	}
	out.Section = *section
	return out, nil
}

func decodeRuleObs(raw json.RawMessage) (mrApprovalRuleObs, error) {
	var obs mrApprovalRuleObs
	if err := validateJSONObjectRoot(raw); err != nil {
		return obs, err
	}
	ap, err := readmeta.DecodeBoolPresence(raw, "approved")
	if err != nil {
		return obs, err
	}
	obs.Approved = presenceBoolPtr(ap)
	hp, err := readmeta.DecodeBoolPresence(raw, "contains_hidden_groups")
	if err != nil {
		return obs, err
	}
	obs.ContainsHiddenGroups = presenceBoolPtr(hp)
	id, err := decodeOptionalInt64(raw, "id")
	if err != nil {
		return obs, err
	}
	obs.ID = id
	req, err := decodeOptionalInt64(raw, "approvals_required")
	if err != nil {
		return obs, err
	}
	obs.ApprovalsRequired = req

	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return obs, err
	}
	if nameRaw, ok := env["name"]; ok {
		s := string(bytes.TrimSpace(nameRaw))
		if s == "null" {
			// null name → leave unknown
		} else if s != "" {
			var name string
			if err := json.Unmarshal(nameRaw, &name); err != nil {
				return obs, fmt.Errorf("name type")
			}
			obs.Name = &name
		} else {
			return obs, fmt.Errorf("name type")
		}
	}
	return obs, nil
}

func decodeApprovalsLegacyRaw(ctx context.Context, raw []byte, section *readmeta.Section) (mrApprovalReadResult, error) {
	out := newApprovalReadResult(endpointApprovals, *section)
	if err := validateJSONObjectRoot(raw); err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approvals payload")
	}

	ap, err := readmeta.DecodeBoolPresence(raw, "approved")
	if err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approvals payload")
	}
	out.Approved = presenceBoolPtr(ap)

	ua, err := readmeta.DecodeBoolPresence(raw, "user_has_approved")
	if err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approvals payload")
	}
	out.ActorApproved = presenceBoolPtr(ua)

	uc, err := readmeta.DecodeBoolPresence(raw, "user_can_approve")
	if err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approvals payload")
	}
	out.ActorCanApprove = presenceBoolPtr(uc)

	req, err := decodeOptionalInt64(raw, "approvals_required")
	if err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approvals payload")
	}
	out.ApprovalsRequired = req
	left, err := decodeOptionalInt64(raw, "approvals_left")
	if err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approvals payload")
	}
	out.ApprovalsLeft = left

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approvals payload")
	}
	for _, f := range []string{"has_approval_rules", "merge_request_approvers_available", "multiple_approval_rules_available", "require_password_to_approve"} {
		if err := validateKnownBoolField(envelope, f); err != nil {
			return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approvals payload")
		}
	}

	rulesLeftRaw, hasRulesLeft, err := validateKnownArrayField(envelope, "approval_rules_left")
	if err != nil {
		return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approval_rules_left")
	}
	switch {
	case !hasRulesLeft:
		out.RulesLeft = nil
		out.RulesCapability = rulesCapabilityUnknown
		section.AddLimitation(readmeta.CodeUnknownCount, "approval_rules_left not present")
	case bytes.Equal(rulesLeftRaw, []byte("null")):
		out.RulesLeft = nil
		out.RulesCapability = rulesCapabilityUnknown
		section.AddLimitation(readmeta.CodePartial, "approval_rules_left null")
	default:
		var objs []json.RawMessage
		if err := json.Unmarshal(rulesLeftRaw, &objs); err != nil {
			return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approval_rules_left")
		}
		out.RulesLeft = make([]mrApprovalRuleObs, 0, len(objs))
		for _, rr := range objs {
			obs, err := decodeRuleObs(rr)
			if err != nil {
				return out, approvalSafeErr(readmeta.CodeHTTPError, "malformed approval_rules_left entry")
			}
			if err := chargeRetainedItems(ctx, 1); err != nil {
				return out, err
			}
			out.RulesLeft = append(out.RulesLeft, obs)
		}
		n := len(out.RulesLeft)
		section.Counts.Items = &n
		out.RulesCapability = rulesCapabilityPartial
		section.AddLimitation(readmeta.CodePartial, "legacy approvals endpoint exposes partial rule observations")
	}
	// Global approved never certifies full rule approval / completeness.
	if out.Approved != nil && *out.Approved {
		section.AddLimitation(readmeta.CodePartial, "global approved does not certify full rule approval")
	}
	out.RulesComplete = readmeta.ContentCompleteUnknown
	section.ContentComplete = readmeta.ContentCompleteUnknown
	out.Section = *section
	return out, nil
}

func readNormalizedApprovals(ctx context.Context, d Deps, owner CanonicalProject, mrIID int64) (mrApprovalReadResult, error) {
	res, _, err := readReviewApprovals(ctx, d, owner, mrIID)
	return res, err
}

// readReviewApprovals is the review-context seam. The raw body is the exact
// payload hashed before presence collapse. Public approval JSON is unchanged.
func readReviewApprovals(ctx context.Context, d Deps, owner CanonicalProject, mrIID int64) (mrApprovalReadResult, []byte, error) {
	section := newApprovalsSection(time.Now())
	projectPath := projectAPIID(owner)

	primary := readApprovalResource(ctx, d, projectPath, mrIID, endpointApprovalState)
	if primary.Err != nil {
		return mrApprovalReadResult{Section: section}, nil, primary.Err
	}

	switch {
	case primary.Status == http.StatusOK:
		if responseWasRedirected(primary.Response) {
			return mrApprovalReadResult{Section: section}, nil, approvalSafeErr(readmeta.CodeHTTPError, "redirected approval_state response")
		}
		out, err := decodeApprovalStateRaw(ctx, primary.Body, &section)
		if err != nil {
			return mrApprovalReadResult{Section: section}, primary.Body, err
		}
		if err := requirePostReadStillValid(ctx); err != nil {
			return mrApprovalReadResult{Section: section}, primary.Body, err
		}
		return out, primary.Body, nil

	case primary.Status == http.StatusMethodNotAllowed:
		if err := budgetAllowsNext(ctx); err != nil {
			return mrApprovalReadResult{Section: section}, nil, err
		}
		if !qualifiesApprovalStateMethodFallback(d, projectPath, mrIID, primary) {
			return mrApprovalReadResult{Section: section}, nil, approvalHTTPStatusErr(http.StatusMethodNotAllowed)
		}
		section.AddLimitation(readmeta.CodeUnsupported, "approval_state GET currently unavailable; cause unknown")
		legacy := readApprovalResource(ctx, d, projectPath, mrIID, endpointApprovals)
		if legacy.Err != nil {
			return mrApprovalReadResult{Section: section}, nil, legacy.Err
		}
		if legacy.Status != http.StatusOK {
			return mrApprovalReadResult{Section: section}, legacy.Body, approvalHTTPStatusErr(legacy.Status)
		}
		if responseWasRedirected(legacy.Response) {
			return mrApprovalReadResult{Section: section}, nil, approvalSafeErr(readmeta.CodeHTTPError, "redirected approvals response")
		}
		out, err := decodeApprovalsLegacyRaw(ctx, legacy.Body, &section)
		if err != nil {
			return mrApprovalReadResult{Section: section}, legacy.Body, err
		}
		if err := requirePostReadStillValid(ctx); err != nil {
			return mrApprovalReadResult{Section: section}, legacy.Body, err
		}
		return out, legacy.Body, nil

	default:
		return mrApprovalReadResult{Section: section}, primary.Body, approvalHTTPStatusErr(primary.Status)
	}
}

type approvalDigestRule struct {
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Approved             string `json:"approved"`
	ApprovalsRequired    string `json:"approvals_required"`
	ContainsHiddenGroups string `json:"contains_hidden_groups"`
}

type approvalDigestDoc struct {
	Endpoint          string                `json:"endpoint"`
	Approved          string                `json:"approved"`
	ActorApproved     string                `json:"actor_approved"`
	ActorCanApprove   string                `json:"actor_can_approve"`
	ApprovalsRequired string                `json:"approvals_required"`
	ApprovalsLeft     string                `json:"approvals_left"`
	RulesState        string                `json:"rules_state"`
	Rules             []approvalDigestRule  `json:"rules"`
	RulesLeftState    string                `json:"rules_left_state"`
	RulesLeft         []approvalDigestRule  `json:"rules_left"`
	Limitations       []readmeta.Limitation `json:"limitations"`
	Result            json.RawMessage       `json:"result,omitempty"`
	Error             string                `json:"error,omitempty"`
}

// approvalSemanticDigest hashes presence before bool collapse. Envelope clocks are omitted.
func approvalSemanticDigest(endpoint string, raw []byte, limitations []readmeta.Limitation) (string, error) {
	doc := approvalDigestDoc{
		Endpoint:    endpoint,
		Limitations: sortedLimitations(limitations),
		Rules:       []approvalDigestRule{},
		RulesLeft:   []approvalDigestRule{},
	}
	env, err := objectFields(raw)
	if err != nil {
		return "", err
	}
	doc.Approved = presenceLabel(raw, "approved")
	doc.ActorApproved = presenceLabel(raw, "user_has_approved")
	doc.ActorCanApprove = presenceLabel(raw, "user_can_approve")
	doc.ApprovalsRequired = scalarLabel(env, "approvals_required")
	doc.ApprovalsLeft = scalarLabel(env, "approvals_left")
	rules, rulesState, err := ruleDigest(env, "rules")
	if err != nil {
		return "", err
	}
	left, leftState, err := ruleDigest(env, "approval_rules_left")
	if err != nil {
		return "", err
	}
	doc.Rules, doc.RulesState = rules, rulesState
	doc.RulesLeft, doc.RulesLeftState = left, leftState
	return hashDigestDoc(doc)
}

// approvalFailureDigest hashes an error skeleton with a null success object and no clocks.
func approvalFailureDigest(endpoint, code string, limitations []readmeta.Limitation) (string, error) {
	doc := approvalDigestDoc{
		Endpoint:          endpoint,
		Approved:          string(readmeta.PresenceAbsent),
		ActorApproved:     string(readmeta.PresenceAbsent),
		ActorCanApprove:   string(readmeta.PresenceAbsent),
		ApprovalsRequired: "absent",
		ApprovalsLeft:     "absent",
		RulesState:        "absent",
		Rules:             []approvalDigestRule{},
		RulesLeftState:    "absent",
		RulesLeft:         []approvalDigestRule{},
		Limitations:       sortedLimitations(limitations),
		Result:            json.RawMessage("null"),
		Error:             code,
	}
	return hashDigestDoc(doc)
}

func hashDigestDoc(doc approvalDigestDoc) (string, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func sortedLimitations(in []readmeta.Limitation) []readmeta.Limitation {
	out := append([]readmeta.Limitation(nil), in...)
	if out == nil {
		out = []readmeta.Limitation{}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Code != out[j].Code {
			return out[i].Code < out[j].Code
		}
		return out[i].Message < out[j].Message
	})
	return out
}

func presenceLabel(raw []byte, field string) string {
	p, err := readmeta.DecodeBoolPresence(raw, field)
	if err != nil {
		return "invalid"
	}
	return string(p)
}

func objectFields(raw []byte) (map[string]json.RawMessage, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	return env, nil
}

func scalarLabel(env map[string]json.RawMessage, field string) string {
	raw, ok := env[field]
	if !ok {
		return "absent"
	}
	s := string(bytes.TrimSpace(raw))
	if s == "null" {
		return "null"
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return "invalid"
	}
	return strconv.FormatInt(n, 10)
}

func ruleDigest(env map[string]json.RawMessage, field string) ([]approvalDigestRule, string, error) {
	raw, ok := env[field]
	if !ok {
		return []approvalDigestRule{}, "absent", nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []approvalDigestRule{}, "null", nil
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, "", err
	}
	out := make([]approvalDigestRule, 0, len(rows))
	for _, row := range rows {
		fields, err := objectFields(row)
		if err != nil {
			return nil, "", err
		}
		name := "absent"
		if nraw, ok := fields["name"]; ok {
			if bytes.Equal(bytes.TrimSpace(nraw), []byte("null")) {
				name = "null"
			} else {
				var s string
				if err := json.Unmarshal(nraw, &s); err != nil {
					return nil, "", err
				}
				name = "value:" + s
			}
		}
		out = append(out, approvalDigestRule{
			ID:                   scalarLabel(fields, "id"),
			Name:                 name,
			Approved:             presenceLabel(row, "approved"),
			ApprovalsRequired:    scalarLabel(fields, "approvals_required"),
			ContainsHiddenGroups: presenceLabel(row, "contains_hidden_groups"),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return approvalRuleLess(out[i], out[j])
	})
	return out, "rows", nil
}

func approvalRuleLess(a, b approvalDigestRule) bool {
	ai, aerr := strconv.ParseInt(a.ID, 10, 64)
	bi, berr := strconv.ParseInt(b.ID, 10, 64)
	if aerr == nil && berr == nil && ai != bi {
		return ai < bi
	}
	if (aerr == nil) != (berr == nil) {
		return aerr == nil
	}
	if a.ID != b.ID {
		return a.ID < b.ID
	}
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	if a.Approved != b.Approved {
		return a.Approved < b.Approved
	}
	if a.ApprovalsRequired != b.ApprovalsRequired {
		return a.ApprovalsRequired < b.ApprovalsRequired
	}
	return a.ContainsHiddenGroups < b.ContainsHiddenGroups
}
