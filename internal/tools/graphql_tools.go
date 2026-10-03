package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vektah/gqlparser/v2/ast"
)

// RegisterGraphQLTools registers GraphQL-based work item and utility tools.
// Every GraphQL call is HTTP POST (including query-looking text), so when a
// Guarded client is present the entire registration closes over it.
func RegisterGraphQLTools(s *mcp.Server, d Deps) {
	if d.Guarded != nil {
		d.Client = d.Guarded
	}
	// PolicyActive allowlists disable arbitrary GraphQL at registration time.
	// The handler also fails closed if somehow invoked.
	if d.Config == nil || !d.Config.PolicyActive() {
		AddTool(s, d, false, "", &mcp.Tool{Name: "execute_graphql", Description: "Run a selected GitLab GraphQL query or mutation"}, executeGraphQL)
	}
	AddTool(s, d, false, "work_items", &mcp.Tool{Name: "get_work_item", Description: "Get a work item by global id"}, getWorkItem)
	AddTool(s, d, false, "work_items", &mcp.Tool{Name: "list_work_items", Description: "List work items for a project"}, listWorkItems)
	AddTool(s, d, true, "work_items", &mcp.Tool{Name: "create_work_item", Description: "Create a work item (requires work_item_type_id gid)"}, createWorkItem)
	AddTool(s, d, true, "work_items", &mcp.Tool{Name: "update_work_item", Description: "Update a work item"}, updateWorkItem)
	AddTool(s, d, true, "work_items", &mcp.Tool{Name: "convert_work_item_type", Description: "Convert work item to another type"}, convertWorkItemType)
	AddTool(s, d, false, "work_items", &mcp.Tool{Name: "list_work_item_statuses", Description: "List statuses for a work item type"}, listWorkItemStatuses)
	AddTool(s, d, false, "work_items", &mcp.Tool{Name: "list_custom_field_definitions", Description: "List custom field definitions for a work item type"}, listCustomFieldDefinitions)
	AddTool(s, d, true, "work_items", &mcp.Tool{Name: "move_work_item", Description: "Move work item to another project"}, moveWorkItem)
	AddTool(s, d, false, "work_items", &mcp.Tool{Name: "list_work_item_notes", Description: "List notes on a work item"}, listWorkItemNotes)
	AddTool(s, d, true, "work_items", &mcp.Tool{Name: "create_work_item_note", Description: "Add a note to a work item"}, createWorkItemNote)
	AddTool(s, d, false, "timeline", &mcp.Tool{Name: "get_timeline_events", Description: "List timeline events for an incident work item"}, getTimelineEvents)
	AddTool(s, d, true, "timeline", &mcp.Tool{Name: "create_timeline_event", Description: "Create a timeline event on an incident"}, createTimelineEvent)
}

func runGQL(ctx context.Context, d Deps, query string, variables map[string]any) (any, error) {
	if variables == nil {
		variables = map[string]any{}
	}
	out, err := doGraphQL(ctx, d.Client, graphqlRequestDTO{
		Query:     query,
		Variables: variables,
	})
	if err != nil {
		return nil, err
	}
	if err := topLevelGraphQLErrors(out); err != nil {
		return nil, err
	}
	return out, nil
}

// graphqlVariablesObject is the MCP object schema for execute_graphql variables.
// Null/omit become an empty map; scalars and arrays are rejected.
type graphqlVariablesObject map[string]any

func (v *graphqlVariablesObject) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*v = graphqlVariablesObject{}
		return nil
	}
	if b[0] != '{' {
		return fmt.Errorf("variables must be a JSON object")
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("variables must be a JSON object: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	*v = graphqlVariablesObject(m)
	return nil
}

func (v graphqlVariablesObject) asMap() map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return map[string]any(v)
}

func graphqlVariablesAsMap(v *graphqlVariablesObject) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	return v.asMap()
}

// executeGraphQLIn is the MCP input for execute_graphql.
// Variables is a nullable JSON object (repo convention *T → [null, object]);
// omit/null map to {}.
type executeGraphQLIn struct {
	Query         string                  `json:"query" jsonschema:"GraphQL document"`
	Variables     *graphqlVariablesObject `json:"variables,omitempty" jsonschema:"JSON object of GraphQL variables; omit or null for empty"`
	OperationName string                  `json:"operation_name,omitempty" jsonschema:"Operation name when the document defines multiple operations"`
}

func executeGraphQL(ctx context.Context, _ *mcp.CallToolRequest, in executeGraphQLIn, d Deps) (*mcp.CallToolResult, any, error) {
	if d.Config != nil && d.Config.PolicyActive() {
		return nil, nil, fmt.Errorf("execute_graphql is disabled when project or group allowlists are configured")
	}
	vars := graphqlVariablesAsMap(in.Variables)
	selected, err := selectGraphQLOperation(in.Query, in.OperationName)
	if err != nil {
		return nil, nil, err
	}
	if d.Config != nil && d.Config.ReadOnly && selected.Kind != ast.Query {
		return nil, nil, fmt.Errorf("read-only mode permits only GraphQL queries; selected %s is denied", selected.Kind)
	}
	out, err := doGraphQL(ctx, d.Client, graphqlRequestDTO{
		Query:         in.Query,
		Variables:     vars,
		OperationName: selected.Name,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := topLevelGraphQLErrors(out); err != nil {
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
	if err := knownMutationPayloadErrors(out, "createWorkItem"); err != nil {
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
	if err := knownMutationPayloadErrors(out, "workItemUpdate"); err != nil {
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
	if err := knownMutationPayloadErrors(out, "workItemConvert"); err != nil {
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
	if err := knownMutationPayloadErrors(out, "workItemMove"); err != nil {
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
	if err := knownMutationPayloadErrors(out, "workItemNoteCreate"); err != nil {
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
	if err := knownMutationPayloadErrors(out, "timelineEventCreate"); err != nil {
		return nil, nil, err
	}
	return nil, out, nil
}
