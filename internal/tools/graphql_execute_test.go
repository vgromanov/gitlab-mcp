package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/vektah/gqlparser/v2/ast"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	glclient "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

func TestSelectGraphQLOperation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		doc      string
		opName   string
		want     string
		wantKind ast.Operation
		wantErr  string
	}{
		{
			name:     "single anonymous query",
			doc:      `{ __typename }`,
			wantKind: ast.Query,
		},
		{
			name:     "single named query without operation_name",
			doc:      `query Q { __typename }`,
			want:     "Q",
			wantKind: ast.Query,
		},
		{
			name:     "multi select named query",
			doc:      `query Q { __typename } mutation M { __typename }`,
			opName:   "Q",
			want:     "Q",
			wantKind: ast.Query,
		},
		{
			name:     "multi select named mutation",
			doc:      `query Q { __typename } mutation M { __typename }`,
			opName:   "M",
			want:     "M",
			wantKind: ast.Mutation,
		},
		{
			name:    "multi missing operation_name",
			doc:     `query Q { __typename } mutation M { __typename }`,
			wantErr: "operation_name is required",
		},
		{
			name:    "anonymous with named multi rejected even with named selection",
			doc:     `{ ok } query Named { ok }`,
			opName:  "Named",
			wantErr: "anonymous GraphQL operations are only allowed",
		},
		{
			name:    "two anonymous ops rejected",
			doc:     `{ a } { b }`,
			wantErr: "anonymous GraphQL operations are only allowed",
		},
		{
			name:    "missing selected name",
			doc:     `query Q { __typename } mutation M { __typename }`,
			opName:  "Nope",
			wantErr: "not found",
		},
		{
			name:    "duplicate operation names",
			doc:     `query Q { __typename } query Q { a }`,
			opName:  "Q",
			wantErr: "duplicate",
		},
		{
			name:    "subscription rejected",
			doc:     `subscription S { __typename }`,
			wantErr: "subscriptions are not supported",
		},
		{
			name:    "malformed",
			doc:     `query {`,
			wantErr: "malformed",
		},
		{
			name:     "comments and string decoys do not confuse parser",
			doc:      "query Q { # mutation Fake { x }\n  field(arg: \"mutation M { y }\") }\nfragment F on T { z }",
			want:     "Q",
			wantKind: ast.Query,
		},
		{
			name:     "fragment sibling with one operation",
			doc:      `query Only { ...F } fragment F on T { id }`,
			want:     "Only",
			wantKind: ast.Query,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := selectGraphQLOperation(tc.doc, tc.opName)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err=%v want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tc.want || got.Kind != tc.wantKind {
				t.Fatalf("got %+v want name=%q kind=%q", got, tc.want, tc.wantKind)
			}
		})
	}
}

func TestGraphQLVariablesObject_Unmarshal(t *testing.T) {
	t.Parallel()
	var v graphqlVariablesObject
	if err := json.Unmarshal([]byte(`null`), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.asMap()) != 0 {
		t.Fatalf("null -> empty, got %#v", v)
	}
	if err := json.Unmarshal([]byte(`{"a":1}`), &v); err != nil {
		t.Fatal(err)
	}
	if v["a"] != float64(1) {
		t.Fatalf("object decode: %#v", v)
	}
	for _, raw := range []string{`[]`, `"x"`, `1`, `true`} {
		var bad graphqlVariablesObject
		if err := json.Unmarshal([]byte(raw), &bad); err == nil {
			t.Fatalf("expected reject for %s", raw)
		}
	}
}

func TestTopLevelAndPayloadGraphQLErrors(t *testing.T) {
	t.Parallel()
	if err := topLevelGraphQLErrors(map[string]any{"data": map[string]any{"ok": true}}); err != nil {
		t.Fatalf("absent errors key: %v", err)
	}
	if err := topLevelGraphQLErrors(map[string]any{"data": map[string]any{"ok": true}, "errors": []any{}}); err != nil {
		t.Fatalf("empty errors: %v", err)
	}
	if err := topLevelGraphQLErrors(map[string]any{
		"data":   map[string]any{"ok": true},
		"errors": nil,
	}); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("want present null fail-closed, got %v", err)
	}
	if err := topLevelGraphQLErrors(map[string]any{
		"data":   map[string]any{"ok": true},
		"errors": []any{map[string]any{"message": "boom"}},
	}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want top-level error, got %v", err)
	}
	if err := topLevelGraphQLErrors(map[string]any{
		"data":   map[string]any{"ok": true},
		"errors": "not-an-array",
	}); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("want malformed top-level errors, got %v", err)
	}
	if err := topLevelGraphQLErrors(map[string]any{
		"data":   map[string]any{"ok": true},
		"errors": map[string]any{"message": "obj"},
	}); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("want malformed object errors, got %v", err)
	}
	// Nested decoy must not trip top-level checker.
	if err := topLevelGraphQLErrors(map[string]any{
		"data": map[string]any{"user": map[string]any{"errors": []any{"nested"}}},
	}); err != nil {
		t.Fatalf("nested decoy: %v", err)
	}
	// Nested malformed user "errors" must also be ignored at top-level.
	if err := topLevelGraphQLErrors(map[string]any{
		"data": map[string]any{"user": map[string]any{"errors": "nested-malformed"}},
	}); err != nil {
		t.Fatalf("nested malformed decoy: %v", err)
	}
	// Nested present-null user "errors" must also be ignored at top-level.
	if err := topLevelGraphQLErrors(map[string]any{
		"data": map[string]any{"user": map[string]any{"errors": nil}},
	}); err != nil {
		t.Fatalf("nested null decoy: %v", err)
	}
	fields := []string{
		"createWorkItem", "workItemUpdate", "workItemConvert",
		"workItemMove", "workItemNoteCreate", "timelineEventCreate",
	}
	for _, field := range fields {
		absent := map[string]any{"data": map[string]any{field: map[string]any{"workItem": map[string]any{"id": "1"}}}}
		if err := knownMutationPayloadErrors(absent, field); err != nil {
			t.Fatalf("%s absent: %v", field, err)
		}
		okBody := map[string]any{"data": map[string]any{field: map[string]any{"errors": []any{}}}}
		if err := knownMutationPayloadErrors(okBody, field); err != nil {
			t.Fatalf("%s empty: %v", field, err)
		}
		nullBody := map[string]any{"data": map[string]any{field: map[string]any{"errors": nil}}}
		if err := knownMutationPayloadErrors(nullBody, field); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("%s present null: %v", field, err)
		}
		badBody := map[string]any{"data": map[string]any{field: map[string]any{"errors": []any{"nope"}}}}
		if err := knownMutationPayloadErrors(badBody, field); err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("%s payload: %v", field, err)
		}
		malformed := map[string]any{"data": map[string]any{field: map[string]any{"errors": "bad"}}}
		if err := knownMutationPayloadErrors(malformed, field); err == nil || !strings.Contains(err.Error(), "malformed") {
			t.Fatalf("%s malformed payload: %v", field, err)
		}
		decoy := map[string]any{"data": map[string]any{
			field:  map[string]any{"errors": []any{}},
			"user": map[string]any{"errors": []any{"ignore-me"}},
		}}
		if err := knownMutationPayloadErrors(decoy, field); err != nil {
			t.Fatalf("%s decoy: %v", field, err)
		}
	}
}

type gqlCapture struct {
	attempts atomic.Int64
	lastBody string
	lastPath string
	handler  http.HandlerFunc
}

func (c *gqlCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.attempts.Add(1)
	c.lastPath = r.URL.Path
	b, _ := io.ReadAll(r.Body)
	c.lastBody = string(b)
	if c.handler != nil {
		c.handler(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"data":{"__typename":"Query"},"errors":[]}`)
}

func newGraphQLDeps(t *testing.T, cfg *config.Config, cap *gqlCapture) Deps {
	t.Helper()
	ts := httptest.NewServer(cap)
	t.Cleanup(ts.Close)
	if cfg == nil {
		cfg = &config.Config{Token: "fake-token-not-live"}
	}
	cfg.Token = "fake-token-not-live"
	cfg.APIURL = ts.URL + "/api/v4"
	cli, err := glclient.NewGuardedClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return Deps{Config: cfg, Client: cli, Guarded: cli}
}

func TestExecuteGraphQL_wireOperationNameAndMixedSelection(t *testing.T) {
	cap := &gqlCapture{}
	d := newGraphQLDeps(t, &config.Config{}, cap)
	doc := `query GetT { __typename } mutation DoIt { __typename }`
	_, _, err := executeGraphQL(context.Background(), nil, executeGraphQLIn{
		Query:         doc,
		OperationName: "GetT",
		Variables:     &graphqlVariablesObject{"x": "y"},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if cap.attempts.Load() != 1 {
		t.Fatalf("attempts=%d", cap.attempts.Load())
	}
	if cap.lastPath != "/api/graphql" {
		t.Fatalf("path=%q", cap.lastPath)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(cap.lastBody), &body); err != nil {
		t.Fatal(err)
	}
	if body["operationName"] != "GetT" {
		t.Fatalf("body=%s", cap.lastBody)
	}
	if body["query"] != doc {
		t.Fatalf("query not preserved")
	}
	vars, _ := body["variables"].(map[string]any)
	if vars["x"] != "y" {
		t.Fatalf("variables=%v", vars)
	}
}

func TestExecuteGraphQL_zeroHTTPGuards(t *testing.T) {
	cap := &gqlCapture{}
	ro := newGraphQLDeps(t, &config.Config{ReadOnly: true}, cap)
	_, _, err := executeGraphQL(context.Background(), nil, executeGraphQLIn{
		Query: `mutation M { __typename }`,
	}, ro)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("want read-only deny, got %v", err)
	}
	if cap.attempts.Load() != 0 {
		t.Fatalf("read-only must not dial, attempts=%d", cap.attempts.Load())
	}

	cap2 := &gqlCapture{}
	pol := newGraphQLDeps(t, &config.Config{AllowedProjectIDs: []string{"1"}}, cap2)
	_, _, err = executeGraphQL(context.Background(), nil, executeGraphQLIn{Query: `{ __typename }`}, pol)
	if err == nil || !strings.Contains(err.Error(), "allowlists") {
		t.Fatalf("want policy deny, got %v", err)
	}
	if cap2.attempts.Load() != 0 {
		t.Fatalf("policy must not dial, attempts=%d", cap2.attempts.Load())
	}

	cap3 := &gqlCapture{}
	d := newGraphQLDeps(t, &config.Config{}, cap3)
	_, _, err = executeGraphQL(context.Background(), nil, executeGraphQLIn{
		Query: `query A { __typename } query B { __typename }`,
	}, d)
	if err == nil {
		t.Fatal("expected multi-op ambiguity error")
	}
	if cap3.attempts.Load() != 0 {
		t.Fatalf("ambiguous multi-op must not dial")
	}

	cap4 := &gqlCapture{}
	mixed := newGraphQLDeps(t, &config.Config{}, cap4)
	_, _, err = executeGraphQL(context.Background(), nil, executeGraphQLIn{
		Query:         `{ ok } query Named { ok }`,
		OperationName: "Named",
	}, mixed)
	if err == nil || !strings.Contains(err.Error(), "anonymous") {
		t.Fatalf("want anonymous/multi reject, got %v", err)
	}
	if cap4.attempts.Load() != 0 {
		t.Fatalf("mixed anonymous+named must not dial even with named selection")
	}
}

func TestExecuteGraphQL_topLevelHTTP200Errors(t *testing.T) {
	cap := &gqlCapture{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"__typename":"Query"},"errors":[{"message":"partial-fail"}]}`)
	}}
	d := newGraphQLDeps(t, &config.Config{}, cap)
	_, _, err := executeGraphQL(context.Background(), nil, executeGraphQLIn{Query: `{ __typename }`}, d)
	if err == nil || !strings.Contains(err.Error(), "partial-fail") {
		t.Fatalf("want tool error, got %v", err)
	}
	if cap.attempts.Load() != 1 {
		t.Fatalf("attempts=%d", cap.attempts.Load())
	}
}

func TestExecuteGraphQL_MCPTopLevelErrorsPresentNullVsAbsent(t *testing.T) {
	// Present JSON null with partial data must fail closed (AC4 review finding).
	nullCap := &gqlCapture{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errors":null,"data":{"__typename":"Query","ok":true}}`)
	}}
	nullDeps := newGraphQLDeps(t, &config.Config{}, nullCap)
	srvNull := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterGraphQLTools(srvNull, nullDeps)
	csNull := testutil.MCPConnect(t, srvNull)
	res, err := csNull.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute_graphql",
		Arguments: map[string]any{"query": `{ __typename }`},
	})
	if err != nil {
		t.Fatalf("present-null protocol: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("present-null must be tool error, got %+v", res)
	}
	if nullCap.attempts.Load() != 1 {
		t.Fatalf("present-null attempts=%d", nullCap.attempts.Load())
	}
	// Direct handler path must match MCP overlay.
	_, _, herr := executeGraphQL(context.Background(), nil, executeGraphQLIn{Query: `{ __typename }`}, nullDeps)
	if herr == nil || !strings.Contains(herr.Error(), "malformed") {
		t.Fatalf("present-null direct want malformed, got %v", herr)
	}

	// Absent errors key with data is success.
	absentCap := &gqlCapture{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"__typename":"Query"}}`)
	}}
	absentDeps := newGraphQLDeps(t, &config.Config{}, absentCap)
	srvAbsent := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterGraphQLTools(srvAbsent, absentDeps)
	csAbsent := testutil.MCPConnect(t, srvAbsent)
	res, err = csAbsent.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute_graphql",
		Arguments: map[string]any{"query": `{ __typename }`},
	})
	if err != nil {
		t.Fatalf("absent protocol: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("absent errors key must succeed, got %+v", res)
	}
	if absentCap.attempts.Load() != 1 {
		t.Fatalf("absent attempts=%d", absentCap.attempts.Load())
	}

	// Empty errors list remains success.
	emptyCap := &gqlCapture{handler: func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"__typename":"Query"},"errors":[]}`)
	}}
	emptyDeps := newGraphQLDeps(t, &config.Config{}, emptyCap)
	srvEmpty := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterGraphQLTools(srvEmpty, emptyDeps)
	csEmpty := testutil.MCPConnect(t, srvEmpty)
	res, err = csEmpty.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "execute_graphql",
		Arguments: map[string]any{"query": `{ __typename }`},
	})
	if err != nil {
		t.Fatalf("empty-list protocol: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("errors:[] must succeed, got %+v", res)
	}
	if emptyCap.attempts.Load() != 1 {
		t.Fatalf("empty-list attempts=%d", emptyCap.attempts.Load())
	}
}

func TestExecuteGraphQL_dailyMutationCompatible(t *testing.T) {
	cap := &gqlCapture{}
	d := newGraphQLDeps(t, &config.Config{}, cap)
	_, _, err := executeGraphQL(context.Background(), nil, executeGraphQLIn{
		Query: `mutation DoIt { __typename }`,
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	if cap.attempts.Load() != 1 {
		t.Fatalf("attempts=%d", cap.attempts.Load())
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(cap.lastBody), &body)
	if body["operationName"] != "DoIt" {
		t.Fatalf("body=%s", cap.lastBody)
	}
}

func TestRegisterGraphQLTools_policySkipsExecuteGraphQL(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	cfg := &config.Config{Token: "x", AllowedProjectIDs: []string{"42"}}
	RegisterGraphQLTools(srv, Deps{Config: cfg})
	cs := testutil.MCPConnect(t, srv)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "execute_graphql" {
			t.Fatal("execute_graphql must not register under PolicyActive")
		}
	}
}

func TestExecuteGraphQL_MCPSchemaVariablesObjectAndCallTool(t *testing.T) {
	cap := &gqlCapture{}
	cfg := &config.Config{Token: "fake-token-not-live"}
	d := newGraphQLDeps(t, cfg, cap)
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "t"}, nil)
	RegisterGraphQLTools(srv, d)
	cs := testutil.MCPConnect(t, srv)

	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var schema any
	for _, tool := range tools.Tools {
		if tool.Name == "execute_graphql" {
			schema = tool.InputSchema
			break
		}
	}
	if schema == nil {
		t.Fatal("missing execute_graphql")
	}
	raw, _ := json.Marshal(schema)
	var schemaMap map[string]any
	if err := json.Unmarshal(raw, &schemaMap); err != nil {
		t.Fatal(err)
	}
	props, _ := schemaMap["properties"].(map[string]any)
	varsSchema, _ := props["variables"].(map[string]any)
	typ, ok := varsSchema["type"].([]any)
	if !ok {
		// Some encoders may emit a single string; require null|object union.
		if varsSchema["type"] == "object" {
			t.Fatalf("variables schema must allow null|object, got object-only: %s", raw)
		}
		t.Fatalf("variables schema want [null,object], got %s", raw)
	}
	gotTypes := map[string]bool{}
	for _, x := range typ {
		gotTypes[fmt.Sprint(x)] = true
	}
	if !gotTypes["null"] || !gotTypes["object"] || len(typ) != 2 {
		t.Fatalf("variables schema want null|object, got %#v from %s", typ, raw)
	}

	callOK := func(name string, args map[string]any, wantVars map[string]any) {
		t.Helper()
		before := cap.attempts.Load()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "execute_graphql",
			Arguments: args,
		})
		if err != nil {
			t.Fatalf("%s protocol: %v", name, err)
		}
		if res != nil && res.IsError {
			t.Fatalf("%s unexpected tool error: %+v", name, res)
		}
		if cap.attempts.Load()-before != 1 {
			t.Fatalf("%s want exactly one HTTP attempt, before=%d after=%d", name, before, cap.attempts.Load())
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(cap.lastBody), &body); err != nil {
			t.Fatalf("%s wire json: %v body=%s", name, err, cap.lastBody)
		}
		rawVars, hasVars := body["variables"]
		if wantVars == nil || len(wantVars) == 0 {
			if hasVars {
				got, _ := rawVars.(map[string]any)
				if len(got) != 0 {
					t.Fatalf("%s want empty/omitted variables, got %#v body=%s", name, rawVars, cap.lastBody)
				}
			}
			return
		}
		got, _ := rawVars.(map[string]any)
		if len(got) != len(wantVars) {
			t.Fatalf("%s variables=%#v want %#v", name, got, wantVars)
		}
		for k, v := range wantVars {
			if fmt.Sprint(got[k]) != fmt.Sprint(v) {
				t.Fatalf("%s variables[%s]=%#v want %#v body=%s", name, k, got[k], v, cap.lastBody)
			}
		}
	}
	callReject := func(name string, args map[string]any) {
		t.Helper()
		before := cap.attempts.Load()
		res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "execute_graphql",
			Arguments: args,
		})
		if err == nil && (res == nil || !res.IsError) {
			t.Fatalf("%s expected variables rejection", name)
		}
		if cap.attempts.Load() != before {
			t.Fatalf("%s rejected variables must not dial (before=%d after=%d)", name, before, cap.attempts.Load())
		}
	}

	callOK("omit", map[string]any{"query": `{ __typename }`}, nil)
	callOK("null", map[string]any{"query": `{ __typename }`, "variables": nil}, nil)
	callOK("object", map[string]any{
		"query":          `query GetT { __typename } mutation DoIt { __typename }`,
		"operation_name": "GetT",
		"variables":      map[string]any{"k": 1},
	}, map[string]any{"k": 1})

	callReject("array", map[string]any{"query": `{ __typename }`, "variables": []any{"nope"}})
	callReject("string", map[string]any{"query": `{ __typename }`, "variables": "nope"})
	callReject("number", map[string]any{"query": `{ __typename }`, "variables": 1})
	callReject("bool", map[string]any{"query": `{ __typename }`, "variables": true})
}

func TestKnownMutationHandlers_payloadErrors(t *testing.T) {
	fields := []struct {
		field string
		call  func(context.Context, Deps) error
	}{
		{"createWorkItem", func(ctx context.Context, d Deps) error {
			_, _, err := createWorkItem(ctx, nil, createWorkItemIn{ProjectPath: "g/p", Title: "t", WorkItemTypeID: "gid://1"}, d)
			return err
		}},
		{"workItemUpdate", func(ctx context.Context, d Deps) error {
			_, _, err := updateWorkItem(ctx, nil, updateWorkItemIn{ID: "gid://1", Attributes: json.RawMessage(`{"title":"x"}`)}, d)
			return err
		}},
		{"workItemConvert", func(ctx context.Context, d Deps) error {
			_, _, err := convertWorkItemType(ctx, nil, convertWorkItemTypeIn{ID: "gid://1", WorkItemTypeID: "gid://2"}, d)
			return err
		}},
		{"workItemMove", func(ctx context.Context, d Deps) error {
			_, _, err := moveWorkItem(ctx, nil, moveWorkItemIn{WorkItemID: "gid://1", TargetPath: "g/p2"}, d)
			return err
		}},
		{"workItemNoteCreate", func(ctx context.Context, d Deps) error {
			_, _, err := createWorkItemNote(ctx, nil, createWorkItemNoteIn{ID: "gid://1", Body: "n"}, d)
			return err
		}},
		{"timelineEventCreate", func(ctx context.Context, d Deps) error {
			_, _, err := createTimelineEvent(ctx, nil, createTimelineEventIn{ID: "gid://1", Tag: "t"}, d)
			return err
		}},
	}
	for _, tc := range fields {
		t.Run(tc.field+"/nonempty", func(t *testing.T) {
			cap := &gqlCapture{handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":{"`+tc.field+`":{"errors":["payload-boom"]},"user":{"errors":["decoy"]}}}`)
			}}
			d := newGraphQLDeps(t, &config.Config{}, cap)
			err := tc.call(context.Background(), d)
			if err == nil || !strings.Contains(err.Error(), "payload-boom") {
				t.Fatalf("want payload error, got %v", err)
			}
			if strings.Contains(err.Error(), "decoy") {
				t.Fatalf("must not surface nested decoy: %v", err)
			}
		})
		t.Run(tc.field+"/present_null", func(t *testing.T) {
			cap := &gqlCapture{handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":{"`+tc.field+`":{"errors":null,"ok":true},"user":{"errors":null}}}`)
			}}
			d := newGraphQLDeps(t, &config.Config{}, cap)
			err := tc.call(context.Background(), d)
			if err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("want present-null malformed, got %v", err)
			}
			if strings.Contains(err.Error(), "user") {
				t.Fatalf("must not surface nested null decoy: %v", err)
			}
			if cap.attempts.Load() != 1 {
				t.Fatalf("attempts=%d", cap.attempts.Load())
			}
		})
		t.Run(tc.field+"/absent_and_empty", func(t *testing.T) {
			absentCap := &gqlCapture{handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":{"`+tc.field+`":{"ok":true},"user":{"errors":null}}}`)
			}}
			if err := tc.call(context.Background(), newGraphQLDeps(t, &config.Config{}, absentCap)); err != nil {
				t.Fatalf("absent payload errors must succeed, got %v", err)
			}
			emptyCap := &gqlCapture{handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"data":{"`+tc.field+`":{"errors":[]},"user":{"errors":["decoy"]}}}`)
			}}
			if err := tc.call(context.Background(), newGraphQLDeps(t, &config.Config{}, emptyCap)); err != nil {
				t.Fatalf("errors:[] must succeed, got %v", err)
			}
		})
	}
}

func TestReviewProfiles_excludeExecuteGraphQL(t *testing.T) {
	for _, profile := range []string{"review_read", "review_write"} {
		cfg := &config.Config{
			Token:        "x",
			ToolProfile:  profile,
			EnabledTools: []string{"execute_graphql"},
			Pipeline:     true,
		}
		if ShouldRegister(cfg, "execute_graphql", "") {
			t.Fatalf("%s must not expose execute_graphql", profile)
		}
	}
}
