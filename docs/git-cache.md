# Git cache resource foundation

This package is the RVG-131 PR-A resource foundation. Production startup does not import it. The helper roles exist only in the test binary's `TestMain`. No acquisition, transport, or authorization bridge is wired here.

## Disk invariant

`Q` is the sum of logical regular-file lengths under the private root, including inflight, metadata, scratch, and the lock. It is not allocated blocks, volume free space, or swap.

| Symbol | Bytes | Meaning |
| --- | ---: | --- |
| P | 67108864 | One pack |
| I | 36001072 | Index v2 for at most 1000000 objects (`1072+36*N`) |
| M | 8192 | Metadata budget, kept until the generation is fully deleted |
| R | 103118128 | `P+I+M` charged for every active or ambiguous slot |
| Broot | 33280 | Ledger, one equal replacement scratch, and the lock reservation |
| Q | Broot+R … 1TiB | Caller quota |

The ledger is 256 bytes of header plus 64 fixed 256-byte records (16640 bytes). At most one `ledger.tmp` exists, and `root.lock` stays empty. `Broot` is reserved before any other nonzero write.

A slot is `EMPTY`, then `RESERVED` (durable before any stage or child), `ACTIVE`, `QUIESCENT`, `VERIFIED`, and `COMMITTED`. Restart treats every slot that is not `COMMITTED` or `EMPTY` as frozen: it keeps a full `R` forever and its directory is not modified. There is no PID, deadline, or free-lock reclamation. `readCount > 0` stays pinned across restart. Unknown names or an invalid record deny the root.

Go checks the class and the remaining grant before every write. A pack of `P+1` is rejected in memory and is not stored. Short writes count as failure. The only Git writer is `index-pack` of one index file, with soft=hard `RLIMIT_FSIZE=I` set and read back before `Exec`. Metadata and the pack spool are capped in Go. Old metadata plus its `.tmp` are charged together, and that sum must stay at or below 7424. The unused 768 bytes of `M` are not a license for another file.

## Filesystem

The parent walks an absolute path with `open`/`openat` using `O_DIRECTORY|O_NOFOLLOW` and never changes its own directory. The final directory must be owned by the effective uid and must not be group- or world-writable. Ancestors may be shared. A test may canonicalize its own temp directory before that walk; production does not silently resolve a rejected path.

A helper in its own process group is the only code that calls `Fchdir`. It creates files with `O_NOFOLLOW`, rejects symlinks, cross-device files, and `nlink != 1`, and `fsync`s the file and the parent directory. Same-parent rename is the only rename. `os.Root` is not used as a nofollow proof.

## Process limits

Helper environment is only `HOME=/nonexistent`, `XDG_CONFIG_HOME=/nonexistent`, `LANG=C`, `LC_ALL=C`, and `GOTRACEBACK=none`, plus the fixed `GIT_*` switches for a Git role. There is no `PATH` and no inherited credential, proxy, or trace variable.

Before `Exec`, the launcher sets soft=hard:

| Limit | Index role | Verifier roles |
| --- | --- | --- |
| `RLIMIT_FSIZE` | I | 0 |
| `RLIMIT_AS` | 8GiB | 8GiB |
| `RLIMIT_CPU` | 30s | 30s |
| `RLIMIT_CORE` | 0 | 0 |
| `RLIMIT_NOFILE` | 64 | 64 |

Production CPU stays soft=hard 30s. The evidence helper may separate soft and hard only in the test process. It prints `cpu-ready` after the limit readback and before the burn. A pass is an actual `Wait` of `SIGXCPU` (exit 1 and `cpu-enforced SIGXCPU`) or of hard-limit `SIGKILL` only when `cpu-ready` was printed and the child consumed CPU time. A kill with no setup marker fails. Equal soft and hard limits do not have to deliver `SIGXCPU`. If the helper is still running after 20s, the parent kills the group and then reads the `Wait` that was already started.

If a limit cannot be set or does not read back, the launcher does not `Exec`. Matching `getrlimit` is not enough for `RLIMIT_AS`. The helper maps 1GiB of `PROT_NONE` anonymous address space, which must succeed, then maps 8GiB+4096, which must fail with `ENOMEM`. The pages are not written. If the oversized map succeeds, or the 8GiB rlimit cannot be installed, the probe fails and `Exec` is skipped. The bound stays 8GiB.

`cat-file` uses one argv element, `--batch-check=%(objectname) %(objecttype)`, on at most 33 full OID lines. That formatter prints a name and a type. It is not `--batch`. Stdout is capped at 4096 bytes. `rev-list` is `--objects --no-object-names --missing=error` plus one 40-hex tip.

A token frame is read only after `RLIMIT_CORE` is confirmed to be 0. Startup must ack within 2 seconds; otherwise the known child group is killed and waited, and the reservation stays charged.

A normal Git role waits for exit 0, then `kill(-pgid, 0) == ESRCH`, and only then measures pack and index. A timeout, nonzero exit, or unknown wait kills the known group, reaps it, and does not promote. Cancellation quiescence is `SIGKILL` of the process group, `Wait` of the leader, then `kill(-pgid, 0) == ESRCH`. Leader `Wait` alone is not quiescence. A `setsid` descendant would escape this check. The audited `index-pack` argv does not call `setsid` and does not take the promisor `pack-objects` path. Unknown closure fails closed.

## Selected Git build

Only this source and these flags are in scope. `git version` is not attestation.

- Archive: `https://www.kernel.org/pub/software/scm/git/git-2.50.1.tar.xz`
- SHA256: `7e3e6c36decbd8f1eedd14d42db6674be03671c2204864befa2a41756c5c8fc4`
- Linux build: `make -j2 NO_CURL=YesPlease NO_EXPAT=YesPlease NO_GETTEXT=YesPlease NO_TCLTK=YesPlease NO_OPENSSL=YesPlease NO_APPLE_COMMON_CRYPTO=YesPlease NO_ICONV=YesPlease git`
- Darwin build: the same command without `NO_ICONV=YesPlease`. The other six flags, the archive hash, and Git 2.50.1 stay unchanged. Darwin's `PRECOMPOSE_UNICODE` path includes `iconv.h` from `precompose_utf8.h`. Keeping `NO_ICONV` skips the earlier include in `compat/posix.h` and the Xcode 16.4 SDK ctype header then collides with `sane-ctype.h`. `setup.c` can still call `probe_utf8_pathname_composition`, which is an in-process writer, not a `start_command`.
- Profile: Linux kernel >= 5.15 on amd64 and arm64, and macOS 15 / Darwin 24 on amd64 and arm64

`index-pack` for the fixed argv (positional pack, `-o objects/pack/input.idx`, `--no-rev-index`, `--threads=1`, no `--stdin`, `--fix-thin`, `--promisor`, or `--keep`) opens the pack read-only (`index-pack.c` `open_pack_file` else branch) and creates the index at the final name with `O_CREAT|O_EXCL` (`pack-write.c` `write_idx_file` when `index_name` is set). `tmp_pack`, `tmp_idx`, and `write_rev_file` are on other branches. `final` renames only when the current name and the final name differ. The only `start_command` in `index-pack.c` is `repack_local_links`, which returns before spawning `pack-objects` when `outgoing_links` is empty. `record_outgoing_links` is set only by `--promisor`. `--strict` runs fsck and does not set that flag. `cat-file --batch-check` uses `BATCH_MODE_INFO` and does not call `print_object_or_die`. `%(objectname)` and `%(objecttype)` do not call `textconv`. `rev-list.c` has no `start_command`. None of those files call `setsid`.

Not closed: a `setsid` child is invisible to `kill(-pgid, 0)`; Trace2 can still open a target if the stripped environment or the allowlist config fails; any other source hash, flag set, or OS is unsupported. Darwin 25 and an `RLIMIT_AS` of 8GiB that the kernel rejects, or that does not stop a virtual map past 8GiB, fail closed. The 8GiB bound is not raised or removed to obtain a green run. The required CI probe fails unless the oversized `PROT_NONE` map returns `ENOMEM`.

## CI

`.github/workflows/ci.yml` adds `gitcache-native` on `ubuntu-24.04`, `ubuntu-24.04-arm`, `macos-15-intel`, and `macos-15`. Each job has a 15 minute timeout, at most two run at once, and `contents: read`. The fixture script verifies the archive hash before unpack and builds only under the runner temp directory. The helper is `CGO_ENABLED=0 go test -c`. The native tests are `go test -mod=vendor -race -count=1 -tags=gitcache_ci -timeout=10m ./internal/gitcache`. A missing fixture or an unusable limit fails the job. The existing integration job is unchanged and is not dispatched by this work.
