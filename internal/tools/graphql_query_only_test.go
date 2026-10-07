package tools

import (
	"context"
	"encoding/json"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"
)

// RVG-175: the review profile only runs query-only GraphQL documents. The scanner
// is an adversarial surface, so it is attacked with a table of obfuscations, a
// generative test with known ground truth and a fuzz target (regression seeds in
// testdata/fuzz/FuzzGQLQueryOnly).

// queryOnlyCases: doc -> accepted. Every reject is a shape GitLab's parser could
// still execute as a mutation (or one the allow-list cannot judge).
var queryOnlyCases = []struct {
	name string
	doc  string
	want bool
}{
	// Plain queries stay usable.
	{"anonymous", `{ currentUser { username } }`, true},
	{"query keyword", `query { a }`, true},
	{"named with variables", `query Q($v: ID!, $n: Int = 3) { a(id: $v, first: $n) }`, true},
	{"directive with object default", `query Q($a: In = {k: "}"}) @include(if: true) { a @skip(if: false) }`, true},
	{"fragment then query", `fragment F on T { a } query { ...F }`, true},
	{"query then fragment", `query { ...F } fragment F on T { a }`, true},
	{"two anonymous", `{ a } { b }`, true},
	{"numbers", `{ a(f: -1.5e+3, i: 0, l: [1, 2]) }`, true},
	{"commas as whitespace", `{,a,,b,}`, true},
	{"CRLF", "query\r\n{\r\n a\r\n}\r\n", true},
	{"tab", "query\t{\ta\t}", true},
	{"comment between keyword and set", "query#c\n{ a }", true},
	{"trailing comment", `{ a } # mutation { b }`, true},
	{"comment ended by lone CR", "{ a } #c\r{ b }", true},
	{"comment with braces and quotes", "# } { \" \"\"\" #\n{ a }", true},
	{"non-ASCII inside string and comment", "{ a(s: \"\u00e9\u200b\") } # \u00e9\u00a0", true},
	// The word "mutation" below the top level or as a name is a query (documented).
	{"field named mutation", `{ mutation { id } }`, true},
	{"alias named mutation", `{ mutation: a }`, true},
	{"argument named mutation", `{ a(mutation: 1) }`, true},
	{"operation named mutation", `query mutation { a }`, true},
	{"fragment named mutation", `fragment mutation on Mutation { a } { ...mutation }`, true},
	{"directive named mutation", `query @mutation(x: {mutation: 1}) { a }`, true},
	{"operation named mutationX", `{ a } query mutationX { b }`, true},
	{"word in string", `{ a(s: "} mutation {") }`, true},
	{"word in escaped-quote string", `{ a(s: "\" mutation { b }") }`, true},
	{"word in block string", `{ a(s: """ " \""" } mutation { """) }`, true},
	{"word in comment", "# mutation { a }\n{ a }", true},
	{"nesting 64", "query " + strings.Repeat("{ a ", 64) + strings.Repeat("}", 64), true},

	// Mutations in every plain position.
	{"mutation", `mutation { a }`, false},
	{"mutation compact", `mutation{a}`, false},
	{"mutation named with vars", `  mutation Named($a: [Int] = [1]) { a }`, false},
	{"after query", `query A { a } mutation B { b }`, false},
	{"after anonymous", `{ a } mutation { b }`, false},
	{"after fragment", `fragment F on T { a } mutation { ...F }`, false},
	{"after fragment and query", `fragment F on T { a } query { ...F } mutation { b }`, false},
	{"after newline", "{ a }\nmutation { b }", false},
	{"after string arg", `{ a(s: "x") } mutation { b }`, false},
	{"after block string arg", `{ a(s: """x""") } mutation { b }`, false},
	{"after escaped backslash string", `{ a(s: "\\") } mutation { b }`, false},
	{"after object default", `query A($v: In = {a: [1, {b: 2}]}) { a } mutation { b }`, false},
	{"with directives", `mutation M($x: In = {k: "v"}) @d(a: 1) { b }`, false},
	{"operationName style multi-op", `query A { a } query B { b } mutation C { c }`, false},
	{"mutation first, query last", `mutation M { a } query Q { b }`, false},
	// Comments and whitespace around the keyword.
	{"leading comment", "# c\nmutation{a}", false},
	{"leading comment with closer", "# } {\nmutation { a }", false},
	{"leading comment with quote", "#\"\nmutation { a }", false},
	{"leading comment with triple quote", "#\"\"\"\nmutation { a }", false},
	{"comment ended by lone CR", "# x\rmutation { a }", false},
	{"comment ended by CRLF", "# x\r\nmutation { a }", false},
	{"keyword then comment", "mutation#c\n{ a }", false},
	{"keyword then comma", `mutation,{ a }`, false},
	{"keyword then CRLF", "mutation\r\n{ a }", false},
	{"keyword then tab", "mutation\t{ a }", false},
	{"keyword split by comment", "mut#c\nation { a }", false},
	{"keyword split by comma", `mut,ation { a }`, false},
	{"keyword case", `Mutation { a }`, false},
	// Unicode and control characters GitLab might treat as ignorable (reject all).
	{"BOM then mutation", "\ufeffmutation { a }", false},
	{"BOM then query", "\ufeff{ a }", false},
	{"NBSP before", "\u00a0mutation { a }", false},
	{"NBSP after keyword", "mutation\u00a0{ a }", false},
	{"zero width space before", "\u200bmutation { a }", false},
	{"zero width space inside keyword", "mut\u200bation { a }", false},
	{"zero width joiner after keyword", "mutation\u200d{ a }", false},
	{"line separator", "{ a }\u2028mutation { b }", false},
	{"paragraph separator", "{ a }\u2029mutation { b }", false},
	{"NEL", "{ a }\u0085mutation { b }", false},
	{"form feed", "\fmutation { a }", false},
	{"vertical tab", "\vmutation { a }", false},
	{"NUL", "\x00mutation { a }", false},
	{"NUL after query", "{ a }\x00", false},
	{"invalid UTF-8 outside", "\xffmutation { a }", false},
	{"invalid UTF-8 inside string", "{ a(s: \"\xff\") }", false},
	{"fullwidth letters", "\uff4dutation { a }", false},
	// Other operations and definitions are not on the allow-list.
	{"subscription", `subscription { a }`, false},
	{"query then subscription", `query { a } subscription { b }`, false},
	{"extend", `extend type Query { a: Int }`, false},
	{"schema", `schema { query: Q }`, false},
	{"type", `type Mutation { a: Int }`, false},
	{"directive definition", `directive @d on FIELD`, false},
	{"description before mutation", `"""d""" mutation { a }`, false},
	{"string before query", `"d" query { a }`, false},
	{"unknown word", `queryx { a }`, false},
	// Stray tokens and brackets.
	{"leading closer", `} mutation { a }`, false},
	{"extra closer", `{ a } } mutation { b }`, false},
	{"extra paren", `{ a } ) mutation { b }`, false},
	{"extra closer at end", `{ a }}`, false},
	{"mismatched", `{ a ] }`, false},
	{"mismatched paren", `{ a( }`, false},
	{"mismatched but balanced", `{ a[ ) }`, false},
	{"closer swallows a definition", `query { a( } mutation { b } ) { c }`, false},
	{"paren as definition", `(a) { b }`, false},
	{"bracket as definition", `[ ]`, false},
	{"directive as definition", `@d { a }`, false},
	{"number as definition", `1 { a }`, false},
	{"spread as definition", `... { a }`, false},
	{"string in head", `query "s" { a }`, false},
	{"semicolon", `{ a }; mutation { b }`, false},
	// Unterminated constructs.
	{"unterminated string", `{ a(s: "x) }`, false},
	{"unterminated block string", `{ a(s: """x) }`, false},
	{"unterminated escape", `{ a(s: "\`, false},
	{"block string escape swallows closer", `{ a(s: """\""") } mutation { b }`, false},
	{"unclosed brace", `{ a `, false},
	{"unclosed after def", `{ a } {`, false},
	{"unclosed paren", `query Q($a: Int { a }`, false},
	{"query without set", `query Q`, false},
	{"bare keyword", `query`, false},
	{"fragment without set", `fragment F on T`, false},
	{"query head only", `query Q($a: Int) `, false},
	{"string with raw LF", "{ a(s: \"x\ny\") }", false},
	{"string with raw CR", "{ a(s: \"x\ry\") }", false},
	{"string with backslash LF", "{ a(s: \"x\\\ny\") } mutation { b }", false},
	{"string with backslash CR", "{ a(s: \"x\\\ry\") } mutation { b }", false},
	// No definition at all.
	{"empty", ``, false},
	{"spaces", "  \n\t ", false},
	{"comment only", `# {a}`, false},
	{"commas only", `,,,`, false},
	// Bounds.
	{"nesting 65", "query " + strings.Repeat("{ a ", 65) + strings.Repeat("}", 65), false},
	{"oversized", strings.Repeat("# x\n", 70000) + "{ a }", false},
}

func TestGQLQueryOnly(t *testing.T) {
	for _, tc := range queryOnlyCases {
		if got := gqlQueryOnly(tc.doc); got != tc.want {
			t.Errorf("%s: gqlQueryOnly(%q) = %v, want %v", tc.name, trimDoc(tc.doc), got, tc.want)
		}
	}
	// A document at the size limit is judged, one byte over is refused.
	pad := strings.Repeat(" ", gqlMaxDocBytes-len("{ a }"))
	if !gqlQueryOnly("{ a }"+pad) || gqlQueryOnly("{ a }"+pad+" ") {
		t.Error("size limit is not exactly gqlMaxDocBytes")
	}
}

func trimDoc(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// Every accepted shape is also a document without a mutation for the (older)
// deny-list scanner; the two must never disagree in the dangerous direction.
func TestGQLQueryOnly_agreesWithDenyList(t *testing.T) {
	for _, tc := range queryOnlyCases {
		if !tc.want {
			continue
		}
		if m, err := gqlHasMutation(tc.doc); m || err != nil {
			t.Errorf("%s: accepted by the allow-list but gqlHasMutation = %v, %v", tc.name, m, err)
		}
	}
}

// Generative test with known ground truth: documents are built from query-only
// and mutation definitions joined by random separators that every GraphQL lexer
// ignores (white space, commas, comments full of braces and quotes). A document
// with a mutation must be refused; one without must be accepted.
func TestGQLQueryOnly_generative(t *testing.T) {
	queries := []string{
		`{ a }`, `query { a }`, `query Q($v: In = {k: "}"}) @d(x: "{") { a(s: """} \""" {""") }`,
		`fragment F on T { a }`, `{ mutation { id } }`, `query mutation { a }`, `query Q { a(s: "mutation { b }") }`,
		`query Q($a: [Int] = [1, {b: 2}]) { a }`,
	}
	mutations := []string{`mutation { a }`, `mutation M($x: In = {k: "v"}) @d { a }`, `mutation{a}`, `mutation Q($a: Int) { a(s: "}") }`}
	seps := []string{" ", "\t", "\n", "\r", "\r\n", ",", "#\n", "# } { \" \"\"\" #\n", "#\"\r", "# mutation { a }\n", "# {\r\n", ", ,"}
	odd := []string{"\ufeff", "\u00a0", "\u200b", "\u2028", "\f", "\x00"}
	rng := rand.New(rand.NewSource(175))
	pick := func(xs []string) string { return xs[rng.Intn(len(xs))] }
	for i := 0; i < 20000; i++ {
		var sb strings.Builder
		hasMutation := false
		sb.WriteString(pick(seps))
		for n := rng.Intn(4) + 1; n > 0; n-- {
			def := pick(queries)
			if rng.Intn(3) == 0 {
				def, hasMutation = pick(mutations), true
			}
			if strings.HasPrefix(def, "mutation") && rng.Intn(2) == 0 {
				def = "mutation" + pick(seps) + strings.TrimPrefix(def, "mutation") // separator after the keyword
			}
			sb.WriteString(def)
			sb.WriteString(pick(seps))
		}
		doc, withOdd := sb.String(), false
		if rng.Intn(4) == 0 { // characters outside GraphQL's ignored set: any verdict but accept-a-mutation is fine
			doc, withOdd = pick(odd)+doc+pick(odd), true
		}
		got := gqlQueryOnly(doc)
		if hasMutation && got {
			t.Fatalf("mutation document accepted: %q", doc)
		}
		if !hasMutation && !withOdd && !got {
			t.Fatalf("query-only document refused: %q", doc)
		}
	}
}

func FuzzGQLQueryOnly(f *testing.F) {
	for _, tc := range queryOnlyCases {
		f.Add(tc.doc)
	}
	f.Fuzz(func(t *testing.T, doc string) {
		if !gqlQueryOnly(doc) {
			return
		}
		if m, err := gqlHasMutation(doc); m || err != nil {
			t.Fatalf("accepted but the deny-list scanner disagrees (%v, %v): %q", m, err, doc)
		}
		if gqlQueryOnly("mutation{a}\n"+doc) || gqlQueryOnly(doc+"\nmutation{a}") || gqlQueryOnly(doc+"\r\nsubscription{a}") {
			t.Fatalf("a definition added next to an accepted document was accepted: %q", doc)
		}
		if !gqlQueryOnly("{a}\n" + doc) {
			t.Fatalf("a query in front of an accepted document changed the verdict: %q", doc)
		}
	})
}

// gqlProfileSession serves reply as the body of POST /api/graphql and returns an
// MCP session with the GraphQL tools for cfg plus a counter of upstream requests.
func gqlProfileSession(t *testing.T, cfg *config.Config, reply string) (*mcp.ClientSession, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	cli, _ := testutil.NewGitLabClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		n.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		writeFixture(w, reply)
	}))
	srv := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "test"}, nil)
	RegisterGraphQLTools(srv, Deps{Config: cfg, Client: cli})
	return testutil.MCPConnect(t, srv), &n
}

func TestExecuteGraphQL_reviewProfileQueryOnly(t *testing.T) {
	const want = "graphql_mutations_not_permitted: this profile allows queries only"
	mutations := []string{
		`mutation { a }`,
		`mutation M($x: In = {k: "v"}) @d { b }`,
		`query Q { a } mutation M { b }`,
		`{ a } mutation { b }`,
		"# } {\nmutation { a }",
		"\ufeffmutation { a }",
		"mutation\u200b{ a }",
		"mut#c\nation { a }",
		`subscription { a }`,
		`{ a(s: "x) } mutation { b }`,
		`mutation { createNote(input: {noteableId: "gid://gitlab/MergeRequest/1", body: "/merge"}) { errors } }`,
		``,
	}
	for _, readOnly := range []bool{false, true} {
		cs, n := gqlProfileSession(t, &config.Config{ToolProfile: config.ProfileReview, ReadOnly: readOnly}, `{"data":{"ok":true}}`)
		for _, q := range mutations {
			res := gqlCall(t, cs, map[string]any{"query": q, "variables": map[string]any{"a": 1}})
			if !res.IsError || contentText(res) != want {
				t.Fatalf("readOnly=%v %q: IsError=%v text=%q", readOnly, q, res.IsError, contentText(res))
			}
		}
		if n.Load() != 0 {
			t.Fatalf("readOnly=%v: %d upstream requests for refused documents", readOnly, n.Load())
		}
		// Queries run, even when they merely mention the word.
		for i, q := range []string{`{ currentUser { username } }`, "# mutation\nquery { a(s: \"mutation\") { mutation } }", `query Q($p: ID!) { project(fullPath: $p) { id } }`} {
			if res := gqlCall(t, cs, map[string]any{"query": q, "variables": map[string]any{"p": "g/p"}}); res.IsError {
				t.Fatalf("readOnly=%v: query refused: %q: %s", readOnly, q, contentText(res))
			}
			if int(n.Load()) != i+1 {
				t.Fatalf("readOnly=%v: requests = %d, want %d", readOnly, n.Load(), i+1)
			}
		}
	}
}

// Other profiles keep today's behaviour: writable passes a mutation, read-only
// refuses it with the read-only message, whatever the profile name is.
func TestExecuteGraphQL_otherProfilesUnchanged(t *testing.T) {
	for name, cfg := range map[string]config.Config{"default": {}, "daily": {UseDailyTools: true}} {
		cs, n := gqlProfileSession(t, &cfg, `{"data":{"ok":true}}`)
		if res := gqlCall(t, cs, map[string]any{"query": "subscription { a }"}); res.IsError || n.Load() != 1 {
			t.Fatalf("%s: writable subscription: IsError=%v requests=%d", name, res.IsError, n.Load())
		}
		if res := gqlCall(t, cs, map[string]any{"query": "mutation { a }"}); res.IsError || n.Load() != 2 {
			t.Fatalf("%s: writable mutation: IsError=%v requests=%d", name, res.IsError, n.Load())
		}
		ro := cfg
		ro.ReadOnly = true
		cs, n = gqlProfileSession(t, &ro, `{"data":{"ok":true}}`)
		res := gqlCall(t, cs, map[string]any{"query": "mutation { a }"})
		if !res.IsError || !strings.Contains(contentText(res), "GraphQL mutations are rejected in read-only mode") || n.Load() != 0 {
			t.Fatalf("%s: read-only mutation: IsError=%v text=%q requests=%d", name, res.IsError, contentText(res), n.Load())
		}
	}
}

// The input is exactly {query, variables} and the request body has no
// operationName / extensions: no persisted-query path can run a stored mutation.
func TestExecuteGraphQL_reviewInputSurface(t *testing.T) {
	cs, _ := gqlProfileSession(t, &config.Config{ToolProfile: config.ProfileReview}, `{"data":{}}`)
	tl, err := cs.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil || len(tl.Tools) != 1 {
		t.Fatalf("tools: %v %d", err, len(tl.Tools))
	}
	raw, _ := json.Marshal(tl.Tools[0].InputSchema)
	var sc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	_ = json.Unmarshal(raw, &sc)
	if len(sc.Properties) != 2 || sc.Properties["query"] == nil || sc.Properties["variables"] == nil {
		t.Fatalf("input properties = %s", raw)
	}
	if d := tl.Tools[0].Description; !strings.Contains(d, "allows queries only") || strings.Contains(d, "mutation") {
		t.Fatalf("review description = %q", d)
	}
	// Extra fields (operationName, extensions) are not part of the schema and are not accepted.
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "execute_graphql", Arguments: map[string]any{"query": "{ a }", "extensions": map[string]any{"persistedQuery": map[string]any{"sha256Hash": "x"}}}})
	if err == nil && !res.IsError {
		t.Fatal("unknown field extensions accepted")
	}
}
