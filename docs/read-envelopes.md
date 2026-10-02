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

## Paging honesty

`pagination_exhausted` is true only when raw `X-Next-Page` is present and indicates no further page.
Absent paging headers keep `pagination_exhausted=false` and mark unknown/partial metadata — SDK `NextPage==0` alone is not exhaustion.
Absent/null `collapsed`/`too_large` presence keeps `content_complete` unknown even when the last-page header is explicit.

## Stable codes

`inaccessible`, `unsupported`, `partial`, `inconsistent`, `collapsed`, `too_large`,
`budget_items`, `budget_bytes`, `budget_elapsed`, `budget_requests`,
`http_error`, `cancelled`, `unknown_count`, `authz_denied`, `identity_unresolved`
