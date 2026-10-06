# GitLab MCP for merge request review — proposal v2

6 October 2026 · Baseline commit `2d132b0` (GitHub `main` = GitLab `main`) · Supersedes v1 of 2 October 2026

## Why a v2

v1 was a good requirements survey, but it was turned into an epic of 34 tickets (RVG-117) and then built without limits. In four days, 25 PRs took production Go from 5.1k to 36.9k lines and tests from 1.8k to 44.5k. Vendored go-git added another 247k lines. None of it was deployed, and the review workflow still took the same number of calls. The work was discarded and the repositories reset to `2d132b0`.

What went wrong, and the rule in this proposal that addresses each:

| What happened | Rule below |
|---|---|
| Every v1 caveat ("prove", "never assume complete") became a hard acceptance criterion; one ticket grew to 2–7k lines | 2, 5 |
| Automated review loops (`@codex review` → fix → re-review, up to 34 rounds per PR) never converged | 6 |
| Speculative infrastructure (server-side Git cache, MCP-side SQLite intent store, HMAC cursors) was built before any of it was needed | 3, Non-goals |
| Delivery and measurement came last, so nothing told anyone to scale back | 1 |
| Four overlapping PRs were open at once | 5 |

## Goal

Make the tl-ops review workflow cheaper and safer. Success is measured on the same workflows that produced the v1 evidence (`tl-ops/.codex/mr-review/gitlab-mcp-tool-evidence.md`):

| Workflow | Baseline MCP calls | Target |
|---|---:|---:|
| Unchanged 11-MR queue scan | 14 | ≤ 4 |
| Two-MR review with one inline finding | 35 | ≤ 18 |
| Clean SHA-bound approval with verification | 8 | ≤ 4 |
| One finding/reply publication with verification | ~5 | ≤ 3 |
| CI watch, per poll | 1 (hand-written GraphQL) | 1 (typed tool) |

The targets are hypotheses. Every milestone ends by deploying and counting again. If a milestone misses its target, the next one is re-planned, not just started.

## Rules

1. **Ship and measure every milestone.** A milestone is done when the connector runs it and the workflows above have been counted again. Merged-but-undeployed code does not count.
2. **Be honest about completeness; don't try to prove it.** Every list or aggregate returns `complete: true|false`. When it is false, it also returns `truncated_reason` and `next_page`. Unknown values are null or omitted. Do not add envelope frameworks, digests, signatures, consistency proofs or presence-aware DTO layers.
3. **The server stays stateless.** No database, Git cache, signed cursors or background workers. GitLab's own page numbers are good enough. Durable state, workflow decisions and worker ownership stay in the tl-ops controller.
4. **Pass through before abstracting.** Wrap GitLab endpoints closely. Add an aggregate tool only where it removes measured calls.
5. **Keep PRs small.** At most ~500 non-test lines per PR, with tests of similar size. Only one feature PR open at a time. No new dependency without a one-line justification in the PR.
6. **Review is human-led.** The author does a self-review against the ticket and CI must be green. A second-opinion review is optional: run it once, on request, and Vladimir decides which findings matter. No auto-review bots and no re-review after every fix.
7. **Timebox.** Every ticket has an estimate. At 2× the estimate, stop and re-scope instead of pushing on.

## Scope

### M1 — Expose existing tools and fix what they get wrong

This is mostly configuration plus small fixes to existing handlers. No new aggregate tools.

- **Review profile** (`GITLAB_TOOL_PROFILE=review`). The current daily reads, plus these existing tools that the connector doesn't expose yet:
  - `get_merge_request_discussion`
  - `create_merge_request_discussion_note` (reply)
  - `resolve_merge_request_thread`
  - `list_pipelines`, `get_pipeline`, `list_pipeline_jobs`, `list_pipeline_trigger_jobs`, `get_pipeline_job`, `get_pipeline_job_output`

  Pipeline write tools stay out. The daily profile does not change.
- **`list_merge_requests`:** add `reviewer_id`, `scope`, `updated_after`/`updated_before` and `order_by`/`sort`. Fix `author_id`, which the group branch currently ignores.
- **Diff getters:** accept `page`/`per_page`, return `next_page` and `complete`, and pass through GitLab's `collapsed`/`too_large` flags.
- **Job trace:** add `tail_lines` and `max_bytes`, and stream into a tail buffer instead of `io.ReadAll`. Return `truncated`. The useful error is usually at the end of the trace.
- **`execute_graphql`:** accept `variables` as an object (the schema currently says array). Parse the operation and reject mutations unless writes are enabled. Annotate the tool as not read-only.
- **`get_project`:** return an allowlisted set of fields, not the full SDK object.
- **Approval state:** fall back only when the endpoint is unsupported (404), and return the same shape either way.
- **SDK retries:** turn off automatic retries for non-GET requests so a write can't be published twice.

### M2 — Two aggregate reads and batch files

- **`get_review_queue(group_id, roles=[reviewer,author], state=opened, updated_after?)`**
  - Resolves the current user.
  - Pages through group MRs for each role, up to a cap.
  - Removes duplicates.
  - Returns `{project_id, iid, title, author, reviewers, sha, updated_at, roles}` and `complete`.
  - List heads are labelled as hints; the snapshot is the authority.
- **`get_review_snapshot(mrs ≤ 10, include=[changes, discussions, approvals, pipeline])`**, with an optional `expected_sha` per MR. Each MR returns:
  - metadata with `diff_refs` and head SHA;
  - the changed-file list (paths and flags, no patches);
  - all discussion pages up to a cap, compacted to IDs, resolution state, authors, bodies, positions and timestamps;
  - normalized approvals;
  - a head pipeline summary.

  At the end the head is read again and `head_changed` is returned. A failure for one MR doesn't fail the batch.
- **`batch_get_file_contents(project_id, sha, paths ≤ 20)`:** returns each file or a per-file error.

### M3 — CI status for a SHA

- **`get_pipeline_status(project_id, sha | mr_iid)`**
  - Finds the pipelines for the SHA and all their jobs, paged.
  - Follows bridges to downstream pipelines, two levels deep.
  - Returns:
    - the overall status;
    - failed jobs;
    - manual jobs, with `allow_failure`;
    - `complete: false` if any downstream pipeline is inaccessible or capped.

  It replaces the hand-written GraphQL poll, and the snapshot's `pipeline` section reuses it.

### M4 — Stateless guarded writes

- **Shared preflight:**
  - In the review profile, `expected_sha` is required. The tool reads the MR first and refuses with `head_changed` before writing.
  - An optional `op_key` is embedded in the note body as `<!-- gitlab-mcp:op=KEY -->`. Before writing, the tool searches the discussions for that marker and returns the existing note instead of posting a duplicate. That makes a retry after a timeout safe without a database.
  - After the write, the tool reads back and returns the IDs plus the current head. If the head moved, it returns `head_changed_after_write`.
- **Thread, reply and resolve:** inline anchors are checked against the MR's current `diff_refs` and changed files. An invalid anchor is an error; the tool never quietly downgrades it to a general note.
- **Approve:** `sha` is required in the review profile. GitLab already rejects a mismatch.
- **Merge:** add `sha`, `squash`, `should_remove_source_branch` and auto-merge. No retry. Return `merged`, or `pending` if the response still says opened; the caller then polls. Merge stays out of the review profile.
- **Spike first (half a day):** check whether GraphQL `createNote(mergeRequestDiffHeadSha)` rejects a stale head on our GitLab version. If it does, replies get a real server-side guard.

### Later, only if measurement shows a need

Change feed for polling, patch windows for very large MRs, batch replies.

## Non-goals

- Server-side Git clone or cache. Very large MRs stay a local-git job in the workflow, and the tools say `complete: false`.
- Any persistence in the MCP server: intents, receipts, leases.
- Signed or expiring cursors, or atomic multi-endpoint snapshots.
- Certifying that a review happened. Tools report facts; the controller and the human decide.
- Trace redaction beyond GitLab's own masking of masked variables.
- Changing the existing project/group allowlist model.

## Risks and open points

- GitLab notes have no compare-and-swap. Preflight, the marker and readback narrow the race window but don't close it, and the tools report it when it happens.
- Deployment is a local `go install` behind the mcp-wrapper bridge. Long-lived processes keep running the old binary, so every delivery has to include a restart and a check of `go version -m`.
- Live tests run in the disposable `ruarmv5/gitlab-mcp-sandbox` project. The approval tests may need a second actor if the instance forbids author approval.
