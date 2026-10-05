# Read envelopes (presence-aware completeness)

Foundation types for honest read-section metadata used by aggregates.
Reference adapter: `list_merge_request_diffs`.

## Tool output shape

Schema-valid success example (full SDK `MergeRequestDiff` field projection; `head_sha` is a real 40-char hex observation):

```json
{
  "diffs": [
    {
      "old_path": "a.go",
      "new_path": "a.go",
      "a_mode": "100644",
      "b_mode": "100644",
      "diff": "+x\n",
      "new_file": false,
      "renamed_file": false,
      "deleted_file": false,
      "generated_file": false,
      "collapsed": false,
      "too_large": false
    }
  ],
  "pagination": { "next_page": 0 },
  "section": {
    "retrieved_at": "2026-10-02T12:00:00Z",
    "source": "gitlab_rest",
    "provider": "gitlab",
    "capability_version": "readmeta.mr_diffs.v1",
    "head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "pagination_exhausted": true,
    "content_complete": "true",
    "consistency": "consistent",
    "limitations": [],
    "next_cursor": null,
    "counts": { "items": 1, "bytes": 120, "files": null },
    "manifest_coverage": "full",
    "patch_coverage": "full"
  }
}
```

Legacy `diffs` and `pagination.next_page` are preserved. `section` is additive.
`diffs` matches the SDK `[]*MergeRequestDiff` projection: empty backend arrays serialize as `[]` (never `null`); JSON `null` elements stay `null` (not zero objects).
`section.head_sha` is JSON `null` unless a valid 40-character hex SHA was observed; short/invalid values are never projected and cannot justify `consistency:"consistent"`.

## Partial example (collapsed)

Schema-valid partial example:

```json
{
  "diffs": [
    {
      "old_path": "big.go",
      "new_path": "big.go",
      "a_mode": "100644",
      "b_mode": "100644",
      "diff": "",
      "new_file": false,
      "renamed_file": false,
      "deleted_file": false,
      "generated_file": false,
      "collapsed": true,
      "too_large": false
    }
  ],
  "pagination": { "next_page": 0 },
  "section": {
    "retrieved_at": "2026-10-02T12:00:00Z",
    "source": "gitlab_rest",
    "provider": "gitlab",
    "capability_version": "readmeta.mr_diffs.v1",
    "head_sha": null,
    "pagination_exhausted": true,
    "content_complete": "false",
    "consistency": "unknown",
    "limitations": [
      { "code": "collapsed", "message": "one or more diffs collapsed" }
    ],
    "next_cursor": null,
    "counts": { "items": 1, "bytes": 80, "files": null },
    "manifest_coverage": "full",
    "patch_coverage": "partial"
  }
}
```

`content_complete` is a string enum: `"true"` | `"false"` | `"unknown"`.
`consistency` stays `"unknown"` unless content-head bracketing verifies the same valid 40-hex `head_sha` before and after the read; drift yields `"inconsistent"`.
`manifest_coverage` and `patch_coverage` are justified independently (collapsed/too_large affects patch, not necessarily the path manifest; `page>1` or unknown paging cannot prove full MR coverage).
`counts.*.null` means unknown (never coerced to `0`).
`next_cursor` for this adapter is an unsigned decimal next-page string when `next_page > 0` — informational only, not signed (cursor-signing is out of scope).

## Legacy getters (RVG-127)

`get_merge_request_diffs`, `get_merge_request_file_diff`, and
`get_merge_request_conflicts` already returned JSON objects. They now attach the
same honesty fields (`pagination`, `section`) without inventing an array→object
migration:

| Tool | Preserved fields | Honesty notes |
|---|---|---|
| `get_merge_request_diffs` | `diffs` | First page only (`per_page` 100). Local `truncate_lines` ⇒ `content_complete:"false"`. |
| `get_merge_request_file_diff` | `diffs` | First page only (`per_page` 200). Requested path missing on a partial page ⇒ limitation **unobserved** (never conclusive absence). |
| `get_merge_request_conflicts` | `has_conflicts`, `detailed_merge_status`, `conflict_files`, `merge_request_iid` | Heuristic marker scan coverage is described in `section`; scan never overrides GitLab mergeability flags. |

Consumer examples (success + partial) for the shared `{diffs, pagination, section}`
envelope shape. Empty filtered `get_merge_request_file_diff` results and empty
`conflict_files` stay JSON `null` (legacy) — see examples under those tools in
[`tools.md`](tools.md). `get_merge_request_diffs` empty pages stay SDK `[]`.

**Success (exhausted known page):**

```json
{
  "diffs": [
    {
      "old_path": "a.go",
      "new_path": "a.go",
      "a_mode": "100644",
      "b_mode": "100644",
      "diff": "+x\n",
      "new_file": false,
      "renamed_file": false,
      "deleted_file": false,
      "generated_file": false,
      "collapsed": false,
      "too_large": false
    }
  ],
  "pagination": { "next_page": 0 },
  "section": {
    "retrieved_at": "2026-10-03T15:00:00Z",
    "source": "gitlab_rest",
    "provider": "gitlab",
    "capability_version": "readmeta.mr_diffs.v1",
    "head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "pagination_exhausted": true,
    "content_complete": "true",
    "consistency": "consistent",
    "limitations": [],
    "next_cursor": null,
    "counts": { "items": 1, "bytes": 120, "files": null },
    "manifest_coverage": "full",
    "patch_coverage": "full"
  }
}
```

**Partial (next page):**

```json
{
  "diffs": [
    {
      "old_path": "a.go",
      "new_path": "a.go",
      "a_mode": "100644",
      "b_mode": "100644",
      "diff": "+x\n",
      "new_file": false,
      "renamed_file": false,
      "deleted_file": false,
      "generated_file": false,
      "collapsed": false,
      "too_large": false
    }
  ],
  "pagination": { "next_page": 2 },
  "section": {
    "retrieved_at": "2026-10-03T15:00:00Z",
    "source": "gitlab_rest",
    "provider": "gitlab",
    "capability_version": "readmeta.mr_diffs.v1",
    "head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    "pagination_exhausted": false,
    "content_complete": "false",
    "consistency": "consistent",
    "limitations": [],
    "next_cursor": "2",
    "counts": { "items": 1, "bytes": 120, "files": null },
    "manifest_coverage": "partial",
    "patch_coverage": "partial"
  }
}
```

## Paging honesty

`pagination_exhausted` is true only when raw `X-Next-Page` is present and indicates no further page.
Absent paging headers keep `pagination_exhausted=false` and mark unknown/partial metadata — SDK `NextPage==0` alone is not exhaustion.
Absent/null `collapsed`/`too_large` presence keeps `content_complete` unknown even when the last-page header is explicit.

## Stable codes

`inaccessible`, `unsupported`, `partial`, `inconsistent`, `collapsed`, `too_large`,
`budget_items`, `budget_bytes`, `budget_elapsed`, `budget_requests`,
`http_error`, `cancelled`, `unknown_count`, `authz_denied`, `identity_unresolved`

## Diff manifest

`get_merge_request_diff_window` and review-context `diff_manifest` share one provider. `manifest_coverage=full` only after the version proof (`state=collected`, canonical `real_size` equals the full path count, closing re-read of head, base, and start). `patch_coverage` stays `unknown` for manifest mode. Missing or malformed state, count, or diffs array stays `content_complete=unknown`. A non-final local window stays `content_complete=false` with `next_cursor` set even when coverage, file count, and the full-sequence digest are already proved. The final page is `content_complete=true`, pagination exhausted, and `next_cursor` null. Compare results stay partial unless an authorized git-cache recovery can produce an exact straight or base→head comparison. Recovery re-authorizes even for warm objects, uses the selected SHAs (it does not recompute a merge-base), and stamps `section.source=git_cache` / `section.provider=git` plus a non-digest `provenance` sibling (projects, SHAs, command semantics). Wrong, missing, or mismatched objects never become a verified complete result. The direct tool needs `GITLAB_MCP_CURSOR_KEY`; continuation is `dm1` ([cursors.md](cursors.md)), with `resync_required` and a fixed 2h TTL. Review-context `diff_manifest` has no `next_cursor`. The version list walks at most 20 pages and then fails closed as `provider_page_ambiguous`; that cap is not raised.

Content mode (`mode=content`, capability `readmeta.diff_content.v1`) selects exact `old_path`/`new_path` matches without globbing. It returns per-file statuses (text, binary, submodule, mode-only, collapsed, too_large, unsupported, malformed, unavailable, metadata) and cropped windows whose hashes cover only returned fragment bytes (`diff_content.window_text.v1` / `diff_content.returned_windows_concat.v1`). `full_patch_hash` is always null: selected or cropped content never claims full MR patch coverage. Known omissions (context removal, cropping, unavailable/binary/submodule/malformed selected text) set `content_complete=false`; an uncropped syntactically valid but unproved-complete source stays `unknown` and never `true` without a new completeness proof. Selector `absent` is reserved for a conclusive negative match on a proved complete manifest; failed/partial reads use `unobserved` (or equivalent non-absent) outcomes. Content mode sets `next_cursor` null and does not mint or accept `dm1` tokens. Straight compare content requires a closing compare read; proved consistency may be `consistent` while coverage stays partial. Binary recognition is framing/metadata only (not substrings inside hunks). Observed `a_mode`/`b_mode` `160000` is submodule. Explicit empty `diff:""` is retained as present metadata (distinct from null/missing/wrong-type). Selected retained patches are capped at 1 MiB post-decode; decoder string materialization of larger values remains wire-budget-bounded and is not a measured peak-memory claim. No-newline markers are selected and budgeted atomically with their source line. Manifest default/dm1 behavior is unchanged when `mode` is omitted or `manifest`.

### Content mode examples

Examples below are tool-tagged for documentation validation. List-diff examples earlier in this file remain `list_merge_request_diffs` OutputSchema fixtures. Content examples are **not** validated against that list schema; they mirror registered `get_merge_request_diff_window` (`mode=content`) CallTool projections. Window/line detail and some section keys may be **illustrative abridgements** (marked `…`); **requested `selectors` path/status sets are complete and authoritative** (not abridged). Mandatory documented enums (`selection.kind`, coverage/`content_complete`/`consistency`, limitation `code`/`message`, selector statuses, hash nullability) must match the live CallTool oracle.

**Success (version, selected text)** — `tool: get_merge_request_diff_window` / content. Actual version-content coverage is `manifest_coverage=unknown`, `patch_coverage=partial`, `selection.kind=full_version`; `content_complete` stays `unknown` when the selected source is syntactically valid but not newly proved complete:

```json
{
  "section": {
    "capability_version": "readmeta.diff_content.v1",
    "content_complete": "unknown",
    "manifest_coverage": "unknown",
    "patch_coverage": "partial",
    "consistency": "consistent",
    "next_cursor": null,
    "limitations": []
  },
  "selection": {"project_id": "42", "merge_request_iid": 1, "kind": "full_version", "version_id": 1},
  "files": [{
    "old_path": "a.go", "new_path": "a.go", "a_mode": "100644", "b_mode": "100644",
    "status": "text",
    "windows": [{"text": "@@ -1 +1 @@\n-a\n+b\n", "window_hash": {"algorithm": "sha256", "scope": "diff_content.window_text.v1", "value": "…"}}]
  }],
  "selectors": [{"path": "a.go", "status": "matched"}],
  "returned_content_hash": {"algorithm": "sha256", "scope": "diff_content.returned_windows_concat.v1", "value": "…"},
  "full_patch_hash": null
}
```

`content_complete` stays `unknown` here when the source is syntactically valid but not newly proved complete for selected text; known omissions force `false` instead.

**Partial (straight compare)** — `tool: get_merge_request_diff_window` / content. Opening+closing compare agree; coverage remains partial; limitation field is `message` (not `detail`); missing selector is `unobserved`, never `absent`:

```json
{
  "section": {
    "capability_version": "readmeta.diff_content.v1",
    "content_complete": "false",
    "manifest_coverage": "unknown",
    "patch_coverage": "partial",
    "consistency": "consistent",
    "next_cursor": null,
    "limitations": [{"code": "partial", "message": "compare"}]
  },
  "selection": {"kind": "incremental", "from_sha": "…", "to_sha": "…", "straight": true},
  "files": [{"status": "text", "windows": [{"text": "@@ -1 +1 @@\n-a\n+b\n", "window_hash": {"scope": "diff_content.window_text.v1", "value": "…"}}]}],
  "selectors": [{"path": "a.go", "status": "matched"}, {"path": "missing.go", "status": "unobserved"}],
  "returned_content_hash": {"scope": "diff_content.returned_windows_concat.v1", "value": "…"},
  "full_patch_hash": null
}
```

**Error / fail-closed** — `tool: get_merge_request_diff_window` / content. Closing drift, duplicate proof members, budget/cancel, or malformed global JSON: no trusted `files` windows and `returned_content_hash` is null (or the tool returns a typed `budget_*` / cancel error). Closing path drift retains requested selectors as `unobserved`. Mandatory coverage/consistency fields match the registered CallTool oracle; other section keys may remain abridged:

```json
{
  "section": {
    "capability_version": "readmeta.diff_content.v1",
    "content_complete": "false",
    "manifest_coverage": "unknown",
    "patch_coverage": "partial",
    "consistency": "inconsistent",
    "next_cursor": null,
    "limitations": [{"code": "partial", "message": "compare"}]
  },
  "selection": {"kind": "incremental", "from_sha": "…", "to_sha": "…", "straight": true},
  "files": [],
  "selectors": [{"path": "a.go", "status": "unobserved"}],
  "returned_content_hash": null,
  "full_patch_hash": null
}
```

**Selector unknown-vs-absent** — `absent` only after a conclusive negative on a proved complete version/tuple manifest; HTTP/identity/count/state failures and straight/partial reads use `unobserved` / `unavailable`. Nullable metadata (`a_mode`/`b_mode`/`binary`/`too_large`/…) stays null when the provider omitted the field; raw `\ No newline at end of file` markers attach to the preceding source line and are never emitted as separate anchorable line kinds. Straight responses may be `consistency=consistent` while `patch_coverage=partial` and `content_complete=false`.
