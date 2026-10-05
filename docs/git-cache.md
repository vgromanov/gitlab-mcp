# Native Go object cache (RVG-131)

Opt-in bare object cache inside the `CGO_ENABLED=0` `gitlab-mcp` binary. Disabled
by default. When disabled, ordinary API metadata, approval, discussion, and
raw-file tools keep their existing behavior and perform no cache filesystem or
network work.

Acquisition still does **not** invoke installed Git. RVG-145 comparison recovery
(`get_merge_request_diff_window`) may run a locked-down no-checkout `git diff`
against already authorized objects when the API cannot supply an exact
comparison. Disable `GITLAB_MCP_GIT_CACHE` to keep honest API-partial results.

## What it does

- Native HTTPS upload-pack via an explicit `http.Client` passed to go-git's
  `UploadPackSession` (no default transport registry, no `git.Fetch`).
- Native Go SSH using the reviewed SSH configuration, identity, algorithm, and
  trust path (same settings as listing; no external `ssh`/`git`, no
  ProxyCommand / Match exec helpers).
- Immutable pack + integrity index + manifest generations under a private
  descriptor-relative root (Linux/Darwin). Default-off Windows builds compile
  via closed platform stubs; enabling the cache on Windows fails closed
  (`gitcache: platform unsupported`). No Windows native cache implementation.
- Single acquisition path: fresh `ResolveGrant` (current actor + canonical
  project/group authz + MR/DiffRefs/source-fork API reads) before warm lookup,
  warm return, cold fetch, publication, and cold return. Caller-populated Grant
  fields cannot authorize. Fetch depth is only 1 or 2 (no full-history).
- When the cache is enabled, `get_merge_request` is the MCP operation that
  runs that path for the requested merge request (HTTPS, depth 2, no
  loopback). Acquisition uses the same resolved project as the metadata
  read, including `GITLAB_PROJECT_ID` / `--default-project` when `project_id`
  is omitted. The tool result then includes a non-secret `git_cache` summary
  beside the merge request. Acquisition failure fails the call. With the
  cache disabled, `get_merge_request` stays the API merge-request read and
  does no cache work. No other metadata, approval, discussion, or raw-file
  tool acquires objects, and no public fetch tool is registered.

## What it never does

- Installed Git, external SSH, credential helpers, hooks, checkout, submodule
  execution, file/local protocol, registry fallback, or subprocess FS helpers.
- Persist raw tokens or reusable token digests in metadata/remotes/logs/URLs.
- Inherit API `GITLAB_INSECURE` / proxy settings for cache TLS.
- Register a public MCP “fetch arbitrary SHA/URL” tool.

## Configuration (cache-only)

| Variable | Flag | Default | Notes |
|---|---|---|---|
| `GITLAB_MCP_GIT_CACHE` | `--git-cache` | `false` | Master enable. |
| `GITLAB_MCP_GIT_CACHE_ROOT` | `--git-cache-root` | empty | Required when enabled. Dedicated operator-selected directory. |
| `GITLAB_MCP_GIT_CACHE_QUOTA_BYTES` | `--git-cache-quota-bytes` | derived default | Logical regular-file budget for the whole root. Unset or `0` uses the derived default. A malformed or negative explicit value is rejected. |
| `GITLAB_MCP_GIT_CACHE_CA_CERT_PATH` | `--git-cache-ca-cert` | empty | PEM CA file **augments** system roots; malformed/unreadable fails closed. |
| `GITLAB_MCP_GIT_CACHE_INSECURE` | `--git-cache-insecure` | `false` | Explicit opt-in; does not follow API insecure. |
| `GITLAB_MCP_GIT_CACHE_INSECURE_HOST` | `--git-cache-insecure-host` | `gitlabci.raiffeisen.ru` | Exact hostname allowed for cache insecure TLS. |

The service applies cache startup TLS configuration on every acquisition; an
internal intent cannot weaken it.

Existing `GITLAB_CA_CERT_PATH` / `GITLAB_INSECURE` / proxy variables continue to
affect only the GitLab API client.

## Resource contract

| Cap | Value | Meaning |
|---|---:|---|
| Advertisement | 32 MiB | Its own HTTP response or SSH read |
| Raw pack | 32 MiB | After the protocol prelude; combined role pack payloads also ≤32 MiB |
| Upload-pack prelude | separate | Shallow lines, flush, and the NAK pkt-line. Not charged against the raw pack. SSH counts this stream apart from the ref advertisement. |
| Single object / delta base / delta output | 8 MiB | Inflated |
| Pack / walk entries | 20 000 | 20,000 wire objects across role fetches; separate bounded walks |
| Delta depth | 32 | Undeltified base is depth 0 |
| Retained pack input | 128 MiB | Separate from output; prior role outputs charged before the next decode |
| Retained resolved output | 128 MiB | Separate from input; one Objects map (acquire/publish must not re-decode into a second map) |
| Logical disk quota | operator `Q` | Includes all identities, generations, staging, ledger, scratch |
| Readers | 8 | In-process pins |
| Context budget | 30 s | Cooperative checkpoints; not a hard RSS/AS/syscall preemption |

Reservation formula: `Broot + charged_generations + inflight_scratch <= Q`.
Reservation is taken **before** every generation output write. Ambiguous /
partial-delete / lost-ack states retain charge and fail closed.

Pack decode uses a narrow bounded custom decoder (not stock go-git pack
preallocation as a size guard). Stock `FetchContext` only cancels transport;
decode/tree/cache paths check `context` cooperatively.

## Storage and lifecycle

- Linux/Darwin: `openat` + `O_NOFOLLOW`, held root/lock descriptors, same-device /
  owner / mode / nlink checks. No `Fchdir` child helper.
- One exclusive root lock for the manager lifetime. Exclusion is the root directory inode plus the lock-file inode; replacing `root.lock` does not admit a second manager, and a changed lock identity fails closed.
- Root scans are bounded before enumeration and reject unknown storage. The
  current format owns no staging children; recovery never clears arbitrary files.
- Ledger v2 binds manifest bytes. Warm loads validate exact index bytes regenerated
  from bounded pack offsets/CRCs, object sets/types and immutable manifest roots.
- `EvictGeneration` deletes only ledger-owned, private, unpinned generation files.
  Failed/partial deletion retains charge; quota admission fails until an explicit
  owned eviction succeeds. Automatic replacement policy is not enabled.
- Closing cancels admitted work before joining it; an uncertain join keeps the
  root lock and permits a later close retry. Reader pins precede warm decode.
- Corrupt warm content returns integrity/corrupt errors; it is not silently
  treated as a cache miss.

## Authorization

`AcquireIntent` identifies an MR; it is not a grant. `GitCacheAuthorizer`
builds the Grant only from fresh API state: `CurrentUser`,
`AuthorizeCanonicalProject` (including group ancestry under policy),
`GetMergeRequest` + DiffRefs, `requireProvenMRForkProjects`, and source-project
role-specific clone URLs bound to the full instance scheme/port/relative root
and exact project paths. The actual outbound API endpoint and credential headers
must match configuration. Each exact commit is freshly verified through its
repository API (head/source, base and start/target) and later through its fetched
commit/tree objects; a commit in another role pack supplies no repository proof.
Expected head/base/start/MRVersion on the intent fail closed when DiffRefs
move. Namespaces use an in-memory actor/token domain key; cold restart may
change the key while old generations remain charged. A failed or short domain
seed read leaves the domain unusable so the next call can retry.

Trust is prepared before warm lookup and refreshed before return/publication.
Explicit CA contents augment an immutable per-process system-root snapshot;
fingerprints include its process epoch and certificate bytes. Malformed configured
CA content fails even with explicit insecure mode. SSH provenance binds effective
configured host policy and public known-host bytes; the same snapshot is passed
to cold transport. Authorized host-key update failures propagate before success.
Depth 1/2 limits transport history, while all three authorized commit/tree roots
must be present for a complete acquisition; missing roots return partial.

## Platform support

| Platform | Default (disabled) | Enabled |
|---|---|---|
| Linux amd64/arm64 | works | supported |
| Darwin amd64/arm64 | works | supported |
| Windows | compiles (stubs; cache graph default-off) | fails closed |

## Library pin

`github.com/go-git/go-git/v5` **v5.19.2** (includes fixes for GHSA-qgq7-7hm3-q39j /
CVE-2026-71557 and prior 5.19.1 advisories). Vendored via `go mod vendor`.

## Deviations and limits

- Generation storage uses a real git pack index v2 produced by go-git's
  `idxfile.Writer` + `Encoder`, fed with offsets/CRCs from the bounded custom
  pack decoder (not stock `packfile.Parser` as a size guard). Exact SHA-1
  no-ofs64 size is `1072+28*N`; reservation ceiling remains `1072+36*N`.
- Thin-pack, REF-to-delta chains with external bases, sideband, submodule
  fetch, and signature verification are unsupported (fail closed).
- SSH is a secure supported subset of OpenSSH configuration (same reviewed
  prototype surface): no encrypted unmatched native fallback, certificates/SK,
  proxy/helper/multiplex, or forcing strict host-key policy against configured
  `StrictHostKeyChecking` / `UpdateHostKeys`. The authorized URL port is the
  default when config has no `Port`, including `%p`. A configured port that
  differs from that URL is refused before dialing.
- Logical quota is not a physical filesystem block cap or hard process RSS.
- Independent Sol review is required before calling this production-safe.
