package intentstore

import (
	"errors"
	"io"
	"os"
	"runtime"
)

func isSymlink(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}

func checkDir(info os.FileInfo) error {
	if isSymlink(info) || !info.IsDir() {
		if isSymlink(info) {
			return ErrSymlink
		}
		return ErrUnsafePermissions
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	if info.Mode().Perm() != 0o700 {
		return ErrUnsafePermissions
	}
	return nil
}

func checkFileMode(info os.FileInfo) error {
	if isSymlink(info) {
		return ErrSymlink
	}
	if !info.Mode().IsRegular() {
		return ErrUnsafePermissions
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	perm := info.Mode().Perm()
	if perm&0o200 == 0 {
		return ErrReadOnly
	}
	if perm != 0o600 {
		return ErrUnsafePermissions
	}
	return nil
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
		if err := checkFileMode(info); err != nil {
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
			if err := checkFileMode(info); err != nil {
				return err
			}
			continue
		}
		if err := os.Chmod(p, 0o600); err != nil {
			return err
		}
		info, err = os.Lstat(p)
		if err != nil {
			return err
		}
		if err := checkFileMode(info); err != nil {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return checkFileMode(info)
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
