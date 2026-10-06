package tools

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	pipelineStatusPerPage    = 100
	pipelineStatusReqDefault = 60
	pipelineStatusReqLimit   = 200
	pipelineStatusMaxDepth   = 2  // bridges are followed from depth 0 down to depth 2
	snapshotPipelineRequests = 30 // upstream requests per MR for the snapshot pipeline section
	pipelineReasonsShown     = 5
)

type getPipelineStatusIn struct {
	ProjectID   any    `json:"project_id,omitempty" jsonschema:"Project id (number or string) or path; defaults to GITLAB_PROJECT_ID"`
	SHA         string `json:"sha,omitempty" jsonschema:"Full 40-character commit SHA whose pipelines to read (give sha or mr_iid, not both)"`
	MRIID       int64  `json:"mr_iid,omitempty" jsonschema:"Merge request iid: its head pipeline is read (give sha or mr_iid, not both)"`
	Ref         string `json:"ref,omitempty" jsonschema:"With sha only: keep pipelines of this branch/tag (e.g. main for a post-merge watch)"`
	Jobs        string `json:"jobs,omitempty" jsonschema:"all (default): every job row; problems: only jobs that are not success plus job_counts per pipeline (smallest response, meant for polling)"`
	MaxRequests int    `json:"max_requests,omitempty" jsonschema:"Upstream requests allowed for the whole call (default 60, max 200); complete is false when the cap is hit"`
}

// statusJob is one job or bridge row; allow_failure, manual and bridge are
// present only when true (compact: an absent flag is false).
type statusJob struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Stage        string `json:"stage"`
	Status       string `json:"status"`
	AllowFailure bool   `json:"allow_failure,omitempty"`
	Manual       bool   `json:"manual,omitempty"`
	Bridge       bool   `json:"bridge,omitempty"`
}

type statusPipeline struct {
	ID         int64          `json:"id"`
	ProjectID  int64          `json:"project_id"`
	SHA        string         `json:"sha"`
	Status     string         `json:"status"`
	Source     string         `json:"source"`
	Depth      int            `json:"depth,omitempty"`
	ParentID   int64          `json:"parent_pipeline_id,omitempty"`
	Jobs       []statusJob    `json:"jobs"`
	JobCounts  map[string]int `json:"job_counts,omitempty"` // jobs=problems only: every job by status
	Incomplete string         `json:"incomplete,omitempty"`
}

// statusJobRef points at a job in pipelines[]; project_id is what get_pipeline_job_output needs.
type statusJobRef struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Stage        string `json:"stage"`
	PipelineID   int64  `json:"pipeline_id"`
	ProjectID    int64  `json:"project_id"`
	AllowFailure bool   `json:"allow_failure,omitempty"`
}

type pipelineStatusOut struct {
	ProjectID         string            `json:"project_id"`
	SHA               string            `json:"sha"`
	OverallStatus     string            `json:"overall_status"`
	Complete          bool              `json:"complete"`
	TruncatedReason   *string           `json:"truncated_reason"`
	Pipelines         []*statusPipeline `json:"pipelines"`
	FailedJobs        []statusJobRef    `json:"failed_jobs"`
	AllowedFailedJobs []statusJobRef    `json:"allowed_failed_jobs"`
	ManualJobs        []statusJobRef    `json:"manual_jobs"`
	Requests          int               `json:"requests"`
}

// pipelineSelector names the roots: the pipelines of a commit (on a ref) or an MR's head pipeline.
type pipelineSelector struct {
	sha, ref     string
	mrIID        int64
	problemsOnly bool
}

type pipeNode struct {
	p   *statusPipeline
	pid string // project reference used for this pipeline's requests
}

// pipelineWalker reads a tree sequentially (no workers, no cache) under one request budget.
type pipelineWalker struct {
	ctx                     context.Context
	d                       Deps
	budget, used            int
	exhausted, problemsOnly bool
	seen                    map[int64]bool
	pipes                   []*statusPipeline
	reasons                 []string
}

// spend takes one request from the budget; false means the cap is reached.
func (w *pipelineWalker) spend() bool {
	if w.used >= w.budget {
		w.exhausted = true
		return false
	}
	w.used++
	return true
}

func (w *pipelineWalker) incomplete(p *statusPipeline, reason string) {
	p.Incomplete = strings.TrimPrefix(p.Incomplete+"; "+reason, "; ")
	w.reasons = append(w.reasons, fmt.Sprintf("pipeline %d (project %d): %s", p.ID, p.ProjectID, reason))
}

func (w *pipelineWalker) capReason() string {
	return fmt.Sprintf("request cap max_requests=%d reached", w.budget)
}

// pages runs fetch for page 1, 2, ... until the last page; ok is false when
// the budget ran out first. fetch returns the next page (0 = done).
func (w *pipelineWalker) pages(fetch func(page int64) (int64, error)) (ok bool, err error) {
	for page := int64(1); page > 0; {
		if !w.spend() {
			return false, nil
		}
		if page, err = fetch(page); err != nil {
			return false, err
		}
	}
	return true, nil
}

func newRootNode(pid string, in *gitlab.PipelineInfo) *pipeNode {
	return &pipeNode{pid: pid, p: &statusPipeline{ID: in.ID, ProjectID: in.ProjectID, SHA: in.SHA, Status: in.Status, Source: in.Source, Jobs: []statusJob{}}}
}

// listRoots reads every pipeline of sha (optionally on ref). A child pipeline
// of a parent_pipeline source is dropped when other roots exist: the walker
// finds it through its parent's bridge, at its real depth.
func (w *pipelineWalker) listRoots(pid, sha, ref string) ([]*pipeNode, error) {
	opt := &gitlab.ListProjectPipelinesOptions{SHA: &sha, OrderBy: gitlab.Ptr("id"), Sort: gitlab.Ptr("asc")}
	if ref != "" {
		opt.Ref = &ref
	}
	var all []*pipeNode
	ok, err := w.pages(func(page int64) (int64, error) {
		opt.ListOptions = gitlab.ListOptions{Page: page, PerPage: pipelineStatusPerPage}
		infos, resp, err := w.d.Client.Pipelines.ListProjectPipelines(pid, opt, gitlab.WithContext(w.ctx))
		if err != nil {
			return 0, err
		}
		for _, in := range infos {
			if in != nil {
				all = append(all, newRootNode(pid, in))
			}
		}
		return resp.NextPage, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list pipelines of %s: %w", sha, err)
	}
	if !ok {
		w.reasons = append(w.reasons, "pipelines of the sha: "+w.capReason()+" before the last page")
	}
	roots := slices.DeleteFunc(slices.Clone(all), func(n *pipeNode) bool { return n.p.Source == "parent_pipeline" })
	if len(roots) == 0 {
		roots = all
	}
	return roots, nil
}

// follow records the downstream pipeline of a bridge and queues it for reading.
// A child that cannot or may not be read keeps the status its bridge reports.
func (w *pipelineWalker) follow(parent *pipeNode, b *gitlab.Bridge, queue *[]*pipeNode) {
	dp := b.DownstreamPipeline
	if dp == nil {
		if b.Status == "running" || b.Status == "success" {
			w.incomplete(parent.p, fmt.Sprintf("bridge %q (job %d) exposes no downstream pipeline", b.Name, b.ID))
		}
		return
	}
	if w.seen[dp.ID] {
		return
	}
	w.seen[dp.ID] = true
	child := newRootNode(strconv.FormatInt(dp.ProjectID, 10), dp)
	child.p.Depth, child.p.ParentID = parent.p.Depth+1, parent.p.ID
	w.pipes = append(w.pipes, child.p)
	if child.p.Depth > pipelineStatusMaxDepth {
		w.incomplete(child.p, fmt.Sprintf("depth limit: not followed below depth %d", pipelineStatusMaxDepth))
		return
	}
	if err := checkAllowedProject(w.d.Config, child.pid); err != nil {
		w.incomplete(child.p, "not read: "+err.Error())
		return
	}
	*queue = append(*queue, child)
}

// addJob records a job row; with jobs=problems a success is only counted.
func (w *pipelineWalker) addJob(p *statusPipeline, id int64, name, stage, status string, allowFailure, bridge bool) {
	j := statusJob{ID: id, Name: name, Stage: stage, Status: status, AllowFailure: allowFailure, Manual: status == "manual", Bridge: bridge}
	if w.problemsOnly {
		if p.JobCounts == nil {
			p.JobCounts = map[string]int{}
		}
		p.JobCounts[j.Status]++
		if j.Status == "success" {
			return
		}
	}
	p.Jobs = append(p.Jobs, j)
}

// readNode reads all jobs, then all bridges, of one pipeline. Only a failing root
// aborts the call; an unreadable child becomes an incomplete row.
func (w *pipelineWalker) readNode(n *pipeNode, queue *[]*pipeNode) error {
	p := n.p
	ok, err := w.pages(func(page int64) (int64, error) {
		jobs, resp, err := w.d.Client.Jobs.ListPipelineJobs(n.pid, p.ID, &gitlab.ListJobsOptions{
			ListOptions: gitlab.ListOptions{Page: page, PerPage: pipelineStatusPerPage},
		}, gitlab.WithContext(w.ctx))
		if err != nil {
			return 0, err
		}
		for _, j := range jobs {
			if j != nil {
				w.addJob(p, j.ID, j.Name, j.Stage, j.Status, j.AllowFailure, false)
			}
		}
		return resp.NextPage, nil
	})
	if err == nil && ok {
		ok, err = w.pages(func(page int64) (int64, error) {
			bridges, resp, err := w.d.Client.Jobs.ListPipelineBridges(n.pid, p.ID, &gitlab.ListJobsOptions{
				ListOptions: gitlab.ListOptions{Page: page, PerPage: pipelineStatusPerPage},
			}, gitlab.WithContext(w.ctx))
			if err != nil {
				return 0, err
			}
			for _, b := range bridges {
				if b != nil {
					w.addJob(p, b.ID, b.Name, b.Stage, b.Status, b.AllowFailure, true)
					w.follow(n, b, queue)
				}
			}
			return resp.NextPage, nil
		})
	}
	switch {
	case w.ctx.Err() != nil:
		return w.ctx.Err()
	case err != nil && p.Depth == 0:
		return fmt.Errorf("read pipeline %d: %w", p.ID, err)
	case err != nil:
		w.incomplete(p, "not readable: "+err.Error())
	case !ok:
		w.incomplete(p, w.capReason())
	}
	return nil
}

// foldStatus is the overall status of a tree from the pipelines' own statuses (GitLab already
// applies allow_failure and manual gates to each). A failure beats a running sibling, active
// states beat a manual gate, and success needs every pipeline success/skipped in a complete tree.
func foldStatus(statuses []string, complete bool) string {
	has := func(want ...string) bool {
		return slices.ContainsFunc(statuses, func(s string) bool { return slices.Contains(want, s) })
	}
	only := func(want ...string) bool { // true when every status is one of want
		return !slices.ContainsFunc(statuses, func(s string) bool { return !slices.Contains(want, s) })
	}
	switch {
	case len(statuses) == 0 && complete:
		return "none"
	case has("failed"):
		return "failed"
	case has("canceled", "canceling"):
		return "canceled"
	case has("running"):
		return "running"
	case has("created", "waiting_for_resource", "preparing", "pending", "scheduled"):
		return "pending"
	case has("manual"):
		return "manual"
	case !complete || len(statuses) == 0:
		return "unknown"
	case only("skipped"):
		return "skipped"
	case only("success", "skipped"):
		return "success"
	}
	return "unknown" // a status this server does not know is never success
}

// readPipelineStatus resolves the roots, walks the tree and assembles the output.
func readPipelineStatus(ctx context.Context, d Deps, pid string, sel pipelineSelector, maxRequests int) (*pipelineStatusOut, error) {
	w := &pipelineWalker{ctx: ctx, d: d, budget: maxRequests, problemsOnly: sel.problemsOnly, seen: map[int64]bool{}}
	sha := sel.sha
	var roots []*pipeNode
	if sel.mrIID > 0 {
		w.spend()
		mr, _, err := d.Client.MergeRequests.GetMergeRequest(pid, sel.mrIID, nil, gitlab.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		sha = mr.SHA
		if hp := mr.HeadPipeline; hp != nil {
			roots = []*pipeNode{newRootNode(pid, &gitlab.PipelineInfo{ID: hp.ID, ProjectID: hp.ProjectID, SHA: hp.SHA, Status: hp.Status, Source: string(hp.Source)})}
		} else if roots, err = w.listRoots(pid, mr.SHA, ""); err != nil { // head pipeline not attached (yet)
			return nil, err
		}
	} else {
		var err error
		if roots, err = w.listRoots(pid, sha, sel.ref); err != nil {
			return nil, err
		}
	}
	queue := roots
	for _, n := range roots {
		w.seen[n.p.ID] = true
		w.pipes = append(w.pipes, n.p)
	}
	for i := 0; i < len(queue); i++ {
		if w.exhausted {
			w.incomplete(queue[i].p, "not read: "+w.capReason())
			continue
		}
		if err := w.readNode(queue[i], &queue); err != nil {
			return nil, err
		}
	}
	return w.output(pid, sha), nil
}

func (w *pipelineWalker) output(pid, sha string) *pipelineStatusOut {
	out := &pipelineStatusOut{
		ProjectID: pid, SHA: sha, Complete: len(w.reasons) == 0, Pipelines: w.pipes, Requests: w.used,
		FailedJobs: []statusJobRef{}, AllowedFailedJobs: []statusJobRef{}, ManualJobs: []statusJobRef{},
	}
	statuses := make([]string, 0, len(w.pipes))
	for _, p := range w.pipes {
		statuses = append(statuses, p.Status)
		for _, j := range p.Jobs {
			ref := statusJobRef{ID: j.ID, Name: j.Name, Stage: j.Stage, PipelineID: p.ID, ProjectID: p.ProjectID, AllowFailure: j.AllowFailure}
			switch {
			case j.Status == "failed" && j.AllowFailure:
				out.AllowedFailedJobs = append(out.AllowedFailedJobs, ref)
			case j.Status == "failed":
				out.FailedJobs = append(out.FailedJobs, ref)
			case j.Manual:
				out.ManualJobs = append(out.ManualJobs, ref)
			}
		}
	}
	out.OverallStatus = foldStatus(statuses, out.Complete)
	if !out.Complete {
		reason := strings.Join(w.reasons[:min(len(w.reasons), pipelineReasonsShown)], "; ")
		if more := len(w.reasons) - pipelineReasonsShown; more > 0 {
			reason += fmt.Sprintf("; +%d more", more)
		}
		out.TruncatedReason = &reason
	}
	return out
}

func getPipelineStatus(ctx context.Context, _ *mcp.CallToolRequest, in getPipelineStatusIn, d Deps) (*mcp.CallToolResult, any, error) {
	sha := strings.ToLower(strings.TrimSpace(in.SHA))
	switch {
	case (sha == "") == (in.MRIID == 0):
		return nil, nil, errInvalid("give exactly one of sha or mr_iid")
	case in.MRIID < 0:
		return nil, nil, errInvalid("mr_iid must be >= 1")
	case sha != "" && !fullSHARe.MatchString(sha):
		return nil, nil, errInvalid(fmt.Sprintf("invalid sha %q: a full 40-character commit SHA is required", in.SHA))
	case in.Ref != "" && in.MRIID != 0:
		return nil, nil, errInvalid("ref can only be combined with sha")
	case in.Jobs != "" && in.Jobs != "all" && in.Jobs != "problems":
		return nil, nil, errInvalid(fmt.Sprintf("invalid jobs %q: must be one of all, problems", in.Jobs))
	}
	pid, err := snapshotProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err == nil {
		err = checkAllowedProject(d.Config, pid)
	}
	if err != nil {
		return nil, nil, err
	}
	maxReq := min(cmp.Or(max(in.MaxRequests, 0), pipelineStatusReqDefault), pipelineStatusReqLimit)
	out, err := readPipelineStatus(ctx, d, pid, pipelineSelector{sha: sha, ref: in.Ref, mrIID: in.MRIID, problemsOnly: in.Jobs == "problems"}, maxReq)
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(out), nil
}

// readSnapshotPipeline is the snapshot's pipeline section: the same read as
// get_pipeline_status with mr_iid and jobs=problems (the snapshot is heavy
// already), under the snapshot's smaller request cap.
func readSnapshotPipeline(ctx context.Context, d Deps, pid string, iid int64, _ snapshotOpts) (any, error) {
	out, err := readPipelineStatus(ctx, d, pid, pipelineSelector{mrIID: iid, problemsOnly: true}, snapshotPipelineRequests)
	if err != nil {
		return nil, err
	}
	return Out(out), nil
}
