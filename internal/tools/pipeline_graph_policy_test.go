package tools

import (
	"fmt"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

func jobWith(status string, allow readmeta.Presence, allowValid bool) graphJob {
	return graphJob{ID: 1, Status: status, StatusKnown: status != "", Allow: allow, AllowValid: allowValid}
}

func TestJobPolicyMatrix(t *testing.T) {
	cases := []struct {
		name    string
		job     graphJob
		outcome string
		reason  string
	}{
		{"required manual", jobWith("manual", readmeta.PresenceFalse, true), policyBlock, "required_manual"},
		{"optional manual", jobWith("manual", readmeta.PresenceTrue, true), policyPass, ""},
		{"manual omitted", jobWith("manual", readmeta.PresenceAbsent, true), policyUnknown, "unknown_policy"},
		{"manual null", jobWith("manual", readmeta.PresenceNull, true), policyUnknown, "unknown_policy"},
		{"failed required", jobWith("failed", readmeta.PresenceFalse, true), policyBlock, "failed_required"},
		{"failed optional", jobWith("failed", readmeta.PresenceTrue, true), policyPass, ""},
		{"failed omitted", jobWith("failed", readmeta.PresenceAbsent, true), policyUnknown, "unknown_policy"},
		{"canceled required", jobWith("canceled", readmeta.PresenceFalse, true), policyBlock, "canceled_required"},
		{"cancelled optional", jobWith("cancelled", readmeta.PresenceTrue, true), policyPass, ""},
		{"canceled omitted", jobWith("canceled", readmeta.PresenceAbsent, true), policyUnknown, "unknown_policy"},
		{"running required", jobWith("running", readmeta.PresenceFalse, true), policyPartial, "in_progress"},
		{"pending optional", jobWith("pending", readmeta.PresenceTrue, true), policyPartial, "in_progress"},
		{"running omitted", jobWith("running", readmeta.PresenceAbsent, true), policyUnknown, "unknown_policy"},
		{"success omitted", jobWith("success", readmeta.PresenceAbsent, true), policyPass, ""},
		{"success false", jobWith("success", readmeta.PresenceFalse, true), policyPass, ""},
		{"unknown status", jobWith("mystery", readmeta.PresenceFalse, true), policyUnknown, "unknown_status"},
		{"missing status", graphJob{Allow: readmeta.PresenceFalse, AllowValid: true}, policyUnknown, "unknown_status"},
		{"skipped known", jobWith("skipped", readmeta.PresenceFalse, true), policyPass, ""},
		{"skipped omitted", jobWith("skipped", readmeta.PresenceAbsent, true), policyUnknown, "unknown_policy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jobPolicy(tc.job)
			if got.Outcome != tc.outcome || got.Reason != tc.reason {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestAssessParentNeverReady(t *testing.T) {
	pass := []policyOutcome{{Outcome: policyPass}}
	cases := []struct {
		name string
		in   assessInput
		want string
	}{
		{"all pass still unknown", assessInput{RelationProven: true, Outcomes: pass}, assessUnknown},
		{"required manual blocks", assessInput{RelationProven: true, Outcomes: []policyOutcome{{Outcome: policyBlock, Reason: "required_manual"}}}, assessBlocked},
		{"optional manual does not block", assessInput{RelationProven: true, Outcomes: pass}, assessUnknown},
		{"unknown policy", assessInput{RelationProven: true, Outcomes: []policyOutcome{{Outcome: policyUnknown, Reason: "unknown_policy"}}}, assessUnknown},
		{"missing pipeline", assessInput{PipelineMissing: true}, assessUnknown},
		{"filter", assessInput{Filter: true, RelationProven: true, Outcomes: pass}, assessUnknown},
		{"partial jobs", assessInput{JobsPartial: true, RelationProven: true, Outcomes: pass}, assessPartial},
		{"running", assessInput{RelationProven: true, Outcomes: []policyOutcome{{Outcome: policyPartial, Reason: "in_progress"}}}, assessPartial},
		{"block wins over partial", assessInput{JobsPartial: true, Outcomes: []policyOutcome{{Outcome: policyBlock, Reason: "failed_required"}}}, assessBlocked},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reasons := assessParent(tc.in)
			if got == "ready" || got != tc.want {
				t.Fatalf("assessment %q reasons %v", got, reasons)
			}
			if !containsExact(reasons, "downstream_unknown") {
				t.Fatalf("reasons %v", reasons)
			}
		})
	}
}

func TestAssessGraphReadyRequiresCompleteDownstream(t *testing.T) {
	pass := []policyOutcome{{Outcome: policyPass}}
	got, reasons := assessGraph(assessInput{
		RelationProven: true,
		Outcomes:       pass,
		Downstream:     downstreamCoverageComplete,
	})
	if got != assessReady {
		t.Fatalf("complete graph %q reasons %v", got, reasons)
	}
	got, _ = assessGraph(assessInput{
		RelationProven: true,
		Outcomes:       pass,
		Downstream:     downstreamCoverageComplete,
		UnseenEdge:     true,
	})
	if got == assessReady {
		t.Fatal("unseen edge must not be ready")
	}
	got, _ = assessGraph(assessInput{
		RelationProven: true,
		Outcomes:       pass,
		Downstream:     downstreamCoveragePartial,
	})
	if got != assessPartial {
		t.Fatalf("partial coverage %q", got)
	}
	got, _ = assessGraph(assessInput{
		RelationProven: true,
		Outcomes:       []policyOutcome{{Outcome: policyBlock, Reason: "failed_required"}},
		Downstream:     downstreamCoverageComplete,
	})
	if got != assessBlocked {
		t.Fatalf("child block %q", got)
	}
}

func TestClassifyRelationEvidence(t *testing.T) {
	const (
		pipelineSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		otherSHA    = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	cases := []struct {
		name   string
		in     relationInput
		kind   string
		proven bool
	}{
		{
			name:   "sha equal alone",
			in:     relationInput{PipelineSHA: pipelineSHA, ExpectedSHA: pipelineSHA},
			kind:   relUnproven,
			proven: false,
		},
		{
			name:   "sha different alone",
			in:     relationInput{PipelineSHA: otherSHA, ExpectedSHA: pipelineSHA},
			kind:   relUnproven,
			proven: false,
		},
		{
			name: "merged result ref",
			in: relationInput{
				MRIID: 7, ListChecked: true, Linked: true, ListExhausted: true,
				Ref: "refs/merge-requests/7/merge", PipelineSHA: otherSHA, ExpectedSHA: pipelineSHA,
			},
			kind:   relMergedResult,
			proven: true,
		},
		{
			name: "branch ref despite different sha",
			in: relationInput{
				MRIID: 7, SourceBranch: "feature", ListChecked: true, Linked: true,
				Ref: "feature", PipelineSHA: otherSHA, ExpectedSHA: pipelineSHA,
			},
			kind:   relBranch,
			proven: true,
		},
		{
			name: "merge request event source is not proof",
			in: relationInput{
				PipelineSHA: pipelineSHA, ExpectedSHA: pipelineSHA,
			},
			kind:   relUnproven,
			proven: false,
		},
		{
			name: "head ref",
			in: relationInput{
				MRIID: 7, ListChecked: true, Linked: true, Ref: "refs/merge-requests/7/head",
				PipelineSHA: pipelineSHA, ExpectedSHA: pipelineSHA,
			},
			kind:   relMRHead,
			proven: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyRelation(tc.in)
			if got.Kind != tc.kind || got.Proven != tc.proven {
				t.Fatalf("%+v", got)
			}
			for _, ev := range got.Evidence {
				if ev == "sha" || ev == "merge_request_event" {
					t.Fatalf("manufactured evidence %q", ev)
				}
			}
			if tc.name == "sha equal alone" && got.SHAComparison != shaEqual {
				t.Fatalf("comparison %s", got.SHAComparison)
			}
			if tc.name == "sha different alone" && got.SHAComparison != shaDifferent {
				t.Fatalf("comparison %s", got.SHAComparison)
			}
			if tc.name == "merged result ref" && got.SHAComparison != shaDifferent {
				t.Fatalf("different SHA must stay visible: %s", got.SHAComparison)
			}
		})
	}
}

func TestLineageKeepsHistory(t *testing.T) {
	// No retried field: the greater id is the latest attempt.
	jobs := []graphJob{
		{ID: 2, Name: "test", NameKnown: true, Status: "success", StatusKnown: true, Allow: readmeta.PresenceFalse, AllowValid: true},
		{ID: 1, Name: "test", NameKnown: true, Status: "failed", StatusKnown: true, Allow: readmeta.PresenceFalse, AllowValid: true},
	}
	groups := buildLineage(jobs, lineageCarry{})
	if len(groups) != 1 || !groups[0].LatestKnown || groups[0].LatestIDs[0] != 2 || len(groups[0].HistoryIDs) != 1 || groups[0].HistoryIDs[0] != 1 {
		t.Fatalf("%+v", groups[0])
	}
	if groups[0].Attempts[1] != attemptHistory || groups[0].Attempts[2] != attemptLatest {
		t.Fatalf("attempts %+v", groups[0].Attempts)
	}
	assessment, _ := assessParent(assessInput{RelationProven: true, Outcomes: groups[0].Outcomes})
	if assessment == assessBlocked || assessment == "ready" {
		t.Fatalf("older failure blocked the latest success: %s", assessment)
	}

	failedLatest := []graphJob{
		{ID: 3, Name: "test", NameKnown: true, Status: "success", StatusKnown: true, Allow: readmeta.PresenceFalse, AllowValid: true},
		{ID: 4, Name: "test", NameKnown: true, Status: "failed", StatusKnown: true, Allow: readmeta.PresenceFalse, AllowValid: true},
	}
	groups = buildLineage(failedLatest, lineageCarry{})
	assessment, _ = assessParent(assessInput{RelationProven: true, Outcomes: groups[0].Outcomes})
	if assessment != assessBlocked {
		t.Fatalf("latest failure must block: %s", assessment)
	}

	olderPage := []graphJob{
		{ID: 1, Name: "test", NameKnown: true, Status: "failed", StatusKnown: true, Allow: readmeta.PresenceFalse, AllowValid: true},
	}
	groups = buildLineage(olderPage, lineageCarry{max: map[string]int64{jobNameKey("test"): 2}})
	if groups[0].LatestKnown || groups[0].Attempts[1] != attemptHistory {
		t.Fatalf("older page treated as latest: %+v", groups[0])
	}
	assessment, _ = assessParent(assessInput{RelationProven: true, Outcomes: groups[0].Outcomes})
	if assessment == assessBlocked {
		t.Fatal("older attempt on a later page blocked after the latest passed")
	}
}

func TestParseGraphJobPresence(t *testing.T) {
	omitted, err := parseGraphJob([]byte(`{"id":9,"name":"manual-job","status":"manual","retried":false}`))
	if err != nil || omitted.Allow != readmeta.PresenceAbsent {
		t.Fatalf("omitted %+v err %v", omitted, err)
	}
	explicit, err := parseGraphJob([]byte(`{"id":9,"name":"manual-job","status":"manual","allow_failure":false,"retried":true}`))
	if err != nil || explicit.Allow != readmeta.PresenceFalse {
		t.Fatalf("explicit %+v err %v", explicit, err)
	}
	if _, err := parseGraphJob([]byte(`{"name":"x"}`)); err == nil {
		t.Fatal("missing id")
	}
}

func TestScopedLineagePolicyDoesNotCrossPipelines(t *testing.T) {
	sha, ref, status, source := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "feature", "success", "push"
	pipe := &pipelineView{ID: 100, SHA: &sha, Ref: &ref, Status: &status, StatusKnown: true, Source: &source}
	w := newGraphWalk(pipe, "42", graphDefaultMaxDepth, graphDefaultMaxNodes)
	rootFail := []graphJob{{
		ID: 1, Name: "test", NameKnown: true,
		Status: "failed", StatusKnown: true,
		Allow: readmeta.PresenceFalse, AllowValid: true,
	}}
	w.current = graphNodeKey{Project: "42", Pipeline: 100}
	w.applyLineageOutcomes(buildLineage(rootFail, lineageCarry{}))
	childOK := []graphJob{{
		ID: 9, Name: "test", NameKnown: true,
		Status: "success", StatusKnown: true,
		Allow: readmeta.PresenceFalse, AllowValid: true,
	}}
	w.current = graphNodeKey{Project: "99", Pipeline: 200}
	w.applyLineageOutcomes(buildLineage(childOK, lineageCarry{}))
	w.syncPolicyFlagsFromLineage()
	if !w.block {
		t.Fatal("root required failure must stay blocked after child success with same job name")
	}
}

func TestBridgeLineageDistinctNamesAcrossCarry(t *testing.T) {
	carry := lineageCarry{max: map[string]int64{jobNameKey("deploy-success"): 10}}
	failed := []graphJob{{
		ID: 5, Name: "deploy-fail", NameKnown: true,
		Status: "failed", StatusKnown: true,
		Allow: readmeta.PresenceFalse, AllowValid: true,
	}}
	groups := buildLineage(failed, carry)
	if len(groups) != 1 || !groups[0].LatestKnown {
		t.Fatalf("distinct name treated as history: %+v", groups[0])
	}
	assessment, _ := assessParent(assessInput{RelationProven: true, Outcomes: groups[0].Outcomes})
	if assessment != assessBlocked {
		t.Fatalf("required failed bridge must block: %s", assessment)
	}
}

func TestLineageCarryCapsFingerprints(t *testing.T) {
	jobs := make([]graphJob, 0, lineageFPCap+2)
	for i := 0; i < lineageFPCap+2; i++ {
		jobs = append(jobs, graphJob{ID: int64(i + 1), Name: fmt.Sprintf("name-%d", i), NameKnown: true})
	}
	carry := mergeLineageCarry(lineageCarry{}, jobs)
	if !carry.saturated || len(carry.max) != lineageFPCap {
		t.Fatalf("saturated %v len %d", carry.saturated, len(carry.max))
	}
	enc := encodeLineageCarry(carry)
	if len(enc) != lineageFPCap+1 || enc[0] != "*" {
		t.Fatalf("encoded %#v", enc)
	}
	got, err := decodeLineageCarry(enc)
	if err != nil || !got.saturated || len(got.max) != lineageFPCap {
		t.Fatalf("decode %+v err %v", got, err)
	}
	if _, err := decodeLineageCarry([]string{"2 test"}); err == nil {
		t.Fatal("name token accepted")
	}
	unseen := []graphJob{{ID: 99, Name: "brand-new", NameKnown: true, Status: "failed", StatusKnown: true, Allow: readmeta.PresenceFalse, AllowValid: true}}
	groups := buildLineage(unseen, carry)
	if groups[0].LatestKnown || groups[0].Attempts[99] != attemptUnknown {
		t.Fatalf("saturated unseen %+v", groups[0])
	}
	if len(groups[0].Outcomes) != 1 || groups[0].Outcomes[0].Outcome != policyUnknown {
		t.Fatalf("saturated unseen must stay unknown, not block: %+v", groups[0])
	}
}

func TestFilterCanonicalDoesNotCollapseSentinel(t *testing.T) {
	empty, err := normalizeJobFilter(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		names    []string
		stages   []string
		statuses []string
	}{
		{"name", []string{"-"}, nil, nil},
		{"stage", nil, []string{"-"}, nil},
		{"status", nil, nil, []string{"-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeJobFilter(tc.names, tc.stages, tc.statuses)
			if err != nil {
				t.Fatal(err)
			}
			if filterCanonical(empty) == filterCanonical(got) {
				t.Fatalf("empty and sentinel encode the same: %s", filterCanonical(empty))
			}
		})
	}
}
