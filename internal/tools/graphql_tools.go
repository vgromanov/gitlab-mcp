package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// RegisterGraphQLTools registers GraphQL-based work item and utility tools.
func RegisterGraphQLTools(s *mcp.Server, d Deps) {
	yes := true
	AddTool(s, d, false, "", &mcp.Tool{
		Name:        "execute_graphql",
		Description: "Run an arbitrary GitLab GraphQL query or mutation (mutations are rejected in read-only mode). Top-level GraphQL errors are returned as tool errors.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &yes, OpenWorldHint: &yes},
	}, executeGraphQL)
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
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty" jsonschema:"JSON object of GraphQL variables; omit or use {}"`
}

func executeGraphQL(ctx context.Context, _ *mcp.CallToolRequest, in executeGraphQLIn, d Deps) (*mcp.CallToolResult, any, error) {
	if d.Config != nil && d.Config.ReadOnly {
		mutation, err := gqlHasMutation(in.Query)
		if err != nil {
			return nil, nil, fmt.Errorf("read-only mode: cannot verify GraphQL document has no mutation: %w", err)
		}
		if mutation {
			return nil, nil, errors.New("GraphQL mutations are rejected in read-only mode")
		}
	}
	if in.Variables == nil {
		in.Variables = map[string]any{}
	}
	out, err := runGQL(ctx, d, in.Query, in.Variables)
	if err != nil {
		return nil, nil, err
	}
	if m, ok := out.(map[string]any); ok {
		if errs, _ := m["errors"].([]any); len(errs) > 0 {
			msgs := make([]string, len(errs))
			for i, e := range errs {
				msgs[i] = fmt.Sprint(e)
				if em, ok := e.(map[string]any); ok {
					if msg, ok := em["message"].(string); ok {
						msgs[i] = msg
					}
				}
			}
			return nil, nil, fmt.Errorf("GraphQL errors: %s", strings.Join(msgs, "; "))
		}
	}
	return nil, out, nil
}

// gqlHasMutation reports whether doc defines a mutation operation anywhere. It
// is a minimal scanner, not a parser: it skips comments and strings, tracks
// bracket depth and checks the first name of every top-level definition, so
// multiple operations, fragments, directives and the word "mutation" inside
// comments/strings are handled. It errors on an unterminated string.
func gqlHasMutation(doc string) (bool, error) {
	depth, defStart := 0, true
	for i := 0; i < len(doc); {
		c := doc[i]
		switch {
		case c == '#':
			for i < len(doc) && doc[i] != '\n' && doc[i] != '\r' {
				i++
			}
		case c == '"':
			end := gqlStringEnd(doc, i)
			if end < 0 {
				return false, errors.New("unterminated string")
			}
			i = end
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
			j := i + 1
			for j < len(doc) && (doc[j] == '_' || doc[j] >= 'a' && doc[j] <= 'z' || doc[j] >= 'A' && doc[j] <= 'Z' || doc[j] >= '0' && doc[j] <= '9') {
				j++
			}
			if depth == 0 && defStart {
				if doc[i:j] == "mutation" {
					return true, nil
				}
				defStart = false
			}
			i = j
		default:
			switch c {
			case '{', '(', '[':
				depth++
			case '}', ')', ']':
				if depth > 0 {
					depth--
				}
				defStart = defStart || c == '}' && depth == 0
			}
			i++
		}
	}
	return false, nil
}

// gqlStringEnd returns the index after the string literal starting at doc[i],
// or -1 if it is unterminated.
func gqlStringEnd(doc string, i int) int {
	if strings.HasPrefix(doc[i:], `"""`) {
		for j := i + 3; j < len(doc); j++ {
			switch {
			case doc[j] == '\\' && strings.HasPrefix(doc[j:], `\"""`):
				j += 3
			case strings.HasPrefix(doc[j:], `"""`):
				return j + 3
			}
		}
		return -1
	}
	for j := i + 1; j < len(doc) && doc[j] != '\n'; j++ {
		switch doc[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
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
