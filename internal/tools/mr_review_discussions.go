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

	"github.com/hashicorp/go-retryablehttp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/cursor"
	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	capabilityReviewContextDiscussionsV1 = "readmeta.review_context.discussions.v1"
	discussionsSemanticSchema            = "discussions.semantic_feedback.v1"
	discussionsPositionSchema            = "discussions.position.v1"
	discussionsFullSchema                = "discussions.full_revision.v1"
	discussionsEvidenceSchema            = "discussions.evidence.v1"
	reviewBracketReserveRequests         = 4
	// Closing bracket charges two reviewChargeItem calls plus one
	// selectReviewVersion row. Two reserved items let that row fail and
	// clear the sibling ref, so the reserve is three.
	reviewBracketReserveItems = 3
	reviewBracketReserveBytes = 64 << 10
	discussionsPerPage        = 20
	discussionsNoteCeiling    = 8 << 20
)

var (
	errDiscLocal   = errors.New("discussions local byte limit")
	errDiscNoteCap = errors.New("discussions note ceiling")
	errDiscDupKey  = errors.New("discussions duplicate json key")
	errDiscStop    = errors.New("discussions note stop")
	errDiscUnwind  = errors.New("discussions unwind")
	errDiscResync  = errors.New("discussion cursor resync")
	errDiscElapsed = errors.New("discussions signed bound")
)

type discussionSelection string

func (s *discussionSelection) UnmarshalJSON(b []byte) error {
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		return errors.New("discussion_selection")
	}
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = discussionSelection(v)
	return nil
}

type discussionNoteOut struct {
	DiscussionID string  `json:"discussion_id"`
	NoteID       *string `json:"note_id"`
	System       *bool   `json:"system"`
	AuthorID     *int64  `json:"author_id"`
	Body         *string `json:"body"`
}

type discussionsView struct {
	Returned               string               `json:"returned"`
	SemanticFeedbackDigest *string              `json:"semantic_feedback_digest"`
	PositionDigest         *string              `json:"position_digest"`
	FullRevisionDigest     *string              `json:"full_revision_digest"`
	Notes                  *[]discussionNoteOut `json:"notes"`
	ReturnedCount          *int                 `json:"returned_count"`
	Section                readmeta.Section     `json:"section"`

	evidence      string
	claimAll      bool
	claimSemantic bool
	semHex        *string
	posHex        *string
	fullHex       *string
}

type presenceValue struct {
	State string `json:"state"`
	Value string `json:"value"`
}

func absentP() presenceValue        { return presenceValue{State: "absent"} }
func nullP() presenceValue          { return presenceValue{State: "null"} }
func valueP(v string) presenceValue { return presenceValue{State: "value", Value: v} }
func boolP(on bool) presenceValue {
	if on {
		return presenceValue{State: "true"}
	}
	return presenceValue{State: "false"}
}

type semRecord struct {
	DiscussionID   string        `json:"discussion_id"`
	IndividualNote presenceValue `json:"individual_note"`
	NoteID         presenceValue `json:"note_id"`
	AuthorID       presenceValue `json:"author_id"`
	Body           presenceValue `json:"body"`
	Resolvable     presenceValue `json:"resolvable"`
	Resolved       presenceValue `json:"resolved"`
	ResolvedBy     presenceValue `json:"resolved_by"`
}

type posRecord struct {
	DiscussionID string        `json:"discussion_id"`
	NoteID       presenceValue `json:"note_id"`
	Position     presenceValue `json:"position"`
}

type fullRecord struct {
	DiscussionID   string        `json:"discussion_id"`
	IndividualNote presenceValue `json:"individual_note"`
	NoteID         presenceValue `json:"note_id"`
	System         presenceValue `json:"system"`
	Type           presenceValue `json:"type"`
	AuthorID       presenceValue `json:"author_id"`
	Body           presenceValue `json:"body"`
	Resolvable     presenceValue `json:"resolvable"`
	Resolved       presenceValue `json:"resolved"`
	ResolvedBy     presenceValue `json:"resolved_by"`
	ResolvedAt     presenceValue `json:"resolved_at"`
	CreatedAt      presenceValue `json:"created_at"`
	UpdatedAt      presenceValue `json:"updated_at"`
	ExpiresAt      presenceValue `json:"expires_at"`
	Position       presenceValue `json:"position"`
	CommitID       presenceValue `json:"commit_id"`
	Internal       presenceValue `json:"internal"`
	NoteableID     presenceValue `json:"noteable_id"`
	NoteableType   presenceValue `json:"noteable_type"`
	NoteableIID    presenceValue `json:"noteable_iid"`
}

type positionCanon struct {
	BaseSHA      presenceValue `json:"base_sha"`
	StartSHA     presenceValue `json:"start_sha"`
	HeadSHA      presenceValue `json:"head_sha"`
	PositionType presenceValue `json:"position_type"`
	OldPath      presenceValue `json:"old_path"`
	NewPath      presenceValue `json:"new_path"`
	OldLine      presenceValue `json:"old_line"`
	NewLine      presenceValue `json:"new_line"`
	LineRange    presenceValue `json:"line_range"`
}

type discussionsEvidenceBundle struct {
	Schema     string `json:"schema"`
	Capability string `json:"capability"`
	Selection  string `json:"selection"`
	Semantic   string `json:"semantic_feedback_digest"`
	Position   string `json:"position_digest"`
	Full       string `json:"full_revision_digest"`
}

type keptNote struct {
	discID     string
	noteText   string
	noteNum    int64
	isShell    bool
	system     bool
	includeSem bool
	ok         bool
	sem        semRecord
	pos        posRecord
	full       fullRecord
	out        discussionNoteOut
}

type discCoord struct {
	P       int
	DI      int
	NI      int
	DID     string
	DiscIDs []string
	NoteIDs []string
}

func (c discCoord) less(o discCoord) bool {
	if c.P != o.P {
		return c.P < o.P
	}
	if c.DI != o.DI {
		return c.DI < o.DI
	}
	return c.NI < o.NI
}

func (c discCoord) same(o discCoord) bool {
	return c.P == o.P && c.DI == o.DI && c.NI == o.NI
}

type discScanState struct {
	seenDisc map[string]struct{}
	seenNote map[string]struct{}
}

type discDecoded struct {
	closed       bool
	notes        []discussionNoteOut
	kept         []keptNote
	inspected    int
	terminal     string
	limitation   string
	next         discCoord
	discussions  int
	withhold     bool
	inconsistent bool
	cursorOK     bool
}

type discussionsQuery struct {
	Page    int `url:"page,omitempty"`
	PerPage int `url:"per_page,omitempty"`
}

type discLimitReader struct {
	r io.ReadCloser
	n int64
}

func (l *discLimitReader) Read(p []byte) (int, error) {
	if l.n <= 0 {
		return 0, errDiscLocal
	}
	if int64(len(p)) > l.n {
		p = p[:l.n]
	}
	n, err := l.r.Read(p)
	l.n -= int64(n)
	return n, err
}

func (l *discLimitReader) Close() error { return l.r.Close() }

func discussionsReadCheckRetry(cap **approvalBodyCapture, limit int64) retryablehttp.CheckRetry {
	return func(ctx context.Context, resp *http.Response, err error) (bool, error) {
		if errors.Is(err, igl.ErrExactReadRedirect) {
			return false, err
		}
		if resp != nil && resp.Body != nil {
			if _, ok := resp.Body.(*approvalBodyCapture); !ok {
				inner := io.ReadCloser(resp.Body)
				if limit >= 0 {
					inner = &discLimitReader{r: resp.Body, n: limit}
				}
				c := &approvalBodyCapture{ReadCloser: inner}
				*cap = c
				resp.Body = c
			}
		}
		return igl.SafeReadCheckRetry(ctx, resp, err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func prefixHash(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	return sha256Hex([]byte(strings.Join(ids, "\n")))
}

func itemDiscSelection(item reviewContextItemIn) string {
	if item.DiscussionSelection == "" {
		return "semantic"
	}
	return string(item.DiscussionSelection)
}

func discRefsOf(b reviewBracket) []string {
	return []string{b.SourceSHA, b.TargetSHA, b.Head, b.Base, b.Start}
}

func sameDiscRefs(got []string, b reviewBracket) bool {
	want := discRefsOf(b)
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func discSection(now time.Time) readmeta.Section {
	return readmeta.Section{
		RetrievedAt:         now.UTC().Format(time.RFC3339),
		Source:              readmeta.SourceGitLabREST,
		Provider:            readmeta.ProviderGitLab,
		CapabilityVersion:   capabilityReviewContextDiscussionsV1,
		HeadSHA:             nil,
		PaginationExhausted: false,
		ContentComplete:     readmeta.ContentCompleteFalse,
		Consistency:         readmeta.ConsistencyUnknown,
		Limitations:         []readmeta.Limitation{},
		NextCursor:          nil,
		Counts:              readmeta.Counts{},
		ManifestCoverage:    readmeta.CoverageUnknown,
		PatchCoverage:       readmeta.CoverageUnknown,
	}
}

func hashDoc(schema string, records any) (string, error) {
	raw, err := json.Marshal(struct {
		V       string `json:"v"`
		Records any    `json:"records"`
	}{V: schema, Records: records})
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

func hashKept(notes []keptNote) (sem, pos, full string, ok bool) {
	semRecs := make([]semRecord, 0)
	posRecs := make([]posRecord, 0)
	fullRecs := make([]fullRecord, 0)
	sorted := append([]keptNote(nil), notes...)
	sortKept(sorted)
	for _, n := range sorted {
		if !n.ok {
			return "", "", "", false
		}
		if n.includeSem {
			semRecs = append(semRecs, n.sem)
		}
		posRecs = append(posRecs, n.pos)
		fullRecs = append(fullRecs, n.full)
	}
	var err error
	if sem, err = hashDoc(discussionsSemanticSchema, semRecs); err != nil {
		return "", "", "", false
	}
	if pos, err = hashDoc(discussionsPositionSchema, posRecs); err != nil {
		return "", "", "", false
	}
	if full, err = hashDoc(discussionsFullSchema, fullRecs); err != nil {
		return "", "", "", false
	}
	return sem, pos, full, true
}

func sortKept(notes []keptNote) {
	for i := 1; i < len(notes); i++ {
		j := i
		for j > 0 && keptLess(notes[j], notes[j-1]) {
			notes[j], notes[j-1] = notes[j-1], notes[j]
			j--
		}
	}
}

func keptLess(a, b keptNote) bool {
	if a.discID != b.discID {
		return a.discID < b.discID
	}
	if a.isShell != b.isShell {
		return a.isShell
	}
	if a.noteNum != b.noteNum {
		return a.noteNum < b.noteNum
	}
	return a.noteText < b.noteText
}

func bundleHex(sem, pos, full string) (string, error) {
	raw, err := json.Marshal(discussionsEvidenceBundle{
		Schema:     discussionsEvidenceSchema,
		Capability: capabilityReviewContextDiscussionsV1,
		Selection:  "all",
		Semantic:   sem,
		Position:   pos,
		Full:       full,
	})
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

func discussionDigestDocuments(raw []byte) (sem, pos, full string, ok bool) {
	st := &discScanState{seenDisc: map[string]struct{}{}, seenNote: map[string]struct{}{}}
	dec := decodeDiscussionPage(bytes.NewReader(raw), nil, "", "", "all", st, func() error { return nil })
	if !dec.closed || dec.terminal != "" || dec.withhold || dec.inconsistent {
		return "", "", "", false
	}
	return hashKept(dec.kept)
}

func (rt *reviewRuntime) discRoomForGET() (limit int64, ok bool, why string) {
	if rt.ctx.Err() != nil {
		return 0, false, reviewClassify(rt.ctx.Err())
	}
	if rt.budget == nil {
		return discussionsNoteCeiling, true, ""
	}
	reqs, _, items := rt.budget.Stats()
	maxItems, _, maxReq, _ := rt.budget.LimitsSnapshot()
	if maxReq > 0 && reqs+1+reviewBracketReserveRequests > maxReq {
		return 0, false, readmeta.CodeBudgetRequests
	}
	if maxItems > 0 && items+1+reviewBracketReserveItems > maxItems {
		return 0, false, readmeta.CodeBudgetItems
	}
	rem := rt.budget.RemainingBytes()
	if rem == 0 {
		return 0, false, readmeta.CodeBudgetBytes
	}
	if rem < 0 {
		return discussionsNoteCeiling, true, ""
	}
	if rem <= reviewBracketReserveBytes {
		return 0, false, readmeta.CodeBudgetBytes
	}
	limit = rem - reviewBracketReserveBytes
	if limit > discussionsNoteCeiling {
		limit = discussionsNoteCeiling
	}
	return limit, true, ""
}

func (rt *reviewRuntime) discAtByteCeiling() bool {
	if rt.budget == nil {
		return true
	}
	_, maxBytes, _, _ := rt.budget.LimitsSnapshot()
	return maxBytes <= 0 || maxBytes >= reviewCeilBytes
}

func (rt *reviewRuntime) chargeDecodedNote(deadline time.Time) error {
	if rt.ctx.Err() != nil {
		return rt.ctx.Err()
	}
	if !deadline.IsZero() && !rt.d.now().Before(deadline) {
		return errDiscElapsed
	}
	if rt.budget != nil {
		_, _, items := rt.budget.Stats()
		maxItems, _, _, _ := rt.budget.LimitsSnapshot()
		if maxItems > 0 && items+1+reviewBracketReserveItems > maxItems {
			return errDiscStop
		}
	}
	if err := reviewChargeItem(rt.ctx); err != nil {
		return errDiscStop
	}
	return nil
}

func (rt *reviewRuntime) readDiscussions(item reviewContextItemIn, owner CanonicalProject, b reviewBracket, out *reviewContextItemOut) error {
	selection := itemDiscSelection(item)
	view := discussionsView{Returned: "none", Section: discSection(rt.now)}
	deadline := rt.now.Add(rt.elapsed)
	expiry := rt.now.Add(cursor.DefaultTTL)
	start := discCoord{P: 1}
	resumed := item.discResume != nil
	var wantDP, wantND string
	if resumed {
		p := item.discResume
		if p.ActorID != rt.actorID || p.Instance != rt.instance || p.PolicyFP != rt.policyFP {
			return errDiscResync
		}
		if p.Scope.ProjectID != strconv.FormatInt(owner.ID, 10) || p.Scope.MergeRequestIID == nil || *p.Scope.MergeRequestIID != item.MergeRequestIID {
			return errDiscResync
		}
		if !sameDiscRefs(p.ImmutableRefs, b) || p.Filters.Selection != selection {
			return errDiscResync
		}
		bound, err := time.Parse(time.RFC3339Nano, p.UpperBound)
		if err != nil {
			return errDiscResync
		}
		exp, err := time.Parse(time.RFC3339Nano, p.ExpiresAt)
		if err != nil {
			return errDiscResync
		}
		deadline, expiry = bound, exp
		c := p.DiscussionsCont
		start = discCoord{P: c.P, DI: c.DI, NI: c.NI, DID: c.DID}
		wantDP, wantND = c.DP, c.ND
		if !rt.d.now().Before(deadline) {
			rt.discDeadline = deadline
			rt.publishDisc(out, &view, selection, nil, nil, 0, false, false, false, false, readmeta.CodeBudgetElapsed, false, discCoord{}, start, deadline, expiry, b)
			return nil
		}
	}
	rt.discDeadline = deadline
	st := &discScanState{seenDisc: map[string]struct{}{}, seenNote: map[string]struct{}{}}
	var returned []discussionNoteOut
	var kept []keptNote
	inspected := 0
	coord := start
	claim := !resumed && start.P == 1 && start.DI == 0 && start.NI == 0
	exhausted := false
	withhold := false
	inconsistent := false
	limitation := ""
	cursorAt := discCoord{}
	mint := false
	published := false
	for pages := 0; pages < reviewCeilRequests; pages++ {
		if err := rt.ctx.Err(); err != nil {
			out.Cause = reviewClassify(err)
			claim = false
			break
		}
		if !rt.d.now().Before(deadline) {
			limitation = readmeta.CodeBudgetElapsed
			claim = false
			mint = false
			break
		}
		limit, room, why := rt.discRoomForGET()
		if !room {
			limitation = why
			claim = false
			if why == readmeta.CodeCancelled || why == readmeta.CodeBudgetElapsed {
				out.Cause = why
			}
			if start.less(coord) {
				cursorAt, mint = coord, true
			}
			break
		}
		var resume *discCoord
		dp, nd := "", ""
		if resumed && coord.same(start) {
			resume = &start
			dp, nd = wantDP, wantND
		}
		resp, dec, transport := rt.fetchDiscPage(owner, item.MergeRequestIID, coord.P, limit, resume, dp, nd, selection, st, deadline)
		if transport != "" {
			limitation = transport
			claim = false
			if transport == readmeta.CodeCancelled || transport == readmeta.CodeBudgetElapsed || transport == readmeta.CodeBudgetBytes || transport == readmeta.CodeBudgetItems || transport == readmeta.CodeBudgetRequests {
				out.Cause = transport
			}
			break
		}
		if resp == nil || resp.StatusCode != http.StatusOK {
			if resp != nil && resp.StatusCode == http.StatusNotFound {
				limitation = readmeta.CodeIdentityUnresolved
			} else {
				limitation = readmeta.CodeHTTPError
			}
			claim = false
			break
		}
		exh, next, shapeOK := discHeaderShape(coord.P, resp.Header, resp.NextPage, dec.closed, dec.discussions)
		inspected += dec.inspected
		if dec.withhold {
			withhold = true
			claim = false
		}
		if dec.inconsistent {
			inconsistent = true
			claim = false
		}
		if !shapeOK {
			limitation = readmeta.CodeProviderPageAmbiguous
			claim = false
			break
		}
		returned = append(returned, dec.notes...)
		kept = append(kept, dec.kept...)
		if len(dec.notes) > 0 || (dec.closed && dec.terminal == "") {
			published = true
		}
		if !rt.d.now().Before(deadline) {
			limitation = readmeta.CodeBudgetElapsed
			claim = false
			mint = false
			break
		}
		if dec.terminal != "" {
			claim = false
			if dec.limitation != "" {
				limitation = dec.limitation
			}
			if dec.terminal == "too_large" || (dec.terminal == "local" && rt.discAtByteCeiling()) {
				limitation = readmeta.CodeTooLarge
				break
			}
			if dec.cursorOK {
				nextC := dec.next
				nextC.P = coord.P
				if start.less(nextC) {
					cursorAt, mint = nextC, true
				}
			}
			break
		}
		if !dec.closed {
			claim = false
			if limitation == "" {
				limitation = readmeta.CodePartial
			}
			if dec.cursorOK {
				nextC := dec.next
				nextC.P = coord.P
				if start.less(nextC) {
					cursorAt, mint = nextC, true
				}
			}
			break
		}
		if exh {
			exhausted = true
			break
		}
		coord = discCoord{P: int(next)}
		if !start.less(coord) {
			limitation = readmeta.CodeProviderPageAmbiguous
			claim = false
			break
		}
	}
	if withhold || inconsistent || resumed {
		claim = false
	}
	rt.publishDisc(out, &view, selection, returned, kept, inspected, published, exhausted, claim, inconsistent, limitation, mint, cursorAt, start, deadline, expiry, b)
	return nil
}

func (rt *reviewRuntime) publishDisc(out *reviewContextItemOut, view *discussionsView, selection string, returned []discussionNoteOut, kept []keptNote, inspected int, published, exhausted, claim, inconsistent bool, limitation string, mint bool, cursorAt, start discCoord, deadline, expiry time.Time, b reviewBracket) {
	if !deadline.IsZero() && !rt.d.now().Before(deadline) {
		claim = false
		mint = false
		if limitation == "" {
			limitation = readmeta.CodeBudgetElapsed
		}
	}
	sec := discSection(rt.now)
	sec.PaginationExhausted = exhausted
	if published || exhausted {
		notes := append([]discussionNoteOut(nil), returned...)
		if notes == nil {
			notes = []discussionNoteOut{}
		}
		view.Notes = &notes
		n := len(notes)
		view.ReturnedCount = &n
		if selection == "all" {
			view.Returned = "all"
		} else {
			view.Returned = "semantic_feedback"
		}
	}
	if inspected > 0 {
		items := inspected
		sec.Counts.Items = &items
	} else if published && limitation == "" {
		zero := 0
		sec.Counts.Items = &zero
	}
	if inconsistent {
		sec.Consistency = readmeta.ConsistencyInconsistent
		claim = false
		mint = false
	}
	if limitation != "" {
		sec.Limitations = []readmeta.Limitation{{Code: limitation, Message: "discussions"}}
		if limitation == readmeta.CodeTooLarge || limitation == readmeta.CodeProviderPageAmbiguous || limitation == readmeta.CodeIdentityUnresolved || limitation == readmeta.CodeHTTPError {
			mint = false
		}
	}
	if claim {
		sem, pos, full, ok := hashKept(kept)
		if ok {
			view.semHex, view.posHex, view.fullHex = &sem, &pos, &full
			if selection == "all" {
				view.claimAll = true
			} else {
				view.claimSemantic = true
			}
		}
	}
	if mint && start.less(cursorAt) {
		if tok, err := rt.mintDiscCursor(b, cursorAt, deadline, expiry, selection); err == nil {
			sec.NextCursor = &tok
		} else {
			sec.Limitations = append(sec.Limitations, readmeta.Limitation{Code: readmeta.CodeCursorCapacity, Message: "discussions"})
		}
	}
	view.Section = sec
	out.Discussions = view
	out.Sections["discussions"] = sec
}

func (rt *reviewRuntime) finalizeDiscBound(out *reviewContextItemOut) {
	if out == nil || rt.discDeadline.IsZero() {
		return
	}
	pastClock := !rt.d.now().Before(rt.discDeadline)
	ctxErr := rt.ctx.Err()
	if !pastClock && ctxErr == nil {
		return
	}
	sec, ok := out.Sections["discussions"]
	if !ok {
		return
	}
	sec.NextCursor = nil
	if sec.ContentComplete == readmeta.ContentCompleteTrue {
		sec.ContentComplete = readmeta.ContentCompleteFalse
	}
	code := readmeta.CodeBudgetElapsed
	if !pastClock && errors.Is(ctxErr, context.Canceled) {
		code = readmeta.CodeCancelled
	}
	sec.Limitations = []readmeta.Limitation{{Code: code, Message: "discussions"}}
	if out.Discussions != nil {
		d := *out.Discussions
		d.SemanticFeedbackDigest = nil
		d.PositionDigest = nil
		d.FullRevisionDigest = nil
		d.claimAll = false
		d.claimSemantic = false
		d.evidence = ""
		d.semHex, d.posHex, d.fullHex = nil, nil, nil
		d.Section = sec
		out.Discussions = &d
	}
	out.Sections["discussions"] = sec
}

func finishDiscussionClaim(out *reviewContextItemOut, head string) {
	if out == nil || out.Discussions == nil {
		return
	}
	d := out.Discussions
	sec := out.Sections["discussions"]
	if head != "" {
		h := head
		sec.HeadSHA = &h
	}
	if d.claimAll && d.semHex != nil && d.posHex != nil && d.fullHex != nil {
		d.SemanticFeedbackDigest = d.semHex
		d.PositionDigest = d.posHex
		d.FullRevisionDigest = d.fullHex
		if ev, err := bundleHex(*d.semHex, *d.posHex, *d.fullHex); err == nil {
			d.evidence = ev
			sec.ContentComplete = readmeta.ContentCompleteTrue
			sec.Consistency = readmeta.ConsistencyConsistent
			sec.PaginationExhausted = true
		}
	} else if d.claimSemantic && d.semHex != nil && d.posHex != nil {
		d.SemanticFeedbackDigest = d.semHex
		d.PositionDigest = d.posHex
		sec.ContentComplete = readmeta.ContentCompleteFalse
		sec.Consistency = readmeta.ConsistencyConsistent
	}
	d.Section = sec
	out.Discussions = d
	out.Sections["discussions"] = sec
}

func (rt *reviewRuntime) mintDiscCursor(b reviewBracket, coord discCoord, deadline, expiry time.Time, selection string) (string, error) {
	iid := b.IID
	until := deadline.UTC().Format(time.RFC3339Nano)
	payload := cursor.Payload{
		SchemaVersion: cursor.SchemaV1,
		Instance:      rt.instance,
		ActorID:       rt.actorID,
		PolicyFP:      rt.policyFP,
		Tool:          cursor.ToolReviewContext,
		Section:       cursor.SectionReviewDiscussions,
		Scope: cursor.Scope{
			Kind:            cursor.ScopeProject,
			ProjectID:       strconv.FormatInt(b.OwnerID, 10),
			MergeRequestIID: &iid,
		},
		Filters: cursor.Filters{
			Until:     until,
			Order:     "provider",
			Selection: selection,
			PerPage:   discussionsPerPage,
		},
		ImmutableRefs: discRefsOf(b),
		UpperBound:    until,
		ExpiresAt:     expiry.UTC().Format(time.RFC3339Nano),
		DiscussionsCont: &cursor.DiscussionsCont{
			V:   cursor.DiscussionsContSchemaDC1,
			P:   coord.P,
			DI:  coord.DI,
			NI:  coord.NI,
			DID: coord.DID,
			DP:  prefixHash(coord.DiscIDs),
			ND:  prefixHash(coord.NoteIDs),
		},
	}
	tok, err := cursor.Encode(rt.d.Config.CursorKey, payload)
	if err != nil {
		return "", err
	}
	return tok, nil
}

func discBoundContext(parent context.Context, now, deadline time.Time) (context.Context, context.CancelFunc) {
	if deadline.IsZero() {
		return context.WithCancel(parent)
	}
	remain := deadline.Sub(now)
	wall := time.Now().Add(remain)
	if remain <= 0 {
		wall = time.Now()
	}
	if parentDeadline, ok := parent.Deadline(); ok && parentDeadline.Before(wall) {
		wall = parentDeadline
	}
	return context.WithDeadline(parent, wall)
}

func (rt *reviewRuntime) fetchDiscPage(owner CanonicalProject, iid int64, page int, limit int64, resume *discCoord, dp, nd, selection string, st *discScanState, deadline time.Time) (*gitlab.Response, discDecoded, string) {
	child, cancel := discBoundContext(rt.ctx, rt.d.now(), deadline)
	defer cancel()
	path := fmt.Sprintf("projects/%s/merge_requests/%d/discussions", gitlab.PathEscape(projectAPIID(owner)), iid)
	var bodyCap *approvalBodyCapture
	req, err := rt.d.Client.NewRequest(http.MethodGet, path, &discussionsQuery{Page: page, PerPage: discussionsPerPage}, []gitlab.RequestOptionFunc{
		gitlab.WithRequestRetry(discussionsReadCheckRetry(&bodyCap, limit)),
		gitlab.WithContext(child),
	})
	if err != nil {
		return nil, discDecoded{}, reviewClassify(err)
	}
	pr, pw := io.Pipe()
	var decoded discDecoded
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer pr.Close()
		decoded = decodeDiscussionPage(pr, resume, dp, nd, selection, st, func() error { return rt.chargeDecodedNote(deadline) })
	}()
	resp, doErr := rt.d.Client.Do(req, pw)
	_ = pw.Close()
	wg.Wait()
	if errors.Is(doErr, igl.ErrBudgetElapsed) || errors.Is(doErr, context.DeadlineExceeded) || errors.Is(child.Err(), context.DeadlineExceeded) {
		return resp, decoded, readmeta.CodeBudgetElapsed
	}
	if bodyCap != nil {
		if readErr := bodyCap.capturedReadErr(); errors.Is(readErr, errDiscLocal) || errors.Is(readErr, errDiscNoteCap) {
			if decoded.terminal == "" || decoded.terminal == "cut" {
				decoded.terminal = ""
				decoded.closed = false
				decoded = cutDecode(decoded, readErr)
			}
		}
	}
	if readErr := bodyCap.capturedReadErr(); readErr != nil && !errors.Is(readErr, errDiscLocal) && !errors.Is(readErr, errDiscNoteCap) {
		if errors.Is(readErr, igl.ErrExactReadRedirect) {
			return resp, discDecoded{}, readmeta.CodeHTTPError
		}
		return resp, discDecoded{}, reviewClassify(readErr)
	}
	if doErr != nil && decoded.terminal == "" && !decoded.closed && len(decoded.notes) == 0 && len(decoded.kept) == 0 {
		if errors.Is(doErr, igl.ErrExactReadRedirect) {
			return resp, discDecoded{}, readmeta.CodeHTTPError
		}
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return resp, discDecoded{}, ""
		}
		if resp != nil && resp.StatusCode != 0 && resp.StatusCode != http.StatusOK {
			return resp, discDecoded{}, ""
		}
		if errors.Is(doErr, context.Canceled) || errors.Is(doErr, context.DeadlineExceeded) {
			return resp, decoded, reviewClassify(doErr)
		}
		if decoded.terminal == "" && !errors.Is(doErr, errDiscLocal) && !errors.Is(doErr, io.ErrClosedPipe) {
			if len(decoded.kept) == 0 && !decoded.closed {
				return resp, decoded, reviewClassify(doErr)
			}
		}
	}
	return resp, decoded, ""
}

func discHeaderShape(page int, hdr http.Header, sdkNext int64, closed bool, discCount int) (exhausted bool, next int64, ok bool) {
	if hdr == nil {
		return false, 0, false
	}
	vals, present := queueNextPageValues(hdr)
	if !present || len(vals) != 1 {
		return false, 0, false
	}
	raw := vals[0]
	if strings.TrimSpace(raw) != raw {
		return false, 0, false
	}
	if !closed {
		if raw == "" || raw == "0" {
			return false, 0, sdkNext == 0
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n != int64(page)+1 || sdkNext != n || strconv.FormatInt(n, 10) != raw {
			return false, 0, false
		}
		return false, n, true
	}
	obs := observeQueuePage(page, hdr, sdkNext, discCount)
	if obs.ambiguous {
		return false, 0, false
	}
	return obs.exhausted, obs.next, true
}

func decodeDiscussionPage(r io.Reader, resume *discCoord, wantDP, wantND, selection string, st *discScanState, charge func() error) discDecoded {
	if st == nil {
		st = &discScanState{seenDisc: map[string]struct{}{}, seenNote: map[string]struct{}{}}
	}
	if st.seenDisc == nil {
		st.seenDisc = map[string]struct{}{}
	}
	if st.seenNote == nil {
		st.seenNote = map[string]struct{}{}
	}
	dec := json.NewDecoder(r)
	dec.UseNumber()
	out := discDecoded{cursorOK: true}
	tok, err := dec.Token()
	if err != nil {
		return cutDecode(out, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		out.terminal = "malformed"
		out.limitation = readmeta.CodePartial
		out.cursorOK = false
		return out
	}
	discIndex := 0
	var completed []string
	reached := resume == nil
	for dec.More() {
		next, err := walkDiscussion(dec, &out, st, resume, wantDP, wantND, selection, charge, discIndex, &completed, &reached)
		if err != nil {
			if errors.Is(err, errDiscUnwind) {
				return out
			}
			return cutDecode(out, err)
		}
		if next {
			discIndex++
			out.discussions++
		}
		if out.terminal != "" {
			return out
		}
	}
	tok, err = dec.Token()
	if err != nil {
		return cutDecode(out, err)
	}
	if d, ok := tok.(json.Delim); !ok || d != ']' {
		out.terminal = "malformed"
		out.limitation = readmeta.CodePartial
		out.cursorOK = false
		out.closed = false
		return out
	}
	if _, err := dec.Token(); err != io.EOF {
		out.terminal = "malformed"
		out.limitation = readmeta.CodePartial
		out.cursorOK = false
		return out
	}
	out.closed = true
	if resume != nil && !reached {
		out.terminal = "prefix"
		out.limitation = readmeta.CodePartial
		out.notes = nil
		out.kept = nil
		out.cursorOK = false
		out.inconsistent = true
	}
	return out
}

func cutDecode(out discDecoded, err error) discDecoded {
	if out.terminal != "" {
		return out
	}
	switch {
	case errors.Is(err, errDiscNoteCap):
		out.terminal = "too_large"
		out.limitation = readmeta.CodeTooLarge
		out.cursorOK = false
	case errors.Is(err, errDiscLocal):
		out.terminal = "local"
		out.limitation = readmeta.CodePartial
	case errors.Is(err, errDiscDupKey):
		out.terminal = "dup"
		out.limitation = readmeta.CodePartial
		out.withhold = true
		out.cursorOK = false
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		out.terminal = "cut"
		out.limitation = readmeta.CodePartial
	default:
		out.terminal = "malformed"
		out.limitation = readmeta.CodePartial
		out.cursorOK = false
	}
	out.closed = false
	return out
}

func walkDiscussion(dec *json.Decoder, out *discDecoded, st *discScanState, resume *discCoord, wantDP, wantND, selection string, charge func() error, discIndex int, completed *[]string, reached *bool) (bool, error) {
	tok, err := dec.Token()
	if err != nil {
		return false, err
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '{' {
		out.terminal = "malformed"
		out.limitation = readmeta.CodePartial
		out.cursorOK = false
		return false, errDiscUnwind
	}
	if resume != nil && !*reached && discIndex == resume.DI {
		if prefixHash(*completed) != wantDP {
			return prefixFail(out)
		}
		if resume.NI == 0 {
			if wantND != "" || resume.DID != "" {
				return prefixFail(out)
			}
			*reached = true
		}
	}
	seen := map[string]struct{}{}
	var discID string
	indiv := absentP()
	notesSeen := false
	noteIndex := 0
	var noteIDs []string
	keptFrom := len(out.kept)
	notesFrom := len(out.notes)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return false, err
		}
		key, ok := keyTok.(string)
		if !ok {
			out.terminal = "malformed"
			out.cursorOK = false
			return false, errDiscUnwind
		}
		if _, dup := seen[key]; dup {
			out.withhold = true
			out.terminal = "dup"
			out.cursorOK = false
			out.limitation = readmeta.CodePartial
			return false, errDiscUnwind
		}
		seen[key] = struct{}{}
		if key == "notes" {
			notesSeen = true
			if err := walkNotes(dec, out, st, resume, wantND, selection, charge, discIndex, discID, indiv, &noteIndex, &noteIDs, completed, reached); err != nil {
				return false, err
			}
			if out.terminal != "" {
				return false, errDiscUnwind
			}
			continue
		}
		raw, err := captureValue(dec, &capCounter{})
		if err != nil {
			return false, err
		}
		switch key {
		case "id":
			id, good := discJSONString(raw)
			if !good || id == "" {
				out.withhold = true
			} else {
				discID = id
			}
		case "individual_note":
			p, good := boolOrNull(raw)
			if !good {
				out.withhold = true
				indiv = absentP()
			} else {
				indiv = p
			}
		}
	}
	end, err := dec.Token()
	if err != nil {
		return false, err
	}
	if d, ok := end.(json.Delim); !ok || d != '}' {
		out.terminal = "malformed"
		out.cursorOK = false
		return false, errDiscUnwind
	}
	if discID == "" {
		out.withhold = true
		out.terminal = "malformed"
		out.cursorOK = false
		out.limitation = readmeta.CodePartial
		return false, errDiscUnwind
	}
	if _, dup := st.seenDisc[discID]; dup {
		out.inconsistent = true
		out.terminal = "conflict"
		out.cursorOK = false
		out.limitation = readmeta.CodeInconsistent
		return false, errDiscUnwind
	}
	st.seenDisc[discID] = struct{}{}
	patchDiscussion(out, keptFrom, notesFrom, discID, indiv)
	if !notesSeen {
		out.withhold = true
	} else if noteIndex == 0 && !out.withhold {
		if resume == nil || *reached {
			shell := shellKept(discID, indiv)
			out.kept = append(out.kept, shell)
			out.notes = append(out.notes, shell.out)
		}
	}
	*completed = append(*completed, discID)
	return true, nil
}

func prefixFail(out *discDecoded) (bool, error) {
	out.terminal = "prefix"
	out.limitation = readmeta.CodePartial
	out.inconsistent = true
	out.cursorOK = false
	out.notes = nil
	out.kept = nil
	return false, errDiscUnwind
}

func walkNotes(dec *json.Decoder, out *discDecoded, st *discScanState, resume *discCoord, wantND, selection string, charge func() error, discIndex int, discID string, indiv presenceValue, noteIndex *int, noteIDs *[]string, completed *[]string, reached *bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		out.withhold = true
		return nil
	}
	d, ok := tok.(json.Delim)
	if !ok || d != '[' {
		out.withhold = true
		out.terminal = "malformed"
		out.cursorOK = false
		return errDiscUnwind
	}
	for dec.More() {
		noteStart := dec.InputOffset()
		raw, err := captureValue(dec, &capCounter{})
		if err != nil {
			out.next = noteCursor(discIndex, *noteIndex, discID, *noteIDs, *completed)
			if noteStart >= discussionsNoteCeiling || errors.Is(err, errDiscNoteCap) {
				out.cursorOK = false
				return errDiscNoteCap
			}
			out.cursorOK = noteStart < discussionsNoteCeiling
			return err
		}
		if resume != nil && !*reached && discIndex < resume.DI {
			id, good := noteIDOf(raw)
			if !good {
				_, _ = prefixFail(out)
				return errDiscUnwind
			}
			if _, dup := st.seenNote[id]; dup {
				out.inconsistent = true
				out.terminal = "conflict"
				out.cursorOK = false
				return errDiscUnwind
			}
			st.seenNote[id] = struct{}{}
			if err := charge(); err != nil {
				delete(st.seenNote, id)
				out.next = noteCursor(discIndex, *noteIndex, discID, *noteIDs, *completed)
				return stopErr(out, err)
			}
			out.inspected++
			*noteIndex++
			continue
		}
		if resume != nil && !*reached && discIndex == resume.DI && *noteIndex < resume.NI {
			id, good := noteIDOf(raw)
			if !good || discID != resume.DID {
				_, _ = prefixFail(out)
				return errDiscUnwind
			}
			if _, dup := st.seenNote[id]; dup {
				out.inconsistent = true
				out.terminal = "conflict"
				out.cursorOK = false
				return errDiscUnwind
			}
			st.seenNote[id] = struct{}{}
			*noteIDs = append(*noteIDs, id)
			if err := charge(); err != nil {
				delete(st.seenNote, id)
				*noteIDs = (*noteIDs)[:len(*noteIDs)-1]
				out.next = noteCursor(discIndex, *noteIndex, discID, *noteIDs, *completed)
				return stopErr(out, err)
			}
			out.inspected++
			*noteIndex++
			if *noteIndex == resume.NI {
				if prefixHash(*noteIDs) != wantND {
					_, _ = prefixFail(out)
					return errDiscUnwind
				}
				*reached = true
			}
			continue
		}
		kn, class := classifyNote(discID, indiv, raw)
		if class == "conflict-id" {
			out.inconsistent = true
			out.terminal = "conflict"
			out.cursorOK = false
			out.limitation = readmeta.CodeInconsistent
			return errDiscUnwind
		}
		if class == "dup-note" {
			out.inconsistent = true
			out.terminal = "conflict"
			out.cursorOK = false
			out.limitation = readmeta.CodeInconsistent
			return errDiscUnwind
		}
		if kn.noteText != "" {
			if _, dup := st.seenNote[kn.noteText]; dup {
				out.inconsistent = true
				out.terminal = "conflict"
				out.cursorOK = false
				out.limitation = readmeta.CodeInconsistent
				return errDiscUnwind
			}
			st.seenNote[kn.noteText] = struct{}{}
		}
		if !kn.ok {
			out.withhold = true
		}
		if err := charge(); err != nil {
			if kn.noteText != "" {
				delete(st.seenNote, kn.noteText)
			}
			out.next = noteCursor(discIndex, *noteIndex, discID, *noteIDs, *completed)
			out.cursorOK = noteStart < discussionsNoteCeiling
			return stopErr(out, err)
		}
		out.inspected++
		if kn.noteText != "" {
			*noteIDs = append(*noteIDs, kn.noteText)
		}
		*noteIndex++
		out.kept = append(out.kept, kn)
		if kn.ok && (selection == "all" || !kn.system) {
			out.notes = append(out.notes, kn.out)
		}
		out.next = noteCursor(discIndex, *noteIndex, discID, *noteIDs, *completed)
	}
	end, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := end.(json.Delim); !ok || d != ']' {
		out.terminal = "malformed"
		out.cursorOK = false
		return errDiscUnwind
	}
	return nil
}

func stopErr(out *discDecoded, err error) error {
	if errors.Is(err, errDiscStop) {
		out.terminal = "stop"
		out.limitation = readmeta.CodeBudgetItems
		return errDiscUnwind
	}
	if errors.Is(err, errDiscElapsed) {
		out.terminal = "elapsed"
		out.limitation = readmeta.CodeBudgetElapsed
		out.cursorOK = false
		return errDiscUnwind
	}
	if errors.Is(err, context.Canceled) {
		out.terminal = "cancel"
		out.limitation = readmeta.CodeCancelled
		out.cursorOK = false
		return errDiscUnwind
	}
	if errors.Is(err, context.DeadlineExceeded) {
		out.terminal = "elapsed"
		out.limitation = readmeta.CodeBudgetElapsed
		out.cursorOK = false
		return errDiscUnwind
	}
	out.terminal = "malformed"
	out.cursorOK = false
	return errDiscUnwind
}

type capCounter struct{ n int }

func captureValue(dec *json.Decoder, cap *capCounter) (json.RawMessage, error) {
	if cap != nil && cap.n > discussionsNoteCeiling {
		return nil, errDiscNoteCap
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return captureObject(dec, cap)
		case '[':
			return captureArray(dec, cap)
		default:
			return nil, errDiscDupKey
		}
	case string:
		b, err := json.Marshal(t)
		if err != nil {
			return nil, err
		}
		return addCap(b, cap)
	case json.Number:
		return addCap(json.RawMessage(t.String()), cap)
	case bool:
		if t {
			return addCap([]byte("true"), cap)
		}
		return addCap([]byte("false"), cap)
	case nil:
		return addCap([]byte("null"), cap)
	default:
		return nil, fmt.Errorf("discussions token")
	}
}

func addCap(b []byte, cap *capCounter) (json.RawMessage, error) {
	if cap != nil {
		cap.n += len(b)
		if cap.n > discussionsNoteCeiling {
			return nil, errDiscNoteCap
		}
	}
	return b, nil
}

func captureObject(dec *json.Decoder, cap *capCounter) (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	seen := map[string]struct{}{}
	first := true
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("discussions key")
		}
		if _, dup := seen[key]; dup {
			return nil, errDiscDupKey
		}
		seen[key] = struct{}{}
		val, err := captureValue(dec, cap)
		if err != nil {
			return nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(key)
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(val)
		if cap != nil && buf.Len() > discussionsNoteCeiling {
			return nil, errDiscNoteCap
		}
	}
	end, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := end.(json.Delim); !ok || d != '}' {
		return nil, fmt.Errorf("discussions object")
	}
	buf.WriteByte('}')
	if buf.Len() > discussionsNoteCeiling {
		return nil, errDiscNoteCap
	}
	return buf.Bytes(), nil
}

func captureArray(dec *json.Decoder, cap *capCounter) (json.RawMessage, error) {
	var buf bytes.Buffer
	buf.WriteByte('[')
	first := true
	for dec.More() {
		val, err := captureValue(dec, cap)
		if err != nil {
			return nil, err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.Write(val)
		if cap != nil && buf.Len() > discussionsNoteCeiling {
			return nil, errDiscNoteCap
		}
	}
	end, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := end.(json.Delim); !ok || d != ']' {
		return nil, fmt.Errorf("discussions array")
	}
	buf.WriteByte(']')
	if buf.Len() > discussionsNoteCeiling {
		return nil, errDiscNoteCap
	}
	return buf.Bytes(), nil
}

func noteCursor(di, ni int, did string, noteIDs, discIDs []string) discCoord {
	c := discCoord{
		DI: di, NI: ni,
		NoteIDs: append([]string(nil), noteIDs...),
		DiscIDs: append([]string(nil), discIDs...),
	}
	if ni > 0 {
		c.DID = did
	}
	return c
}

func patchDiscussion(out *discDecoded, keptFrom, notesFrom int, discID string, indiv presenceValue) {
	for i := keptFrom; i < len(out.kept); i++ {
		if out.kept[i].discID == "" {
			out.kept[i].discID = discID
			out.kept[i].sem.DiscussionID = discID
			out.kept[i].pos.DiscussionID = discID
			out.kept[i].full.DiscussionID = discID
			out.kept[i].out.DiscussionID = discID
		}
		out.kept[i].sem.IndividualNote = indiv
		out.kept[i].full.IndividualNote = indiv
	}
	for i := notesFrom; i < len(out.notes); i++ {
		if out.notes[i].DiscussionID == "" {
			out.notes[i].DiscussionID = discID
		}
	}
}

func shellKept(discID string, indiv presenceValue) keptNote {
	noteID := absentP()
	kn := keptNote{
		discID: discID, isShell: true, includeSem: true, ok: true,
		sem: semRecord{
			DiscussionID: discID, IndividualNote: indiv, NoteID: noteID,
			AuthorID: absentP(), Body: absentP(), Resolvable: absentP(), Resolved: absentP(), ResolvedBy: absentP(),
		},
		pos: posRecord{DiscussionID: discID, NoteID: noteID, Position: absentP()},
	}
	kn.full = fullFrom(discID, indiv, noteID, absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP(), absentP())
	kn.out = discussionNoteOut{DiscussionID: discID}
	return kn
}

func fullFrom(discID string, indiv, noteID, system, typ, author, body, resolvable, resolved, resolvedBy, resolvedAt, created, updated, expires, position, commitID, internal, noteableID, noteableType, noteableIID presenceValue) fullRecord {
	return fullRecord{
		DiscussionID: discID, IndividualNote: indiv, NoteID: noteID, System: system, Type: typ,
		AuthorID: author, Body: body, Resolvable: resolvable, Resolved: resolved, ResolvedBy: resolvedBy,
		ResolvedAt: resolvedAt, CreatedAt: created, UpdatedAt: updated, ExpiresAt: expires, Position: position,
		CommitID: commitID, Internal: internal, NoteableID: noteableID, NoteableType: noteableType, NoteableIID: noteableIID,
	}
}

func classifyNote(discID string, indiv presenceValue, raw json.RawMessage) (keptNote, string) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte{'{'}) {
		return keptNote{}, "bad"
	}
	kn := keptNote{discID: discID, includeSem: true, ok: true}
	idText, idOK := noteIDOf(raw)
	if !idOK {
		kn.ok = false
		kn.sem.NoteID = absentP()
	} else {
		kn.noteText = idText
		n, nerr := strconv.ParseInt(idText, 10, 64)
		if nerr != nil {
			kn.ok = false
		} else {
			kn.noteNum = n
		}
		kn.sem.NoteID = valueP(idText)
		kn.out.NoteID = &idText
	}
	sys, sysOK := presenceBool(obj, "system")
	switch {
	case sysOK && sys.State == "true":
		kn.full.System = sys
		kn.system = true
		kn.includeSem = false
		t := true
		kn.out.System = &t
	case sysOK && sys.State == "false":
		kn.full.System = sys
		f := false
		kn.out.System = &f
	default:
		kn.ok = false
		kn.includeSem = false
		kn.system = false
		if sysOK {
			kn.full.System = sys
		} else {
			kn.full.System = absentP()
		}
	}
	author, authorN, authorOK := authorPresence(obj)
	if !authorOK {
		kn.ok = false
	}
	kn.sem.AuthorID = author
	if author.State == "value" {
		kn.out.AuthorID = &authorN
	}
	body, bodyOK := presenceString(obj, "body")
	if !bodyOK {
		kn.ok = false
	}
	kn.sem.Body = body
	if body.State == "value" {
		v := body.Value
		kn.out.Body = &v
	}
	resv, resvOK := presenceBool(obj, "resolvable")
	if !resvOK {
		kn.ok = false
	}
	resol, resolOK := presenceBool(obj, "resolved")
	if !resolOK {
		kn.ok = false
	}
	by, byOK := resolvedByPresence(obj)
	if !byOK {
		kn.ok = false
	}
	pos, posOK := positionPresence(obj)
	if !posOK {
		kn.ok = false
	}
	typ, typOK := presenceString(obj, "type")
	if !typOK {
		kn.ok = false
	}
	created, cOK := presenceString(obj, "created_at")
	updated, uOK := presenceString(obj, "updated_at")
	expires, eOK := presenceString(obj, "expires_at")
	resolvedAt, rOK := presenceString(obj, "resolved_at")
	commitID, commitOK := presenceString(obj, "commit_id")
	internal, intOK := presenceBool(obj, "internal")
	noteableID, id1OK := presenceInt(obj, "noteable_id")
	noteableIID, id2OK := presenceInt(obj, "noteable_iid")
	noteableType, ntOK := presenceString(obj, "noteable_type")
	if !cOK || !uOK || !eOK || !rOK || !commitOK || !intOK || !id1OK || !id2OK || !ntOK {
		kn.ok = false
	}
	if author.State != "value" || body.State != "value" || !idOK {
		kn.ok = false
	}
	kn.sem.DiscussionID = discID
	kn.sem.IndividualNote = indiv
	kn.sem.Resolvable = resv
	kn.sem.Resolved = resol
	kn.sem.ResolvedBy = by
	kn.pos = posRecord{DiscussionID: discID, NoteID: kn.sem.NoteID, Position: pos}
	kn.full = fullFrom(discID, indiv, kn.sem.NoteID, kn.full.System, typ, author, body, resv, resol, by, resolvedAt, created, updated, expires, pos, commitID, internal, noteableID, noteableType, noteableIID)
	kn.out.DiscussionID = discID
	return kn, ""
}

func noteIDOf(raw json.RawMessage) (string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", false
	}
	p, _, ok := presencePosInt(obj, "id")
	if !ok || p.State != "value" {
		return "", false
	}
	return p.Value, true
}

func discJSONString(raw json.RawMessage) (string, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || s == "" {
		return "", false
	}
	return s, true
}

func boolOrNull(raw json.RawMessage) (presenceValue, bool) {
	s := bytes.TrimSpace(raw)
	switch string(s) {
	case "null":
		return nullP(), true
	case "true":
		return boolP(true), true
	case "false":
		return boolP(false), true
	default:
		return absentP(), false
	}
}

func presenceBool(obj map[string]json.RawMessage, key string) (presenceValue, bool) {
	raw, ok := obj[key]
	if !ok {
		return absentP(), true
	}
	return boolOrNull(raw)
}

func presenceString(obj map[string]json.RawMessage, key string) (presenceValue, bool) {
	raw, ok := obj[key]
	if !ok {
		return absentP(), true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nullP(), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return absentP(), false
	}
	return valueP(s), true
}

func presenceInt(obj map[string]json.RawMessage, key string) (presenceValue, bool) {
	raw, ok := obj[key]
	if !ok {
		return absentP(), true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nullP(), true
	}
	text := strings.TrimSpace(string(raw))
	if !canonicalInt(text) {
		return absentP(), false
	}
	return valueP(text), true
}

func presencePosInt(obj map[string]json.RawMessage, key string) (presenceValue, int64, bool) {
	raw, ok := obj[key]
	if !ok {
		return absentP(), 0, true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nullP(), 0, true
	}
	text := strings.TrimSpace(string(raw))
	if !canonicalPosInt(text) {
		return absentP(), 0, false
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return absentP(), 0, false
	}
	return valueP(text), n, true
}

func canonicalInt(s string) bool {
	if s == "0" {
		return true
	}
	if strings.HasPrefix(s, "-") {
		return canonicalPosInt(s[1:])
	}
	return canonicalPosInt(s)
}

func canonicalPosInt(s string) bool {
	if s == "" || s[0] < '1' || s[0] > '9' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func authorPresence(obj map[string]json.RawMessage) (presenceValue, int64, bool) {
	raw, ok := obj["author"]
	if !ok {
		return absentP(), 0, true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nullP(), 0, true
	}
	var author map[string]json.RawMessage
	if err := json.Unmarshal(raw, &author); err != nil {
		return absentP(), 0, false
	}
	p, n, good := presencePosInt(author, "id")
	if !good || p.State != "value" {
		return p, n, false
	}
	return p, n, true
}

func resolvedByPresence(obj map[string]json.RawMessage) (presenceValue, bool) {
	raw, ok := obj["resolved_by"]
	if !ok {
		return absentP(), true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nullP(), true
	}
	var by map[string]json.RawMessage
	if err := json.Unmarshal(raw, &by); err != nil {
		return absentP(), false
	}
	p, _, good := presencePosInt(by, "id")
	if !good || p.State != "value" {
		return absentP(), false
	}
	return p, true
}

func canonJSON(raw []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(bytes.TrimSpace(raw)))
	dec.UseNumber()
	out, err := writeCanon(dec)
	if err != nil {
		return "", false
	}
	if _, err := dec.Token(); err != io.EOF {
		return "", false
	}
	return out, true
}

func writeCanon(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			return writeCanonObject(dec)
		case '[':
			return writeCanonArray(dec)
		default:
			return "", fmt.Errorf("discussions canon")
		}
	case string:
		b, err := json.Marshal(t)
		if err != nil {
			return "", err
		}
		return string(b), nil
	case json.Number:
		return t.String(), nil
	case bool:
		if t {
			return "true", nil
		}
		return "false", nil
	case nil:
		return "null", nil
	default:
		return "", fmt.Errorf("discussions canon")
	}
}

func writeCanonObject(dec *json.Decoder) (string, error) {
	type pair struct{ key, val string }
	var pairs []pair
	seen := map[string]struct{}{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", err
		}
		key, ok := keyTok.(string)
		if !ok {
			return "", fmt.Errorf("discussions canon key")
		}
		if _, dup := seen[key]; dup {
			return "", errDiscDupKey
		}
		seen[key] = struct{}{}
		val, err := writeCanon(dec)
		if err != nil {
			return "", err
		}
		pairs = append(pairs, pair{key: key, val: val})
	}
	end, err := dec.Token()
	if err != nil {
		return "", err
	}
	if d, ok := end.(json.Delim); !ok || d != '}' {
		return "", fmt.Errorf("discussions canon object")
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	var buf strings.Builder
	buf.WriteByte('{')
	for i, p := range pairs {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(p.key)
		if err != nil {
			return "", err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		buf.WriteString(p.val)
	}
	buf.WriteByte('}')
	return buf.String(), nil
}

func writeCanonArray(dec *json.Decoder) (string, error) {
	var buf strings.Builder
	buf.WriteByte('[')
	first := true
	for dec.More() {
		val, err := writeCanon(dec)
		if err != nil {
			return "", err
		}
		if !first {
			buf.WriteByte(',')
		}
		first = false
		buf.WriteString(val)
	}
	end, err := dec.Token()
	if err != nil {
		return "", err
	}
	if d, ok := end.(json.Delim); !ok || d != ']' {
		return "", fmt.Errorf("discussions canon array")
	}
	buf.WriteByte(']')
	return buf.String(), nil
}

func positionPresence(obj map[string]json.RawMessage) (presenceValue, bool) {
	raw, ok := obj["position"]
	if !ok {
		return absentP(), true
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nullP(), true
	}
	var pos map[string]json.RawMessage
	if err := json.Unmarshal(raw, &pos); err != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte{'{'}) {
		return absentP(), false
	}
	canon := positionCanon{}
	good := true
	var field presenceValue
	var fieldOK bool
	field, fieldOK = presenceString(pos, "base_sha")
	canon.BaseSHA, good = takeP(good, field, fieldOK)
	field, fieldOK = presenceString(pos, "start_sha")
	canon.StartSHA, good = takeP(good, field, fieldOK)
	field, fieldOK = presenceString(pos, "head_sha")
	canon.HeadSHA, good = takeP(good, field, fieldOK)
	field, fieldOK = presenceString(pos, "position_type")
	canon.PositionType, good = takeP(good, field, fieldOK)
	field, fieldOK = presenceString(pos, "old_path")
	canon.OldPath, good = takeP(good, field, fieldOK)
	field, fieldOK = presenceString(pos, "new_path")
	canon.NewPath, good = takeP(good, field, fieldOK)
	field, fieldOK = presenceInt(pos, "old_line")
	canon.OldLine, good = takeP(good, field, fieldOK)
	field, fieldOK = presenceInt(pos, "new_line")
	canon.NewLine, good = takeP(good, field, fieldOK)
	if lr, ok := pos["line_range"]; !ok {
		canon.LineRange = absentP()
	} else if bytes.Equal(bytes.TrimSpace(lr), []byte("null")) {
		canon.LineRange = nullP()
	} else if bytes.HasPrefix(bytes.TrimSpace(lr), []byte{'{'}) || bytes.HasPrefix(bytes.TrimSpace(lr), []byte{'['}) {
		canonRange, canonOK := canonJSON(lr)
		if !canonOK {
			good = false
		} else {
			canon.LineRange = valueP(canonRange)
		}
	} else {
		good = false
	}
	if !good {
		return absentP(), false
	}
	rawCanon, err := json.Marshal(canon)
	if err != nil {
		return absentP(), false
	}
	return valueP(string(rawCanon)), true
}

func takeP(prev bool, p presenceValue, ok bool) (presenceValue, bool) {
	return p, prev && ok
}
