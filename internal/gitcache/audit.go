package gitcache

// SelectedBuild is the only Git build this package will exec.
// Version text alone is not attestation. Callers must pass a measured identity.
const (
	GitSourceVersion = "2.50.1"
	GitSourceSHA256  = "7e3e6c36decbd8f1eedd14d42db6674be03671c2204864befa2a41756c5c8fc4"
	GitArchiveURL    = "https://www.kernel.org/pub/software/scm/git/git-2.50.1.tar.xz"
)

// CommonBuildFlags are the flags shared by Linux and Darwin, in order.
var CommonBuildFlags = []string{
	"NO_CURL=YesPlease",
	"NO_EXPAT=YesPlease",
	"NO_GETTEXT=YesPlease",
	"NO_TCLTK=YesPlease",
	"NO_OPENSSL=YesPlease",
	"NO_APPLE_COMMON_CRYPTO=YesPlease",
}

// GitBuildFlags is the Linux fixture list, including NO_ICONV.
var GitBuildFlags = append(append([]string{}, CommonBuildFlags...), "NO_ICONV=YesPlease")

// FlagsForOS returns the only flag list accepted for that OS.
// Darwin omits NO_ICONV so compat/posix.h includes iconv.h before sane-ctype.h.
// Linux keeps NO_ICONV. Any other OS, or a swapped list, is rejected.
func FlagsForOS(osName string) ([]string, error) {
	switch osName {
	case "linux":
		return append([]string{}, GitBuildFlags...), nil
	case "darwin":
		return append([]string{}, CommonBuildFlags...), nil
	default:
		return nil, ErrUnsupported
	}
}

// AuditNote records the selected-build writer and descendant closure.
// It is source evidence for git-2.50.1, not a claim about any other binary.
const AuditNote = `
source: git-2.50.1.tar.xz sha256 7e3e6c36decbd8f1eedd14d42db6674be03671c2204864befa2a41756c5c8fc4, read at git.c, common-init.c, config.c, trace2.c, trace2/tr2_sysenv.c, trace2/tr2_dst.c, setup.c, revision.c, object-store.c, promisor-remote.c, pager.c, pack-write.c, csum-file.c, fsck.c, builtin/index-pack.c, builtin/cat-file.c, builtin/rev-list.c, help.c.
profile: Linux>=5.15 amd64/arm64 and macOS 15 / Darwin 24 amd64/arm64. Other OS builds, Apple Git, and unaudited patches are unsupported.
startup: common-init.c trace2_initialize runs only after the process environment exists. tr2_sysenv_load uses read_very_early_config, which sets ignore_repo. git_config_system honors GIT_CONFIG_NOSYSTEM. GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM select those files. Repo config is read later by the builtin. The only accepted config bytes are the core allowlist (repositoryformatversion, bare, logallrefupdates, fsyncMethod).
index-pack writers, fixed argv (positional pack, -o final idx, --no-rev-index, --threads=1, no --stdin, --fix-thin, --promisor, or --keep): open_pack_file (index-pack.c:359) takes the else branch, xopen O_RDONLY, output_fd=-1, so the tmp_pack odb_mkstemp at :365 is not reached. write_idx_file (pack-write.c:89) sees a non-nil index_name, unlinks that name, and xopen O_CREAT|O_EXCL|O_WRONLY; tmp_idx at :87 is not reached. --no-rev-index sets rev_index=0 (:2005) and write_rev_file is skipped (:2101). opts.flags clears WRITE_REV first (:2039). conclude_pack rewrites the pack only when fix_thin_pack is set; that flag is only set by --fix-thin. final() (:1608) calls rename_tmp_packfile, which renames only when the final name differs from the current name (:1597); the explicit index path is the same name and is chmod 0444. No setsid in index-pack.c, pack-write.c, cat-file.c, or rev-list.c.
descendants: the only start_command in index-pack.c is repack_local_links (:1849), and that function returns immediately when outgoing_links is empty (:1826). record_outgoing_links is assigned only from --promisor (:1958). do_record_outgoing_links runs only inside that flag (:961), even though --strict also enters the fsck block at :929. cat-file --batch-check sets BATCH_MODE_INFO (:1001). batch_object_write emits the format and calls print_object_or_die only for BATCH_MODE_CONTENTS (:556). %(objectname) and %(objecttype) (expand_atom :319 and :322) copy the hex id and the type name. textconv_object and filter_object are inside print_object_or_die and require transform_mode from --textconv or --filters, which this argv does not pass. rev-list.c has no start_command or run_command. --threads=1 does not add a pack-objects child. Builtin dispatch of these names does not take the alias or dashed external path.
darwin recipe: Linux keeps NO_ICONV. Darwin omits only that flag. compat/posix.h includes iconv.h at the NO_ICONV guard (posix.h:227) before sane-ctype.h (posix.h:449). Defining NO_ICONV skips that include. Darwin config.mak.uname always adds -DPRECOMPOSE_UNICODE and compat/precompose_utf8.o. precompose_utf8.h then includes iconv.h after the ctype macros, which collides with the Xcode 16.4 SDK _ctype.h inline functions. Omitting NO_ICONV on Darwin restores the upstream include order. It does not change archive hash, version, or the other six flags. The file-creating probe is probe_utf8_pathname_composition (precompose_utf8.c:45), called only from create_default_files (setup.c:2401), which is called only from init_db (setup.c:2597). init_db is reached from cmd_init_db (builtin/init-db.c:255) and cmd_clone (builtin/clone.c:1180). version, index-pack, cat-file, and rev-list do not call init_db. git.c:462 calls precompose_argv_prefix for every builtin; precompose_string_if_needed (precompose_utf8.c:68) returns the original pointer unless core.precomposeunicode is already 1 and the argument contains a non-ASCII byte. The fixed role argv is ASCII, so that path does not call iconv and does not create a file. precompose_utf8_readdir can iconv a directory entry when a command reads a directory; that is a read-side transform, not the init probe.
closure: the only setsid() outside tests and documentation is daemonize (setup.c:2008). Callers are builtin/gc.c:956, builtin/gc.c:1637, and daemon.c:1448. version, index-pack, cat-file, and rev-list do not call it. git.c run_argv calls handle_builtin before any alias (git.c:811). A matching builtin exits at git.c:747 and does not reach execv_dashed_external (git.c:781) or the alias run_command sites (git.c:396 and git.c:841). --no-pager sets use_pager=0 (git.c:196). commit_pager_choice (git.c:471) then sets GIT_PAGER=cat and does not call setup_pager (pager.c:172). Trace2 targets come from tr2_sysenv_get. tr2_sysenv_load (trace2/tr2_sysenv.c:96) uses read_very_early_config, which sets ignore_repo, ignore_worktree, and ignore_cmdline (config.c:2187). The role environment replaces the process environment and sets none of GIT_TRACE2, GIT_TRACE2_EVENT, or GIT_TRACE2_PERF. tr2_dst_get_trace_fd returns fd 0 and does not open when that value is unset (trace2/tr2_dst.c:324). A trace2 key in the generation is not an input to that early read. --alternate-refs is the only revision argument that calls for_each_alternate_ref (revision.c:2878); the rev-list argv does not pass it. object-store.c:503 is inside read_alternate_refs and is reached only from that walk. index-pack calls promisor_remote_get_direct (index-pack.c:1510) only after repo_has_promisor_remote (index-pack.c:1497). The allowlisted config has no promisor remote. create_tmp_packfile is reached from pack-objects and bulk-checkin, not from these four builtins. csum-file.c writes the hash fd it is given and opens /dev/null only as a verify sink. fsck.c has no odb_mkstemp. A different source hash, flag set, or OS is not this audit.
not closed: RLIMIT_AS of 8GiB is part of the role contract. Installing the rlimit is not enforcement: before ACK and Exec the helper must succeed a PROT_NONE map under the cap and fail a map of 8GiB+4096 with ENOMEM. If the kernel rejects the rlimit, the under-map fails, or the oversized map succeeds, Exec is skipped. A successful compile is not that proof.
`

// BuildIdentity is measured from the fixture, not from git version alone.
type BuildIdentity struct {
	SourceSHA256 string
	Flags        []string
	OS           string
	Arch         string
	Compiler     string
	BinarySHA256 string
	VersionLine  string
}

// IdentityOK accepts only the pinned source and the exact flag list.
// OS must be the audited profile. Darwin major 24 is macOS 15; Linux is accepted
// only as linux (kernel floor is checked by the caller when uname -r is present).
func IdentityOK(id BuildIdentity, kernelRelease string, darwinMajor int) error {
	if id.SourceSHA256 != GitSourceSHA256 {
		return ErrAudit
	}
	want, err := FlagsForOS(id.OS)
	if err != nil {
		return ErrUnsupported
	}
	if len(id.Flags) != len(want) {
		return ErrAudit
	}
	for i := range want {
		if id.Flags[i] != want[i] {
			return ErrAudit
		}
	}
	if id.BinarySHA256 == "" || id.Compiler == "" || id.VersionLine == "" {
		return ErrAudit
	}
	switch id.OS {
	case "linux":
		if !linuxKernelOK(kernelRelease) {
			return ErrUnsupported
		}
	case "darwin":
		if darwinMajor != 24 {
			return ErrUnsupported
		}
	default:
		return ErrUnsupported
	}
	if id.Arch != "amd64" && id.Arch != "arm64" {
		return ErrUnsupported
	}
	return nil
}

func linuxKernelOK(release string) bool {
	// Expect a leading N.M. Require N>5 or N==5 && M>=15. Empty fails closed.
	var maj, min int
	n := 0
	for i := 0; i < len(release); i++ {
		c := release[i]
		if c == '.' {
			n++
			if n == 2 {
				break
			}
			continue
		}
		if c < '0' || c > '9' {
			return false
		}
		if n == 0 {
			maj = maj*10 + int(c-'0')
		} else if n == 1 {
			min = min*10 + int(c-'0')
		}
	}
	if n < 1 {
		return false
	}
	return maj > 5 || (maj == 5 && min >= 15)
}
