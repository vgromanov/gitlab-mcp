package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterGraphQLTools registers GraphQL-based work item and utility tools.
func RegisterGraphQLTools(s *mcp.Server, d Deps) {
	AddTool(s, d, false, "", &mcp.Tool{Name: "execute_graphql", Description: "Run an arbitrary GitLab GraphQL query or mutation"}, executeGraphQL)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_work_item", Description: "Get a work item by global id"}, getWorkItem)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_work_items", Description: "List work items for a project"}, listWorkItems)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_work_item", Description: "Create a work item (requires work_item_type_id gid)"}, createWorkItem)
	AddTool(s, d, true, "", &mcp.Tool{Name: "update_work_item", Description: "Update a work item"}, updateWorkItem)
	AddTool(s, d, true, "", &mcp.Tool{Name: "convert_work_item_type", Description: "Convert work item to another type"}, convertWorkItemType)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_work_item_statuses", Description: "List statuses for a work item type"}, listWorkItemStatuses)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_custom_field_definitions", Description: "List custom field definitions for a work item type"}, listCustomFieldDefinitions)
	AddTool(s, d, true, "", &mcp.Tool{Name: "move_work_item", Description: "Move work item to another project"}, moveWorkItem)
	AddTool(s, d, false, "", &mcp.Tool{Name: "list_work_item_notes", Description: "List notes on a work item"}, listWorkItemNotes)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_work_item_note", Description: "Add a note to a work item"}, createWorkItemNote)
	AddTool(s, d, false, "", &mcp.Tool{Name: "get_timeline_events", Description: "List timeline events for an incident work item"}, getTimelineEvents)
	AddTool(s, d, true, "", &mcp.Tool{Name: "create_timeline_event", Description: "Create a timeline event on an incident"}, createTimelineEvent)
}

func runGQL(ctx context.Context, d Deps, query string, variables map[string]any) (any, error) {
	var out any
	_, err := d.Client.GraphQL.Do(gitlab.GraphQLQuery{
		Query:     query,
		Variables: variables,
	}, &out, gitlab.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	return out, nil
}

type executeGraphQLIn struct {
	Query     string          `json:"query"`
	Variables json.RawMessage `json:"variables,omitempty" jsonschema:"JSON object of GraphQL variables; omit or use {}"`
}

func executeGraphQL(ctx context.Context, _ *mcp.CallToolRequest, in executeGraphQLIn, d Deps) (*mcp.CallToolResult, any, error) {
	var vars map[string]any
	if len(bytes.TrimSpace(in.Variables)) > 0 {
		if err := json.Unmarshal(in.Variables, &vars); err != nil {
			return nil, nil, fmt.Errorf("variables must be a JSON object: %w", err)
		}
	}
	if vars == nil {
		vars = map[string]any{}
	}
	out, err := runGQL(ctx, d, in.Query, vars)
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type getWorkItemIn struct {
	ID string `json:"id" jsonschema:"Work item global id, e.g. gid://gitlab/WorkItem/123"`
}

func getWorkItem(ctx context.Context, _ *mcp.CallToolRequest, in getWorkItemIn, d Deps) (*mcp.CallToolResult, any, error) {
	q := `query($id: WorkItemID!) { workItem(id: $id) { id iid title state stateEnum workItemType { id name } project { id fullPath } description webUrl } }`
	out, err := runGQL(ctx, d, q, map[string]any{"id": in.ID})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type listWorkItemsIn struct {
	ProjectPath string   `json:"project_path" jsonschema:"Namespace/project path"`
	First       int      `json:"first,omitempty"`
	Types       []string `json:"types,omitempty"`
}

func listWorkItems(ctx context.Context, _ *mcp.CallToolRequest, in listWorkItemsIn, d Deps) (*mcp.CallToolResult, any, error) {
	if in.First <= 0 {
		in.First = 20
	}
	if in.First > 100 {
		in.First = 100
	}
	q := `query($fullPath: ID!, $first: Int, $types: [WorkItemTypeFilterInput!]) {
  project(fullPath: $fullPath) {
    workItems(first: $first, filter: { types: $types }) {
      nodes { id iid title state workItemType { name } }
      pageInfo { endCursor hasNextPage }
    }
  }
}`
	vars := map[string]any{"fullPath": in.ProjectPath, "first": in.First}
	if len(in.Types) > 0 {
		var tf []map[string]any
		for _, t := range in.Types {
			tf = append(tf, map[string]any{"name": t})
		}
		vars["types"] = tf
	} else {
		vars["types"] = nil
	}
	out, err := runGQL(ctx, d, q, vars)
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type createWorkItemIn struct {
	ProjectPath    string  `json:"project_path"`
	Title          string  `json:"title"`
	WorkItemTypeID string  `json:"work_item_type_id" jsonschema:"gid://gitlab/WorkItems::Type/..."`
	Description    *string `json:"description,omitempty"`
	Confidential   *bool   `json:"confidential,omitempty"`
}

func createWorkItem(ctx context.Context, _ *mcp.CallToolRequest, in createWorkItemIn, d Deps) (*mcp.CallToolResult, any, error) {
	input := map[string]any{
		"projectPath":    in.ProjectPath,
		"title":          in.Title,
		"workItemTypeId": in.WorkItemTypeID,
	}
	if in.Description != nil {
		input["descriptionWidget"] = map[string]any{"description": *in.Description}
	}
	if in.Confidential != nil {
		input["confidential"] = *in.Confidential
	}
	q := `mutation($input: CreateWorkItemInput!) {
  createWorkItem(input: $input) {
    workItem { id iid title webUrl }
    errors
  }
}`
	out, err := runGQL(ctx, d, q, map[string]any{"input": input})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type updateWorkItemIn struct {
	ID         string          `json:"id"`
	Attributes json.RawMessage `json:"attributes" jsonschema:"JSON object: GraphQL WorkItemUpdateAttributes / workItemWidget fields"`
}

func updateWorkItem(ctx context.Context, _ *mcp.CallToolRequest, in updateWorkItemIn, d Deps) (*mcp.CallToolResult, any, error) {
	var attrs map[string]any
	if len(bytes.TrimSpace(in.Attributes)) == 0 {
		return nil, nil, fmt.Errorf("attributes is required")
	}
	if err := json.Unmarshal(in.Attributes, &attrs); err != nil {
		return nil, nil, fmt.Errorf("attributes must be a JSON object: %w", err)
	}
	q := `mutation($input: UpdateWorkItemInput!) {
  workItemUpdate(input: $input) {
    workItem { id iid title }
    errors
  }
}`
	out, err := runGQL(ctx, d, q, map[string]any{"input": map[string]any{"id": in.ID, "workItemWidget": attrs}})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type convertWorkItemTypeIn struct {
	ID             string `json:"id"`
	WorkItemTypeID string `json:"work_item_type_id"`
}

func convertWorkItemType(ctx context.Context, _ *mcp.CallToolRequest, in convertWorkItemTypeIn, d Deps) (*mcp.CallToolResult, any, error) {
	q := `mutation($input: WorkItemConvertInput!) {
  workItemConvert(input: $input) {
    workItem { id iid workItemType { name } }
    errors
  }
}`
	out, err := runGQL(ctx, d, q, map[string]any{"input": map[string]any{"id": in.ID, "workItemTypeId": in.WorkItemTypeID}})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type listWorkItemStatusesIn struct {
	ProjectPath    string `json:"project_path"`
	WorkItemTypeID string `json:"work_item_type_id"`
}

func listWorkItemStatuses(ctx context.Context, _ *mcp.CallToolRequest, in listWorkItemStatusesIn, d Deps) (*mcp.CallToolResult, any, error) {
	q := `query($fullPath: ID!, $typeId: WorkItemsTypeID!) {
  project(fullPath: $fullPath) {
    workItemStatuses(workItemTypeId: $typeId) { nodes { id name } }
  }
}`
	out, err := runGQL(ctx, d, q, map[string]any{"fullPath": in.ProjectPath, "typeId": in.WorkItemTypeID})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type listCustomFieldDefinitionsIn struct {
	ProjectPath    string `json:"project_path"`
	WorkItemTypeID string `json:"work_item_type_id"`
}

func listCustomFieldDefinitions(ctx context.Context, _ *mcp.CallToolRequest, in listCustomFieldDefinitionsIn, d Deps) (*mcp.CallToolResult, any, error) {
	q := `query($fullPath: ID!, $typeId: WorkItemsTypeID!) {
  project(fullPath: $fullPath) {
    workItemCustomFieldDefinitions(workItemTypeId: $typeId) {
      nodes { id name }
    }
  }
}`
	out, err := runGQL(ctx, d, q, map[string]any{"fullPath": in.ProjectPath, "typeId": in.WorkItemTypeID})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type moveWorkItemIn struct {
	WorkItemID string `json:"work_item_id"`
	TargetPath string `json:"target_project_path"`
}

func moveWorkItem(ctx context.Context, _ *mcp.CallToolRequest, in moveWorkItemIn, d Deps) (*mcp.CallToolResult, any, error) {
	q := `mutation($input: WorkItemMoveInput!) {
  workItemMove(input: $input) {
    workItem { id project { fullPath } }
    errors
  }
}`
	out, err := runGQL(ctx, d, q, map[string]any{"input": map[string]any{"id": in.WorkItemID, "projectPath": in.TargetPath}})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type listWorkItemNotesIn struct {
	ID string `json:"id"`
}

func listWorkItemNotes(ctx context.Context, _ *mcp.CallToolRequest, in listWorkItemNotesIn, d Deps) (*mcp.CallToolResult, any, error) {
	q := `query($id: WorkItemID!) {
  workItem(id: $id) {
    widgets {
      ... on WorkItemWidgetNotes {
        notes { nodes { id body author { username } createdAt } }
      }
    }
  }
}`
	out, err := runGQL(ctx, d, q, map[string]any{"id": in.ID})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type createWorkItemNoteIn struct {
	ID       string `json:"id"`
	Body     string `json:"body"`
	Internal *bool  `json:"internal,omitempty"`
}

func createWorkItemNote(ctx context.Context, _ *mcp.CallToolRequest, in createWorkItemNoteIn, d Deps) (*mcp.CallToolResult, any, error) {
	input := map[string]any{"id": in.ID, "note": in.Body}
	if in.Internal != nil {
		input["internal"] = *in.Internal
	}
	q := `mutation($input: WorkItemNoteCreateInput!) {
  workItemNoteCreate(input: $input) {
    note { id body }
    errors
  }
}`
	out, err := runGQL(ctx, d, q, map[string]any{"input": input})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type getTimelineEventsIn struct {
	ID string `json:"id" jsonschema:"Incident work item gid"`
}

func getTimelineEvents(ctx context.Context, _ *mcp.CallToolRequest, in getTimelineEventsIn, d Deps) (*mcp.CallToolResult, any, error) {
	q := `query($id: WorkItemID!) {
  workItem(id: $id) {
    widgets {
      ... on WorkItemWidgetTimelineEvents {
        timelineEvents { nodes { id happenedAt action }
        }
      }
    }
  }
}`
	out, err := runGQL(ctx, d, q, map[string]any{"id": in.ID})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}

type createTimelineEventIn struct {
	ID         string  `json:"id"`
	Tag        string  `json:"tag"`
	Note       string  `json:"note,omitempty"`
	HappenedAt *string `json:"happened_at,omitempty"`
}

func createTimelineEvent(ctx context.Context, _ *mcp.CallToolRequest, in createTimelineEventIn, d Deps) (*mcp.CallToolResult, any, error) {
	input := map[string]any{"workItemId": in.ID, "tag": in.Tag}
	if in.Note != "" {
		input["note"] = in.Note
	}
	if in.HappenedAt != nil {
		input["happenedAt"] = *in.HappenedAt
	}
	q := `mutation($input: TimelineEventCreateInput!) {
  timelineEventCreate(input: $input) {
    timelineEvent { id tag happenedAt }
    errors
  }
}`
	out, err := runGQL(ctx, d, q, map[string]any{"input": input})
	if err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}
