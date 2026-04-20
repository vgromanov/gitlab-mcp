package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterDraftNotes registers MR draft note tools.
func RegisterDraftNotes(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_draft_note", Description: "Get a single draft note on an MR"}, getDraftNote)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_draft_notes", Description: "List draft notes on an MR"}, listDraftNotes)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_draft_note", Description: "Create a draft note"}, createDraftNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "update_draft_note", Description: "Update a draft note"}, updateDraftNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "delete_draft_note", Description: "Delete a draft note"}, deleteDraftNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "publish_draft_note", Description: "Publish one draft note"}, publishDraftNote)
	AddTool(s, d, true, "", &mcp.Tool{Name: "bulk_publish_draft_notes", Description: "Publish all draft notes on an MR"}, bulkPublishDraftNotes)
}

type getDraftNoteIn struct {
	pidMR
	DraftNoteID int64 `json:"draft_note_id"`
}

func getDraftNote(ctx context.Context, _ *mcp.CallToolRequest, in getDraftNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	n, _, err := d.Client.DraftNotes.GetDraftNote(pid, in.MergeRequestIID, in.DraftNoteID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(n), nil
}

type listDraftNotesIn struct {
	pidMR
	Pagination
}

func listDraftNotes(ctx context.Context, _ *mcp.CallToolRequest, in listDraftNotesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	page, perPage := in.ListOpts()
	notes, resp, err := d.Client.DraftNotes.ListDraftNotes(pid, in.MergeRequestIID, &gitlab.ListDraftNotesOptions{
		ListOptions: gitlab.ListOptions{Page: int64(page), PerPage: int64(perPage)},
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"draft_notes": notes, "pagination": map[string]any{"next_page": resp.NextPage}}), nil
}

type createDraftNoteIn struct {
	pidMR
	Note                  string                  `json:"note"`
	CommitID              *string                 `json:"commit_id,omitempty"`
	InReplyToDiscussionID *string                 `json:"in_reply_to_discussion_id,omitempty"`
	ResolveDiscussion     *bool                   `json:"resolve_discussion,omitempty"`
	Position              *gitlab.PositionOptions `json:"position,omitempty"`
}

func createDraftNote(ctx context.Context, _ *mcp.CallToolRequest, in createDraftNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	opt := &gitlab.CreateDraftNoteOptions{Note: gitlab.Ptr(in.Note)}
	if in.CommitID != nil {
		opt.CommitID = in.CommitID
	}
	if in.InReplyToDiscussionID != nil {
		opt.InReplyToDiscussionID = in.InReplyToDiscussionID
	}
	if in.ResolveDiscussion != nil {
		opt.ResolveDiscussion = in.ResolveDiscussion
	}
	if in.Position != nil {
		opt.Position = in.Position
	}
	n, _, err := d.Client.DraftNotes.CreateDraftNote(pid, in.MergeRequestIID, opt, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(n), nil
}

type updateDraftNoteIn struct {
	pidMR
	DraftNoteID int64  `json:"draft_note_id"`
	Note        string `json:"note"`
}

func updateDraftNote(ctx context.Context, _ *mcp.CallToolRequest, in updateDraftNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	n, _, err := d.Client.DraftNotes.UpdateDraftNote(pid, in.MergeRequestIID, in.DraftNoteID, &gitlab.UpdateDraftNoteOptions{
		Note: gitlab.Ptr(in.Note),
	}, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(n), nil
}

type deleteDraftNoteIn struct {
	pidMR
	DraftNoteID int64 `json:"draft_note_id"`
}

func deleteDraftNote(ctx context.Context, _ *mcp.CallToolRequest, in deleteDraftNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	_, err = d.Client.DraftNotes.DeleteDraftNote(pid, in.MergeRequestIID, in.DraftNoteID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"deleted": true}), nil
}

type publishDraftNoteIn struct {
	pidMR
	DraftNoteID int64 `json:"draft_note_id"`
}

func publishDraftNote(ctx context.Context, _ *mcp.CallToolRequest, in publishDraftNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	_, err = d.Client.DraftNotes.PublishDraftNote(pid, in.MergeRequestIID, in.DraftNoteID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"published": true}), nil
}

type bulkPublishDraftNotesIn struct {
	pidMR
}

func bulkPublishDraftNotes(ctx context.Context, _ *mcp.CallToolRequest, in bulkPublishDraftNotesIn, d Deps) (*mcp.CallToolResult, any, error) {
	pid, err := in.resolve(d)
	if err != nil {
		return nil, nil, err
	}
	_, err = d.Client.DraftNotes.PublishAllDraftNotes(pid, in.MergeRequestIID, gitlab.WithContext(ctx))
	if err != nil {
		return nil, nil, err
	}
	return nil, Out(map[string]any{"published_all": true}), nil
}
