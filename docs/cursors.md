# Signed opaque cursors

`gitlab-mcp` can paginate selected read tools with **HMAC-SHA256 authenticated
opaque v1 cursors**. This document covers operator configuration and the
`list_commits` reference continuation.

## Operator key

| Item | Contract |
|---|---|
| Env | `GITLAB_MCP_CURSOR_KEY` |
| Format | Raw secret, **≥ 32 bytes** (not base64-wrapped by the server) |
| CLI | None (no secret flag) |
| TTL | Fixed **2 hours** absolute; not renewed on each page; no TTL env |
| Missing key | Server starts; legacy offset tools work; **cursor mode fails** with an actionable configuration error (never a false one-page “complete” aggregate) |
| Invalid present key | `Validate` fails at startup **without echoing** the key |
| Rotation | Immediately invalidates outstanding cursors (`resync_required`) |

Never log `Config` structs that embed the key. Cursor payloads never contain
tokens, keys, or note text.

## Opt-in (`list_commits`)

| Mode | How |
|---|---|
| Legacy (default) | Omit `use_cursor` and `cursor` — exact historical offset `page`/`per_page` and `commits` + `pagination.next_page` output |
| Initial cursor | `use_cursor=true` (optionally `page=1` / omit page) |
| Resume | Non-empty `cursor` from the prior section; **strictly repeat** the normalized original selection (`ref_name`, `path`, `since`, caller `until`, `per_page`). Discovery upper bound is bound separately and is not slid from a new wall clock. Changed filters → `resync_required` before guard/list. |
| Rejected | `page>1` with `use_cursor` or with `cursor` (ambiguous) |

Cursor mode normalizes `per_page` like legacy, then **caps at 50** so one
previous-page guard plus one requested page fit the 100-item invocation budget.
The normalized size is bound into the cursor.

## Pinning and continuation

1. Resolve authenticated actor (`GET /user` → numeric `ActorID` only; never the
   PAT) and **canonical numeric project** identity (always via project lookup —
   including when allowlists are empty; path aliases resume as the same numeric
   id). Instance binding is a credential-safe canonical URL
   (`scheme://host[:port]/path` only: lowercased host, default ports omitted,
   trailing slash stripped; userinfo/query/fragment are stripped and never
   serialized into the cursor or errors).
2. Pin immutable refs (plural binding; `list_commits` uses a one-element tip SHA
   from `GetCommit(ref_name or HEAD)`; future MR adapters may bind base+head).
3. Discovery upper bound = `min(caller until, initial UTC time)`; absolute expiry
   = `now + 2h`. Both are copied across pages and never renewed.
4. List commits against the pinned tip SHA + bound filters; **preserve provider order**.
5. On resume: crypto-validate cursor → compare **policy fingerprint** and
   credential-safe **instance** to the decoded payload (**before** authz/content)
   → re-resolve actor (`GET /user`) → reauthorize canonical project →
   re-fetch the **signed previous page** (guard) → verify sequence digest / last
   SHA → only then fetch the next page. Guard data is never returned. Allowlist
   edits that change the fingerprint return `resync_required` even when they
   would also revoke the old project; same-fingerprint revocation via live
   project/group state returns safe `authz_denied`.
6. Boundary drift, tamper, expiry, rotation, or binding mismatch →
   `resync_required` with **zero** further list/ref continuation.

SDK `ListCommitsOptions` has **no** `order_by`. Client-side timestamp+SHA sorting
across offset pages is **not** treated as a global stable order. When order or
exhaustion cannot be proven, section `consistency` and `content_complete` stay
`unknown`. Missing, contradictory, repeating, or jumping `X-Next-Page` values
never mint a continuation cursor and never claim false exhaustion; responses keep
valid current-page items with explicit resync/unknown limitations. Signed page
state requires sequential `provider_next_page == page+1` for resumable cursors.

## Transport section

Cursor-mode responses include a read `section` (reusing shared completeness field
types; commits-local capability `readmeta.list_commits.v1`) with
`retrieved_at`, `source`, `provider`, `capability_version`, `head_sha`,
`pagination_exhausted`, `content_complete`, `consistency`, `limitations`,
`next_cursor`, and nullable counts. Pagination exhaustion alone never proves
content complete.

## Budgets

Default invocation budget: **100 items / 8 MiB / 30 s / 16 requests**, covering
identity, canonical project lookup, pin, previous-page guard, and list calls.
Cancellation closes work; errors use safe static projections.

## Future aggregates

Review aggregates that depend on signed cursors must fail closed when the signing
key is missing — the same configuration error contract as `list_commits` cursor
mode. Codec bindings also cover group-queue and project/pipeline scopes.

`get_merge_request_review_queue` uses authenticated v1 cursors with typed
`queue_cont` (`rq2`) on `ScopeGroupQueue` (empty immutable refs; no commit-shaped
pagination). It pins `until=min(caller updated_before, initial UTC)` with
RFC3339Nano, including fractional seconds, and absolute expiry at mint time;
changed selection/bounds/policy fail `resync_required` before discovery.
Candidate map entries are written only after owner, group, exact-MR, and
source/downstream proof. Queue pagination accepts explicit exhaustion or
`current+1` with raw-header/SDK agreement. Signed rq2 payloads must use the
exact normalized kind×state streams, canonical `project:iid` keys, a closed
limitation set, and no duplicate JSON members. A stop inside a discussion note
list is terminal with no cursor. Terminal capacity, ambiguity, or cursor
overflow keeps proved successes, reports `known_terminal_omitted` when that
count is known, and sets `next_cursor=null`. Moving discovery always reports
consistency unknown with a limitation and never treats provider exhaustion alone
as content complete.

`get_merge_request_review_context` signs one v1 token per proved merge request.
Scope kind is `review_context` with the canonical owner project id and a positive
IID. `immutable_refs` and `page_state` are empty, `per_page` is 1, and `queue_cont`
is absent. `list_commits`, `rq2`, and pipeline tokens are unchanged. Complete and
excluded masks are disjoint, sorted, and union to the requested set. Digests exist
only for complete sections. `retrieved_at + 5m` is `write_fresh_until`; `retrieved_at
+ 2h` is `expires_at`. Neither window is renewed. `Decode` proves HMAC and this
structure. `VerifyContextBinding` checks the live instance, actor, policy, scope,
and selection, then an independently supplied live ref tuple: owner, source, and
target project ids and branches, source and target SHAs, and the version id,
head, base, and start. A missing observation fails closed. `metadata`, `approvals`, `discussions`, and `pipeline_graph` are complete evidence when their digest rules match. `discussions` is complete only when `Digests["discussions"]` is the SHA-256 of the `discussions.evidence.v1` bundle for one fresh exhaustive `all` walk. `pipeline_graph` may be requested, excluded, or complete only when `Digests["pipeline_graph"]` is the graph digest from a fresh exhaustive `get_merge_request_pipeline_graph` walk (`downstream_coverage=complete`, `content_complete=true`, `consistency=consistent`, no open `next_cursor`, no `partial` limitation on the section). A complete mask without that digest, a digest without that completeness, or a digest key when the section was not requested, is rejected. `diff_manifest` may be requested, excluded, or complete only when `evidence` is exactly `{"diff_manifest":"diff_manifest.v1"}`. A complete `diff_manifest` without that version, or evidence on any other section, is rejected. Old metadata, approvals, and discussions tokens stay valid. A metadata-only token fails a demand for `approvals`. Missing `GITLAB_MCP_CURSOR_KEY`
is the same class of configuration error as the queue. The signature does not
attest that a human reviewed the change.

`dc1` continues `get_merge_request_review_context` section `discussions` on a project scope and IID. It is not a context ref: `context_ref` is nil, `page_state` is empty, `per_page` is 20, order is `provider`, and selection is `semantic` or `all`. Five immutable refs are source, target, version head, base, and start, with source equal to version head. `upper_bound` and `until` are the reconciliation deadline copied forward. `expires_at` is the first page's `now+2h`, copied forward. Fields `p`, `di`, `ni`, `did`, `dp`, and `nd` store the next discussion and note coordinates and prefix hashes only, never bodies or timestamps. Resume re-reads the bracket and those refs before a discussions GET. A resumed tail never mints `Digests["discussions"]`.

`dm1` continues `get_merge_request_diff_window` section `diff_manifest` in manifest mode only. Schema is v1. It is not `dc1` and not a review-context cursor: review-context `diff_manifest` keeps `next_cursor` null. Resume must repeat the same selection and `per_page` (1..50). The token binds actor, policy fingerprint, instance, project, IID, and one exclusive mode: version id, or base/start/head, or from/to with `straight`. It also binds distinct `immutable_refs`, the ordered full-sequence digest, the proved count, the prefix digest, and the offset. HMAC and those scope checks run before any reauthorization or refetch. `expires_at` is the first page's `retrieved_at` plus 2h; `upper_bound` is that first `retrieved_at` and neither value slides. A mismatched or incomplete resume emits no entries, digest, or cursor (`resync_required` when the token itself does not bind). An incomplete proof mints no `dm1` cursor. Content mode (`mode=content`) rejects `cursor` input and always returns `next_cursor=null`; it does not mint `dm1`.

Signed v1 cursors for `get_merge_request_pipeline_graph` use pipeline scope with `graph_cont` (jobs phase, bridges phase, queued downstream nodes). They are not review-context refs: `get_merge_request_review_context` may accept one as `cursors[].section=pipeline_graph` to resume paging, but a response that started only from such a cursor never sets `Digests["pipeline_graph"]`. Review-context complete graph evidence requires the digest from a fresh walk through exhaustion as above.
