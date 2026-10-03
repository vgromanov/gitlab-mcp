package tools

import (
	"fmt"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// selectedGraphQLOperation is the AST-chosen operation for execute_graphql.
type selectedGraphQLOperation struct {
	Name string
	Kind ast.Operation
}

// selectGraphQLOperation parses document with gqlparser and selects one operation.
// One operation may omit a name only when it is the sole operation. Multiple
// operations require unique explicit names and an explicit operationName;
// anonymous/multi ambiguity is rejected even when a named selection is supplied.
// Subscriptions, duplicates, missing selection, and malformed docs are rejected.
func selectGraphQLOperation(document, operationName string) (selectedGraphQLOperation, error) {
	doc, err := parser.ParseQuery(&ast.Source{Name: "execute_graphql", Input: document})
	if err != nil {
		return selectedGraphQLOperation{}, fmt.Errorf("malformed GraphQL document: %w", err)
	}
	if doc == nil || len(doc.Operations) == 0 {
		return selectedGraphQLOperation{}, fmt.Errorf("GraphQL document has no operations")
	}

	anonymous := 0
	seen := make(map[string]struct{}, len(doc.Operations))
	for _, op := range doc.Operations {
		if op == nil {
			continue
		}
		if op.Name == "" {
			anonymous++
			continue
		}
		if _, dup := seen[op.Name]; dup {
			return selectedGraphQLOperation{}, fmt.Errorf("duplicate GraphQL operation name %q", op.Name)
		}
		seen[op.Name] = struct{}{}
	}
	// Anonymous operations are legal only for a single-operation document.
	if anonymous > 0 && len(doc.Operations) != 1 {
		return selectedGraphQLOperation{}, fmt.Errorf("anonymous GraphQL operations are only allowed when the document has exactly one operation")
	}

	var selected *ast.OperationDefinition
	switch {
	case operationName == "":
		if len(doc.Operations) != 1 {
			return selectedGraphQLOperation{}, fmt.Errorf("operation_name is required when the document has multiple operations")
		}
		selected = doc.Operations[0]
	default:
		for _, op := range doc.Operations {
			if op != nil && op.Name == operationName {
				selected = op
				break
			}
		}
		if selected == nil {
			return selectedGraphQLOperation{}, fmt.Errorf("GraphQL operation %q not found", operationName)
		}
	}
	if selected == nil {
		return selectedGraphQLOperation{}, fmt.Errorf("GraphQL document has no operations")
	}
	if selected.Operation == ast.Subscription {
		return selectedGraphQLOperation{}, fmt.Errorf("GraphQL subscriptions are not supported")
	}
	kind := selected.Operation
	if kind == "" {
		kind = ast.Query
	}
	return selectedGraphQLOperation{Name: selected.Name, Kind: kind}, nil
}
