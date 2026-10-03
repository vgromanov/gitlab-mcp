# Approval reads (`get_merge_request_approval_state`)

Normalized, presence-aware merge-request approval reads with a single
evidence-based fallback.

## Capability

- `section.capability_version`: `readmeta.mr_approvals.v1`
- `section.source` / `section.provider`: `gitlab_rest` / `gitlab`
- `section.head_sha`: always JSON `null` (approval endpoints do not supply a
  content HEAD SHA; MR metadata SHA is never copied into the section)
- `section.consistency`: always `unknown`

## Endpoints

| Label (`endpoint`) | GitLab resource | Role |
|---|---|---|
| `approval_state` | `GET .../merge_requests/:iid/approval_state` | Primary |
| `approvals` | `GET .../merge_requests/:iid/approvals` | Legacy fallback (at most once) |

Primary success is one logical approval read (fallback count 0). Authorization
and MR identity verification run first inside the same invocation budget and
are counted separately. Request-local retries are disabled; redirects are still
followed by the HTTP stack, separately budgeted, and rejected as unusable for
success or fallback qualification (`Request.Response` redirect marker — final
URL alone is not enough).

## Fallback predicate (method unavailability only)

After canonical owner + THIS MR / source / downstream access verification in
both policy-active and legacy allow-all modes, at most one legacy `/approvals`
read is permitted only when the original primary GET returns:

- HTTP **405**
- **unredirected**
- from the **authenticated configured HTTPS** API authority / base
- targeting the exact canonical project + MR IID + `approval_state` resource
- with a valid, nonempty `Allow` list (all header field values / comma tokens)
  that **excludes** `GET` (any case)

Missing, blank, malformed, quoted, or GET-permitting `Allow` values fail closed.
HTTP (non-TLS), disabled/missing TLS verification evidence (`VerifiedChains`
empty), changed targets, or contradictory routing/auth evidence fail closed.

This establishes only that GET is currently unavailable at that resource. The
feature, edition, license, or primary-permission cause remains **unknown**.
Ordinary infrastructure for the verified authority may advertise method
refusal. No product-unsupported inference.

**Never** fall back on: generic 404 (even when the MR is visible), 401, 403,
429, any 5xx, malformed primary success, transport failure (including
non-EOF error-body read failures such as truncated or interrupted 405
bodies), or budget / cancellation exhaustion. Cancellation or elapsed
exhaustion discovered after a completed SDK body read still rejects
publishing that success. A qualifying 405 followed by fallback failure or a
malformed fallback object yields a safe failure / unknown — never a third
endpoint, retry, or recurse. Completed reads that land exactly at the
request-budget ceiling remain valid; that ceiling only gates the next
dispatch.

## Normalized output shape

Both endpoint successes share the same keys and nullability policy:

| Field | Meaning |
|---|---|
| `endpoint` | `approval_state` or `approvals` |
| `approved` | overall approval observation (`true`/`false`/JSON `null` unknown). Primary `approval_state` has no top-level overall field — stays `null`. Never inferred from rule subsets. |
| `actor_approved` | actor has approved (`null` when absent) |
| `actor_can_approve` | actor eligibility (`null` when absent) |
| `approvals_required` / `approvals_left` | counts or `null` |
| `rules` | full rule observations from `approval_state` (`null` when absent/null; `[]` when explicit empty) |
| `rules_left` | legacy partial subset from `approvals` (`null` when absent/null; `[]` when explicit empty) |
| `rules_capability` | `full` (primary rules field present) \| `partial` (legacy rules_left array present) \| `unknown` (absent/null) |
| `rules_complete` | `false` only with an explicit incompleteness signal (e.g. hidden groups); otherwise `unknown`. Never `true` from approved booleans, empty rules, or missing hidden-groups. |
| `section` | `readmeta.Section` envelope (`counts.items` null when rule arrays absent/null) |

Explicit JSON `false` / `0` / `[]` are preserved and distinct from missing /
`null` (unknown). Global `approved=true`, empty rules, hidden groups, or
unavailable rule detail **never** certifies full rule approval. Incomplete
results are never returned as empty complete success.

Malformed roots (`null`, non-object, wrong known-field types including
`approval_rules_overwritten`, truncated JSON, trailing junk) fail closed
without fallback. Retained rule observations charge the invocation item budget.

## Errors

HTTP / transport failures project to stable `readmeta` codes with short kinds.
Projected errors must not include raw SDK URLs, response bodies, tokens,
project paths, headers, or proxy details.

## Migration from raw SDK shapes

Previously this tool returned whichever raw SDK object succeeded
(`MergeRequestApprovalState` or `MergeRequestApprovals`) after a catch-all
fallback on any primary error. Callers must now consume the normalized object
above plus `section`, and must not assume SDK field bags or that a fallback
implies an older GitLab product edition.
