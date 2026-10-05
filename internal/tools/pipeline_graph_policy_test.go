package tools

import (
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
	groups := buildLineage(jobs)
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
	groups = buildLineage(failedLatest)
	assessment, _ = assessParent(assessInput{RelationProven: true, Outcomes: groups[0].Outcomes})
	if assessment != assessBlocked {
		t.Fatalf("latest failure must block: %s", assessment)
	}
}

func TestParseGraphJobPresence(t *testing.T) {
	omitted, err := parseGraphJob([]byte(`{"id":9,"name":"manual-job","status":"manual"}`))
	if err != nil || omitted.Allow != readmeta.PresenceAbsent || omitted.Retried != readmeta.PresenceAbsent {
		t.Fatalf("omitted %+v err %v", omitted, err)
	}
	explicit, err := parseGraphJob([]byte(`{"id":9,"name":"manual-job","status":"manual","allow_failure":false,"retried":true}`))
	if err != nil || explicit.Allow != readmeta.PresenceFalse || explicit.Retried != readmeta.PresenceTrue {
		t.Fatalf("explicit %+v err %v", explicit, err)
	}
	if _, err := parseGraphJob([]byte(`{"name":"x"}`)); err == nil {
		t.Fatal("missing id")
	}
}
