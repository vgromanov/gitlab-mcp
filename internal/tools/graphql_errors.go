package tools

import (
	"fmt"
	"strings"
)

// topLevelGraphQLErrors turns nonempty top-level GraphQL errors into a tool error.
// Missing errors key and errors:[] are success. A present null or other non-array
// top-level errors value fails closed. Nested fields named "errors" are ignored.
func topLevelGraphQLErrors(out any) error {
	root, ok := out.(map[string]any)
	if !ok || root == nil {
		return nil
	}
	raw, exists := root["errors"]
	if !exists {
		return nil
	}
	// Present null is malformed (distinct from an absent key).
	if raw == nil {
		return fmt.Errorf("malformed GraphQL top-level errors value")
	}
	msgs, fail, ok := graphQLErrorMessages(raw)
	if !ok {
		return fmt.Errorf("malformed GraphQL top-level errors value")
	}
	if !fail {
		return nil
	}
	return fmt.Errorf("GraphQL errors: %s", strings.Join(msgs, "; "))
}

// knownMutationPayloadErrors checks documented payload errors for one known
// mutation field under data.<field>.errors. It never walks arbitrary user fields.
// Absent errors and errors:[] succeed; a present null or other non-array value
// fails closed.
func knownMutationPayloadErrors(out any, field string) error {
	root, ok := out.(map[string]any)
	if !ok || root == nil {
		return nil
	}
	data, _ := root["data"].(map[string]any)
	if data == nil {
		return nil
	}
	payload, _ := data[field].(map[string]any)
	if payload == nil {
		return nil
	}
	raw, exists := payload["errors"]
	if !exists {
		return nil
	}
	if raw == nil {
		return fmt.Errorf("malformed GraphQL %s errors value", field)
	}
	msgs, fail, ok := graphQLErrorMessages(raw)
	if !ok {
		return fmt.Errorf("malformed GraphQL %s errors value", field)
	}
	if !fail {
		return nil
	}
	return fmt.Errorf("GraphQL %s errors: %s", field, strings.Join(msgs, "; "))
}

func graphQLErrorMessages(raw any) (msgs []string, fail bool, ok bool) {
	switch v := raw.(type) {
	case []any:
		if len(v) == 0 {
			return nil, false, true
		}
		out := make([]string, 0, len(v))
		for _, item := range v {
			switch e := item.(type) {
			case string:
				if e != "" {
					out = append(out, e)
				} else {
					out = append(out, "unknown error")
				}
			case map[string]any:
				if m, _ := e["message"].(string); m != "" {
					out = append(out, m)
				} else {
					out = append(out, fmt.Sprint(e))
				}
			default:
				out = append(out, fmt.Sprint(e))
			}
		}
		return out, true, true
	case []string:
		if len(v) == 0 {
			return nil, false, true
		}
		return append([]string(nil), v...), true, true
	default:
		return nil, false, false
	}
}
