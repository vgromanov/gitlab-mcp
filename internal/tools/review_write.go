package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// Shared, stateless guard for review-profile writes that carry a note body
// (RVG-169). Nothing is registered here: RVG-171/172 call it from their tools.
// GitLab does not reject a stale head on notes (RVG-168), so the expected_sha
// preflight plus the post-write head readback is the only head guard, and the
// helper never sends merge_request_diff_head_sha.

const (
	opMarkerPrefix         = "gitlab-mcp:op="
	dedupeScanPagesDefault = 10 // pages of 100 discussions
	dedupePerPage          = 100
)

var opKeyRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)

// opMarker is the invisible HTML comment that makes a note findable on retry.
func opMarker(key string) string { return "<!-- " + opMarkerPrefix + key + " -->" }

// headChangedError refuses a write whose expected_sha is not the MR head.
type headChangedError struct{ Expected, Current string }

func (e *headChangedError) Error() string {
	return fmt.Sprintf("head_changed: expected_sha %s but the merge request head is %s; nothing was written", e.Expected, e.Current)
}

// dedupeIncompleteError refuses a write when the op_key search hit its page cap.
type dedupeIncompleteError struct{ Pages int }

func (e *dedupeIncompleteError) Error() string {
	return fmt.Sprintf("dedupe_incomplete: complete=false, op_key not found in the first %d pages of discussions and more exist; nothing was written", e.Pages)
}

// checkExpectedHead requires expected_sha, reads the MR and refuses with
// head_changed (carrying the current SHA) before anything is written.
func checkExpectedHead(ctx context.Context, d Deps, pid string, iid int64, expected string) (string, error) {
	if strings.TrimSpace(expected) == "" {
		return "", errInvalid("expected_sha is required")
	}
	mr, _, err := d.Client.MergeRequests.GetMergeRequest(pid, iid, nil, gitlab.WithContext(ctx))
	if err != nil {
		return "", fmt.Errorf("read merge request: %w", err)
	}
	if mr.SHA != expected {
		return "", &headChangedError{Expected: expected, Current: mr.SHA}
	}
	return mr.SHA, nil
}

// bodyLines splits a body the way it is compared: CR removed, trimmed lines.
func bodyLines(s string) []string {
	lines := strings.Split(strings.ReplaceAll(s, "\r", ""), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return lines
}

// lastLine is the last non-blank line of a note body.
func lastLine(body string) string {
	lines := bodyLines(body)
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i] != "" {
			return lines[i]
		}
	}
	return ""
}

// missingLines counts non-blank lines of posted that the stored body lacks.
func missingLines(posted, stored string) int {
	have := map[string]int{}
	for _, l := range bodyLines(stored) {
		have[l]++
	}
	missing := 0
	for _, l := range bodyLines(posted) {
		if l == "" {
			continue
		}
		if have[l] > 0 {
			have[l]--
		} else {
			missing++
		}
	}
	return missing
}

// findOpNote pages the MR's discussions for a note written by userID (not a
// system note) whose last non-blank line is the marker. Hitting the page cap
// without an answer is an error, never "not found".
func findOpNote(ctx context.Context, d Deps, pid string, iid, userID int64, marker string, maxPages int) (noteID int64, discussionID string, found bool, err error) {
	for page := int64(1); page <= int64(maxPages); page++ {
		discs, resp, lerr := d.Client.Discussions.ListMergeRequestDiscussions(pid, iid, &gitlab.ListMergeRequestDiscussionsOptions{
			ListOptions: gitlab.ListOptions{Page: page, PerPage: dedupePerPage},
		}, gitlab.WithContext(ctx))
		if lerr != nil {
			return 0, "", false, fmt.Errorf("search discussions for op_key (page %d): %w", page, lerr)
		}
		for _, disc := range discs {
			if disc == nil {
				continue
			}
			for _, n := range disc.Notes {
				if n != nil && !n.System && n.Author.ID == userID && lastLine(n.Body) == marker {
					return n.ID, disc.ID, true, nil
				}
			}
		}
		if resp.NextPage == 0 {
			return 0, "", false, nil
		}
	}
	return 0, "", false, &dedupeIncompleteError{Pages: maxPages}
}

// guardedWriteReq describes one guarded note write. Write performs the real
// SDK call with the final body and returns the new note (and discussion) ids.
type guardedWriteReq struct {
	ProjectID    string
	IID          int64
	ExpectedSHA  string
	OpKey        string // optional; makes a retry safe
	Body         string
	MaxScanPages int // op_key search cap in pages of 100 discussions; 0 = default
	Write        func(ctx context.Context, body string) (noteID int64, discussionID string, err error)
}

// guardedResult is what a guarded write reports. Error is the error-level
// signal for anomalies after the write (the ids are still valid).
type guardedResult struct {
	Written               bool   `json:"written"`
	Deduplicated          bool   `json:"deduplicated"`
	NoteID                int64  `json:"note_id"`
	DiscussionID          string `json:"discussion_id,omitempty"`
	HeadSHA               string `json:"head_sha"`
	HeadChangedAfterWrite bool   `json:"head_changed_after_write"`
	BodyModified          bool   `json:"body_modified"`
	LinesChanged          int    `json:"lines_changed"`
	Error                 string `json:"error,omitempty"`
}

// guardedNoteWrite runs: validate -> head preflight -> sanitise body (+ op_key
// marker) -> dedupe -> write -> readback. Refusals (head_changed,
// dedupe_incomplete, invalid input) come back as errors with nothing written.
func guardedNoteWrite(ctx context.Context, d Deps, req guardedWriteReq) (*guardedResult, error) {
	if req.OpKey != "" && !opKeyRe.MatchString(req.OpKey) {
		return nil, errInvalid("op_key must match [A-Za-z0-9._:-]{1,64}")
	}
	if strings.TrimSpace(req.Body) == "" {
		return nil, errInvalid("body is required")
	}
	head, err := checkExpectedHead(ctx, d, req.ProjectID, req.IID, req.ExpectedSHA)
	if err != nil {
		return nil, err
	}
	body, changed := sanitizeNoteBody(req.Body)
	res := &guardedResult{HeadSHA: head, BodyModified: changed > 0, LinesChanged: changed}
	if req.OpKey != "" {
		marker := opMarker(req.OpKey)
		body = strings.TrimRight(body, "\r\n") + "\n\n" + marker
		me, _, uerr := d.Client.Users.CurrentUser(gitlab.WithContext(ctx))
		if uerr != nil {
			return nil, fmt.Errorf("resolve current user: %w", uerr)
		}
		maxPages := req.MaxScanPages
		if maxPages < 1 {
			maxPages = dedupeScanPagesDefault
		}
		noteID, discID, found, ferr := findOpNote(ctx, d, req.ProjectID, req.IID, me.ID, marker, maxPages)
		if ferr != nil {
			return nil, ferr
		}
		if found {
			res.Deduplicated, res.NoteID, res.DiscussionID = true, noteID, discID
			return res, nil
		}
	}
	noteID, discID, werr := req.Write(ctx, body)
	if werr != nil {
		var nw interface{ NothingWritten() bool } // a refusal before the POST wrote nothing
		if req.OpKey != "" && !errors.As(werr, &nw) {
			werr = fmt.Errorf("%w (the write may have been applied; retrying with the same op_key will not duplicate it)", werr)
		}
		return nil, werr
	}
	res.Written, res.NoteID, res.DiscussionID = true, noteID, discID
	res.readBack(ctx, d, req, head, body)
	return res, nil
}

// readBack re-reads the stored note and the MR head. A failed read is reported
// in Error, never taken as success. Posted lines missing from the stored note
// mean GitLab consumed them (a command ran): the error-level signal.
func (r *guardedResult) readBack(ctx context.Context, d Deps, req guardedWriteReq, head, posted string) {
	var problems []string
	if r.NoteID == 0 {
		problems = append(problems, "readback_failed: the write returned no note")
	} else if n, _, err := d.Client.Notes.GetMergeRequestNote(req.ProjectID, req.IID, r.NoteID, gitlab.WithContext(ctx)); err != nil {
		problems = append(problems, fmt.Sprintf("readback_failed: read note: %v", err))
	} else if m := missingLines(posted, n.Body); m > 0 {
		problems = append(problems, fmt.Sprintf("stored_body_differs: %d line(s) of the posted body are missing from the stored note", m))
	}
	r.HeadSHA = ""
	if mr, _, err := d.Client.MergeRequests.GetMergeRequest(req.ProjectID, req.IID, nil, gitlab.WithContext(ctx)); err != nil {
		problems = append(problems, fmt.Sprintf("readback_failed: read merge request head: %v", err))
	} else {
		r.HeadSHA, r.HeadChangedAfterWrite = mr.SHA, mr.SHA != head
	}
	r.Error = strings.Join(problems, "; ")
}

// sanitizeNoteBody makes a note body inert before it is sent. It is silent by
// design: callers report only how many lines changed (never what or why), and
// the function is idempotent. A literal op marker prefix in the caller's text is
// defused too, so quoted text cannot forge a dedupe marker.
func sanitizeNoteBody(body string) (string, int) {
	rs := []rune(body)
	out := make([]rune, 0, len(rs)+8)
	changed, atStart := 0, true
	for i, r := range rs {
		switch {
		case isLineBreak(r):
			atStart = true
		case atStart && (r == '>' || isPad(r)):
		case atStart && r == '/' && wordFollows(rs[i+1:]):
			out, changed, atStart = append(out, '\\'), changed+1, false
		default:
			atStart = false
		}
		out = append(out, r)
	}
	s := string(out)
	if strings.Contains(s, opMarkerPrefix) {
		for _, l := range strings.FieldsFunc(s, isLineBreak) {
			if strings.Contains(l, opMarkerPrefix) {
				changed++
			}
		}
		s = strings.ReplaceAll(s, opMarkerPrefix, "gitlab-mcp:op&#61;")
	}
	return s, changed
}

func isLineBreak(r rune) bool {
	return r == '\n' || r == '\r' || r == '\v' || r == '\f' || r == '\u0085' || r == '\u2028' || r == '\u2029'
}

// isPad is any separator, control, format or combining rune.
func isPad(r rune) bool { return unicode.In(r, unicode.Z, unicode.C, unicode.M) }

// wordFollows reports whether a word rune comes next, past control/format/mark runes.
func wordFollows(rs []rune) bool {
	for _, r := range rs {
		if unicode.In(r, unicode.C, unicode.M) {
			continue
		}
		return unicode.IsLetter(r) || unicode.IsNumber(r) || r == '_'
	}
	return false
}
