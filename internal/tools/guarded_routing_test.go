package tools

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	glclient "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

// TestRegisteredHandlers_routeMutationAndGraphQLToGuarded proves AddTool and
// RegisterGraphQLTools close over Deps.Guarded for mutation and GraphQL paths.
func TestRegisteredHandlers_routeMutationAndGraphQLToGuarded(t *testing.T) {
	readRec := &testutil.RecordingHandler{Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `[]`)
	})}
	guardRec := &testutil.RecordingHandler{Next: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	})}
	readTS := httptest.NewServer(readRec)
	t.Cleanup(readTS.Close)
	guardTS := httptest.NewServer(guardRec)
	t.Cleanup(guardTS.Close)

	readCli, err := glclient.NewClient(&config.Config{Token: "fake-token-not-live", APIURL: readTS.URL + "/api/v4"})
	if err != nil {
		t.Fatal(err)
	}
	guardCli, err := glclient.NewGuardedClient(&config.Config{Token: "fake-token-not-live", APIURL: guardTS.URL + "/api/v4"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Token: "fake-token-not-live"}
	d := Deps{Config: cfg, Client: readCli, Guarded: guardCli}

	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterRepository(srv, d)
	RegisterGraphQLTools(srv, d)
	cs := testutil.MCPConnect(t, srv)

	// Mutating REST tool must hit guarded recorder exactly once (500, no retry).
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "create_repository",
		Arguments: map[string]any{"name": "rvg125-fixture"},
	}); err != nil {
		t.Fatalf("create_repository CallTool protocol err: %v", err)
	}
	if got := guardRec.Attempts(); got != 1 {
		t.Fatalf("create_repository guarded attempts=%d want 1 snapshot=%v", got, guardRec.Snapshot())
	}
	if readRec.Attempts() != 0 {
		t.Fatalf("create_repository must not hit read client, attempts=%d", readRec.Attempts())
	}

	// GraphQL (even query-looking) must hit guarded path — one attempt, no retry.
	beforeGQL := guardRec.Attempts()
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute_graphql",
		Arguments: map[string]any{"query": "{ __typename }"},
	}); err != nil {
		t.Fatalf("execute_graphql CallTool protocol err: %v", err)
	}
	if got := guardRec.Attempts() - beforeGQL; got != 1 {
		t.Fatalf("execute_graphql guarded delta=%d want 1 total=%d", got, guardRec.Attempts())
	}

	// Safe read tool should use read client, not guarded.
	beforeRead := readRec.Attempts()
	beforeGuard := guardRec.Attempts()
	// Direct handler (bypasses MCP) proves read client dials.
	if _, _, err := searchRepositories(context.Background(), nil, searchRepositoriesIn{}, Deps{Config: cfg, Client: readCli}); err != nil {
		t.Fatalf("direct searchRepositories: %v", err)
	}
	if readRec.Attempts() <= beforeRead {
		t.Fatalf("direct searchRepositories should hit read client; before=%d after=%d", beforeRead, readRec.Attempts())
	}
	afterDirect := readRec.Attempts()
	resRead, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "search_repositories",
		Arguments: map[string]any{
			"page":     float64(1),
			"per_page": float64(20),
		},
	})
	if err != nil {
		t.Fatalf("search_repositories CallTool protocol err: %v", err)
	}
	if readRec.Attempts() <= afterDirect {
		var msg string
		if resRead != nil {
			for _, c := range resRead.Content {
				if t, ok := c.(*mcp.TextContent); ok {
					msg += t.Text
				}
			}
		}
		t.Fatalf("CallTool search_repositories should hit read client; before=%d after=%d isError=%v msg=%q",
			afterDirect, readRec.Attempts(), resRead != nil && resRead.IsError, msg)
	}
	if guardRec.Attempts() != beforeGuard {
		t.Fatalf("search_repositories must not hit guarded client; before=%d after=%d", beforeGuard, guardRec.Attempts())
	}
}

func TestAddTool_mutatingUsesGuardedClientPointer(t *testing.T) {
	readRec := &testutil.RecordingHandler{Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":1}`)
	})}
	guardRec := &testutil.RecordingHandler{Next: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"message":"rate"}`)
	})}
	readTS := httptest.NewServer(readRec)
	t.Cleanup(readTS.Close)
	guardTS := httptest.NewServer(guardRec)
	t.Cleanup(guardTS.Close)

	readCli, err := glclient.NewClient(&config.Config{Token: "fake-token-not-live", APIURL: readTS.URL + "/api/v4"})
	if err != nil {
		t.Fatal(err)
	}
	guardCli, err := glclient.NewGuardedClient(&config.Config{Token: "fake-token-not-live", APIURL: guardTS.URL + "/api/v4"})
	if err != nil {
		t.Fatal(err)
	}
	d := Deps{Config: &config.Config{Token: "t"}, Client: readCli, Guarded: guardCli}
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterRepository(srv, d)
	cs := testutil.MCPConnect(t, srv)
	_, _ = cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "create_repository",
		Arguments: map[string]any{
			"name": "rvg125-fixture",
		},
	})
	if guardRec.Attempts() != 1 {
		t.Fatalf("create_repository guarded attempts=%d want 1 (429 one attempt)", guardRec.Attempts())
	}
	if readRec.Attempts() != 0 {
		t.Fatalf("create_repository must not use read client, read attempts=%d", readRec.Attempts())
	}
}
