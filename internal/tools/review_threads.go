package tools

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
)

// Review-profile guards for thread, reply and resolve (RVG-171), on top of the
// RVG-169 note helper. GitLab silently re-anchors a stale position and answers
// a bare 500 for some bad ones (RVG-168), so anchors are checked here first.

const anchorScanPages = 10 // pages of 100 diffs read to find the anchored file

func reviewProfile(d Deps) bool {
	return d.Config != nil && d.Config.ToolProfile == config.ProfileReview
}

// refusedError is a refusal before anything is written (a bad anchor, an unknown discussion).
type refusedError struct{ code, msg string }

func (e *refusedError) Error() string        { return e.code + ": " + e.msg + "; nothing was written" }
func (e *refusedError) NothingWritten() bool { return true }

func deref[T any](p *T) (v T) {
	if p != nil {
		v = *p
	}
	return v
}

// checkAnchor compares the position with the MR's CURRENT diff_refs and diff:
// all three SHAs must match, the path must be among the changed files, and a
// text position's line must be one the diff shows on the side given.
func checkAnchor(ctx context.Context, d Deps, pid string, iid int64, pos *gitlab.PositionOptions) error {
	mr, _, err := d.Client.MergeRequests.GetMergeRequest(pid, iid, nil, gitlab.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("read merge request: %w", err)
	}
	cur := mr.DiffRefs
	if deref(pos.BaseSHA) != cur.BaseSha || deref(pos.StartSHA) != cur.StartSha || deref(pos.HeadSHA) != cur.HeadSha {
		return &refusedError{"anchor_stale", fmt.Sprintf("position base_sha/start_sha/head_sha must all equal the merge request's current diff_refs (base_sha %s, start_sha %s, head_sha %s); re-read the diff and anchor against it", cur.BaseSha, cur.StartSha, cur.HeadSha)}
	}
	paths := slices.DeleteFunc([]string{deref(pos.NewPath), deref(pos.OldPath)}, func(s string) bool { return s == "" })
	if len(paths) == 0 {
		return &refusedError{"anchor_invalid", "position needs new_path or old_path"}
	}
	for page := int64(1); page <= anchorScanPages; page++ {
		diffs, resp, lerr := d.Client.MergeRequests.ListMergeRequestDiffs(pid, iid, &gitlab.ListMergeRequestDiffsOptions{
			ListOptions: gitlab.ListOptions{Page: page, PerPage: 100},
		}, gitlab.WithContext(ctx))
		if lerr != nil {
			return fmt.Errorf("read merge request diffs (page %d): %w", page, lerr)
		}
		for _, df := range diffs {
			if df == nil || (!slices.Contains(paths, df.NewPath) && !slices.Contains(paths, df.OldPath)) {
				continue
			}
			text := deref(pos.PositionType) == "" || deref(pos.PositionType) == "text"
			if text && (pos.NewLine != nil || pos.OldLine != nil) && !df.Collapsed && !df.TooLarge && df.Diff != "" &&
				!lineInDiff(df.Diff, deref(pos.OldLine), deref(pos.NewLine)) {
				return &refusedError{"anchor_not_in_diff", fmt.Sprintf("old_line %d / new_line %d is not a line of the diff of %s on the side given (new_line alone: an added line; old_line alone: a removed line; both: an unchanged line)", deref(pos.OldLine), deref(pos.NewLine), paths[0])}
			}
			return nil
		}
		if resp.NextPage == 0 {
			return &refusedError{"anchor_not_in_diff", fmt.Sprintf("%s is not among the changed files of the merge request", paths[0])}
		}
	}
	return &refusedError{"anchor_unverifiable", fmt.Sprintf("%s was not found in the first %d pages of the merge request's diffs and more exist", paths[0], anchorScanPages)}
}

var hunkRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// lineInDiff reports whether a unified diff holds the line: new_line alone must
// be an added line, old_line alone a removed one, both a context pair of a hunk
// or, when neither number lies in a hunk, an unchanged line outside the hunks
// (GitLab accepts those and validates them against the file itself).
func lineInDiff(diff string, oldLine, newLine int64) bool {
	var o, n int64
	var oldSeen, newSeen, ok bool
	for _, l := range strings.Split(strings.TrimRight(diff, "\n"), "\n") {
		if m := hunkRe.FindStringSubmatch(l); m != nil {
			o, _ = strconv.ParseInt(m[1], 10, 64)
			n, _ = strconv.ParseInt(m[2], 10, 64)
			continue
		}
		if l == "" {
			continue
		}
		isOld, isNew := l[0] != '+' && l[0] != '\\' && o == oldLine, l[0] != '-' && l[0] != '\\' && n == newLine
		oldSeen, newSeen = oldSeen || isOld, newSeen || isNew
		switch l[0] {
		case '+':
			ok, n = ok || (isNew && oldLine == 0), n+1
		case '-':
			ok, o = ok || (isOld && newLine == 0), o+1
		case ' ':
			ok, o, n = ok || (isOld && isNew), o+1, n+1
		}
	}
	return ok || (oldLine > 0 && newLine > 0 && !oldSeen && !newSeen)
}

// threadWriteError maps a failed thread POST: 400 line_code is a bad anchor,
// any 5xx is how GitLab refuses a position it cannot resolve.
func threadWriteError(err error) error {
	var er *gitlab.ErrorResponse
	if !errors.As(err, &er) || er.Response == nil {
		return err
	}
	switch s := er.Response.StatusCode; {
	case s == 400 && strings.Contains(er.Message, "line_code"):
		return &refusedError{"anchor_invalid", "GitLab rejected the position: the path or line is not valid in the diff"}
	case s >= 500:
		return fmt.Errorf("gitlab_error: GitLab answered %d; for a thread with a position this is how GitLab refuses a position it cannot resolve, and nothing was created in the measured cases; list the discussions before retrying", s)
	}
	return err
}

// notFound turns a 404 (the SDK reports every one as ErrNotFound) into not_found.
func notFound(err error, what string) error {
	if errors.Is(err, gitlab.ErrNotFound) {
		return &refusedError{"not_found", what + ": the discussion does not exist or is not visible"}
	}
	return fmt.Errorf("%s: %w", what, err)
}

func guardedOutcome(res *guardedResult, err error) (*mcp.CallToolResult, any, error) {
	if err != nil {
		return nil, nil, err
	}
	if res.Error != "" {
		return &mcp.CallToolResult{IsError: true}, res, nil
	}
	return nil, res, nil
}

func guardedThread(ctx context.Context, d Deps, pid string, in createMergeRequestThreadIn) (*mcp.CallToolResult, any, error) {
	return guardedOutcome(guardedNoteWrite(ctx, d, guardedWriteReq{
		ProjectID: pid, IID: in.MergeRequestIID, ExpectedSHA: deref(in.ExpectedSHA), OpKey: deref(in.OpKey), Body: in.Body,
		Write: func(ctx context.Context, body string) (int64, string, error) {
			opt := &gitlab.CreateMergeRequestDiscussionOptions{Body: gitlab.Ptr(body), Position: in.Position}
			if in.Position != nil {
				if err := checkAnchor(ctx, d, pid, in.MergeRequestIID, in.Position); err != nil {
					return 0, "", err
				}
			}
			disc, _, err := d.Client.Discussions.CreateMergeRequestDiscussion(pid, in.MergeRequestIID, opt, gitlab.WithContext(ctx))
			if err != nil {
				return 0, "", threadWriteError(err)
			}
			if len(disc.Notes) == 0 {
				return 0, disc.ID, nil
			}
			return disc.Notes[0].ID, disc.ID, nil
		},
	}))
}

func guardedReply(ctx context.Context, d Deps, pid string, in createMergeRequestDiscussionNoteIn) (*mcp.CallToolResult, any, error) {
	return guardedOutcome(guardedNoteWrite(ctx, d, guardedWriteReq{
		ProjectID: pid, IID: in.MergeRequestIID, ExpectedSHA: deref(in.ExpectedSHA), OpKey: deref(in.OpKey), Body: in.Body,
		Write: func(ctx context.Context, body string) (int64, string, error) {
			n, _, err := d.Client.Discussions.AddMergeRequestDiscussionNote(pid, in.MergeRequestIID, in.DiscussionID, &gitlab.AddMergeRequestDiscussionNoteOptions{Body: gitlab.Ptr(body)}, gitlab.WithContext(ctx))
			if err != nil {
				return 0, "", notFound(err, "reply")
			}
			return n.ID, in.DiscussionID, nil
		},
	}))
}

// resolveResult is the review-profile resolve outcome. Written is false when the
// thread was already in the requested state (nothing was sent).
type resolveResult struct {
	DiscussionID          string `json:"discussion_id"`
	Resolved              bool   `json:"resolved"`
	Written               bool   `json:"written"`
	AlreadyInState        bool   `json:"already_in_state"`
	HeadSHA               string `json:"head_sha"`
	HeadChangedAfterWrite bool   `json:"head_changed_after_write"`
	Error                 string `json:"error,omitempty"`
}

// threadState reports whether every resolvable note of the thread is resolved.
func threadState(disc *gitlab.Discussion) (resolved, resolvable bool) {
	resolved = true
	for _, n := range disc.Notes {
		if n != nil && n.Resolvable {
			resolvable, resolved = true, resolved && n.Resolved
		}
	}
	return resolved && resolvable, resolvable
}

// guardedResolve: head preflight (resolve has no sha parameter), state read,
// no write when the thread already is in the requested state, else write and
// read the state and the head back.
func guardedResolve(ctx context.Context, d Deps, pid string, in resolveMergeRequestThreadIn) (*mcp.CallToolResult, any, error) {
	head, err := checkExpectedHead(ctx, d, pid, in.MergeRequestIID, deref(in.ExpectedSHA))
	if err != nil {
		return nil, nil, err
	}
	disc, _, err := d.Client.Discussions.GetMergeRequestDiscussion(pid, in.MergeRequestIID, in.DiscussionID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, notFound(err, "read discussion")
	}
	cur, resolvable := threadState(disc)
	if !resolvable {
		return nil, nil, errInvalid("not_resolvable: the discussion has no resolvable note; nothing was written")
	}
	res := &resolveResult{DiscussionID: in.DiscussionID, Resolved: cur, HeadSHA: head}
	if cur == in.Resolved {
		res.AlreadyInState = true
		return nil, res, nil
	}
	put, _, err := d.Client.Discussions.ResolveMergeRequestDiscussion(pid, in.MergeRequestIID, in.DiscussionID, &gitlab.ResolveMergeRequestDiscussionOptions{Resolved: gitlab.Ptr(in.Resolved)}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	res.Written = true
	res.Resolved, _ = threadState(put)
	var problems []string
	if back, _, err := d.Client.Discussions.GetMergeRequestDiscussion(pid, in.MergeRequestIID, in.DiscussionID, gitlab.WithContext(ctx)); err != nil {
		problems = append(problems, "readback_failed: read discussion: "+err.Error())
	} else if res.Resolved, _ = threadState(back); res.Resolved != in.Resolved {
		problems = append(problems, fmt.Sprintf("state_differs: the discussion reads resolved=%t after the write, requested %t", res.Resolved, in.Resolved))
	}
	res.HeadSHA = ""
	if mr, _, err := d.Client.MergeRequests.GetMergeRequest(pid, in.MergeRequestIID, nil, gitlab.WithContext(ctx)); err != nil {
		problems = append(problems, "readback_failed: read merge request head: "+err.Error())
	} else {
		res.HeadSHA, res.HeadChangedAfterWrite = mr.SHA, mr.SHA != head
	}
	res.Error = strings.Join(problems, "; ")
	if res.Error != "" {
		return &mcp.CallToolResult{IsError: true}, res, nil
	}
	return nil, res, nil
}

const reviewWriteDoc = " REQUIRED expected_sha: the full head SHA you reviewed; refused with head_changed (the current SHA is in the message) when the head moved, nothing written."

func threadDescription(d Deps) string {
	if !reviewProfile(d) {
		return "Start a new MR discussion thread (optionally on a diff line)"
	}
	return "Start a new MR discussion thread, optionally on a diff line." + reviewWriteDoc + " Optional op_key ([A-Za-z0-9._:-], up to 64): a retry with the same op_key returns the existing thread (deduplicated: true) instead of writing a second one. A position must carry base_sha, start_sha and head_sha equal to the MR's current diff_refs and a path and line that are in the diff; otherwise it is refused (anchor_stale, anchor_not_in_diff, anchor_unverifiable, anchor_invalid) and nothing is written; it is never turned into a general note. Returns written, deduplicated, note_id, discussion_id, head_sha, head_changed_after_write, body_modified, lines_changed and error (a readback problem; the ids stay valid). gitlab_error: list the discussions before retrying"
}

func replyDescription(d Deps) string {
	if !reviewProfile(d) {
		return "Reply in an MR discussion thread"
	}
	return "Reply in an MR discussion thread." + reviewWriteDoc + " Optional op_key ([A-Za-z0-9._:-], up to 64): a retry with the same op_key returns the existing note (deduplicated: true) instead of writing a second one. Returns written, deduplicated, note_id, discussion_id, head_sha, head_changed_after_write, body_modified, lines_changed and error (a readback problem; the ids stay valid)"
}

func resolveDescription(d Deps) string {
	if !reviewProfile(d) {
		return "Resolve or unresolve an MR discussion"
	}
	return "Resolve or unresolve an MR discussion." + reviewWriteDoc + " Idempotent: a thread already in the requested state is not written (written: false, already_in_state: true). Returns discussion_id, resolved (read back), written, already_in_state, head_sha, head_changed_after_write and error (readback_failed / state_differs). not_resolvable: the discussion has no resolvable note; not_found: no such discussion"
}
