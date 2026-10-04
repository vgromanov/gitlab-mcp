package readmeta

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestObservedHeadSHA(t *testing.T) {
	ok40 := "AbcDef0123456789abcdef0123456789ABCDEF01"
	got, ok := ObservedHeadSHA(ok40)
	if !ok || got != strings.ToLower(ok40) {
		t.Fatalf("got %q ok=%v", got, ok)
	}
	for _, bad := range []string{"", "abcdeadbeef", "gggggggggggggggggggggggggggggggggggggggg", strings.Repeat("a", 39), strings.Repeat("a", 41)} {
		if _, ok := ObservedHeadSHA(bad); ok {
			t.Fatalf("expected reject %q", bad)
		}
	}
}

func TestSectionJSON_lockedShape(t *testing.T) {
	s := NewMRDiffsSection(time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	s.ApplyPaging(PagingObservation{SDKNextPage: 0, HeaderPresent: true, HeaderValue: "", ExhaustedObserved: true, PagingKnown: true})
	s.ContentComplete = ContentCompleteFalse
	s.Consistency = ConsistencyConsistent
	s.ManifestCoverage = CoveragePartial
	s.PatchCoverage = CoverageUnknown
	n := 2
	s.Counts.Items = &n
	// bytes/files stay null (unknown)
	s.AddLimitation(CodeCollapsed, "diff collapsed")

	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["source"] != SourceGitLabREST || m["provider"] != ProviderGitLab {
		t.Fatalf("source/provider: %v", m)
	}
	if m["capability_version"] != CapabilityMRDiffsV1 {
		t.Fatalf("capability: %v", m["capability_version"])
	}
	if m["head_sha"] != nil {
		t.Fatalf("head_sha want null, got %v", m["head_sha"])
	}
	if m["content_complete"] != ContentCompleteFalse {
		t.Fatalf("content_complete: %T %v", m["content_complete"], m["content_complete"])
	}
	if _, isBool := m["content_complete"].(bool); isBool {
		t.Fatal("content_complete must be string enum, not bool")
	}
	if m["pagination_exhausted"] != true {
		t.Fatal("expected exhausted true when header present empty")
	}
	if m["next_cursor"] != nil {
		t.Fatalf("next_cursor want null, got %v", m["next_cursor"])
	}
	counts := m["counts"].(map[string]any)
	if counts["items"].(float64) != 2 {
		t.Fatalf("items: %v", counts["items"])
	}
	if counts["bytes"] != nil || counts["files"] != nil {
		t.Fatalf("unknown counts must be null: %v", counts)
	}
}

func TestObservePaging_headerAbsentNotExhausted(t *testing.T) {
	obs := ObservePaging(http.Header{}, 0)
	if obs.ExhaustedObserved || obs.PagingKnown {
		t.Fatalf("absent headers must not claim exhausted: %+v", obs)
	}
	s := NewMRDiffsSection(time.Now())
	s.ApplyPaging(obs)
	if s.PaginationExhausted {
		t.Fatal("pagination_exhausted must stay false when headers absent")
	}
	if s.NextCursor != nil {
		t.Fatal("next_cursor nil when sdk next 0")
	}
	found := false
	for _, lim := range s.Limitations {
		if lim.Code == CodeUnknownCount {
			found = true
		}
	}
	if !found {
		t.Fatal("expected unknown_count limitation when paging unknown")
	}
}

func TestObservePaging_explicitExhausted(t *testing.T) {
	h := http.Header{}
	h.Set("X-Next-Page", "")
	obs := ObservePaging(h, 0)
	if !obs.HeaderPresent || !obs.ExhaustedObserved {
		t.Fatalf("want explicit exhausted: %+v", obs)
	}
	h2 := http.Header{}
	h2.Set("X-Next-Page", "3")
	obs2 := ObservePaging(h2, 3)
	if obs2.ExhaustedObserved {
		t.Fatal("next page 3 is not exhausted")
	}
	s := NewMRDiffsSection(time.Now())
	s.ApplyPaging(obs2)
	if s.NextCursor == nil || *s.NextCursor != "3" {
		t.Fatalf("next_cursor=%v", s.NextCursor)
	}
}

func TestDecodeBoolPresence(t *testing.T) {
	cases := []struct {
		raw  string
		want Presence
	}{
		{`{}`, PresenceAbsent},
		{`{"allow_failure":null}`, PresenceNull},
		{`{"allow_failure":false}`, PresenceFalse},
		{`{"allow_failure":true}`, PresenceTrue},
		{`{"other":true}`, PresenceAbsent},
		{``, PresenceAbsent},
		{`null`, PresenceAbsent},
	}
	for _, tc := range cases {
		p, err := DecodeBoolPresence(json.RawMessage(tc.raw), "allow_failure")
		if err != nil || p != tc.want {
			t.Fatalf("%s: got %s err=%v want %s", tc.raw, p, err, tc.want)
		}
	}
}

func TestDecodeBoolPresence_rejectsInvalid(t *testing.T) {
	bad := []string{
		`{"allow_failure":1}`,
		`{"allow_failure":"true"}`,
		`{"allow_failure":[]}`,
		`{"allow_failure":{}}`,
		`[]`,
		`"true"`,
		`{"allow_failure":true`,           // truncated
		`{"allow_failure":true} trailing`, // trailing
		`{`,                               // truncated object
		`{"allow_failure":tru}`,           // malformed literal
	}
	for _, raw := range bad {
		p, err := DecodeBoolPresence(json.RawMessage(raw), "allow_failure")
		if err == nil {
			t.Fatalf("%q: expected error, got presence=%s", raw, p)
		}
		if p != "" {
			t.Fatalf("%q: on error presence must be empty, got %q", raw, p)
		}
	}
}

func TestContentCompleteRejectsBoolInFixtureSense(t *testing.T) {
	// Ensure marshaled section never emits boolean content_complete.
	s := NewMRDiffsSection(time.Now())
	s.ContentComplete = ContentCompleteUnknown
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), `"content_complete":true`) || strings.Contains(string(b), `"content_complete":false`) {
		t.Fatalf("boolean content_complete leaked: %s", b)
	}
}

func TestAllCodesClosed(t *testing.T) {
	if len(AllCodes) != 19 {
		t.Fatalf("closed set size=%d", len(AllCodes))
	}
}
