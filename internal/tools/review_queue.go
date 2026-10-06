package tools

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

const (
	queueMaxPagesDefault = 5
	queueMaxPagesLimit   = 20
	queuePerPageDefault  = 100
)

// queueRoles is the canonical role order; rows list their roles in this order.
var queueRoles = []string{"reviewer", "author"}

type getReviewQueueIn struct {
	GroupID      string   `json:"group_id" jsonschema:"Group id or full path"`
	Roles        []string `json:"roles,omitempty" jsonschema:"reviewer and/or author (default both)"`
	State        *string  `json:"state,omitempty" jsonschema:"opened (default), closed, locked, merged or all"`
	UpdatedAfter *string  `json:"updated_after,omitempty" jsonschema:"Only MRs updated at or after this time (RFC3339 or YYYY-MM-DD); sent to GitLab at whole-second granularity"`
	PerPage      int      `json:"per_page,omitempty" jsonschema:"MRs per request (default 100, max 100)"`
	MaxPages     int      `json:"max_pages,omitempty" jsonschema:"Pages read per role (default 5, max 20); complete is false when the cap is hit"`
}

type queueUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// queueRow is one deduplicated MR. SHA is the list's head, a hint only.
type queueRow struct {
	ProjectID int64       `json:"project_id"`
	IID       int64       `json:"iid"`
	Title     string      `json:"title"`
	WebURL    string      `json:"web_url"`
	Author    *queueUser  `json:"author"`
	Reviewers []queueUser `json:"reviewers"`
	SHA       string      `json:"sha"`
	UpdatedAt *time.Time  `json:"updated_at"`
	Roles     []string    `json:"roles"`
}

func toQueueUser(u *gitlab.BasicUser) *queueUser {
	if u == nil {
		return nil
	}
	return &queueUser{ID: u.ID, Username: u.Username, Name: u.Name}
}

func newQueueRow(mr *gitlab.BasicMergeRequest) *queueRow {
	r := &queueRow{
		ProjectID: mr.ProjectID, IID: mr.IID, Title: mr.Title, WebURL: mr.WebURL,
		Author: toQueueUser(mr.Author), SHA: mr.SHA, UpdatedAt: mr.UpdatedAt,
	}
	r.Reviewers = queueUsers(mr.Reviewers)
	return r
}

// queueUsers compacts users, skipping nils; the result is never nil.
func queueUsers(us []*gitlab.BasicUser) []queueUser {
	out := []queueUser{}
	for _, u := range us {
		if u != nil {
			out = append(out, *toQueueUser(u))
		}
	}
	return out
}

// parseQueueInput validates the input and returns the roles (canonical order),
// state and optional updated_after.
func parseQueueInput(in getReviewQueueIn) (roles []string, state string, after *time.Time, err error) {
	if strings.TrimSpace(in.GroupID) == "" {
		return nil, "", nil, errInvalid("group_id is required")
	}
	want := map[string]bool{}
	for _, r := range in.Roles {
		if !slices.Contains(queueRoles, r) {
			return nil, "", nil, errInvalid(fmt.Sprintf("invalid role %q: must be one of %s", r, strings.Join(queueRoles, ", ")))
		}
		want[r] = true
	}
	for _, r := range queueRoles {
		if want[r] || len(want) == 0 {
			roles = append(roles, r)
		}
	}
	state = "opened"
	if in.State != nil {
		if err = oneOf("state", in.State, "opened", "closed", "locked", "merged", "all"); err != nil {
			return nil, "", nil, err
		}
		state = *in.State
	}
	if in.UpdatedAfter != nil {
		t, perr := parseCommitTime(*in.UpdatedAfter)
		if perr != nil {
			return nil, "", nil, errInvalid(fmt.Sprintf("invalid updated_after %q: use RFC3339 or YYYY-MM-DD", *in.UpdatedAfter))
		}
		after = &t
	}
	return roles, state, after, nil
}

func getReviewQueue(ctx context.Context, _ *mcp.CallToolRequest, in getReviewQueueIn, d Deps) (*mcp.CallToolResult, any, error) {
	roles, state, after, err := parseQueueInput(in)
	if err != nil {
		return nil, nil, err
	}
	perPage, maxPages := in.PerPage, in.MaxPages
	if perPage < 1 || perPage > queuePerPageDefault {
		perPage = queuePerPageDefault
	}
	if maxPages < 1 {
		maxPages = queueMaxPagesDefault
	}
	maxPages = min(maxPages, queueMaxPagesLimit)

	me, _, err := d.Client.Users.CurrentUser(gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, fmt.Errorf("resolve current user: %w", err)
	}

	type key struct{ project, iid int64 }
	rows := map[key]*queueRow{}
	var reasons []string
	for _, role := range roles {
		opt := &gitlab.ListGroupMergeRequestsOptions{
			ListOptions: gitlab.ListOptions{PerPage: int64(perPage)},
			State:       &state, UpdatedAfter: after, OrderBy: gitlab.Ptr("updated_at"), Sort: gitlab.Ptr("desc"),
		}
		if role == "reviewer" {
			opt.ReviewerID = gitlab.ReviewerID(me.ID)
		} else {
			opt.AuthorID = &me.ID
		}
		for page := int64(1); page <= int64(maxPages); page++ {
			opt.Page = page
			mrs, resp, lerr := d.Client.MergeRequests.ListGroupMergeRequests(in.GroupID, opt, gitlab.WithContext(ctx))
			if lerr != nil {
				return nil, nil, fmt.Errorf("list %s merge requests (page %d): %w", role, page, lerr)
			}
			for _, mr := range mrs {
				if mr == nil {
					continue
				}
				k := key{mr.ProjectID, mr.IID}
				if rows[k] == nil {
					rows[k] = newQueueRow(mr)
				}
				rows[k].Roles = append(rows[k].Roles, role)
			}
			if resp.NextPage == 0 {
				break
			}
			if page == int64(maxPages) {
				reasons = append(reasons, fmt.Sprintf("%s: more pages exist after max_pages=%d (per_page=%d)", role, maxPages, perPage))
			}
		}
	}

	list := make([]*queueRow, 0, len(rows))
	for _, r := range rows {
		list = append(list, r)
	}
	slices.SortFunc(list, func(a, b *queueRow) int {
		if c := compareTimePtr(b.UpdatedAt, a.UpdatedAt); c != 0 {
			return c
		}
		return cmp.Or(cmp.Compare(a.ProjectID, b.ProjectID), cmp.Compare(a.IID, b.IID))
	})

	var reason any
	if len(reasons) > 0 {
		reason = strings.Join(reasons, "; ")
	}
	return nil, Out(map[string]any{
		"merge_requests":   list,
		"complete":         len(reasons) == 0,
		"truncated_reason": reason,
		"current_user":     queueUser{ID: me.ID, Username: me.Username, Name: me.Name},
	}), nil
}

// compareTimePtr orders nil before any time.
func compareTimePtr(a, b *time.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return a.Compare(*b)
}
