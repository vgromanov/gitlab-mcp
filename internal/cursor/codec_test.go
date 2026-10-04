package cursor

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, MinKeyBytes)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func basePayload() Payload {
	return Payload{
		SchemaVersion: SchemaV1,
		Instance:      "https://gitlab.example/api/v4",
		ActorID:       42,
		PolicyFP:      "policyfp",
		Tool:          ToolListCommits,
		Section:       SectionListCommits,
		Scope: Scope{
			Kind:      ScopeProject,
			ProjectID: "123",
		},
		Filters: Filters{
			RefName:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Path:        "a.go",
			Since:       "2026-01-01T00:00:00Z",
			CallerUntil: "2026-10-03T11:00:00Z",
			Until:       "2026-10-03T12:00:00Z",
			Order:       "provider_default",
			Selection:   "list_commits",
			PerPage:     20,
		},
		ImmutableRefs: []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		UpperBound:    "2026-10-03T12:00:00Z",
		ExpiresAt:     "2026-10-03T14:00:00Z",
		PageState: PageState{
			Page:             1,
			PerPage:          20,
			SequenceDigest:   SequenceDigest([]string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}),
			LastSHA:          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ItemsOnPage:      1,
			ProviderNextPage: 2,
		},
	}
}

func errorsIsResync(err error) bool {
	return err != nil && (err == ErrResyncRequired || err.Error() == ResyncRequired)
}

func TestEncodeDecode_roundTrip(t *testing.T) {
	key := testKey(t)
	p := basePayload()
	tok, err := Encode(key, p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := Decode(key, tok, time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ActorID != p.ActorID || TipRef(got.ImmutableRefs) != TipRef(p.ImmutableRefs) || got.PageState.LastSHA != p.PageState.LastSHA {
		t.Fatalf("mismatch: %+v", got)
	}
}

func TestDecode_failuresUniformResync(t *testing.T) {
	key := testKey(t)
	clk := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)
	tok, err := Encode(key, basePayload())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		key  []byte
		tok  string
		now  time.Time
	}{
		{"tamper", key, tok[:len(tok)-2] + "zz", clk},
		{"expiry", key, tok, time.Date(2026, 10, 3, 15, 0, 0, 0, time.UTC)},
		{"rotation", testKey(t), tok, clk},
		{"empty", key, "", clk},
		{"oversized", key, "v1." + strings.Repeat("a", MaxEncodedBytes) + ".bb", clk},
		{"bad_version", key, "v2." + strings.Split(tok, ".")[1] + "." + strings.Split(tok, ".")[2], clk},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode(tc.key, tc.tok, tc.now)
			if !errorsIsResync(err) {
				t.Fatalf("want resync_required, got %v", err)
			}
			if strings.Contains(err.Error(), string(key)) {
				t.Fatal("key leaked in error")
			}
		})
	}
}

func TestDecode_strictJSONUnknownAndTrailing(t *testing.T) {
	key := testKey(t)
	p := basePayload()
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	// Inject unknown field
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["extra_unknown"] = "x"
	rawUnk, _ := json.Marshal(m)
	tokUnk := signRaw(t, key, rawUnk)
	if _, err := Decode(key, tokUnk, time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)); !errorsIsResync(err) {
		t.Fatalf("unknown field: %v", err)
	}
	// Trailing content after object
	rawTrail := append(append([]byte{}, raw...), []byte(`{"x":1}`)...)
	tokTrail := signRaw(t, key, rawTrail)
	if _, err := Decode(key, tokTrail, time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)); !errorsIsResync(err) {
		t.Fatalf("trailing: %v", err)
	}
}

func signRaw(t *testing.T, key, raw []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(raw)
	return "v1." + base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func TestScopeBindings(t *testing.T) {
	key := testKey(t)
	now := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)
	mr := int64(12)
	pipe := int64(55)
	scopes := []Scope{
		{Kind: ScopeProject, ProjectID: "1"},
		{Kind: ScopeProject, ProjectID: "1", MergeRequestIID: &mr},
		{Kind: ScopeGroupQueue, GroupID: "9"},
		{Kind: ScopePipeline, ProjectID: "1", PipelineID: &pipe},
	}
	for _, sc := range scopes {
		p := basePayload()
		p.Scope = sc
		if sc.Kind == ScopeProject && sc.MergeRequestIID != nil {
			// Future MR shape: plural immutable refs (base+head)
			p.ImmutableRefs = []string{
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			}
		}
		if sc.Kind == ScopeGroupQueue {
			p.Tool = ToolReviewQueue
			p.Section = SectionReviewQueue
			p.ImmutableRefs = nil
			p.PageState = PageState{}
			p.QueueCont = &QueueCont{
				V:     QueueContSchemaRQ2,
				Phase: "discover",
				Kinds: []string{"reviewer"},
				KP: []QueueKindProg{{
					Kind: "reviewer", State: "opened", P: 1, N: 2, E: false, CN: 0, PD: "", PSz: 20,
				}},
				CM: nil,
				EI: 0,
			}
		}
		tok, err := Encode(key, p)
		if err != nil {
			t.Fatalf("scope %v encode: %v", sc.Kind, err)
		}
		got, err := Decode(key, tok, now)
		if err != nil {
			t.Fatalf("scope %v decode: %v", sc.Kind, err)
		}
		if got.Scope.Kind != sc.Kind {
			t.Fatalf("kind %s", got.Scope.Kind)
		}
	}
}

func TestValidateKey(t *testing.T) {
	if err := ValidateKey(nil); err != nil {
		t.Fatal(err)
	}
	if err := ValidateKey([]byte("short")); err != ErrInvalidKey {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(ErrInvalidKey.Error(), "short") {
		t.Fatal("must not echo key material")
	}
}

func TestMatchBinding_independentFieldMutationTable(t *testing.T) {
	p := basePayload()
	type expect struct {
		instance string
		actor    int64
		policy   string
		tool     string
		section  string
		scope    Scope
		filters  Filters
		refs     []string
		upper    string
	}
	base := expect{
		instance: p.Instance,
		actor:    p.ActorID,
		policy:   p.PolicyFP,
		tool:     p.Tool,
		section:  p.Section,
		scope:    p.Scope,
		filters:  p.Filters,
		refs:     append([]string{}, p.ImmutableRefs...),
		upper:    p.UpperBound,
	}
	check := func(e expect) error {
		return MatchBinding(p, e.instance, e.actor, e.policy, e.tool, e.section, e.scope, e.filters, e.refs, e.upper)
	}
	if err := check(base); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	mutations := []struct {
		name string
		mut  func(e *expect)
	}{
		{"instance", func(e *expect) { e.instance = "https://other.example/api/v4" }},
		{"actor", func(e *expect) { e.actor = 99 }},
		{"policy", func(e *expect) { e.policy = "otherfp" }},
		{"tool", func(e *expect) { e.tool = "other_tool" }},
		{"section", func(e *expect) { e.section = "other_section" }},
		{"project", func(e *expect) { e.scope.ProjectID = "999" }},
		{"path_filter", func(e *expect) { e.filters.Path = "other.go" }},
		{"caller_until", func(e *expect) { e.filters.CallerUntil = "2026-09-01T00:00:00Z" }},
		{"per_page", func(e *expect) { e.filters.PerPage = 10 }},
		{"refs", func(e *expect) { e.refs = []string{"cccccccccccccccccccccccccccccccccccccccc"} }},
		{"refs_arity", func(e *expect) {
			e.refs = []string{e.refs[0], "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
		}},
		{"upper", func(e *expect) { e.upper = "2026-10-03T11:00:00Z" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			e := base
			e.refs = append([]string{}, base.refs...)
			e.scope = base.scope
			e.filters = base.filters
			tc.mut(&e)
			if err := check(e); !errorsIsResync(err) {
				t.Fatalf("mutated %s must resync, got %v", tc.name, err)
			}
		})
	}
}

func TestPageState_rejectsSkipLoopJumpAndIncompleteResume(t *testing.T) {
	key := testKey(t)
	now := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		mut  func(*Payload)
	}{
		{"repeat_next", func(p *Payload) { p.PageState.ProviderNextPage = 1 }},
		{"jump_next", func(p *Payload) { p.PageState.ProviderNextPage = 4 }},
		{"missing_digest", func(p *Payload) { p.PageState.SequenceDigest = "" }},
		{"bad_digest", func(p *Payload) { p.PageState.SequenceDigest = "not-hex" }},
		{"missing_last", func(p *Payload) { p.PageState.LastSHA = "" }},
		{"items_overflow", func(p *Payload) { p.PageState.ItemsOnPage = 99 }},
		{"items_zero_resume", func(p *Payload) { p.PageState.ItemsOnPage = 0 }},
		{"per_page_mismatch", func(p *Payload) { p.PageState.PerPage = 10 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := basePayload()
			tc.mut(&p)
			if err := func() error { _, e := Encode(key, p); return e }(); err == nil || !errorsIsResync(err) {
				t.Fatalf("encode should reject: %v", err)
			}
			// Also reject on decode path via crafted signed payload bypassing Encode validate:
			// use Encode on valid then we only test Encode validate here; decode uses same validatePayload.
			raw, _ := json.Marshal(p)
			tok := signRaw(t, key, raw)
			if _, err := Decode(key, tok, now); !errorsIsResync(err) {
				t.Fatalf("decode should reject: %v", err)
			}
		})
	}
}

func TestSequenceDigest_stable(t *testing.T) {
	a := SequenceDigest([]string{"aa", "bb"})
	b := SequenceDigest([]string{"aa", "bb"})
	c := SequenceDigest([]string{"bb", "aa"})
	if a != b || a == c || !isHexSHA256(a) {
		t.Fatalf("digest a=%s b=%s c=%s", a, b, c)
	}
}

func TestFakeClock_expiry(t *testing.T) {
	key := testKey(t)
	clk := &FakeClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	p := basePayload()
	p.ExpiresAt = clk.Now().Add(DefaultTTL).Format(time.RFC3339)
	tok, err := Encode(key, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(key, tok, clk.Now()); err != nil {
		t.Fatal(err)
	}
	clk.Advance(DefaultTTL + time.Second)
	if _, err := Decode(key, tok, clk.Now()); !errorsIsResync(err) {
		t.Fatalf("want expiry resync, got %v", err)
	}
}

func TestPayload_allBindingFieldsPresent(t *testing.T) {
	key := testKey(t)
	p := basePayload()
	tok, err := Encode(key, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(key, tok, time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"instance", got.Instance != ""},
		{"actor", got.ActorID > 0},
		{"policy", got.PolicyFP != ""},
		{"tool", got.Tool != ""},
		{"section", got.Section != ""},
		{"scope", got.Scope.Kind == ScopeProject},
		{"filters", got.Filters.PerPage == 20},
		{"immutable_refs", len(got.ImmutableRefs) == 1 && len(got.ImmutableRefs[0]) == 40},
		{"upper_bound", got.UpperBound != ""},
		{"expires", got.ExpiresAt != ""},
		{"page_state", got.PageState.Page == 1 && got.PageState.ProviderNextPage == 2},
		{"schema", got.SchemaVersion == SchemaV1},
	}
	for _, c := range checks {
		if !c.ok {
			t.Fatalf("missing binding %s", c.name)
		}
	}
}

func TestCanonicalInstance_stripsCredentialsNeverEchoes(t *testing.T) {
	const sentinel = "s3cr3tPASS-userinfo-query-sentinel"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "userinfo_query_fragment_default_https_port",
			in:   "https://oauth:" + sentinel + "@GitLab.Example:443/api/v4/?" + "token=" + sentinel + "#" + sentinel,
			want: "https://gitlab.example/api/v4",
		},
		{
			name: "http_default_port_and_trailing_slash",
			in:   "HTTP://user:" + sentinel + "@gitlab.example:80/api/v4/",
			want: "http://gitlab.example/api/v4",
		},
		{
			name: "custom_port_kept",
			in:   "https://gitlab.example:8443/api/v4",
			want: "https://gitlab.example:8443/api/v4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CanonicalInstance(tc.in)
			if err != nil {
				t.Fatalf("CanonicalInstance: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if strings.Contains(got, sentinel) || strings.Contains(strings.ToLower(got), "oauth:") {
				t.Fatal("canonical instance leaked credential material")
			}
			// Bound into a signed cursor — decoded payload must not contain sentinel.
			key := testKey(t)
			p := basePayload()
			p.Instance = got
			tok, err := Encode(key, p)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(tok, sentinel) {
				t.Fatal("token encoding leaked sentinel")
			}
			dec, err := Decode(key, tok, time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC))
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(dec)
			if strings.Contains(string(raw), sentinel) {
				t.Fatal("decoded payload leaked sentinel")
			}
			if dec.Instance != tc.want {
				t.Fatalf("bound instance %q", dec.Instance)
			}
		})
	}
	// Invalid URL error must be static — never echo raw credential-bearing input.
	bad := "https://:" + sentinel + "@/not a url"
	_, err := CanonicalInstance(bad)
	if err == nil {
		t.Fatal("expected invalid instance error")
	}
	if err != ErrInvalidInstance {
		t.Fatalf("want ErrInvalidInstance, got %v", err)
	}
	if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), bad) {
		t.Fatalf("error echoed secret/raw URL: %v", err)
	}
}

func TestCanonicalInstance_rejectsNonHTTP(t *testing.T) {
	if _, err := CanonicalInstance("ftp://gitlab.example/api/v4"); err != ErrInvalidInstance {
		t.Fatalf("got %v", err)
	}
	if _, err := CanonicalInstance(""); err != ErrInvalidInstance {
		t.Fatalf("got %v", err)
	}
}

func TestTerminalPageState_emptyOK(t *testing.T) {
	key := testKey(t)
	p := basePayload()
	p.PageState = PageState{Page: 3, PerPage: 20, ProviderNextPage: 0, ItemsOnPage: 0}
	tok, err := Encode(key, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(key, tok, time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
}

func TestQueueCont_rejectsContradictoryReplay(t *testing.T) {
	key := testKey(t)
	p := basePayload()
	p.Tool = ToolReviewQueue
	p.Section = SectionReviewQueue
	p.Scope = Scope{Kind: ScopeGroupQueue, GroupID: "9"}
	p.ImmutableRefs = nil
	p.PageState = PageState{}
	p.QueueCont = &QueueCont{
		V:     QueueContSchemaRQ2,
		Phase: "discover",
		Kinds: []string{"reviewer"},
		KP: []QueueKindProg{{
			Kind: "reviewer", State: "opened", P: 1, N: 2, E: true, CN: 0, PD: "", PSz: 20,
		}},
	}
	if _, err := Encode(key, p); err == nil {
		t.Fatal("exhausted with nonzero next must reject")
	}
	p.QueueCont.KP[0].E = false
	p.QueueCont.KP[0].N = 0
	p.QueueCont.KP[0].CN = 1
	p.QueueCont.KP[0].PD = ""
	if _, err := Encode(key, p); err == nil {
		t.Fatal("cn>0 with empty pd must reject")
	}
	p.QueueCont.KP[0].CN = 0
	p.QueueCont.Lim = []string{"", "dedupe_capacity"}
	if _, err := Encode(key, p); err == nil {
		t.Fatal("empty lim code must reject")
	}
}
