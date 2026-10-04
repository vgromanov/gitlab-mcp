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

func TestEncodeDecode_fractionalExpires(t *testing.T) {
	key := testKey(t)
	t.Run("fractional", func(t *testing.T) {
		p := basePayload()
		p.UpperBound = "2026-10-03T12:00:00.7Z"
		p.Filters.Until = p.UpperBound
		p.ExpiresAt = "2026-10-03T14:00:00.5Z"
		tok, err := Encode(key, p)
		if err != nil {
			t.Fatalf("encode fractional: %v", err)
		}
		got, err := Decode(key, tok, time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("decode fractional: %v", err)
		}
		if got.ExpiresAt != p.ExpiresAt || got.UpperBound != p.UpperBound || got.Filters.Until != p.Filters.Until {
			t.Fatalf("roundtrip upper=%s until=%s exp=%s", got.UpperBound, got.Filters.Until, got.ExpiresAt)
		}
	})
	t.Run("integer-second", func(t *testing.T) {
		p := basePayload()
		tok, err := Encode(key, p)
		if err != nil {
			t.Fatalf("encode integer: %v", err)
		}
		got, err := Decode(key, tok, time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC))
		if err != nil {
			t.Fatalf("decode integer: %v", err)
		}
		if got.ExpiresAt != p.ExpiresAt || got.UpperBound != p.UpperBound {
			t.Fatalf("integer changed upper=%s exp=%s", got.UpperBound, got.ExpiresAt)
		}
	})
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

func queuePayloadOK() Payload {
	p := basePayload()
	p.Tool = ToolReviewQueue
	p.Section = SectionReviewQueue
	p.Scope = Scope{Kind: ScopeGroupQueue, GroupID: "9"}
	p.ImmutableRefs = nil
	p.PageState = PageState{}
	head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p.QueueCont = &QueueCont{
		V:     QueueContSchemaRQ2,
		Phase: "emit",
		Kinds: []string{"authored", "reviewer"},
		KI:    2,
		KP: []QueueKindProg{
			{Kind: "authored", State: "opened", P: 1, N: 0, E: true, CN: 0, PD: "", PSz: 20},
			{Kind: "reviewer", State: "opened", P: 1, N: 0, E: true, CN: 0, PD: "", PSz: 20},
		},
		CM: []QueueCandidate{{
			K: "42:1", B: 5, U: "2026-10-03T11:00:00.123456789Z", H: &head,
		}},
		EI:   0,
		Lim:  nil,
		Term: false,
	}
	return p
}

func TestQueueCont_roundTripCandidatesAndRejectTamper(t *testing.T) {
	key := testKey(t)
	now := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)
	p := queuePayloadOK()
	tok, err := Encode(key, p)
	if err != nil {
		t.Fatalf("encode ok payload: %v", err)
	}
	got, err := Decode(key, tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.QueueCont == nil || len(got.QueueCont.CM) != 1 || got.QueueCont.CM[0].K != "42:1" {
		t.Fatalf("round-trip cm: %+v", got.QueueCont)
	}

	// Rotation: different key rejects without transport.
	other := testKey(t)
	if _, err := Decode(other, tok, now); err == nil {
		t.Fatal("key rotation must reject")
	}

	// Tamper payload bytes after MAC → resync.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		t.Fatalf("token shape %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0x5a
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(raw) + "." + parts[2]
	if _, err := Decode(key, tampered, now); err == nil {
		t.Fatal("tampered token must reject")
	}

	// Expiry: advance past TTL.
	if _, err := Decode(key, tok, now.Add(3*time.Hour)); err == nil {
		t.Fatal("expired token must reject")
	}
}

func TestQueueCont_rejectsBadCandidatesKeysBitsAndProgress(t *testing.T) {
	key := testKey(t)
	cases := []struct {
		name string
		mut  func(*Payload)
	}{
		{"bad_key", func(p *Payload) { p.QueueCont.CM[0].K = "0:1" }},
		{"bad_bits", func(p *Payload) { p.QueueCont.CM[0].B = 0 }},
		{"bits_outside_kinds", func(p *Payload) { p.QueueCont.CM[0].B = 2 }}, // ongoing not requested
		{"bad_time", func(p *Payload) { p.QueueCont.CM[0].U = "not-a-time" }},
		{"bad_head", func(p *Payload) {
			h := "zzzz"
			p.QueueCont.CM[0].H = &h
		}},
		{"dup_key", func(p *Payload) {
			c := p.QueueCont.CM[0]
			p.QueueCont.CM = append(p.QueueCont.CM, c)
		}},
		{"ki_overflow", func(p *Payload) { p.QueueCont.KI = 99 }},
		{"phase_discover_ei", func(p *Payload) {
			p.QueueCont.Phase = "discover"
			p.QueueCont.EI = 1
		}},
		{"unsorted_kinds", func(p *Payload) { p.QueueCont.Kinds = []string{"reviewer", "authored"} }},
		{"dup_stream", func(p *Payload) {
			p.QueueCont.KP = append(p.QueueCont.KP, p.QueueCont.KP[0])
		}},
		{"page_state_pollution", func(p *Payload) {
			p.PageState = PageState{Page: 1, PerPage: 20, ProviderNextPage: 2}
		}},
		{"legacy_refs", func(p *Payload) {
			p.ImmutableRefs = []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := queuePayloadOK()
			tc.mut(&p)
			if _, err := Encode(key, p); err == nil {
				t.Fatal("expected encode reject")
			}
		})
	}
}

func observedLive(p Payload) ReviewLiveRefs {
	c := p.ContextRef
	return ReviewLiveRefs{
		OwnerProjectID: c.OwnerProjectID, SourceProjectID: c.SourceProjectID, TargetProjectID: c.TargetProjectID,
		SourceBranch: c.SourceBranch, TargetBranch: c.TargetBranch,
		SourceSHA: c.SourceSHA, TargetSHA: c.TargetSHA, VersionID: c.VersionID,
		VersionHead: c.VersionHead, VersionBase: c.VersionBase, VersionStart: c.VersionStart,
	}
}

func reviewContextPayload(now time.Time) Payload {
	iid := int64(7)
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	start := strings.Repeat("c", 40)
	target := strings.Repeat("d", 40)
	ret := now.UTC().Format(time.RFC3339)
	return Payload{
		SchemaVersion: SchemaV1,
		Instance:      "https://gitlab.example/api/v4",
		ActorID:       7,
		PolicyFP:      "policyfp",
		Tool:          ToolReviewContext,
		Section:       SectionReviewContext,
		Scope:         Scope{Kind: ScopeReviewContext, ProjectID: "42", MergeRequestIID: &iid},
		Filters:       Filters{Selection: "metadata", Until: ret, PerPage: 1},
		UpperBound:    ret,
		ExpiresAt:     now.UTC().Add(DefaultTTL).Format(time.RFC3339),
		ContextRef: &ContextRef{
			OwnerProjectID: 42, SourceProjectID: 42, TargetProjectID: 43,
			SourceBranch: "feature", TargetBranch: "main",
			SourceSHA: head, TargetSHA: target, VersionID: 5,
			VersionHead: head, VersionBase: base, VersionStart: start,
			Requested: []string{"metadata"}, Complete: []string{"metadata"}, Excluded: []string{},
			Digests:           map[string]string{"metadata": strings.Repeat("ab", 32)},
			RetrievedAt:       ret,
			WriteFreshUntil:   now.UTC().Add(ReviewWriteFresh).Format(time.RFC3339),
			BracketConsistent: true,
		},
	}
}

func TestReviewContextToken(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	p := reviewContextPayload(now)
	tok, err := Encode(key, p)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := Decode(key, tok, now)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := VerifyContextBinding(got, p.Instance, p.ActorID, p.PolicyFP, p.Tool, p.Section, p.Scope, p.Filters, p.UpperBound, observedLive(p), []string{"metadata"}); err != nil {
		t.Fatalf("verify metadata: %v", err)
	}
	if err := VerifyContextBinding(got, p.Instance, p.ActorID, p.PolicyFP, p.Tool, p.Section, p.Scope, p.Filters, p.UpperBound, observedLive(p), []string{"approvals"}); err == nil {
		t.Fatal("metadata-only token satisfied an approvals demand")
	}
	if err := VerifyContextBinding(got, p.Instance, p.ActorID, p.PolicyFP, p.Tool, p.Section, p.Scope, p.Filters, p.UpperBound, observedLive(p), []string{"metadata", "approvals"}); err == nil {
		t.Fatal("metadata-only token satisfied a full review demand")
	}
	if ContextWriteFresh(got, now.Add(ReviewWriteFresh)) {
		t.Fatal("write freshness must be exclusive at the boundary")
	}
	if !ContextWriteFresh(got, now.Add(time.Minute)) {
		t.Fatal("write freshness inside the window")
	}
	if err := VerifyContextBinding(got, p.Instance, 8, p.PolicyFP, p.Tool, p.Section, p.Scope, p.Filters, p.UpperBound, observedLive(p), []string{"metadata"}); err == nil {
		t.Fatal("actor mismatch")
	}

	bad := []struct {
		name string
		mut  func(*Payload)
	}{
		{"overlap", func(p *Payload) {
			p.ContextRef.Excluded = []string{"metadata"}
			p.ContextRef.Requested = []string{"metadata"}
		}},
		{"excluded digest", func(p *Payload) {
			p.ContextRef.Digests["approvals"] = strings.Repeat("cd", 32)
		}},
		{"unsupported complete", func(p *Payload) {
			p.ContextRef.Complete = []string{"unsupported"}
			p.ContextRef.Requested = []string{"unsupported"}
			p.ContextRef.Excluded = []string{}
			p.ContextRef.Digests = map[string]string{"unsupported": strings.Repeat("ab", 32)}
			p.Filters.Selection = "unsupported"
		}},
		{"non-positive iid", func(p *Payload) {
			zero := int64(0)
			p.Scope.MergeRequestIID = &zero
		}},
		{"bracket false with complete", func(p *Payload) { p.ContextRef.BracketConsistent = false }},
		{"owner id", func(p *Payload) { p.ContextRef.OwnerProjectID = 0 }},
		{"source id", func(p *Payload) { p.ContextRef.SourceProjectID = 0 }},
		{"target id", func(p *Payload) { p.ContextRef.TargetProjectID = 0 }},
		{"version id", func(p *Payload) { p.ContextRef.VersionID = 0 }},
		{"scope project", func(p *Payload) { p.Scope.ProjectID = "99" }},
		{"blank source branch", func(p *Payload) { p.ContextRef.SourceBranch = "  " }},
		{"padded source branch", func(p *Payload) { p.ContextRef.SourceBranch = " feature" }},
		{"blank target branch", func(p *Payload) { p.ContextRef.TargetBranch = "" }},
		{"padded target branch", func(p *Payload) { p.ContextRef.TargetBranch = "main " }},
		{"source sha", func(p *Payload) { p.ContextRef.SourceSHA = "abc"; p.ContextRef.VersionHead = "abc" }},
		{"target sha", func(p *Payload) { p.ContextRef.TargetSHA = strings.Repeat("A", 40) }},
		{"version base", func(p *Payload) { p.ContextRef.VersionBase = strings.Repeat("g", 40) }},
		{"version start", func(p *Payload) { p.ContextRef.VersionStart = "" }},
		{"head differs from source", func(p *Payload) { p.ContextRef.VersionHead = strings.Repeat("e", 40) }},
		{"unsorted mask", func(p *Payload) {
			p.ContextRef.Requested = []string{"metadata", "approvals"}
			p.ContextRef.Complete = []string{"approvals"}
			p.ContextRef.Excluded = []string{"metadata"}
			p.ContextRef.Digests = map[string]string{"approvals": strings.Repeat("ab", 32)}
			p.Filters.Selection = "metadata,approvals"
		}},
		{"duplicate complete", func(p *Payload) {
			p.ContextRef.Requested = []string{"metadata"}
			p.ContextRef.Complete = []string{"metadata", "metadata"}
			p.ContextRef.Excluded = []string{}
		}},
		{"union short", func(p *Payload) {
			p.ContextRef.Requested = []string{"approvals", "metadata"}
			p.ContextRef.Complete = []string{"metadata"}
			p.ContextRef.Excluded = []string{}
			p.Filters.Selection = "approvals,metadata"
		}},
		{"digest shape", func(p *Payload) { p.ContextRef.Digests["metadata"] = "zz" }},
		{"missing digest", func(p *Payload) { p.ContextRef.Digests = map[string]string{} }},
		{"retrieved at", func(p *Payload) { p.ContextRef.RetrievedAt = "yesterday" }},
		{"write fresh", func(p *Payload) { p.ContextRef.WriteFreshUntil = "tomorrow" }},
		{"expires window", func(p *Payload) { p.ExpiresAt = now.Add(time.Hour).UTC().Format(time.RFC3339) }},
		{"upper bound", func(p *Payload) { p.UpperBound = now.Add(time.Minute).UTC().Format(time.RFC3339) }},
		{"selection", func(p *Payload) { p.Filters.Selection = "approvals" }},
		{"immutable refs", func(p *Payload) { p.ImmutableRefs = []string{strings.Repeat("a", 40)} }},
		{"per page", func(p *Payload) { p.Filters.PerPage = 2 }},
		{"nil ref", func(p *Payload) { p.ContextRef = nil }},
		{"group scope", func(p *Payload) { p.Scope.GroupID = "9" }},
		{"pipeline scope", func(p *Payload) {
			id := int64(9)
			p.Scope.PipelineID = &id
		}},
		{"page state", func(p *Payload) { p.PageState.Page = 1 }},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			cp := reviewContextPayload(now)
			tc.mut(&cp)
			if _, err := Encode(key, cp); err == nil {
				t.Fatal("expected reject")
			}
			raw, err := json.Marshal(cp)
			if err != nil {
				t.Fatal(err)
			}
			signed := signRaw(t, key, raw)
			if _, err := Decode(key, signed, now); !errorsIsResync(err) {
				t.Fatalf("hmac-valid semantic reject: %v", err)
			}
		})
	}

	fresh := reviewContextPayload(now)
	fresh.ContextRef.WriteFreshUntil = now.Add(3 * time.Hour).UTC().Format(time.RFC3339)
	raw, err := json.Marshal(fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decode(key, signRaw(t, key, raw), now); !errorsIsResync(err) {
		t.Fatal("freshness after expiry must fail")
	}
	corrupt := tok[:len(tok)-1] + "A"
	if _, err := Decode(key, corrupt, now); !errorsIsResync(err) {
		t.Fatal("corrupt hmac")
	}
	if _, err := Decode(key, tok, now.Add(DefaultTTL)); !errorsIsResync(err) {
		t.Fatal("expired token")
	}

	legacyNow := time.Date(2026, 10, 3, 13, 0, 0, 0, time.UTC)
	legacy := basePayload()
	ltok, err := Encode(key, legacy)
	if err != nil {
		t.Fatalf("list_commits: %v", err)
	}
	if _, err := Decode(key, ltok, legacyNow); err != nil {
		t.Fatalf("list_commits round trip: %v", err)
	}
	pipe := int64(55)
	pipePayload := basePayload()
	pipePayload.Scope = Scope{Kind: ScopePipeline, ProjectID: "1", PipelineID: &pipe}
	ptok, err := Encode(key, pipePayload)
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	if _, err := Decode(key, ptok, legacyNow); err != nil {
		t.Fatalf("pipeline round trip: %v", err)
	}
	qp := queuePayloadOK()
	qtok, err := Encode(key, qp)
	if err != nil {
		t.Fatalf("rq2: %v", err)
	}
	if _, err := Decode(key, qtok, legacyNow); err != nil {
		t.Fatalf("rq2 round trip: %v", err)
	}
}

func TestDiscussionsEvidenceAndDC1(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	sem := strings.Repeat("ab", 32)
	pos := strings.Repeat("cd", 32)
	full := strings.Repeat("ef", 32)
	bundle := []byte(`{"schema":"discussions.evidence.v1","capability":"readmeta.review_context.discussions.v1","selection":"all","semantic_feedback_digest":"` + sem + `","position_digest":"` + pos + `","full_revision_digest":"` + full + `"}`)
	sum := sha256.Sum256(bundle)
	dig := fmtSHA(sum[:])
	p := reviewContextPayload(now)
	p.ContextRef.Requested = []string{"discussions", "metadata"}
	p.ContextRef.Complete = []string{"discussions", "metadata"}
	p.ContextRef.Excluded = []string{}
	p.ContextRef.Digests = map[string]string{"discussions": dig, "metadata": strings.Repeat("12", 32)}
	p.Filters.Selection = "discussions,metadata"
	tok, err := Encode(key, p)
	if err != nil {
		t.Fatalf("discussions evidence encode: %v", err)
	}
	if _, err := Decode(key, tok, now); err != nil {
		t.Fatalf("discussions evidence decode: %v", err)
	}
	p.ContextRef.Digests["pipeline_graph"] = strings.Repeat("34", 32)
	if _, err := Encode(key, p); err == nil {
		t.Fatal("extra digest key")
	}
	pipe := reviewContextPayload(now)
	pipe.ContextRef.Requested = []string{"pipeline_graph"}
	pipe.ContextRef.Complete = []string{"pipeline_graph"}
	pipe.ContextRef.Excluded = []string{}
	pipe.ContextRef.Digests = map[string]string{"pipeline_graph": strings.Repeat("ab", 32)}
	pipe.Filters.Selection = "pipeline_graph"
	if _, err := Encode(key, pipe); err == nil {
		t.Fatal("pipeline_graph complete")
	}
	meta := reviewContextPayload(now)
	if _, err := Encode(key, meta); err != nil {
		t.Fatalf("metadata token: %v", err)
	}
	dc := discussionsCursorPayload(now)
	dtok, err := Encode(key, dc)
	if err != nil {
		t.Fatalf("dc1 encode: %v", err)
	}
	if _, err := Decode(key, dtok, now); err != nil {
		t.Fatalf("dc1 decode: %v", err)
	}
	qp := queuePayloadOK()
	qp.DiscussionsCont = dc.DiscussionsCont
	if _, err := Encode(key, qp); err == nil {
		t.Fatal("dc1 on queue token")
	}
	lc := basePayload()
	lc.DiscussionsCont = dc.DiscussionsCont
	if _, err := Encode(key, lc); err == nil {
		t.Fatal("dc1 on list_commits token")
	}
}

func discussionsCursorPayload(now time.Time) Payload {
	iid := int64(7)
	head := strings.Repeat("a", 40)
	target := strings.Repeat("d", 40)
	base := strings.Repeat("b", 40)
	start := strings.Repeat("c", 40)
	bound := now.UTC().Format(time.RFC3339)
	return Payload{
		SchemaVersion:   SchemaV1,
		Instance:        "https://gitlab.example/api/v4",
		ActorID:         7,
		PolicyFP:        "policyfp",
		Tool:            ToolReviewContext,
		Section:         SectionReviewDiscussions,
		Scope:           Scope{Kind: ScopeProject, ProjectID: "42", MergeRequestIID: &iid},
		Filters:         Filters{Until: bound, Order: "provider", Selection: "all", PerPage: 20},
		ImmutableRefs:   []string{head, target, head, base, start},
		UpperBound:      bound,
		ExpiresAt:       now.UTC().Add(DefaultTTL).Format(time.RFC3339),
		DiscussionsCont: &DiscussionsCont{V: DiscussionsContSchemaDC1, P: 1},
	}
}

func fmtSHA(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexdigits[c>>4]
		out[i*2+1] = hexdigits[c&0x0f]
	}
	return string(out)
}

func TestDiffManifestEvidenceKeyset(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	p := reviewContextPayload(now)
	p.ContextRef.Requested = []string{"diff_manifest", "metadata"}
	p.ContextRef.Complete = []string{"diff_manifest", "metadata"}
	p.ContextRef.Excluded = []string{}
	p.ContextRef.Digests = map[string]string{"diff_manifest": strings.Repeat("ab", 32), "metadata": strings.Repeat("cd", 32)}
	p.ContextRef.Evidence = map[string]string{"diff_manifest": DiffManifestEvidenceV1}
	p.Filters.Selection = "diff_manifest,metadata"
	if _, err := Encode(key, p); err != nil {
		t.Fatal(err)
	}
	p.ContextRef.Evidence = map[string]string{"diff_manifest": "nope"}
	if _, err := Encode(key, p); err == nil {
		t.Fatal("bad evidence accepted")
	}
	p.ContextRef.Evidence = nil
	if _, err := Encode(key, p); err == nil {
		t.Fatal("complete diff_manifest without evidence accepted")
	}
}
