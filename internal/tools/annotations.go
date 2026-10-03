package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tools that mutate remote or local environment despite AddTool mutating=false.
// Legacy mutating bits / ReadOnly registration gating are intentionally preserved;
// these overrides only affect MCP ToolAnnotations.
var annotationForceNotReadOnly = map[string]struct{}{
	"upload_markdown":        {},
	"execute_graphql":        {},
	"download_job_artifacts": {},
	"download_release_asset": {},
}

// annotationAdditiveMutating lists mutating tools that are only additive creates
// (destructiveHint=false). Audited against handlers — exclude overwrite/update/delete,
// pipeline/job execution, draft publish (consumes draft state), and similar.
var annotationAdditiveMutating = map[string]struct{}{
	"create_repository":                    {},
	"fork_repository":                      {},
	"create_branch":                        {},
	"create_merge_request":                 {},
	"create_merge_request_note":            {},
	"create_merge_request_thread":          {},
	"create_merge_request_discussion_note": {},
	"create_note":                          {},
	"approve_merge_request":                {},
	"create_issue":                         {},
	"create_issue_link":                    {},
	"create_issue_note":                    {},
	"create_label":                         {},
	"create_milestone":                     {},
	"create_wiki_page":                     {},
	"create_group_wiki_page":               {},
	"create_release":                       {},
	"create_release_evidence":              {},
	"create_draft_note":                    {},
	"create_work_item":                     {},
	"create_work_item_note":                {},
	"create_timeline_event":                {},
	// intentionally NOT additive:
	// create_or_update_file, push_files (overwrite/delete actions),
	// create_pipeline / play_pipeline_job (arbitrary job execution),
	// publish_draft_note / bulk_publish_draft_notes (consume/change draft state),
	// updates/deletes/cancels/merges.
}

func boolPtr(v bool) *bool { return &v }

func copyTool(tool *mcp.Tool) *mcp.Tool {
	if tool == nil {
		return nil
	}
	cp := *tool
	if tool.Annotations != nil {
		ann := *tool.Annotations
		if tool.Annotations.DestructiveHint != nil {
			v := *tool.Annotations.DestructiveHint
			ann.DestructiveHint = &v
		}
		if tool.Annotations.OpenWorldHint != nil {
			v := *tool.Annotations.OpenWorldHint
			ann.OpenWorldHint = &v
		}
		cp.Annotations = &ann
	}
	if tool.Icons != nil {
		cp.Icons = append([]mcp.Icon(nil), tool.Icons...)
	}
	return &cp
}

// applyToolAnnotations sets truthful MCP annotations on a tool copy.
// Does not claim idempotentHint=true (SDK omitempty default is false).
func applyToolAnnotations(tool *mcp.Tool, mutating bool) *mcp.Tool {
	out := copyTool(tool)
	if out == nil {
		return nil
	}
	if out.Annotations != nil {
		return out
	}

	readOnly := !mutating
	if _, force := annotationForceNotReadOnly[out.Name]; force {
		readOnly = false
	}

	ann := &mcp.ToolAnnotations{ReadOnlyHint: readOnly}
	if !readOnly {
		if mutating {
			if _, additive := annotationAdditiveMutating[out.Name]; additive {
				ann.DestructiveHint = boolPtr(false)
			} else {
				ann.DestructiveHint = boolPtr(true)
			}
		} else {
			// Side-effect overrides despite mutating=false registration.
			// upload / execute_graphql / os.Create download writers: destructive.
			ann.DestructiveHint = boolPtr(true)
		}
	}
	out.Annotations = ann
	return out
}
