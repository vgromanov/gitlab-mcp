package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	snapshotMaxMRs              = 10
	snapshotChangesPerPage      = 100
	snapshotChangesPagesDefault = 3
	snapshotChangesPagesLimit   = 10
)

type snapshotMRRef struct {
	ProjectID   any    `json:"project_id,omitempty" jsonschema:"Project id (number or string) or path; defaults to GITLAB_PROJECT_ID. A get_review_queue row's project_id can be passed as is"`
	IID         int64  `json:"iid" jsonschema:"Merge request iid"`
	ExpectedSHA string `json:"expected_sha,omitempty" jsonschema:"Head SHA the caller already holds; head_changed is reported when the current head differs"`
}

type getReviewSnapshotIn struct {
	MRs             []snapshotMRRef `json:"mrs" jsonschema:"1 to 10 merge requests"`
	Include         []string        `json:"include,omitempty" jsonschema:"Sections to read: changes, approvals (discussions and pipeline are not implemented yet and are rejected). Omitted = every implemented section; [] = metadata only"`
	ChangesMaxPages int             `json:"changes_max_pages,omitempty" jsonschema:"changes: pages of 100 files read per MR (default 3, max 10); complete is false when the cap is hit"`
}

// snapshotOpts are the per-call knobs passed to every section reader.
type snapshotOpts struct{ changesMaxPages int }

// snapshotSection is one entry of the include enum. A nil read means the
// section is part of the contract but not implemented yet: asking for it is
// rejected. Later tickets add a reader here and nothing else.
type snapshotSection struct {
	name string
	read func(ctx context.Context, d Deps, pid string, iid int64, opt snapshotOpts) (any, error)
}

var snapshotSections = []snapshotSection{
	{"changes", readSnapshotChanges},
	{"approvals", readSnapshotApprovals},
	{"discussions", nil},
	{"pipeline", nil},
}

// parseSnapshotInclude resolves include to the sections to read, in table order.
func parseSnapshotInclude(include []string) ([]snapshotSection, error) {
	var all, ready []string
	for _, s := range snapshotSections {
		all = append(all, s.name)
		if s.read != nil {
			ready = append(ready, s.name)
		}
	}
	want := map[string]bool{}
	for _, name := range include {
		i := indexSection(name)
		switch {
		case i < 0:
			return nil, errInvalid(fmt.Sprintf("invalid include %q: must be one of %s", name, strings.Join(all, ", ")))
		case snapshotSections[i].read == nil:
			return nil, errInvalid(fmt.Sprintf("include %q is not implemented yet (available: %s)", name, strings.Join(ready, ", ")))
		}
		want[name] = true
	}
	var out []snapshotSection
	for _, s := range snapshotSections {
		if s.read != nil && (include == nil || want[s.name]) {
			out = append(out, s)
		}
	}
	return out, nil
}

func indexSection(name string) int {
	for i, s := range snapshotSections {
		if s.name == name {
			return i
		}
	}
	return -1
}

// snapshotProjectID accepts a string id/path or a JSON number (queue rows emit numbers).
func snapshotProjectID(v any, def string) (string, error) {
	switch x := v.(type) {
	case nil:
		return ResolveProjectID("", def)
	case string:
		return ResolveProjectID(x, def)
	case float64:
		if x >= 1 && x == math.Trunc(x) {
			return strconv.FormatInt(int64(x), 10), nil
		}
	}
	return "", errors.New("project_id must be a string or a positive integer")
}

func getReviewSnapshot(ctx context.Context, _ *mcp.CallToolRequest, in getReviewSnapshotIn, d Deps) (*mcp.CallToolResult, any, error) {
	if n := len(in.MRs); n < 1 || n > snapshotMaxMRs {
		return nil, nil, errInvalid(fmt.Sprintf("mrs must hold 1 to %d merge requests, got %d", snapshotMaxMRs, n))
	}
	sections, err := parseSnapshotInclude(in.Include)
	if err != nil {
		return nil, nil, err
	}
	opt := snapshotOpts{changesMaxPages: in.ChangesMaxPages}
	if opt.changesMaxPages < 1 {
		opt.changesMaxPages = snapshotChangesPagesDefault
	}
	opt.changesMaxPages = min(opt.changesMaxPages, snapshotChangesPagesLimit)

	// Sequential on purpose: at most 10 MRs, each a handful of requests; no workers.
	mrs := make([]map[string]any, 0, len(in.MRs))
	for _, ref := range in.MRs {
		mrs = append(mrs, snapshotOne(ctx, d, ref, sections, opt))
	}
	names := make([]string, 0, len(sections))
	for _, s := range sections {
		names = append(names, s.name)
	}
	return nil, Out(map[string]any{"merge_requests": mrs, "include": names}), nil
}

// snapshotOne reads one MR. Failures stay inside its entry: an MR-level error
// leaves only {project_id, iid, error}; a section error replaces that section.
func snapshotOne(ctx context.Context, d Deps, ref snapshotMRRef, sections []snapshotSection, opt snapshotOpts) map[string]any {
	out := map[string]any{"project_id": ref.ProjectID, "iid": ref.IID}
	pid, err := snapshotProjectID(ref.ProjectID, d.Config.DefaultProjectID)
	if err == nil {
		err = checkAllowedProject(d.Config, pid)
	}
	if err == nil && ref.IID < 1 {
		err = errors.New("iid must be >= 1")
	}
	var mr *gitlab.MergeRequest
	if err == nil {
		out["project_id"] = echoProject(ref.ProjectID, pid)
		mr, _, err = d.Client.MergeRequests.GetMergeRequest(pid, ref.IID, nil, gitlab.WithContext(ctx))
	}
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	out["title"], out["web_url"], out["state"], out["draft"] = mr.Title, mr.WebURL, mr.State, mr.Draft
	out["author"], out["reviewers"] = toQueueUser(mr.Author), queueUsers(mr.Reviewers)
	out["source_branch"], out["target_branch"] = mr.SourceBranch, mr.TargetBranch
	out["sha"], out["diff_refs"], out["updated_at"] = mr.SHA, mr.DiffRefs, mr.UpdatedAt
	out["detailed_merge_status"] = mr.DetailedMergeStatus
	if ref.ExpectedSHA != "" {
		out["expected_sha"], out["head_changed"] = ref.ExpectedSHA, ref.ExpectedSHA != mr.SHA
	}
	for _, s := range sections {
		sec, serr := s.read(ctx, d, pid, ref.IID, opt)
		if serr != nil {
			sec = map[string]any{"error": serr.Error()}
		}
		out[s.name] = sec
	}
	return out
}

// echoProject echoes the caller's project_id, or the resolved default when none was given.
func echoProject(given any, resolved string) any {
	if given == nil {
		return resolved
	}
	return given
}

// readSnapshotChanges lists changed files (no patches), up to changesMaxPages
// pages. complete is false, with the next page, when GitLab still had more.
func readSnapshotChanges(ctx context.Context, d Deps, pid string, iid int64, opt snapshotOpts) (any, error) {
	files := []map[string]any{}
	for page := int64(1); ; page++ {
		diffs, resp, err := d.Client.MergeRequests.ListMergeRequestDiffs(pid, iid, &gitlab.ListMergeRequestDiffsOptions{
			ListOptions: gitlab.ListOptions{Page: page, PerPage: snapshotChangesPerPage},
		}, gitlab.WithContext(ctx))
		if err != nil {
			return nil, fmt.Errorf("list changes (page %d): %w", page, err)
		}
		for _, df := range diffs {
			if df == nil {
				continue
			}
			files = append(files, map[string]any{
				"old_path": df.OldPath, "new_path": df.NewPath,
				"new_file": df.NewFile, "renamed_file": df.RenamedFile, "deleted_file": df.DeletedFile,
				"collapsed": df.Collapsed, "too_large": df.TooLarge,
			})
		}
		if resp.NextPage == 0 {
			return map[string]any{"files": files, "complete": true, "truncated_reason": nil, "next_page": 0}, nil
		}
		if page >= int64(opt.changesMaxPages) {
			return map[string]any{
				"files": files, "complete": false, "next_page": resp.NextPage,
				"truncated_reason": fmt.Sprintf("more pages exist after changes_max_pages=%d (per_page=%d)", opt.changesMaxPages, snapshotChangesPerPage),
			}, nil
		}
	}
}

func readSnapshotApprovals(ctx context.Context, d Deps, pid string, iid int64, _ snapshotOpts) (any, error) {
	return readApprovalState(ctx, d, pid, iid)
}
