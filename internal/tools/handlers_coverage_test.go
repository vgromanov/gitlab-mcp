package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/config"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/testutil"

	gitlab "gitlab.com/gitlab-org/api/client-go/v2"
)

func gitlabAPIStub(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		w.Header().Set("Content-Type", "application/json")

		switch {
		case path == "/api/graphql":
			_, _ = io.WriteString(w, `{"data":{"ok":true}}`)
		case strings.Contains(path, "/raw") || strings.Contains(path, "/trace") ||
			strings.Contains(path, "/artifacts") || strings.Contains(path, "/downloads") ||
			strings.Contains(path, "/markdown_uploads/"):
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, "blob-bytes")
		case strings.Contains(path, "/repository/files/") && r.Method != http.MethodGet:
			_, _ = io.WriteString(w, `{"file_path":"f.go","branch":"main"}`)
		case strings.Contains(path, "/repository/compare"):
			_, _ = io.WriteString(w, `{"diffs":[{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\nline\n"}]}`)
		case strings.Contains(path, "/repository/commits") && r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"id":"abc","short_id":"abc","title":"msg"}`)
		case strings.Contains(path, "/merge_requests") && strings.Contains(path, "/diffs"):
			_, _ = io.WriteString(w, `[{"old_path":"a.go","new_path":"a.go","diff":"<<<<<<< HEAD\nfoo\n=======\nbar\n>>>>>>> branch\n"}]`)
		case strings.Contains(path, "/approvals") || strings.Contains(path, "/approval_state"):
			_, _ = io.WriteString(w, `{"approved":true,"approvals_required":1}`)
		case strings.Contains(path, "/users"):
			_, _ = io.WriteString(w, `[{"id":1,"username":"alice"}]`)
		case r.Method == http.MethodDelete:
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && (looksLikeListAPI(path) || strings.HasSuffix(path, "/diff") || strings.HasSuffix(path, "/diffs")):
			_, _ = io.WriteString(w, `[]`)
		case strings.Contains(path, "/merge_requests/") && r.Method == http.MethodGet &&
			!strings.Contains(path, "/notes") && !strings.Contains(path, "/discussions") &&
			!strings.Contains(path, "/draft_notes") && !strings.Contains(path, "/versions") &&
			!strings.Contains(path, "/diffs") && !strings.Contains(path, "/approvals"):
			_, _ = io.WriteString(w, `{"id":1,"iid":1,"title":"t","has_conflicts":true,"detailed_merge_status":"conflict"}`)
		case strings.Contains(path, "/notes") || strings.Contains(path, "/draft_notes"):
			_, _ = io.WriteString(w, `{"id":1,"body":"n","note":"n"}`)
		case strings.Contains(path, "/discussions") && (r.Method == http.MethodPost || r.Method == http.MethodPut):
			_, _ = io.WriteString(w, `{"id":"d1","individual_note":true,"notes":[{"id":1,"body":"n","type":"DiscussionNote"}]}`)
		case strings.Contains(path, "/discussions/") && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"id":"d1","individual_note":false,"notes":[{"id":1,"body":"n"}]}`)
		case strings.Contains(path, "/repository/commits"):
			_, _ = io.WriteString(w, `{"id":"abc","short_id":"abc","title":"t","message":"m"}`)
		case strings.Contains(path, "/repository/branches"):
			_, _ = io.WriteString(w, `{"name":"f","commit":{"id":"abc","short_id":"abc"}}`)
		default:
			_, _ = io.WriteString(w, `{
				"id":1,"iid":1,"title":"t","name":"n","username":"alice","sha":"abc",
				"status":"success","tag_name":"v1","path_with_namespace":"g/p","full_path":"g/p",
				"web_url":"http://x","content":"","slug":"home","body":"n","note":"n",
				"diff_refs":{"base_sha":"a","head_sha":"b","start_sha":"a"},
				"assets":{"links":[{"name":"bin","url":"REPLACE","direct_asset_url":"REPLACE"}]},
				"artifacts":[],"artifacts_file":{"filename":"a.zip"}
			}`)
		}
	})
}

// looksLikeListAPI is true when the final path segment is a collection name.
func looksLikeListAPI(path string) bool {
	collections := map[string]struct{}{
		"issues": {}, "projects": {}, "namespaces": {}, "search": {}, "events": {},
		"labels": {}, "pipelines": {}, "jobs": {}, "deployments": {}, "environments": {},
		"milestones": {}, "wikis": {}, "releases": {}, "hooks": {}, "members": {},
		"tree": {}, "commits": {}, "discussions": {}, "notes": {}, "draft_notes": {},
		"versions": {}, "iterations": {}, "merge_requests": {}, "links": {},
		"bridges": {}, "burndown_events": {}, "users": {}, "diff": {}, "diffs": {},
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 {
		return false
	}
	_, ok := collections[parts[len(parts)-1]]
	return ok
}

func testDeps(t *testing.T) Deps {
	t.Helper()
	cli, _ := testutil.NewGitLabClient(t, gitlabAPIStub(t))
	return Deps{
		Config: &config.Config{
			Token:            "test-token",
			DefaultProjectID: "42",
			Wiki:             true,
			Milestone:        true,
			Pipeline:         true,
		},
		Client: cli,
	}
}

func ptr[T any](v T) *T { return &v }

func TestHandlers_coverageHappyPaths(t *testing.T) {
	d := testDeps(t)
	ctx := context.Background()
	ok := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	fail := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}

	state, scope, search, ref, st, desc := "opened", "all", "foo", "main", "success", "d"
	boolT, msg := true, "merge msg"
	pidMR1 := pidMR{ProjectID: "42", MergeRequestIID: 1}
	pidIss1 := pidIssue{ProjectID: "42", IssueIID: 1}

	_, _, err := listIssues(ctx, nil, listIssuesIn{State: &state, Scope: &scope, Search: &search}, d)
	ok("listIssues", err)
	_, _, err = myIssues(ctx, nil, myIssuesIn{State: &state}, d)
	ok("myIssues", err)
	_, _, err = listProjectIssues(ctx, nil, listProjectIssuesIn{ProjectID: "42", Labels: "bug", Search: &search, State: &state}, d)
	ok("listProjectIssues", err)
	_, _, err = getIssue(ctx, nil, getIssueIn{pidIssue: pidIss1}, d)
	ok("getIssue", err)
	_, _, err = createIssue(ctx, nil, createIssueIn{ProjectID: "42", Title: "t", Description: &desc, Labels: []string{"bug"}}, d)
	ok("createIssue", err)
	_, _, err = updateIssue(ctx, nil, updateIssueIn{pidIssue: pidIss1, Title: &desc, StateEvent: &state}, d)
	ok("updateIssue", err)
	_, _, err = deleteIssue(ctx, nil, deleteIssueIn{pidIssue: pidIss1}, d)
	ok("deleteIssue", err)
	_, _, err = listIssueLinks(ctx, nil, listIssueLinksIn{pidIssue: pidIss1}, d)
	ok("listIssueLinks", err)
	_, _, err = getIssueLink(ctx, nil, getIssueLinkIn{pidIssue: pidIss1, IssueLinkID: 9}, d)
	ok("getIssueLink", err)
	_, _, err = createIssueLink(ctx, nil, createIssueLinkIn{pidIssue: pidIss1, TargetProjectID: "42", TargetIssueIID: "2", LinkType: ptr("relates_to")}, d)
	ok("createIssueLink", err)
	_, _, err = deleteIssueLink(ctx, nil, deleteIssueLinkIn{pidIssue: pidIss1, IssueLinkID: 9}, d)
	ok("deleteIssueLink", err)

	_, _, err = listIssueDiscussions(ctx, nil, listIssueDiscussionsIn{pidIssue: pidIss1}, d)
	ok("listIssueDiscussions", err)
	_, _, err = createIssueNote(ctx, nil, createIssueNoteIn{pidIssue: pidIss1, DiscussionID: "d1", Body: "hi"}, d)
	ok("createIssueNote", err)
	_, _, err = updateIssueNote(ctx, nil, updateIssueNoteIn{pidIssue: pidIss1, DiscussionID: "d1", NoteID: 3, Body: "upd"}, d)
	ok("updateIssueNote", err)

	_, _, err = mergeMergeRequest(ctx, nil, mergeMergeRequestIn{pidMR: pidMR1, MergeCommitMessage: &msg, ShouldRemoveSourceBranch: &boolT}, d)
	ok("mergeMergeRequest", err)
	_, _, err = createMergeRequest(ctx, nil, createMergeRequestIn{ProjectID: "42", SourceBranch: "f", TargetBranch: "main", Title: "t", Description: &desc, RemoveSourceBranch: &boolT}, d)
	ok("createMergeRequest", err)
	_, _, err = getMergeRequest(ctx, nil, getMergeRequestIn{pidMR: pidMR1, IncludeRebaseInProgress: &boolT}, d)
	ok("getMergeRequest", err)
	_, _, err = getMergeRequestDiffs(ctx, nil, getMergeRequestDiffsIn{pidMR: pidMR1, TruncateLines: 1}, d)
	ok("getMergeRequestDiffs", err)
	_, _, err = listMergeRequestDiffs(ctx, nil, listMergeRequestDiffsIn{pidMR: pidMR1, Unidiff: &boolT}, d)
	ok("listMergeRequestDiffs", err)
	_, _, err = getMergeRequestConflicts(ctx, nil, getMergeRequestConflictsIn{pidMR: pidMR1}, d)
	ok("getMergeRequestConflicts", err)
	_, _, err = listMergeRequestChangedFiles(ctx, nil, listMergeRequestChangedFilesIn{pidMR: pidMR1, ExcludedFilePatterns: []string{"vendor/*", "exact"}}, d)
	ok("listMergeRequestChangedFiles", err)
	_, _, err = getMergeRequestFileDiff(ctx, nil, getMergeRequestFileDiffIn{pidMR: pidMR1, Files: []string{"a.go"}, TruncateLines: 2}, d)
	ok("getMergeRequestFileDiff", err)
	_, _, err = listMergeRequestVersions(ctx, nil, listMergeRequestVersionsIn{pidMR: pidMR1}, d)
	ok("listMergeRequestVersions", err)
	_, _, err = getMergeRequestVersion(ctx, nil, getMergeRequestVersionIn{pidMR: pidMR1, VersionID: 5}, d)
	ok("getMergeRequestVersion", err)
	_, _, err = updateMergeRequest(ctx, nil, updateMergeRequestIn{pidMR: pidMR1, Title: &desc, Description: &desc, TargetBranch: &ref, StateEvent: &state}, d)
	ok("updateMergeRequest", err)
	_, _, err = listMergeRequests(ctx, nil, listMergeRequestsIn{ProjectID: ptr("42"), State: &state}, d)
	ok("listMergeRequests project", err)
	_, _, err = listMergeRequests(ctx, nil, listMergeRequestsIn{GroupID: ptr("10"), State: &state, AuthorID: ptr(int64(1))}, d)
	ok("listMergeRequests group", err)
	_, _, err = listMergeRequests(ctx, nil, listMergeRequestsIn{State: &state}, d)
	ok("listMergeRequests global", err)
	_, _, err = approveMergeRequest(ctx, nil, approveMergeRequestIn{pidMR: pidMR1, SHA: ptr("abc")}, d)
	ok("approveMergeRequest", err)
	_, _, err = unapproveMergeRequest(ctx, nil, unapproveMergeRequestIn{pidMR: pidMR1}, d)
	ok("unapproveMergeRequest", err)
	_, _, err = getMergeRequestApprovalState(ctx, nil, getMergeRequestApprovalStateIn{pidMR: pidMR1}, d)
	ok("getMergeRequestApprovalState", err)
	_, _, err = mergeMergeRequest(ctx, nil, mergeMergeRequestIn{pidMR: pidMR{ProjectID: "42", MergeRequestIID: 0}}, d)
	fail("mergeMergeRequest bad iid", err)

	pos := &gitlab.PositionOptions{
		BaseSHA: gitlab.Ptr("a"), StartSHA: gitlab.Ptr("a"), HeadSHA: gitlab.Ptr("b"),
		NewPath: gitlab.Ptr("a.go"), NewLine: gitlab.Ptr(int64(1)), PositionType: gitlab.Ptr("text"),
	}
	_, _, err = createNote(ctx, nil, createNoteIn{ProjectID: "42", NoteableType: "merge_request", NoteableIID: 1, Body: "n", Internal: &boolT}, d)
	ok("createNote", err)
	_, _, err = createMergeRequestThread(ctx, nil, createMergeRequestThreadIn{pidMR: pidMR1, Body: "t", Position: pos}, d)
	ok("createMergeRequestThread", err)
	_, _, err = mrDiscussions(ctx, nil, mrDiscussionsIn{pidMR: pidMR1}, d)
	ok("mrDiscussions", err)
	_, _, err = resolveMergeRequestThread(ctx, nil, resolveMergeRequestThreadIn{pidMR: pidMR1, DiscussionID: "d1", Resolved: true}, d)
	ok("resolveMergeRequestThread", err)
	_, _, err = updateMergeRequestNote(ctx, nil, updateMergeRequestNoteIn{pidMR: pidMR1, NoteID: 2, Body: "u"}, d)
	ok("updateMergeRequestNote", err)
	_, _, err = createMergeRequestNote(ctx, nil, createMergeRequestNoteIn{pidMR: pidMR1, Body: "n"}, d)
	ok("createMergeRequestNote", err)
	_, _, err = deleteMergeRequestDiscussionNote(ctx, nil, deleteMergeRequestDiscussionNoteIn{pidMR: pidMR1, DiscussionID: "d1", NoteID: 2}, d)
	ok("deleteMergeRequestDiscussionNote", err)
	_, _, err = updateMergeRequestDiscussionNote(ctx, nil, updateMergeRequestDiscussionNoteIn{pidMR: pidMR1, DiscussionID: "d1", NoteID: 2, Body: "u"}, d)
	ok("updateMergeRequestDiscussionNote", err)
	_, _, err = createMergeRequestDiscussionNote(ctx, nil, createMergeRequestDiscussionNoteIn{pidMR: pidMR1, DiscussionID: "d1", Body: "n"}, d)
	ok("createMergeRequestDiscussionNote", err)
	_, _, err = deleteMergeRequestNote(ctx, nil, deleteMergeRequestNoteIn{pidMR: pidMR1, NoteID: 2}, d)
	ok("deleteMergeRequestNote", err)
	_, _, err = getMergeRequestNote(ctx, nil, getMergeRequestNoteIn{pidMR: pidMR1, NoteID: 2}, d)
	ok("getMergeRequestNote", err)
	_, _, err = getMergeRequestNotes(ctx, nil, getMergeRequestNotesIn{pidMR: pidMR1, Sort: ptr("asc")}, d)
	ok("getMergeRequestNotes", err)
	_, _, err = getMergeRequestDiscussion(ctx, nil, getMergeRequestDiscussionIn{pidMR: pidMR1, DiscussionID: "d1"}, d)
	ok("getMergeRequestDiscussion", err)

	_, _, err = getDraftNote(ctx, nil, getDraftNoteIn{pidMR: pidMR1, DraftNoteID: 1}, d)
	ok("getDraftNote", err)
	_, _, err = listDraftNotes(ctx, nil, listDraftNotesIn{pidMR: pidMR1}, d)
	ok("listDraftNotes", err)
	_, _, err = createDraftNote(ctx, nil, createDraftNoteIn{pidMR: pidMR1, Note: "n", Position: pos}, d)
	ok("createDraftNote", err)
	_, _, err = updateDraftNote(ctx, nil, updateDraftNoteIn{pidMR: pidMR1, DraftNoteID: 1, Note: "u"}, d)
	ok("updateDraftNote", err)
	_, _, err = deleteDraftNote(ctx, nil, deleteDraftNoteIn{pidMR: pidMR1, DraftNoteID: 1}, d)
	ok("deleteDraftNote", err)
	_, _, err = publishDraftNote(ctx, nil, publishDraftNoteIn{pidMR: pidMR1, DraftNoteID: 1}, d)
	ok("publishDraftNote", err)
	_, _, err = bulkPublishDraftNotes(ctx, nil, bulkPublishDraftNotesIn{pidMR: pidMR1}, d)
	ok("bulkPublishDraftNotes", err)

	_, _, err = listLabels(ctx, nil, listLabelsIn{ProjectID: "42", Search: &search}, d)
	ok("listLabels", err)
	_, _, err = getLabel(ctx, nil, getLabelIn{ProjectID: "42", LabelID: "bug"}, d)
	ok("getLabel", err)
	_, _, err = getLabel(ctx, nil, getLabelIn{ProjectID: "42", LabelID: "7"}, d)
	ok("getLabel numeric", err)
	_, _, err = createLabel(ctx, nil, createLabelIn{ProjectID: "42", Name: "bug", Color: "#f00", Description: &desc}, d)
	ok("createLabel", err)
	_, _, err = updateLabel(ctx, nil, updateLabelIn{ProjectID: "42", LabelID: "bug", NewName: ptr("bug2"), Color: ptr("#0f0")}, d)
	ok("updateLabel", err)
	_, _, err = deleteLabel(ctx, nil, deleteLabelIn{ProjectID: "42", LabelID: "bug"}, d)
	ok("deleteLabel", err)

	_, _, err = listPipelines(ctx, nil, listPipelinesIn{ProjectID: "42", Ref: &ref, Status: &st}, d)
	ok("listPipelines", err)
	_, _, err = getPipeline(ctx, nil, getPipelineIn{ProjectID: "42", PipelineID: 9}, d)
	ok("getPipeline", err)
	_, _, err = listPipelineJobs(ctx, nil, listPipelineJobsIn{ProjectID: "42", PipelineID: 9}, d)
	ok("listPipelineJobs", err)
	_, _, err = listPipelineTriggerJobs(ctx, nil, listPipelineTriggerJobsIn{ProjectID: "42", PipelineID: 9}, d)
	ok("listPipelineTriggerJobs", err)
	_, _, err = getPipelineJob(ctx, nil, getPipelineJobIn{ProjectID: "42", JobID: 3}, d)
	ok("getPipelineJob", err)
	_, _, err = getPipelineJobOutput(ctx, nil, getPipelineJobOutputIn{ProjectID: "42", JobID: 3, TruncateLines: 1}, d)
	ok("getPipelineJobOutput", err)
	_, _, err = createPipeline(ctx, nil, createPipelineIn{ProjectID: "42", Ref: "main", Variables: map[string]string{"A": "1"}}, d)
	ok("createPipeline", err)
	_, _, err = retryPipeline(ctx, nil, retryPipelineIn{ProjectID: "42", PipelineID: 9}, d)
	ok("retryPipeline", err)
	_, _, err = cancelPipeline(ctx, nil, cancelPipelineIn{ProjectID: "42", PipelineID: 9}, d)
	ok("cancelPipeline", err)
	_, _, err = playPipelineJob(ctx, nil, playPipelineJobIn{ProjectID: "42", JobID: 3}, d)
	ok("playPipelineJob", err)
	_, _, err = retryPipelineJob(ctx, nil, retryPipelineJobIn{ProjectID: "42", JobID: 3}, d)
	ok("retryPipelineJob", err)
	_, _, err = cancelPipelineJob(ctx, nil, cancelPipelineJobIn{ProjectID: "42", JobID: 3}, d)
	ok("cancelPipelineJob", err)

	_, _, err = listDeployments(ctx, nil, listDeploymentsIn{ProjectID: "42", Status: &st, Environment: ptr("prod")}, d)
	ok("listDeployments", err)
	_, _, err = getDeployment(ctx, nil, getDeploymentIn{ProjectID: "42", DeploymentID: 1}, d)
	ok("getDeployment", err)
	_, _, err = listEnvironments(ctx, nil, listEnvironmentsIn{ProjectID: "42", Search: &search}, d)
	ok("listEnvironments", err)
	_, _, err = getEnvironment(ctx, nil, getEnvironmentIn{ProjectID: "42", EnvironmentID: 1}, d)
	ok("getEnvironment", err)

	_, _, err = listJobArtifacts(ctx, nil, listJobArtifactsIn{ProjectID: "42", JobID: 3}, d)
	ok("listJobArtifacts", err)
	tmpZip := filepath.Join(t.TempDir(), "a.zip")
	_, _, err = downloadJobArtifacts(ctx, nil, downloadJobArtifactsIn{ProjectID: "42", JobID: 3, LocalPath: tmpZip}, d)
	ok("downloadJobArtifacts", err)
	_, _, err = getJobArtifactFile(ctx, nil, getJobArtifactFileIn{ProjectID: "42", JobID: 3, ArtifactPath: "out.txt", AsBase64: true}, d)
	ok("getJobArtifactFile b64", err)
	_, _, err = getJobArtifactFile(ctx, nil, getJobArtifactFileIn{ProjectID: "42", JobID: 3, ArtifactPath: "out.txt"}, d)
	ok("getJobArtifactFile text", err)

	_, _, err = listMilestones(ctx, nil, listMilestonesIn{ProjectID: "42", State: &state}, d)
	ok("listMilestones", err)
	_, _, err = getMilestone(ctx, nil, getMilestoneIn{ProjectID: "42", MilestoneID: 1}, d)
	ok("getMilestone", err)
	_, _, err = createMilestone(ctx, nil, createMilestoneIn{ProjectID: "42", Title: "m", Description: &desc, DueDate: ptr("2020-01-01")}, d)
	ok("createMilestone", err)
	_, _, err = editMilestone(ctx, nil, editMilestoneIn{ProjectID: "42", MilestoneID: 1, Title: &desc, StateEvent: &state, DueDate: ptr("2020-01-02")}, d)
	ok("editMilestone", err)
	_, _, err = deleteMilestone(ctx, nil, deleteMilestoneIn{ProjectID: "42", MilestoneID: 1}, d)
	ok("deleteMilestone", err)
	_, _, err = getMilestoneIssues(ctx, nil, getMilestoneIssuesIn{ProjectID: "42", MilestoneID: 1}, d)
	ok("getMilestoneIssues", err)
	_, _, err = getMilestoneMergeRequests(ctx, nil, getMilestoneMergeRequestsIn{ProjectID: "42", MilestoneID: 1}, d)
	ok("getMilestoneMergeRequests", err)
	_, _, err = promoteMilestone(ctx, nil, promoteMilestoneIn{ProjectID: "42", MilestoneID: 1}, d)
	fail("promoteMilestone", err)
	_, _, err = getMilestoneBurndownEvents(ctx, nil, getMilestoneBurndownEventsIn{MilestoneID: 1}, d)
	fail("getMilestoneBurndownEvents no group", err)
	_, _, err = getMilestoneBurndownEvents(ctx, nil, getMilestoneBurndownEventsIn{GroupID: "10", MilestoneID: 1}, d)
	ok("getMilestoneBurndownEvents", err)

	_, _, err = listWikiPages(ctx, nil, listWikiPagesIn{ProjectID: "42", WithContent: &boolT}, d)
	ok("listWikiPages", err)
	_, _, err = getWikiPage(ctx, nil, getWikiPageIn{ProjectID: "42", Slug: "home", Version: ptr("1")}, d)
	ok("getWikiPage", err)
	_, _, err = createWikiPage(ctx, nil, createWikiPageIn{ProjectID: "42", Title: "Home", Content: "c", Format: ptr("markdown")}, d)
	ok("createWikiPage", err)
	_, _, err = updateWikiPage(ctx, nil, updateWikiPageIn{ProjectID: "42", Slug: "home", Title: ptr("H2"), Content: ptr("c2")}, d)
	ok("updateWikiPage", err)
	_, _, err = deleteWikiPage(ctx, nil, deleteWikiPageIn{ProjectID: "42", Slug: "home"}, d)
	ok("deleteWikiPage", err)
	_, _, err = listGroupWikiPages(ctx, nil, listGroupWikiPagesIn{GroupID: "10", WithContent: &boolT}, d)
	ok("listGroupWikiPages", err)
	_, _, err = getGroupWikiPage(ctx, nil, getGroupWikiPageIn{GroupID: "10", Slug: "home"}, d)
	ok("getGroupWikiPage", err)
	_, _, err = createGroupWikiPage(ctx, nil, createGroupWikiPageIn{GroupID: "10", Title: "Home", Content: "c"}, d)
	ok("createGroupWikiPage", err)
	_, _, err = updateGroupWikiPage(ctx, nil, updateGroupWikiPageIn{GroupID: "10", Slug: "home", Content: ptr("c2")}, d)
	ok("updateGroupWikiPage", err)
	_, _, err = deleteGroupWikiPage(ctx, nil, deleteGroupWikiPageIn{GroupID: "10", Slug: "home"}, d)
	ok("deleteGroupWikiPage", err)

	_, _, err = listReleases(ctx, nil, listReleasesIn{ProjectID: "42"}, d)
	ok("listReleases", err)
	_, _, err = getRelease(ctx, nil, getReleaseIn{ProjectID: "42", TagName: "v1"}, d)
	ok("getRelease", err)
	_, _, err = createRelease(ctx, nil, createReleaseIn{ProjectID: "42", TagName: "v1", Name: ptr("n"), Description: &desc, Ref: &ref}, d)
	ok("createRelease", err)
	_, _, err = updateRelease(ctx, nil, updateReleaseIn{ProjectID: "42", TagName: "v1", Description: &desc}, d)
	ok("updateRelease", err)
	_, _, err = deleteRelease(ctx, nil, deleteReleaseIn{ProjectID: "42", TagName: "v1"}, d)
	ok("deleteRelease", err)
	_, _, err = createReleaseEvidence(ctx, nil, createReleaseEvidenceIn{ProjectID: "42", TagName: "v1"}, d)
	fail("createReleaseEvidence", err)
	_, _, err = downloadReleaseAsset(ctx, nil, downloadReleaseAssetIn{ProjectID: "42", LocalPath: filepath.Join(t.TempDir(), "x")}, d)
	fail("downloadReleaseAsset missing", err)

	assetTS := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "asset")
	}))
	t.Cleanup(assetTS.Close)
	_, _, err = downloadReleaseAsset(ctx, nil, downloadReleaseAssetIn{
		ProjectID: "42", DirectURL: assetTS.URL, LocalPath: filepath.Join(t.TempDir(), "asset.bin"),
	}, d)
	ok("downloadReleaseAsset direct", err)

	_, _, err = getProject(ctx, nil, getProjectIn{ProjectID: "42"}, d)
	ok("getProject", err)
	_, _, err = listProjectMembers(ctx, nil, listProjectMembersIn{ProjectID: "42", Query: &search}, d)
	ok("listProjectMembers", err)
	_, _, err = listGroupProjects(ctx, nil, listGroupProjectsIn{GroupID: "10", IncludeSubGroups: &boolT, Search: &search, Archived: ptr(false)}, d)
	ok("listGroupProjects", err)
	_, _, err = listNamespaces(ctx, nil, listNamespacesIn{Search: &search}, d)
	ok("listNamespaces", err)
	_, _, err = getNamespace(ctx, nil, getNamespaceIn{NamespaceID: "10"}, d)
	ok("getNamespace", err)
	_, _, err = verifyNamespace(ctx, nil, verifyNamespaceIn{Path: "g/p", ParentID: ptr(int64(1))}, d)
	ok("verifyNamespace", err)
	_, _, err = getUsers(ctx, nil, getUsersIn{Usernames: []string{" alice ", "", "missing"}}, d)
	ok("getUsers", err)

	_, _, err = searchRepositories(ctx, nil, searchRepositoriesIn{Search: &search}, d)
	ok("searchRepositories", err)
	_, _, err = createRepository(ctx, nil, createRepositoryIn{Name: "n", Path: ptr("n"), Description: &desc, Visibility: ptr("private"), InitializeWithReadme: &boolT, NamespaceID: ptr(int64(1))}, d)
	ok("createRepository", err)
	_, _, err = forkRepository(ctx, nil, forkRepositoryIn{ProjectID: "42", Name: ptr("f"), Path: ptr("f"), NamespacePath: ptr("g"), NamespaceID: ptr(int64(1))}, d)
	ok("forkRepository", err)
	_, _, err = getFileContents(ctx, nil, getFileContentsIn{ProjectID: "42"}, d)
	ok("getFileContents tree", err)
	_, _, err = getFileContents(ctx, nil, getFileContentsIn{ProjectID: "42", FilePath: "f.go", Ref: "main"}, d)
	ok("getFileContents file", err)
	_, _, err = createOrUpdateFile(ctx, nil, createOrUpdateFileIn{ProjectID: "42", FilePath: "f.go", Branch: "main", Content: "x", CommitMessage: "c"}, d)
	ok("createOrUpdateFile create", err)
	_, _, err = createOrUpdateFile(ctx, nil, createOrUpdateFileIn{ProjectID: "42", FilePath: "f.go", Branch: "main", Content: "x", CommitMessage: "c", Mode: "update"}, d)
	ok("createOrUpdateFile update", err)
	_, _, err = createOrUpdateFile(ctx, nil, createOrUpdateFileIn{ProjectID: "42", FilePath: "f.go", Branch: "main", Content: "x", CommitMessage: "c", Mode: "nope"}, d)
	fail("createOrUpdateFile bad mode", err)
	content := "x"
	_, _, err = pushFiles(ctx, nil, pushFilesIn{ProjectID: "42", Branch: "main", CommitMessage: "c", StartBranch: ptr("main"), Actions: []fileActionIn{{Action: "create", FilePath: "a.go", Content: &content}}}, d)
	ok("pushFiles", err)
	_, _, err = getRepositoryTree(ctx, nil, getRepositoryTreeIn{ProjectID: "42", Ref: "main", Path: "/", Recursive: &boolT}, d)
	ok("getRepositoryTree", err)
	_, _, err = createBranch(ctx, nil, createBranchIn{ProjectID: ptr("42"), Branch: "f", Ref: "main"}, d)
	ok("createBranch", err)
	_, _, err = listCommits(ctx, nil, listCommitsIn{ProjectID: "42", RefName: "main", Path: "a.go", Since: "2020-01-01T00:00:00Z", Until: "2020-02-01T00:00:00Z"}, d)
	ok("listCommits", err)
	_, _, err = getCommit(ctx, nil, getCommitIn{ProjectID: "42", Sha: "abc"}, d)
	ok("getCommit", err)
	_, _, err = getCommitDiff(ctx, nil, getCommitDiffIn{ProjectID: "42", Sha: "abc", TruncateLines: 1}, d)
	ok("getCommitDiff", err)
	_, _, err = getBranchDiffs(ctx, nil, getBranchDiffsIn{ProjectID: "42", From: "main", To: "f", TruncateLines: 1}, d)
	ok("getBranchDiffs", err)

	_, _, err = listGroupIterations(ctx, nil, listGroupIterationsIn{GroupID: "10", State: &state}, d)
	ok("listGroupIterations", err)
	_, _, err = listEvents(ctx, nil, listEventsIn{Action: ptr("pushed")}, d)
	ok("listEvents", err)
	_, _, err = getProjectEvents(ctx, nil, getProjectEventsIn{ProjectID: "42", Action: ptr("pushed")}, d)
	ok("getProjectEvents", err)

	_, _, err = listWebhooks(ctx, nil, listWebhooksIn{ProjectID: ptr("42")}, d)
	ok("listWebhooks project", err)
	_, _, err = listWebhooks(ctx, nil, listWebhooksIn{GroupID: ptr("10")}, d)
	ok("listWebhooks group", err)
	_, _, err = listWebhooks(ctx, nil, listWebhooksIn{}, d)
	fail("listWebhooks none", err)
	_, _, err = listWebhookEvents(ctx, nil, listWebhookEventsIn{ProjectID: "42", HookID: 1}, d)
	fail("listWebhookEvents", err)
	_, _, err = getWebhookEvent(ctx, nil, getWebhookEventIn{ProjectID: "42", HookID: 1, EventID: 2}, d)
	fail("getWebhookEvent", err)

	_, _, err = executeGraphQL(ctx, nil, executeGraphQLIn{Query: "{ ok }", Variables: json.RawMessage(`{"a":1}`)}, d)
	ok("executeGraphQL", err)
	_, _, err = executeGraphQL(ctx, nil, executeGraphQLIn{Query: "{ ok }"}, d)
	ok("executeGraphQL no vars", err)
	_, _, err = executeGraphQL(ctx, nil, executeGraphQLIn{Query: "{ ok }", Variables: json.RawMessage(`not-json`)}, d)
	fail("executeGraphQL bad vars", err)
	_, _, err = getWorkItem(ctx, nil, getWorkItemIn{ID: "gid://gitlab/WorkItem/1"}, d)
	ok("getWorkItem", err)
	_, _, err = listWorkItems(ctx, nil, listWorkItemsIn{ProjectPath: "g/p", First: 10, Types: []string{"ISSUE"}}, d)
	ok("listWorkItems", err)
	_, _, err = listWorkItems(ctx, nil, listWorkItemsIn{ProjectPath: "g/p", First: 0}, d)
	ok("listWorkItems default first", err)
	_, _, err = listWorkItems(ctx, nil, listWorkItemsIn{ProjectPath: "g/p", First: 500}, d)
	ok("listWorkItems clamp first", err)
	_, _, err = createWorkItem(ctx, nil, createWorkItemIn{ProjectPath: "g/p", Title: "t", WorkItemTypeID: "gid://gitlab/WorkItemsType/1", Description: &desc, Confidential: &boolT}, d)
	ok("createWorkItem", err)
	_, _, err = updateWorkItem(ctx, nil, updateWorkItemIn{ID: "gid://gitlab/WorkItem/1", Attributes: json.RawMessage(`{"title":"t2"}`)}, d)
	ok("updateWorkItem", err)
	_, _, err = updateWorkItem(ctx, nil, updateWorkItemIn{ID: "gid://gitlab/WorkItem/1"}, d)
	fail("updateWorkItem empty attrs", err)
	_, _, err = convertWorkItemType(ctx, nil, convertWorkItemTypeIn{ID: "gid://gitlab/WorkItem/1", WorkItemTypeID: "gid://gitlab/WorkItemsType/2"}, d)
	ok("convertWorkItemType", err)
	_, _, err = listWorkItemStatuses(ctx, nil, listWorkItemStatusesIn{ProjectPath: "g/p", WorkItemTypeID: "gid://gitlab/WorkItemsType/1"}, d)
	ok("listWorkItemStatuses", err)
	_, _, err = listCustomFieldDefinitions(ctx, nil, listCustomFieldDefinitionsIn{ProjectPath: "g/p", WorkItemTypeID: "gid://gitlab/WorkItemsType/1"}, d)
	ok("listCustomFieldDefinitions", err)
	_, _, err = moveWorkItem(ctx, nil, moveWorkItemIn{WorkItemID: "gid://gitlab/WorkItem/1", TargetPath: "g/p2"}, d)
	ok("moveWorkItem", err)
	_, _, err = listWorkItemNotes(ctx, nil, listWorkItemNotesIn{ID: "gid://gitlab/WorkItem/1"}, d)
	ok("listWorkItemNotes", err)
	_, _, err = createWorkItemNote(ctx, nil, createWorkItemNoteIn{ID: "gid://gitlab/WorkItem/1", Body: "n", Internal: &boolT}, d)
	ok("createWorkItemNote", err)
	_, _, err = getTimelineEvents(ctx, nil, getTimelineEventsIn{ID: "gid://gitlab/WorkItem/1"}, d)
	ok("getTimelineEvents", err)
	_, _, err = createTimelineEvent(ctx, nil, createTimelineEventIn{ID: "gid://gitlab/WorkItem/1", Tag: "start", Note: "n", HappenedAt: ptr("2020-01-01T00:00:00Z")}, d)
	ok("createTimelineEvent", err)

	tmpFile := filepath.Join(t.TempDir(), "up.txt")
	if err := os.WriteFile(tmpFile, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = uploadMarkdown(ctx, nil, uploadMarkdownIn{ProjectID: "42", FilePath: tmpFile}, d)
	ok("uploadMarkdown", err)
	_, _, err = downloadAttachment(ctx, nil, downloadAttachmentIn{ProjectID: "42", Secret: "sec", Filename: "up.txt", AsBase64: true}, d)
	ok("downloadAttachment b64", err)
	_, _, err = downloadAttachment(ctx, nil, downloadAttachmentIn{ProjectID: "42", Secret: "sec", Filename: "up.txt"}, d)
	ok("downloadAttachment text", err)

	if err := checkAllowedProject(&config.Config{}, "1"); err != nil {
		t.Fatal(err)
	}
	if err := checkAllowedProject(&config.Config{AllowedProjectIDs: []string{"9"}}, "1"); err == nil {
		t.Fatal("expected allowlist error")
	}
	if err := checkAllowedProject(&config.Config{AllowedProjectIDs: []string{"1"}}, "1"); err != nil {
		t.Fatal(err)
	}
	_ = Out(map[string]any{"x": 1})
	if _, err := ToJSONTree(make(chan int)); err == nil {
		t.Fatal("expected ToJSONTree error")
	}
	if matched, _ := pathMatch("vendor/*", "vendor/x"); !matched {
		t.Fatal("pathMatch prefix")
	}
	if matched, _ := pathMatch("a.go", "a.go"); !matched {
		t.Fatal("pathMatch exact")
	}
	if matched, _ := pathMatch("", "a"); matched {
		t.Fatal("empty pattern")
	}
	if _, err := parseCommitTime("2020-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := parseCommitTime("nope"); err == nil {
		t.Fatal("expected parseCommitTime error")
	}
	if n, err := IntFromAny(int(3)); err != nil || n != 3 {
		t.Fatalf("IntFromAny int: %v %d", err, n)
	}
	if n, err := IntFromAny("9"); err != nil || n != 9 {
		t.Fatalf("IntFromAny string: %v %d", err, n)
	}
	if _, err := IntFromAny(nil); err == nil {
		t.Fatal("IntFromAny nil")
	}
	if _, err := IntFromAny(true); err == nil {
		t.Fatal("IntFromAny bool")
	}
	if TruncateLines("a\nb\nc", 10) != "a\nb\nc" {
		t.Fatal("TruncateLines no-op")
	}
	if ParseID(" x ") != "x" {
		t.Fatal("ParseID")
	}
	_, _ = ResolveProjectID("", "def")
}

func TestDeleteGroupWikiPage_coverage(t *testing.T) {
	d := testDeps(t)
	_, _, err := deleteGroupWikiPage(context.Background(), nil, deleteGroupWikiPageIn{GroupID: "10", Slug: "home"}, d)
	if err != nil {
		t.Fatal(err)
	}
}
