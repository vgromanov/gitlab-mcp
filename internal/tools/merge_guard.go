package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
)

// Approve and merge guards (RVG-172). Neither carries a note body, so the note
// helper of RVG-169 is not involved; GitLab itself refuses a stale sha on both
// (409), which makes a separate head preflight redundant.

var (
	approveCodes = map[int]string{401: "approval_not_allowed", 403: "approval_not_allowed", 404: "not_found", 409: "head_changed"}
	mergeCodes   = map[int]string{401: "merge_not_permitted", 403: "merge_not_permitted", 404: "not_found", 405: "not_mergeable", 406: "not_mergeable", 409: "head_changed", 422: "not_mergeable"}
)

// refusal turns a GitLab rejection into "<code>: GitLab answered <status> <message>".
// A 409 that is not about the sha stays "conflict". Other errors pass through.
func refusal(err error, codes map[int]string) error {
	if errors.Is(err, gitlab.ErrNotFound) { // the SDK reports every 404 as this sentinel
		return fmt.Errorf("%s: GitLab answered 404 (the merge request does not exist or is not visible)", codes[404])
	}
	var er *gitlab.ErrorResponse
	if !errors.As(err, &er) || er.Response == nil {
		return err
	}
	status := er.Response.StatusCode
	code, ok := codes[status]
	if !ok {
		return err
	}
	msg := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(er.Message), "{message: "), "}")
	if status == 409 && !strings.Contains(strings.ToLower(msg), "sha") {
		code = "conflict"
	}
	return fmt.Errorf("%s: GitLab answered %d %s", code, status, msg)
}

// mergeRefusal maps a rejected merge; for "not mergeable" it adds the reason
// GitLab reports on the MR (best effort, one read).
func mergeRefusal(ctx context.Context, d Deps, pid string, iid int64, err error) error {
	e := refusal(err, mergeCodes)
	if !strings.HasPrefix(e.Error(), "not_mergeable") {
		return e
	}
	if mr, _, gerr := d.Client.MergeRequests.GetMergeRequest(pid, iid, nil, gitlab.WithContext(ctx)); gerr == nil {
		e = fmt.Errorf("%w (state=%s, detailed_merge_status=%s)", e, mr.State, mr.DetailedMergeStatus)
	}
	return e
}

// mergeOutcome reports merged only when GitLab says the MR is merged: from the
// merge response, else from one re-read. Anything else is pending (the caller
// polls get_merge_request; the merge is never submitted twice). The MR object
// keeps its fields; result and pending_reason are added.
func mergeOutcome(ctx context.Context, d Deps, pid string, iid int64, mr *gitlab.MergeRequest) any {
	readErr := ""
	if mr.State != "merged" {
		if cur, _, err := d.Client.MergeRequests.GetMergeRequest(pid, iid, nil, gitlab.WithContext(ctx)); err != nil {
			readErr = err.Error()
		} else {
			mr = cur
		}
	}
	out, _ := Out(mr).(map[string]any)
	switch {
	case mr.State == "merged":
		out["result"] = "merged"
	case mr.MergeWhenPipelineSucceeds:
		out["result"], out["pending_reason"] = "pending", "auto_merge_scheduled"
	default:
		out["result"], out["pending_reason"] = "pending", "not_merged_yet"
	}
	if readErr != "" {
		out["readback_error"] = "readback_failed: " + readErr
	}
	return out
}

// approveResult is the review-profile approval result: what GitLab accepted and
// what a re-read shows. Error is set when a readback failed (never read as success).
type approveResult struct {
	Approved              bool                              `json:"approved"`
	SHA                   string                            `json:"sha"`
	HeadSHA               string                            `json:"head_sha"`
	HeadChangedAfterWrite bool                              `json:"head_changed_after_write"`
	ApprovalsLeft         int64                             `json:"approvals_left"`
	ApprovalState         *gitlab.MergeRequestApprovalState `json:"approval_state"`
	Error                 string                            `json:"error,omitempty"`
}

func approvalReadBack(ctx context.Context, d Deps, pid string, iid int64, sha string, a *gitlab.MergeRequestApprovals) *approveResult {
	res := &approveResult{Approved: true, SHA: sha, ApprovalsLeft: a.ApprovalsLeft}
	var problems []string
	if st, err := readApprovalState(ctx, d, pid, iid); err != nil {
		problems = append(problems, "readback_failed: approval state: "+err.Error())
	} else {
		res.ApprovalState = st
	}
	if mr, _, err := d.Client.MergeRequests.GetMergeRequest(pid, iid, nil, gitlab.WithContext(ctx)); err != nil {
		problems = append(problems, "readback_failed: merge request head: "+err.Error())
	} else {
		res.HeadSHA, res.HeadChangedAfterWrite = mr.SHA, mr.SHA != sha
	}
	res.Error = strings.Join(problems, "; ")
	return res
}

func approveDescription(d Deps) string {
	if d.Config != nil && d.Config.ToolProfile == config.ProfileReview {
		return "Approve a merge request. sha is REQUIRED: the full head SHA you reviewed; GitLab refuses the approval (head_changed) when the head moved. Returns approved, sha, head_sha, head_changed_after_write, approvals_left and approval_state, read back after the write. Errors: approval_not_allowed (e.g. own MR), head_changed, not_found"
	}
	return "Approve a merge request. Optional sha (full head SHA): GitLab refuses the approval (head_changed) when the head moved"
}
