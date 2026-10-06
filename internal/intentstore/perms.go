package intentstore

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
)

func isSymlink(info os.FileInfo) bool {
	m := info.Mode()
	// Go 1.25 reports a Windows directory junction or mount point as
	// ModeIrregular (reparse point), not ModeSymlink. The database open
	// would otherwise follow it after the walk accepted the component.
	return m&os.ModeSymlink != 0 || m&os.ModeIrregular != 0
}

// rejectDotDot refuses a path that still contains `..`. filepath.Clean
// would collapse /safe/link/../db before the descriptor walk, while
// the database open still receives the original name and can land under the link.
func rejectDotDot(path string) error {
	for _, part := range strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	}) {
		if part == ".." {
			return errors.New("intent store: path must not contain ..")
		}
	}
	return nil
}

func checkDir(path string, info os.FileInfo) error {
	if isSymlink(info) {
		return ErrSymlink
	}
	if !info.IsDir() {
		return ErrUnsafePermissions
	}
	return checkDirAccess(path, info)
}

func checkFileMode(path string, info os.FileInfo) error {
	if isSymlink(info) {
		return ErrSymlink
	}
	if !info.Mode().IsRegular() {
		return ErrUnsafePermissions
	}
	return checkFileAccess(path, info)
}

type fileID struct {
	dev uint64
	ino uint64
	ok  bool
}

func sameFile(a, b fileID) bool {
	return a.ok && b.ok && a.dev == b.dev && a.ino == b.ino
}

// windowsPathPrefixes walks a Windows path one component at a time.
// Drive roots stay absolute: filepath.Join("C:", "private") is C:private,
// which would skip C:\private\link and follow a junction there.
func windowsPathPrefixes(clean string) []string {
	vol := windowsVolume(clean)
	rest := clean[len(vol):]
	sep := `\`
	if strings.Contains(clean, `/`) && !strings.Contains(clean, `\`) {
		sep = `/`
	}
	acc := vol
	var out []string
	for _, part := range strings.Split(rest, sep) {
		if part == "" {
			if acc == "" {
				acc = sep
			}
			continue
		}
		acc = joinWindowsAbs(acc, part, sep)
		out = append(out, acc)
	}
	return out
}

func windowsVolume(clean string) string {
	if share := uncShare(clean); share != "" {
		return share
	}
	if len(clean) >= 2 && ((clean[0] >= 'A' && clean[0] <= 'Z') || (clean[0] >= 'a' && clean[0] <= 'z')) && clean[1] == ':' {
		return clean[:2]
	}
	return ""
}

func joinWindowsAbs(acc, part, sep string) string {
	switch {
	case acc == "" || acc == sep:
		return sep + part
	case len(acc) == 2 && acc[1] == ':':
		return acc + sep + part
	case strings.HasSuffix(acc, sep):
		return acc + part
	default:
		return acc + sep + part
	}
}

func lstat(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if isSymlink(info) {
		return nil, ErrSymlink
	}
	return info, nil
}

// classifyHeader rejects non-empty files that are not an intent-store
// container. Empty files are initialized by Open.
func classifyHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	want := int(pageSize) + headerMagicOffset + 4
	buf := make([]byte, want)
	n, err := io.ReadAtLeast(f, buf, headerLen)
	if n == 0 && errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return ErrCorrupt
	}
	if n < headerLen {
		return ErrCorrupt
	}
	if boltMagicAt(buf, headerMagicOffset) || boltMagicAt(buf, int(pageSize)+headerMagicOffset) {
		return nil
	}
	return ErrCorrupt
}

func boltMagicAt(buf []byte, off int) bool {
	if off < 0 || off+4 > len(buf) {
		return false
	}
	return binary.LittleEndian.Uint32(buf[off:]) == containerMagic
}

func fileSize(path string) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	if isSymlink(info) {
		return 0, ErrSymlink
	}
	return info.Size(), nil
}
