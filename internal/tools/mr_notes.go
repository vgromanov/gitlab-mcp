package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterMRNotes registers MR/issue note and discussion tools.
func RegisterMRNotes(s *mcp.Server, d Deps) {
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_note", Description: "Add a note to an issue or merge request"}, createNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_merge_request_thread", Description: "Start a new MR discussion thread (optionally on a diff line)"}, createMergeRequestThread)
	AddTool(s, d, false, "", &mcp.Tool{Name: "mr_discussions", Description: "List MR discussions"}, mrDiscussions)
	AddTool(s, d, true, "", &mcp.Tool{Name: "resolve_merge_request_thread", Description: "Resolve or unresolve an MR discussion"}, resolveMergeRequestThread)
	AddTool(s, d, true, "", &mcp.Tool{Name: "update_merge_request_note", Description: "Edit a top-level MR note"}, updateMergeRequestNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_merge_request_note", Description: "Add a top-level MR note"}, createMergeRequestNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "delete_merge_request_discussion_note", Description: "Delete a note inside an MR discussion"}, deleteMergeRequestDiscussionNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "update_merge_request_discussion_note", Description: "Edit a note inside an MR discussion"}, updateMergeRequestDiscussionNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_merge_request_discussion_note", Description: "Reply in an MR discussion thread"}, createMergeRequestDiscussionNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "delete_merge_request_note", Description: "Delete a top-level MR note"}, deleteMergeRequestNote)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_merge_request_note", Description: "Get a single MR note"}, getMergeRequestNote)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_merge_request_notes", Description: "List MR notes"}, getMergeRequestNotes)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_merge_request_discussion", Description: "Get one MR discussion by id"}, getMergeRequestDiscussion)
}

type createNoteIn struct {
	ProjectID string `json:"project_id"`
	//nolint:misspell // GitLab uses "noteable_*" in REST payloads.
	NoteableType string `json:"noteable_type" jsonschema:"issue or merge_request"`
	//nolint:misspell // GitLab uses "noteable_*" in REST payloads.
	NoteableIID int64  `json:"noteable_iid"`
	Body        string `json:"body"`
	Internal    *bool  `json:"internal,omitempty"`
}

func createNote(ctx context.Context, _ *mcp.CallToolRequest, in createNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := ResolveProjectID(in.ProjectID, d.Config.DefaultProjectID)
	if err != nil {
		return nil, nil, err
	}
	if err := checkAllowedProject(d.Config, pid); err != nil {
		return nil, nil, err
	}
	switch in.NoteableType {
	case "issue":
		opt := &gitlab.CreateIssueNoteOptions{Body: gitlab.Ptr(in.Body)}
		if in.Internal != nil {
			opt.Internal = in.Internal
		}
		n, _, err := d.Client.Notes.CreateIssueNote(pid, in.NoteableIID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(n), nil
	case "merge_request":
		opt := &gitlab.CreateMergeRequestNoteOptions{Body: gitlab.Ptr(in.Body)}
		n, _, err := d.Client.Notes.CreateMergeRequestNote(pid, in.NoteableIID, opt, gitlab.WithContext(ctx))
		if err != nil {
			return nil, nil, err
		}
		return nil, Out(n), nil
	default:
		//nolint:misspell // Mirrors GitLab's "noteable_type" wording.
		return nil, nil, errInvalid("noteable_type must be issue or merge_request")
	}
}

type createMergeRequestThreadIn struct {
	pidMR
	Body     string                  `json:"body"`
	Position *gitlab.PositionOptions `json:"position,omitempty"`
}

func createMergeRequestThread(ctx context.Context, _ *mcp.CallToolRequest, in createMergeRequestThreadIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreateMergeRequestDiscussionOptions{Body: gitlab.Ptr(in.Body)}
	if in.Position != nil {
		opt.Position = in.Position
	}
	disc, _, err := d.Client.Discussions.CreateMergeRequestDiscussion(pid, in.MergeRequestIID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(disc), nil
}

type mrDiscussionsIn struct {
	pidMR
	Pagination
}

func mrDiscussions(ctx context.Context, _ *mcp.CallToolRequest, in mrDiscussionsIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	discs, resp, err := d.Client.Discussions.ListMergeRequestDiscussions(pid, in.MergeRequestIID, &gitlab.ListMergeRequestDiscussionsOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"discussions": discs, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type resolveMergeRequestThreadIn struct {
	pidMR
	DiscussionID string `json:"discussion_id"`
	Resolved     bool   `json:"resolved"`
}

func resolveMergeRequestThread(ctx context.Context, _ *mcp.CallToolRequest, in resolveMergeRequestThreadIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	disc, _, err := d.Client.Discussions.ResolveMergeRequestDiscussion(pid, in.MergeRequestIID, in.DiscussionID, &gitlab.ResolveMergeRequestDiscussionOptions{
		Resolved: gitlab.Ptr(in.Resolved),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(disc), nil
}

type updateMergeRequestNoteIn struct {
	pidMR
	NoteID int64  `json:"note_id"`
	Body   string `json:"body"`
}

func updateMergeRequestNote(ctx context.Context, _ *mcp.CallToolRequest, in updateMergeRequestNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	n, _, err := d.Client.Notes.UpdateMergeRequestNote(pid, in.MergeRequestIID, in.NoteID, &gitlab.UpdateMergeRequestNoteOptions{
		Body: gitlab.Ptr(in.Body),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(n), nil
}

type createMergeRequestNoteIn struct {
	pidMR
	Body string `json:"body"`
}

func createMergeRequestNote(ctx context.Context, _ *mcp.CallToolRequest, in createMergeRequestNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	n, _, err := d.Client.Notes.CreateMergeRequestNote(pid, in.MergeRequestIID, &gitlab.CreateMergeRequestNoteOptions{
		Body: gitlab.Ptr(in.Body),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(n), nil
}

type deleteMergeRequestDiscussionNoteIn struct {
	pidMR
	DiscussionID string `json:"discussion_id"`
	NoteID       int64  `json:"note_id"`
}

func deleteMergeRequestDiscussionNote(ctx context.Context, _ *mcp.CallToolRequest, in deleteMergeRequestDiscussionNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	_, err = d.Client.Discussions.DeleteMergeRequestDiscussionNote(pid, in.MergeRequestIID, in.DiscussionID, in.NoteID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"deleted": true}), nil
}

type updateMergeRequestDiscussionNoteIn struct {
	pidMR
	DiscussionID string `json:"discussion_id"`
	NoteID       int64  `json:"note_id"`
	Body         string `json:"body"`
}

func updateMergeRequestDiscussionNote(ctx context.Context, _ *mcp.CallToolRequest, in updateMergeRequestDiscussionNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	n, _, err := d.Client.Discussions.UpdateMergeRequestDiscussionNote(pid, in.MergeRequestIID, in.DiscussionID, in.NoteID, &gitlab.UpdateMergeRequestDiscussionNoteOptions{
		Body: gitlab.Ptr(in.Body),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(n), nil
}

type createMergeRequestDiscussionNoteIn struct {
	pidMR
	DiscussionID string `json:"discussion_id"`
	Body         string `json:"body"`
}

func createMergeRequestDiscussionNote(ctx context.Context, _ *mcp.CallToolRequest, in createMergeRequestDiscussionNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	n, _, err := d.Client.Discussions.AddMergeRequestDiscussionNote(pid, in.MergeRequestIID, in.DiscussionID, &gitlab.AddMergeRequestDiscussionNoteOptions{
		Body: gitlab.Ptr(in.Body),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(n), nil
}

type deleteMergeRequestNoteIn struct {
	pidMR
	NoteID int64 `json:"note_id"`
}

func deleteMergeRequestNote(ctx context.Context, _ *mcp.CallToolRequest, in deleteMergeRequestNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	_, err = d.Client.Notes.DeleteMergeRequestNote(pid, in.MergeRequestIID, in.NoteID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"deleted": true}), nil
}

type getMergeRequestNoteIn struct {
	pidMR
	NoteID int64 `json:"note_id"`
}

func getMergeRequestNote(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	n, _, err := d.Client.Notes.GetMergeRequestNote(pid, in.MergeRequestIID, in.NoteID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(n), nil
}

type getMergeRequestNotesIn struct {
	pidMR
	Pagination
	Sort *string `json:"sort,omitempty"`
}

func getMergeRequestNotes(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestNotesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	opt := &gitlab.ListMergeRequestNotesOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
		Sort:        in.Sort,
	}
	notes, resp, err := d.Client.Notes.ListMergeRequestNotes(pid, in.MergeRequestIID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"notes": notes, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type getMergeRequestDiscussionIn struct {
	pidMR
	DiscussionID string `json:"discussion_id"`
}

func getMergeRequestDiscussion(ctx context.Context, _ *mcp.CallToolRequest, in getMergeRequestDiscussionIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	disc, _, err := d.Client.Discussions.GetMergeRequestDiscussion(pid, in.MergeRequestIID, in.DiscussionID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(disc), nil
}

func errInvalid(msg string) error {
	return &invalidArg{msg: msg}
}

type invalidArg struct{ msg string }

func (e *invalidArg) Error() string { return e.msg }
