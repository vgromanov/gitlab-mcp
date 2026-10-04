package gitcache

import (
	"encoding/hex"
	"strings"
)

// IndexArgv is the only index-pack command this package can launch.
// The index path is the final name, so Git creates that file directly.
func IndexArgv(git string) ([]string, error) {
	if err := absoluteTool(git); err != nil {
		return nil, err
	}
	return append(gitPrefix(git),
		"index-pack",
		"--strict",
		"--index-version=2",
		"--no-rev-index",
		"--threads=1",
		"--max-input-size=67108864",
		"-o", "objects/pack/input.idx",
		"objects/pack/input.pack",
	), nil
}

// CatFileCheckArg is the only cat-file formatter. It prints a name and a type.
// It is one argv element. It is not --batch and does not request object bytes.
const CatFileCheckArg = "--batch-check=%(objectname) %(objecttype)"

// CatFileStdoutMax is the cat-file stdout cap from the approved design.
const CatFileStdoutMax = 4096

// AllowedConfig is the only repository config a generation may store.
// Later builtins read this file; anything else is not the audited allowlist.
const AllowedConfig = "[core]\n" +
	"\trepositoryformatversion = 0\n" +
	"\tbare = true\n" +
	"\tlogallrefupdates = false\n" +
	"\tfsyncMethod = fsync\n"

// AllowedHEAD is the only HEAD a generation may store.
const AllowedHEAD = "ref: refs/heads/acquired\n"

// VersionArgv is the fixed version role.
func VersionArgv(git string) ([]string, error) {
	if err := absoluteTool(git); err != nil {
		return nil, err
	}
	return append(gitPrefix(git), "version"), nil
}

// RevListArgv streams objects reachable from one full tip OID.
// The tip is 40 hex digits. Revision syntax is rejected.
func RevListArgv(git, tip string) ([]string, error) {
	if err := absoluteTool(git); err != nil {
		return nil, err
	}
	if !fullOID(tip) {
		return nil, ErrPath
	}
	return append(gitPrefix(git),
		"rev-list", "--objects", "--no-object-names", "--missing=error", tip,
	), nil
}

// CatFileArgv is the fixed cat-file role. Stdin is a separate bounded OID list.
func CatFileArgv(git string) ([]string, error) {
	if err := absoluteTool(git); err != nil {
		return nil, err
	}
	return append(gitPrefix(git), "cat-file", CatFileCheckArg), nil
}

func gitPrefix(git string) []string {
	return []string{
		git,
		"--no-pager",
		"--no-replace-objects",
		"--no-lazy-fetch",
		"--no-optional-locks",
		"-c", "core.fsyncMethod=fsync",
		"-c", "pack.writeReverseIndex=false",
	}
}

func fullOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func absoluteTool(git string) error {
	if git == "" || git[0] != '/' || strings.Contains(git, "..") || strings.ContainsAny(git, "\x00 \t\n") {
		return ErrPath
	}
	return nil
}

// CatFileInput builds the only stdin frame a cat-file role may see.
// More than 33 object ids, or any non-hex byte, is rejected before a process starts.
func CatFileInput(oids []string) ([]byte, error) {
	if len(oids) > 33 {
		return nil, ErrQuota
	}
	var b strings.Builder
	for _, oid := range oids {
		if len(oid) != 40 {
			return nil, ErrPath
		}
		if _, err := hex.DecodeString(oid); err != nil {
			return nil, ErrPath
		}
		b.WriteString(oid)
		b.WriteByte('\n')
	}
	if b.Len() > 1353 {
		return nil, ErrQuota
	}
	return []byte(b.String()), nil
}

// AdmitPack rejects a pack write that would pass PackMax before any byte is stored.
func AdmitPack(have, n uint64) error {
	if n > PackMax || have > PackMax-n {
		return ErrQuota
	}
	return nil
}

// AdmitMetaBody rejects repository config and HEAD that are not the allowlist.
func AdmitMetaBody(name string, data []byte) error {
	switch name {
	case "config":
		if string(data) != AllowedConfig {
			return ErrAudit
		}
	case "HEAD":
		if string(data) != AllowedHEAD {
			return ErrAudit
		}
	}
	return nil
}

// AdmitMeta rejects one metadata file, or the simultaneous old-plus-scratch sum.
func AdmitMeta(name string, n uint64, simultaneous uint64) error {
	caps := map[string]uint64{
		"config":              MetaConfig,
		"HEAD":                MetaHEAD,
		"refs/heads/acquired": MetaRef,
		"provenance":          MetaProvenance,
		"manifest":            MetaManifest,
	}
	cap, ok := caps[name]
	if !ok || n > cap || simultaneous > MetaSimul || simultaneous > MetaMax {
		return ErrQuota
	}
	return nil
}
