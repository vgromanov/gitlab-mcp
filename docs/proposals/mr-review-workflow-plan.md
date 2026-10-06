# MR review workflow — delivery plan

Implements [proposal v2](mr-review-workflow.md). 6 October 2026. Baseline `2d132b0`.

## How every ticket runs

1. Branch from fresh `main`, named `feature/<ID>-<slug>`. Only one feature PR is open at a time.
2. Post a short plan on the issue (at most 10 steps) before writing code.
3. Implement within the size cap: ~500 non-test lines. At 2× the estimate or over the cap, stop and split.
4. Self-review against the ticket's acceptance list. CI must be green, and coverage must stay ≥ 80%.
5. A second opinion is optional, run once on request. Vladimir triages its findings. **This replaces the `/ship` independent-review gate for this epic, as an explicit user waiver.** No automated re-review, no `@codex review`.
6. Merge. At the end of a milestone, do the delivery steps below.

Estimates are elapsed effort for one worker. "LOC" means non-test production lines.

## Environment

- **Deployment.** Codex and Claude reach go-gitlab through the mcp-wrapper bridge on `127.0.0.1:12020`. The bridge starts `~/go/bin/gitlab-mcp` using the `go-gitlab` entry in `~/.cursor/mcp.json` (currently `USE_DAILY_TOOLS=true`). Cursor runs the same binary directly. The deployed build is `1e32d0e`, which differs from `2d132b0` only in tests.
- **Delivery at the end of each milestone:**
  1. Fast-forward GitLab `main` and wait for GitLab CI to pass.
  2. Run `go install ./cmd/gitlab-mcp` from that commit and check `go version -m ~/go/bin/gitlab-mcp`.
  3. Restart the bridge. Restart or close stale `gitlab-mcp` processes; they keep running the old binary.
  4. Re-run the workflows and record the counts.
- **Review profile.** Add a second server entry, `go-gitlab-review` with `GITLAB_TOOL_PROFILE=review`, used by the Codex review job. The `go-gitlab` daily entry stays as it is for other work.
- **Consumer.** The Codex review job runs every 30 minutes. It lists open MRs (Vladimir's and teammates'), checks discussion state, reviews, and publishes. Measured workflows come from this job, and milestone delivery includes updating its instructions to use the new tools.
- **Sandbox.** [`ruarmv5/gitlab-mcp-sandbox`](https://gitlabci.raiffeisen.ru/ruarmv5/gitlab-mcp-sandbox), project ID 61378, private, owned by Vladimir. All live tests and fixtures go there, and any of it may be wiped. The parent/child CI fixture is seeded in ticket 14. For approval tests, enable author self-approval on the sandbox, or add a second actor if the instance forbids it.

## M1 — Expose existing tools and fix what they get wrong

| # | Ticket | Est. | LOC | Acceptance |
|---|---|---|---|---|
| 1 | Coverage gate in GitHub CI | 0.25d | ~30 | CI fails when total coverage drops below 80% |
| 2 | `review` tool profile | 0.5d | ~150 | The profile exposes the daily reads plus the discussion get/reply/resolve and six pipeline read tools, with no pipeline writes. Daily profile unchanged. Tested against the registered `tools/list` |
| 3 | `list_merge_requests` filters | 0.5d | ~150 | `reviewer_id`, `scope`, `updated_after`/`before`, `order_by`/`sort` reach the wire on project, group and global lists. A test proves the group `author_id` bug is fixed |
| 4 | Honest diff pagination | 0.5d | ~150 | The three diff getters accept `page`/`per_page` and return `next_page`/`complete`. Collapsed and too-large flags are passed through. Fixture with more than 100 files |
| 5 | Job trace tail window | 0.5d | ~100 | `tail_lines`/`max_bytes` are streamed without `ReadAll`. Returns `truncated`. A 50 MB fixture stays within bounded memory |
| 6 | Safe `execute_graphql` | 0.5d | ~150 | `variables` is an object. Mutations are rejected unless writes are enabled. Annotation says not read-only. Top-level GraphQL errors become tool errors |
| 7 | Safe project projections, approval fallback, no write retries | 1d | ~200 | `get_project`, `create_repository` and every other tool that returns a project return allowlisted fields only. No `runners_token` or similar fields (seen live on 6 Oct). Approval fallback only on 404, same output shape. A POST/PUT that gets a 5xx is not retried (test counts requests) |
| 8 | **Deliver M1** | 0.5d | 0 | GitLab `main` fast-forwarded with CI green, connector running the review profile, the five workflows recounted in the tl-ops evidence file |

## M2 — Two aggregate reads and batch files

| # | Ticket | Est. | LOC | Acceptance |
|---|---|---|---|---|
| 9 | `get_review_queue` | 1d | ~300 | Reviewer and author roles, deduplicated, `complete` flag, cap respected. Fixture covers several pages and an MR in both roles |
| 10 | `get_review_snapshot`: metadata, changes, approvals | 1d | ~350 | Batch of up to 10. `expected_sha` → `head_changed`. One MR failing doesn't fail the others |
| 11 | `get_review_snapshot`: discussions + head recheck | 1d | ~250 | All pages up to the cap, compacted. The head is re-read at the end. A fixture with a push between reads sets `head_changed` |
| 12 | `batch_get_file_contents` | 0.5d | ~150 | Exact SHA, up to 20 paths, per-file errors |
| 13 | **Deliver M2** | 0.5d | 0 | Deployed, tl-ops review flow switched to queue and snapshot, workflows recounted. Decide whether M3 is still worth it |

## M3 — CI status for a SHA

| # | Ticket | Est. | LOC | Acceptance |
|---|---|---|---|---|
| 14 | `get_pipeline_status` and snapshot `pipeline` section | 1.5d | ~400 | Jobs and bridges paged. Downstream pipelines followed two levels deep. Manual and `allow_failure` jobs classified. An inaccessible child sets `complete: false` |
| 15 | **Deliver M3** | 0.25d | 0 | CI watch in tl-ops switched from raw GraphQL, still one call per poll |

## M4 — Stateless guarded writes

| # | Ticket | Est. | LOC | Acceptance |
|---|---|---|---|---|
| 16 | Spike: GraphQL `createNote` head guard | 0.5d | 0 | Short report: does `mergeRequestDiffHeadSha` reject a stale head on our version? Tested on a disposable MR |
| 17 | Write preflight + `op_key` marker helper | 1d | ~250 | `expected_sha` check, marker search, readback, `head_changed_after_write` |
| 18 | Guarded thread / reply / resolve | 1d | ~250 | Uses the ticket 17 helper. An invalid inline anchor is an error, never a general note |
| 19 | Approve with required SHA; merge options | 1d | ~200 | Approve requires `sha` in the review profile. Merge takes `sha`/squash/remove-branch/auto-merge, never retries, returns `pending` when still opened |
| 20 | **Deliver M4** | 1d | 0 | Live test on a disposable MR covering timeout-retry de-duplication, stale head refusal and merge pending. tl-ops publication switched. Workflows recounted |

Total: about 14 working days across 20 tickets.

## Settled before starting

- RVG-117 and the old Linear project were deleted. The new project and its tickets track this plan.
- Codex automatic reviews are off. Second opinions are semi-automatic runs that Vladimir starts.
- The mcp-wrapper replay issue is already fixed in the wrapper and is out of scope.

## Linear

Project [gitlab-mcp](https://linear.app/rvg-hub/project/gitlab-mcp-dfc284a74acd), with milestones M1–M4. Epic RVG-153.

| Plan # | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 | 9 | 10 | 11 | 12 | 13 | 14 | 15 | 16 | 17 | 18 | 19 | 20 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| RVG- | 154 | 155 | 156 | 157 | 158 | 159 | 160 | 161 | 162 | 163 | 164 | 165 | 166 | 167 | 170 | 168 | 169 | 171 | 172 | 173 |
