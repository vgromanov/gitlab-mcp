package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	igl "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitlab"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/tools/readmeta"
)

const (
	policyPass    = "pass"
	policyBlock   = "block"
	policyPartial = "partial"
	policyUnknown = "unknown"

	assessBlocked = "blocked"
	assessPartial = "partial"
	assessUnknown = "unknown"

	downstreamCoverageUnknown = "unknown"

	attemptLatest  = "latest"
	attemptHistory = "history"
	attemptUnknown = "unknown"

	relMergedResult = "merged_result"
	relBranch       = "branch"
	relMRHead       = "merge_request_head"
	relUnproven     = "unproven"

	shaEqual     = "equal"
	shaDifferent = "different"
	shaUnknown   = "unknown"
)

// graphJob is one parent-pipeline job decoded without SDK bool erasure.
type graphJob struct {
	ID           int64
	Name         string
	NameKnown    bool
	Stage        string
	StageKnown   bool
	Status       string
	StatusKnown  bool
	Allow        readmeta.Presence
	AllowValid   bool
	Retried      readmeta.Presence
	RetriedValid bool
}

type policyOutcome struct {
	Outcome string
	Reason  string
}

type lineageGroup struct {
	Name        *string
	LatestKnown bool
	LatestIDs   []int64
	HistoryIDs  []int64
	Attempts    map[int64]string
	Outcomes    []policyOutcome
}

type relationResult struct {
	Kind          string
	Proven        bool
	Evidence      []string
	SHAComparison string
}

type assessInput struct {
	PipelineMissing   bool
	PipelineAmbiguous bool
	Filter            bool
	JobsPartial       bool
	RelationProven    bool
	Outcomes          []policyOutcome
}

// parseGraphJob decodes one raw job object. Malformed identity is an error.
// A present non-bool allow_failure stays unknown rather than false.
// GitLab pipeline jobs have no retried field; a decoded value is recorded and
// is not used to choose the latest attempt.
func parseGraphJob(raw json.RawMessage) (graphJob, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return graphJob{}, fmt.Errorf("malformed job")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return graphJob{}, fmt.Errorf("malformed job")
	}
	id, ok := positiveJSONID(fields["id"])
	if !ok {
		return graphJob{}, fmt.Errorf("malformed job")
	}
	job := graphJob{ID: id, AllowValid: true, RetriedValid: true}
	if name, known, ok := jsonStringField(fields, "name"); ok {
		job.Name, job.NameKnown = name, known
	} else {
		return graphJob{}, fmt.Errorf("malformed job")
	}
	if stage, known, ok := jsonStringField(fields, "stage"); ok {
		job.Stage, job.StageKnown = stage, known
	} else {
		return graphJob{}, fmt.Errorf("malformed job")
	}
	if status, known, ok := jsonStringField(fields, "status"); ok {
		job.Status, job.StatusKnown = status, known
	} else {
		return graphJob{}, fmt.Errorf("malformed job")
	}
	allow, err := igl.AllowFailurePresence(trimmed)
	if err != nil {
		job.AllowValid = false
	} else {
		job.Allow = allow
	}
	retried, err := readmeta.DecodeBoolPresence(trimmed, "retried")
	if err != nil {
		job.RetriedValid = false
	} else {
		job.Retried = retried
	}
	return job, nil
}

func positiveJSONID(raw json.RawMessage) (int64, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil || n < 1 {
		return 0, false
	}
	// Reject non-integers such as 1.5, which Unmarshal into int64 truncates.
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil || float64(n) != f {
		return 0, false
	}
	return n, true
}

// jsonStringField reports (value, known, typeOK). Absent and null are known=false.
func jsonStringField(fields map[string]json.RawMessage, key string) (string, bool, bool) {
	raw, ok := fields[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", false, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, false
	}
	if strings.TrimSpace(s) == "" {
		return "", false, true
	}
	return s, true, true
}

// jobPolicy maps one observed job to pass, block, partial, or unknown.
// Success does not consult allow_failure. Every other status with missing
// allow_failure is unknown. Status manual is the unplayed manual state.
func jobPolicy(job graphJob) policyOutcome {
	if !job.StatusKnown {
		return policyOutcome{Outcome: policyUnknown, Reason: "unknown_status"}
	}
	status := strings.ToLower(strings.TrimSpace(job.Status))
	allowKnown := job.AllowValid && (job.Allow == readmeta.PresenceTrue || job.Allow == readmeta.PresenceFalse)
	switch status {
	case "success", "successful":
		return policyOutcome{Outcome: policyPass}
	case "skipped":
		if !allowKnown {
			return policyOutcome{Outcome: policyUnknown, Reason: "unknown_policy"}
		}
		return policyOutcome{Outcome: policyPass}
	case "failed":
		return allowTerminal(job, allowKnown, "failed_required")
	case "canceled", "cancelled":
		return allowTerminal(job, allowKnown, "canceled_required")
	case "manual":
		return allowTerminal(job, allowKnown, "required_manual")
	case "running", "pending", "created", "preparing", "scheduled", "waiting_for_resource", "waiting_for_callback", "canceling":
		if !allowKnown {
			return policyOutcome{Outcome: policyUnknown, Reason: "unknown_policy"}
		}
		return policyOutcome{Outcome: policyPartial, Reason: "in_progress"}
	default:
		return policyOutcome{Outcome: policyUnknown, Reason: "unknown_status"}
	}
}

func allowTerminal(job graphJob, allowKnown bool, blockReason string) policyOutcome {
	if !job.AllowValid || job.Allow == readmeta.PresenceAbsent || job.Allow == readmeta.PresenceNull {
		return policyOutcome{Outcome: policyUnknown, Reason: "unknown_policy"}
	}
	if !allowKnown {
		return policyOutcome{Outcome: policyUnknown, Reason: "unknown_policy"}
	}
	if job.Allow == readmeta.PresenceFalse {
		return policyOutcome{Outcome: policyBlock, Reason: blockReason}
	}
	return policyOutcome{Outcome: policyPass}
}

// buildLineage groups jobs by name, keeps every attempt, and emits policy
// outcomes for the latest attempt only. History is never dropped.
// The latest attempt is the greatest job id in the group. GitLab's jobs API
// does not send retried; include_retried only adds older attempts, and a
// retry is a new job with a higher id.
func buildLineage(jobs []graphJob) []lineageGroup {
	order := []string{}
	byKey := map[string][]graphJob{}
	for _, job := range jobs {
		key := "id:" + strconv.FormatInt(job.ID, 10)
		if job.NameKnown {
			key = "name:" + job.Name
		}
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = append(byKey[key], job)
	}
	out := make([]lineageGroup, 0, len(order))
	for _, key := range order {
		out = append(out, lineageOne(byKey[key]))
	}
	return out
}

func lineageOne(jobs []graphJob) lineageGroup {
	g := lineageGroup{Attempts: map[int64]string{}, LatestIDs: []int64{}, HistoryIDs: []int64{}}
	if len(jobs) == 0 {
		return g
	}
	if jobs[0].NameKnown {
		name := jobs[0].Name
		g.Name = &name
	}
	latest := jobs[0]
	for _, job := range jobs[1:] {
		if job.ID > latest.ID {
			latest = job
		}
	}
	g.LatestKnown = true
	g.LatestIDs = []int64{latest.ID}
	g.Attempts[latest.ID] = attemptLatest
	g.Outcomes = append(g.Outcomes, jobPolicy(latest))
	for _, job := range jobs {
		if job.ID == latest.ID {
			continue
		}
		g.HistoryIDs = append(g.HistoryIDs, job.ID)
		g.Attempts[job.ID] = attemptHistory
	}
	sort.Slice(g.HistoryIDs, func(i, j int) bool { return g.HistoryIDs[i] < g.HistoryIDs[j] })
	return g
}

// assessParent never returns ready. Downstream coverage is unknown until
// bridge traversal exists, and a visible required failure is still blocked.
func assessParent(in assessInput) (string, []string) {
	reasons := []string{"downstream_unknown"}
	if in.PipelineMissing {
		return assessUnknown, sortReasons(append(reasons, "pipeline_missing"))
	}
	if in.PipelineAmbiguous {
		return assessUnknown, sortReasons(append(reasons, "pipeline_ambiguous"))
	}
	if in.Filter {
		reasons = append(reasons, "job_filter")
	}
	if in.JobsPartial {
		reasons = append(reasons, "jobs_partial")
	}
	if !in.RelationProven {
		reasons = append(reasons, "relation_unproven")
	}
	hasBlock, hasPartial := false, false
	for _, o := range in.Outcomes {
		switch o.Outcome {
		case policyBlock:
			hasBlock = true
			if o.Reason != "" {
				reasons = append(reasons, o.Reason)
			}
		case policyPartial:
			hasPartial = true
			if o.Reason != "" {
				reasons = append(reasons, o.Reason)
			}
		case policyUnknown:
			if o.Reason != "" {
				reasons = append(reasons, o.Reason)
			}
		}
	}
	switch {
	case hasBlock:
		return assessBlocked, sortReasons(reasons)
	case in.JobsPartial || hasPartial:
		return assessPartial, sortReasons(reasons)
	default:
		return assessUnknown, sortReasons(reasons)
	}
}

func sortReasons(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, r := range in {
		if r == "" {
			continue
		}
		if _, ok := seen[r]; ok {
			continue
		}
		seen[r] = struct{}{}
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

type relationInput struct {
	MRIID         int64
	SourceBranch  string
	ListChecked   bool
	Linked        bool
	ListExhausted bool
	Ref           string
	PipelineSHA   string
	ExpectedSHA   string
}

// classifyRelation proves branch or merged-result only from MR-list membership
// plus an exact ref. SHA comparison and pipeline source are not evidence.
func classifyRelation(in relationInput) relationResult {
	res := relationResult{
		Kind:          relUnproven,
		SHAComparison: compareSHA(in.PipelineSHA, in.ExpectedSHA),
		Evidence:      []string{},
	}
	if !in.ListChecked {
		return res
	}
	if !in.Linked {
		if in.ListExhausted {
			res.Evidence = append(res.Evidence, "absent_from_mr_pipeline_list")
		} else {
			res.Evidence = append(res.Evidence, "mr_pipeline_list_partial")
		}
		return res
	}
	res.Evidence = append(res.Evidence, "mr_pipeline_list")
	if in.MRIID > 0 && in.Ref == fmt.Sprintf("refs/merge-requests/%d/merge", in.MRIID) {
		res.Kind = relMergedResult
		res.Proven = true
		res.Evidence = append(res.Evidence, "ref_merge_result")
		return res
	}
	if in.MRIID > 0 && in.Ref == fmt.Sprintf("refs/merge-requests/%d/head", in.MRIID) {
		res.Kind = relMRHead
		res.Proven = true
		res.Evidence = append(res.Evidence, "ref_merge_request_head")
		return res
	}
	if in.SourceBranch != "" && in.Ref == in.SourceBranch {
		res.Kind = relBranch
		res.Proven = true
		res.Evidence = append(res.Evidence, "ref_source_branch")
		return res
	}
	return res
}

func compareSHA(pipelineSHA, expected string) string {
	got, okGot := readmeta.ObservedHeadSHA(pipelineSHA)
	want, okWant := readmeta.ObservedHeadSHA(expected)
	if !okGot || !okWant {
		return shaUnknown
	}
	if got == want {
		return shaEqual
	}
	return shaDifferent
}

type jobFilter struct {
	Names    []string
	Stages   []string
	Statuses []string
}

func (f jobFilter) active() bool {
	return len(f.Names) > 0 || len(f.Stages) > 0 || len(f.Statuses) > 0
}

func (f jobFilter) match(job graphJob) bool {
	if len(f.Names) > 0 && !containsExact(f.Names, job.Name) {
		return false
	}
	if len(f.Stages) > 0 && !containsExact(f.Stages, job.Stage) {
		return false
	}
	if len(f.Statuses) > 0 {
		if !job.StatusKnown || !containsExact(f.Statuses, strings.ToLower(job.Status)) {
			return false
		}
	}
	return true
}

func containsExact(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// normalizeJobFilter sorts unique tokens. Statuses are lowercased.
// Tokens containing a separator are rejected so the cursor binding stays exact.
func normalizeJobFilter(names, stages, statuses []string) (jobFilter, error) {
	n, err := normTokens(names, false)
	if err != nil {
		return jobFilter{}, err
	}
	s, err := normTokens(stages, false)
	if err != nil {
		return jobFilter{}, err
	}
	st, err := normTokens(statuses, true)
	if err != nil {
		return jobFilter{}, err
	}
	return jobFilter{Names: n, Stages: s, Statuses: st}, nil
}

func normTokens(in []string, lower bool) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			return nil, fmt.Errorf("empty job filter")
		}
		if strings.ContainsAny(s, ",;|\n") {
			return nil, fmt.Errorf("job filter contains a reserved separator")
		}
		if lower {
			s = strings.ToLower(s)
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func filterCanonical(f jobFilter) string {
	return "names=" + canonJoin(f.Names) + ";stages=" + canonJoin(f.Stages) + ";statuses=" + canonJoin(f.Statuses)
}

func canonJoin(xs []string) string {
	if len(xs) == 0 {
		return "-"
	}
	return strings.Join(xs, ",")
}
