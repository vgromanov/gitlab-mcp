package cursor

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func fixtureLive() ReviewLiveRefs {
	return ReviewLiveRefs{
		OwnerProjectID: 42, SourceProjectID: 42, TargetProjectID: 43,
		SourceBranch: "feature", TargetBranch: "main",
		SourceSHA: strings.Repeat("a", 40), TargetSHA: strings.Repeat("d", 40), VersionID: 5,
		VersionHead: strings.Repeat("a", 40), VersionBase: strings.Repeat("b", 40), VersionStart: strings.Repeat("c", 40),
	}
}

func TestReviewLiveBinding_independentTuple(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	p := reviewContextPayload(now)
	tok, err := Encode(key, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(key, tok, now)
	if err != nil {
		t.Fatal(err)
	}
	live := fixtureLive()
	if err := VerifyContextBinding(got, p.Instance, p.ActorID, p.PolicyFP, p.Tool, p.Section, p.Scope, p.Filters, p.UpperBound, live, []string{"metadata"}); err != nil {
		t.Fatalf("matching observation: %v", err)
	}
	if err := VerifyContextBinding(got, p.Instance, p.ActorID, p.PolicyFP, p.Tool, p.Section, p.Scope, p.Filters, p.UpperBound, ReviewLiveRefs{}, []string{"metadata"}); err == nil {
		t.Fatal("missing observation accepted")
	}
	moved := live
	moved.VersionHead = strings.Repeat("e", 40)
	moved.SourceSHA = moved.VersionHead
	if err := VerifyContextBinding(got, p.Instance, p.ActorID, p.PolicyFP, p.Tool, p.Section, p.Scope, p.Filters, p.UpperBound, moved, []string{"metadata"}); err == nil {
		t.Fatal("moved head accepted")
	}
	fields := []struct {
		name string
		mut  func(*ReviewLiveRefs)
	}{
		{"owner", func(r *ReviewLiveRefs) { r.OwnerProjectID = 99 }},
		{"source project", func(r *ReviewLiveRefs) { r.SourceProjectID = 99 }},
		{"target project", func(r *ReviewLiveRefs) { r.TargetProjectID = 99 }},
		{"source branch", func(r *ReviewLiveRefs) { r.SourceBranch = "other" }},
		{"target branch", func(r *ReviewLiveRefs) { r.TargetBranch = "other" }},
		{"source sha", func(r *ReviewLiveRefs) { r.SourceSHA = strings.Repeat("e", 40) }},
		{"target sha", func(r *ReviewLiveRefs) { r.TargetSHA = strings.Repeat("e", 40) }},
		{"version id", func(r *ReviewLiveRefs) { r.VersionID = 9 }},
		{"version base", func(r *ReviewLiveRefs) { r.VersionBase = strings.Repeat("e", 40) }},
		{"version start", func(r *ReviewLiveRefs) { r.VersionStart = strings.Repeat("e", 40) }},
	}
	for _, tc := range fields {
		t.Run(tc.name, func(t *testing.T) {
			cp := fixtureLive()
			tc.mut(&cp)
			if err := VerifyContextBinding(got, p.Instance, p.ActorID, p.PolicyFP, p.Tool, p.Section, p.Scope, p.Filters, p.UpperBound, cp, []string{"metadata"}); err == nil {
				t.Fatalf("%s mismatch accepted", tc.name)
			}
		})
	}
}

func TestReviewContext_fillerCompleteRejected(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"diff_manifest"} {
		p := reviewContextPayload(now)
		p.ContextRef.Requested = []string{name}
		p.ContextRef.Complete = []string{name}
		p.ContextRef.Excluded = []string{}
		p.ContextRef.Digests = map[string]string{name: strings.Repeat("ab", 32)}
		p.Filters.Selection = name
		if _, err := Encode(key, p); err == nil {
			t.Errorf("Encode accepted complete %s", name)
		}
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(key, signRaw(t, key, raw), now); err == nil {
			t.Errorf("Decode accepted HMAC-valid complete %s", name)
		}
	}
	disc := reviewContextPayload(now)
	disc.ContextRef.Requested = []string{"discussions"}
	disc.ContextRef.Complete = []string{"discussions"}
	disc.ContextRef.Excluded = []string{}
	disc.ContextRef.Digests = map[string]string{"discussions": strings.Repeat("ab", 32)}
	disc.Filters.Selection = "discussions"
	if _, err := Encode(key, disc); err != nil {
		t.Fatalf("discussions evidence: %v", err)
	}
	okp := reviewContextPayload(now)
	okp.ContextRef.Requested = []string{"diff_manifest", "discussions", "metadata", "pipeline_graph"}
	okp.ContextRef.Complete = []string{"metadata"}
	okp.ContextRef.Excluded = []string{"diff_manifest", "discussions", "pipeline_graph"}
	okp.ContextRef.Digests = map[string]string{"metadata": strings.Repeat("ab", 32)}
	okp.Filters.Selection = strings.Join(okp.ContextRef.Requested, ",")
	tok, err := Encode(key, okp)
	if err != nil {
		t.Fatalf("metadata complete with fillers excluded: %v", err)
	}
	got, err := Decode(key, tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyContextBinding(got, okp.Instance, okp.ActorID, okp.PolicyFP, okp.Tool, okp.Section, okp.Scope, okp.Filters, okp.UpperBound, fixtureLive(), []string{"metadata"}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyContextBinding(got, okp.Instance, okp.ActorID, okp.PolicyFP, okp.Tool, okp.Section, okp.Scope, okp.Filters, okp.UpperBound, fixtureLive(), []string{"discussions"}); err == nil {
		t.Fatal("filler demand accepted")
	}
}
