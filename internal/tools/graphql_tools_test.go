package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

// gqlSession serves reply as the body of POST /api/graphql (HTTP 200), records
// the request bodies and returns an MCP session with the GraphQL tools.
func gqlSession(t *testing.T, readOnly bool, reply string) (*mcp.ClientSession, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	var n atomic.Int32
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		n.Add(1)
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		bodies = append(bodies, m)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, reply)
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterGraphQLTools(srv, Deps{Config: &config.Config{ReadOnly: readOnly}, Client: cli})
	return testutil.MCPConnect(t, srv), &bodies
}

func gqlCall(t *testing.T, cs *mcp.ClientSession, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "execute_graphql", Arguments: args})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	return res
}

func TestExecuteGraphQL_objectVariables(t *testing.T) {
	cs, bodies := gqlSession(t, false, `{"data":{"project":{"id":"gid://gitlab/Project/1"}}}`)

	// Declared schema: variables is an object, and the tool is not read-only.
	tl, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var tool *mcp.Tool
	for _, x := range tl.Tools {
		if x.Name == "execute_graphql" {
			tool = x
		}
	}
	if tool == nil {
		t.Fatal("execute_graphql not listed")
	}
	schema, _ := json.Marshal(tool.InputSchema)
	var sc struct {
		Properties map[string]struct {
			Type any `json:"type"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(schema, &sc)
	if ty, _ := json.Marshal(sc.Properties["variables"].Type); !strings.Contains(string(ty), "object") || strings.Contains(string(schema), `"array"`) {
		t.Fatalf("variables schema = %s", schema)
	}
	if a := tool.Annotations; a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Fatalf("annotations = %+v, want not read-only", a)
	}

	vars := map[string]any{"path": "g/p", "first": float64(5), "nested": map[string]any{"a": []any{"x"}}}
	res := gqlCall(t, cs, map[string]any{"query": `query($path: ID!) { project(fullPath: $path) { id } }`, "variables": vars})
	if res.IsError {
		t.Fatalf("unexpected error: %s", contentText(res))
	}
	if len(*bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(*bodies))
	}
	got, _ := json.Marshal((*bodies)[0]["variables"])
	want, _ := json.Marshal(vars)
	if string(got) != string(want) {
		t.Fatalf("request variables = %s, want %s", got, want)
	}

	// Array variables are rejected by the schema, not forwarded.
	if res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "execute_graphql", Arguments: map[string]any{"query": "{a}", "variables": []any{1}}}); err == nil && !res.IsError {
		t.Fatal("array variables accepted")
	}
	// Omitted or null variables are accepted and sent as an empty object (or not at all).
	for _, args := range []map[string]any{{"query": "{ a }"}, {"query": "{ a }", "variables": nil}} {
		if res := gqlCall(t, cs, args); res.IsError {
			t.Fatalf("%v: %s", args, contentText(res))
		}
		if v, ok := (*bodies)[len(*bodies)-1]["variables"]; ok {
			if m, _ := v.(map[string]any); len(m) != 0 {
				t.Fatalf("empty variables sent as %v", v)
			}
		}
	}
}

func TestExecuteGraphQL_readOnlyRejectsMutation(t *testing.T) {
	cs, bodies := gqlSession(t, true, `{"data":{"ok":true}}`)
	for _, q := range []string{
		`mutation { a }`,
		`query Q { a } mutation M($x: In = {k: "v"}) @d { b }`,
		"# comment\nmutation{a}",
		`mutation { a(s: "unterminated) }`,
	} {
		res := gqlCall(t, cs, map[string]any{"query": q})
		if !res.IsError {
			t.Fatalf("read-only accepted %q", q)
		}
	}
	if len(*bodies) != 0 {
		t.Fatalf("%d HTTP requests made for rejected documents", len(*bodies))
	}
	if txt := contentText(gqlCall(t, cs, map[string]any{"query": "mutation { a }"})); !strings.Contains(txt, "read-only") {
		t.Fatalf("error text = %q", txt)
	}

	// Queries still run in read-only mode, even if they mention the word.
	if res := gqlCall(t, cs, map[string]any{"query": "# mutation\nquery { a(s: \"mutation\") { mutation } }"}); res.IsError {
		t.Fatalf("query rejected: %s", contentText(res))
	}
	if len(*bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(*bodies))
	}

	// With writes enabled the mutation goes through.
	cs, bodies = gqlSession(t, false, `{"data":{"ok":true}}`)
	if res := gqlCall(t, cs, map[string]any{"query": "mutation { a }"}); res.IsError || len(*bodies) != 1 {
		t.Fatalf("writable mutation: err=%v requests=%d", res.IsError, len(*bodies))
	}
}

func TestExecuteGraphQL_graphQLErrors200(t *testing.T) {
	cs, _ := gqlSession(t, false, `{"data":null,"errors":[{"message":"Field 'x' doesn't exist"},{"message":"second"},"odd"]}`)
	res := gqlCall(t, cs, map[string]any{"query": "{ x }"})
	if txt := contentText(res); !res.IsError || !strings.Contains(txt, "Field 'x' doesn't exist") || !strings.Contains(txt, "second") || !strings.Contains(txt, "odd") {
		t.Fatalf("IsError=%v text=%q", res.IsError, txt)
	}

	cs, _ = gqlSession(t, false, `{"data":{"x":1},"errors":[]}`)
	if res := gqlCall(t, cs, map[string]any{"query": "{ x }"}); res.IsError {
		t.Fatalf("empty errors treated as failure: %s", contentText(res))
	}
}

func TestGQLHasMutation(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want bool
	}{
		{`{ a }`, false},
		{`query { a }`, false},
		{`query Q($m: Int = 1) @mutation(x: {mutation: 1}) { mutation }`, false},
		{`subscription { mutation }`, false},
		{`fragment mutation on Mutation { a } query { ...mutation }`, false},
		{"# mutation { a }\n{ a }", false},
		{`{ a(s: "mutation { b }", t: "q\" mutation") }`, false},
		{`{ a(s: """ mutation " \""" mutation """) }`, false},
		{`mutation { a }`, true},
		{`mutation{a}`, true},
		{`  mutation Named($a: [Int] = [1]) { a }`, true},
		{`query A { a } mutation B { b }`, true},
		{`fragment F on T { a } # c` + "\r" + `mutation { a }`, true},
		{`query A($v: In = {a: [1, {b: 2}]}) { a } mutation { b }`, true},
		{`{ a } } mutation { b }`, true},
		{``, false},
	} {
		got, err := gqlHasMutation(tc.doc)
		if err != nil || got != tc.want {
			t.Errorf("gqlHasMutation(%q) = %v, %v; want %v", tc.doc, got, err, tc.want)
		}
	}
	for _, doc := range []string{`{ a(s: "x) }`, `{ a(s: """x) }`, `{ a(s: "\`} {
		if _, err := gqlHasMutation(doc); err == nil {
			t.Errorf("gqlHasMutation(%q): want error for unterminated string", doc)
		}
	}
}
