package tools

import (
	"encoding/json"
	"fmt"
)

// ToJSONTree converts v to a JSON-serializable tree (maps/slices) for MCP structured output.
func ToJSONTree(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Out converts v to a JSON tree for tool results. Panics only on marshal bugs (should not happen for API types).
func Out(v any) any {
	o, err := ToJSONTree(v)
	if err != nil {
		panic(fmt.Errorf("Out: %w", err))
	}
	return o
}
