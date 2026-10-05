package intentstore

import (
	"database/sql"
	"errors"
	"io"
	"os"
	"strings"
)

func isSymlink(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
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

func removeStoreFiles(path string) {
	for _, p := range sidecarPaths(path) {
		_ = os.Remove(p)
	}
}

// removeCreatedStore deletes a file this process created only when it has
// not become an initialized intent store. Another process can open the
// empty file and finish initialization while this creator is still failing.
// When the state cannot be read, the files are left in place.
func removeCreatedStore(path string) {
	if storeInitialized(path, nil) {
		return
	}
	removeStoreFiles(path)
}

func storeInitialized(path string, db *sql.DB) bool {
	if id, ok := readAppID(db); ok {
		return id == ApplicationID
	}
	if path == "" {
		return true
	}
	dsn := sqliteFileURI(path, "mode=ro&_query_only=1")
	probe, err := sql.Open("sqlite", dsn)
	if err != nil {
		return true
	}
	defer probe.Close()
	probe.SetMaxOpenConns(1)
	if err := probe.Ping(); err != nil {
		return true
	}
	id, ok := readAppID(probe)
	if !ok {
		return true
	}
	return id == ApplicationID
}

func readAppID(db *sql.DB) (uint32, bool) {
	if db == nil {
		return 0, false
	}
	var appID int64
	if err := db.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
		return 0, false
	}
	return uint32(appID), true
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

// classifyHeader rejects non-empty files that are not SQLite.
// Empty files are initialized by Open.
func classifyHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 16)
	n, err := f.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return ErrCorrupt
	}
	if n == 0 {
		return nil
	}
	const magic = "SQLite format 3\x00"
	if n < len(magic) || string(buf[:len(magic)]) != magic {
		return ErrCorrupt
	}
	return nil
}

func sidecarPaths(path string) []string {
	return []string{path, path + "-wal", path + "-shm"}
}

func existingSidecars(path string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	for _, p := range sidecarPaths(path)[1:] {
		info, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if isSymlink(info) {
			return nil, ErrSymlink
		}
		if err := checkFileMode(p, info); err != nil {
			return nil, err
		}
		out[p] = struct{}{}
	}
	return out, nil
}

func lockDownNewSidecars(path string, before map[string]struct{}) error {
	for _, p := range sidecarPaths(path)[1:] {
		info, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if isSymlink(info) {
			return ErrSymlink
		}
		if _, ok := before[p]; ok {
			if err := checkFileMode(p, info); err != nil {
				return err
			}
			continue
		}
		if err := establishPrivate(p, false); err != nil {
			return err
		}
		info, err = os.Lstat(p)
		if err != nil {
			return err
		}
		if err := checkFileMode(p, info); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkFileMode(path, info)
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

func bytesOnDisk(path string) (int64, error) {
	var total int64
	for _, p := range sidecarPaths(path) {
		info, err := os.Lstat(p)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return 0, err
		}
		if isSymlink(info) {
			return 0, ErrSymlink
		}
		total += info.Size()
	}
	return total, nil
}
