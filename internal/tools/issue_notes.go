package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterIssueNotes registers issue discussion / note tools.
func RegisterIssueNotes(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_issue_discussions", Description: "List discussions on an issue"}, listIssueDiscussions)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_issue_note", Description: "Reply in an issue discussion thread"}, createIssueNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "update_issue_note", Description: "Edit a note in an issue discussion"}, updateIssueNote)
}

type listIssueDiscussionsIn struct {
	pidIssue
	Pagination
}

func listIssueDiscussions(ctx context.Context, _ *mcp.CallToolRequest, in listIssueDiscussionsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	discs, resp, err := d.Client.Discussions.ListIssueDiscussions(pid, in.IssueIID, &gitlab.ListIssueDiscussionsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"discussions": discs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type createIssueNoteIn struct {
	pidIssue
	DiscussionID string `json:"discussion_id"`
	Body         string `json:"body"`
}

func createIssueNote(ctx context.Context, _ *mcp.CallToolRequest, in createIssueNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	n, _, err := d.Client.Discussions.AddIssueDiscussionNote(pid, in.IssueIID, in.DiscussionID, &gitlab.AddIssueDiscussionNoteOptions{
		Body: gitlab.Ptr(in.Body),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(n), nil
}

type updateIssueNoteIn struct {
	pidIssue
	DiscussionID string `json:"discussion_id"`
	NoteID       int64  `json:"note_id"`
	Body         string `json:"body"`
}

func updateIssueNote(ctx context.Context, _ *mcp.CallToolRequest, in updateIssueNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	n, _, err := d.Client.Discussions.UpdateIssueDiscussionNote(pid, in.IssueIID, in.DiscussionID, in.NoteID, &gitlab.UpdateIssueDiscussionNoteOptions{
		Body: gitlab.Ptr(in.Body),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(n), nil
}
