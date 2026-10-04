package gitcache

// SelectedBuild is the only Git build this package will exec.
// Version text alone is not attestation. Callers must pass a measured identity.
const (
	GitSourceVersion = "2.50.1"
	GitSourceSHA256  = "7e3e6c36decbd8f1eedd14d42db6674be03671c2204864befa2a41756c5c8fc4"
	GitArchiveURL    = "https://www.kernel.org/pub/software/scm/git/git-2.50.1.tar.xz"
)

// GitBuildFlags is the fixture make invocation, in order.
var GitBuildFlags = []string{
	"NO_CURL=YesPlease",
	"NO_EXPAT=YesPlease",
	"NO_GETTEXT=YesPlease",
	"NO_TCLTK=YesPlease",
	"NO_OPENSSL=YesPlease",
	"NO_APPLE_COMMON_CRYPTO=YesPlease",
	"NO_ICONV=YesPlease",
}

// AuditNote records the selected-build writer and descendant closure.
// It is source evidence for git-2.50.1, not a claim about any other binary.
const AuditNote = `
source: git-2.50.1.tar.xz sha256 7e3e6c36decbd8f1eedd14d42db6674be03671c2204864befa2a41756c5c8fc4, read at builtin/index-pack.c, pack-write.c, builtin/cat-file.c, builtin/rev-list.c.
profile: Linux>=5.15 amd64/arm64 and macOS 15 / Darwin 24 amd64/arm64. Other OS builds, Apple Git, and unaudited patches are unsupported.
startup: common-init.c trace2_initialize runs only after the process environment exists. tr2_sysenv_load uses read_very_early_config, which sets ignore_repo. git_config_system honors GIT_CONFIG_NOSYSTEM. GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM select those files. Repo config is read later by the builtin. The only accepted config bytes are the core allowlist (repositoryformatversion, bare, logallrefupdates, fsyncMethod).
index-pack writers, fixed argv (positional pack, -o final idx, --no-rev-index, --threads=1, no --stdin, --fix-thin, --promisor, or --keep): open_pack_file (index-pack.c:359) takes the else branch, xopen O_RDONLY, output_fd=-1, so the tmp_pack odb_mkstemp at :365 is not reached. write_idx_file (pack-write.c:89) sees a non-nil index_name, unlinks that name, and xopen O_CREAT|O_EXCL|O_WRONLY; tmp_idx at :87 is not reached. --no-rev-index sets rev_index=0 (:2005) and write_rev_file is skipped (:2101). opts.flags clears WRITE_REV first (:2039). conclude_pack rewrites the pack only when fix_thin_pack is set; that flag is only set by --fix-thin. final() (:1608) calls rename_tmp_packfile, which renames only when the final name differs from the current name (:1597); the explicit index path is the same name and is chmod 0444. No setsid in index-pack.c, pack-write.c, cat-file.c, or rev-list.c.
descendants: the only start_command in index-pack.c is repack_local_links (:1849), and that function returns immediately when outgoing_links is empty (:1826). record_outgoing_links is assigned only from --promisor (:1958). do_record_outgoing_links runs only inside that flag (:961), even though --strict also enters the fsck block at :929. cat-file --batch-check sets BATCH_MODE_INFO (:1001). batch_object_write emits the format and calls print_object_or_die only for BATCH_MODE_CONTENTS (:556). %(objectname) and %(objecttype) (expand_atom :319 and :322) copy the hex id and the type name. textconv_object and filter_object are inside print_object_or_die and require transform_mode from --textconv or --filters, which this argv does not pass. rev-list.c has no start_command or run_command. --threads=1 does not add a pack-objects child. Builtin dispatch of these names does not take the alias or dashed external path.
not closed: a setsid descendant would leave the process group; group ESRCH does not see it. Trace2 still opens a target if the stripped environment or the allowlist config fails to suppress it. A binary whose build flags or source hash differ is not this audit. RLIMIT_AS of 8GiB is part of the role contract. Installing the rlimit is not enforcement: the helper must fail a PROT_NONE map of 8GiB+4096 with ENOMEM after a 1GiB map succeeds. If the kernel rejects the rlimit, or the oversized map succeeds, Exec is skipped and the required CI probe fails.
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
	if len(id.Flags) != len(GitBuildFlags) {
		return ErrAudit
	}
	for i := range GitBuildFlags {
		if id.Flags[i] != GitBuildFlags[i] {
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
