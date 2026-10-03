package tools

import (
	"context"
	"fmt"
	"net/http"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

// graphqlRequestDTO is the supported GraphQL POST body including operationName.
// The vendored GraphQLQuery type lacks operationName; do not edit the SDK.
type graphqlRequestDTO struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables,omitempty"`
	OperationName string         `json:"operationName,omitempty"`
}

// doGraphQL POSTs to /api/graphql via Client.NewRequest/Do with context retained.
func doGraphQL(ctx context.Context, client *gitlab.Client, dto graphqlRequestDTO) (any, error) {
	if client == nil {
		return nil, fmt.Errorf("gitlab client is nil")
	}
	if dto.Variables == nil {
		dto.Variables = map[string]any{}
	}
	req, err := client.NewRequest(http.MethodPost, "", dto, []gitlab.RequestOptionFunc{
		gitlab.WithContext(ctx),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create GraphQL request: %w", err)
	}
	req.URL.Path = gitlab.GraphQLAPIEndpoint
	var out any
	if _, err := client.Do(req, &out); err != nil {
		return nil, err
	}
	return out, nil
}
