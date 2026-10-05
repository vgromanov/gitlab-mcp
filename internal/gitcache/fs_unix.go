//go:build linux || darwin

package gitcache

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

// walkRoot opens each path component with O_DIRECTORY|O_NOFOLLOW and never
// Fchdir. The final directory must be owned by the effective uid and must not
// be group- or world-writable. Symlink components are rejected.
func walkRoot(path string) (*os.File, uint64, error) {
	if path == "" || !filepath.IsAbs(path) || len(path) > bounds.MaxPathBytes {
		return nil, 0, ErrPath
	}
	clean := filepath.Clean(path)
	if clean != path && clean+"/" != path {
		// allow trailing slash only via Clean normalization check
	}
	parts := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(parts) > bounds.MaxPathDepth || len(parts) == 0 || (len(parts) == 1 && parts[0] == "") {
		return nil, 0, ErrPath
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, 0, ErrPath
	}
	cur := fd
	for i, name := range parts {
		if name == "" || name == "." || name == ".." || strings.Contains(name, string(os.PathSeparator)) {
			syscall.Close(cur)
			return nil, 0, ErrPath
		}
		if len(name) > 255 {
			syscall.Close(cur)
			return nil, 0, ErrPath
		}
		next, err := openatDir(cur, name)
		syscall.Close(cur)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: %v", ErrPath, err)
		}
		cur = next
		if i == len(parts)-1 {
			st, err := fstat(cur)
			if err != nil {
				syscall.Close(cur)
				return nil, 0, ErrPath
			}
			if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
				syscall.Close(cur)
				return nil, 0, ErrPath
			}
			if uint32(st.Uid) != uint32(os.Geteuid()) {
				syscall.Close(cur)
				return nil, 0, ErrPath
			}
			if st.Mode&0o7777 != 0o700 {
				syscall.Close(cur)
				return nil, 0, ErrPath
			}
			f := os.NewFile(uintptr(cur), clean)
			return f, uint64(st.Dev), nil
		}
	}
	syscall.Close(cur)
	return nil, 0, ErrPath
}

func componentOK(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.Contains(name, "/") || strings.Contains(name, "\x00") {
		return false
	}
	return len(name) <= 255
}

func splitRel(rel string) ([]string, error) {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return nil, ErrPath
	}
	parts := strings.Split(rel, "/")
	if len(parts) > bounds.MaxPathDepth {
		return nil, ErrPath
	}
	var out []string
	var n int
	for _, p := range parts {
		if !componentOK(p) {
			return nil, ErrPath
		}
		n += len(p) + 1
		if n > bounds.MaxPathBytes {
			return nil, ErrPath
		}
		out = append(out, p)
	}
	return out, nil
}

func checkRegular(st *syscall.Stat_t, rootDev uint64) error {
	if uint64(st.Dev) != rootDev {
		return ErrPath
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return ErrPath
	}
	if st.Nlink != 1 {
		return ErrPath
	}
	if uint32(st.Uid) != uint32(os.Geteuid()) {
		return ErrPath
	}
	// Private regular files only: reject any group/other bits.
	if st.Mode&0o7777 != 0o600 {
		return ErrPath
	}
	return nil
}

func checkDir(st *syscall.Stat_t, rootDev uint64) error {
	if uint64(st.Dev) != rootDev {
		return ErrPath
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return ErrPath
	}
	if uint32(st.Uid) != uint32(os.Geteuid()) {
		return ErrPath
	}
	if st.Mode&0o7777 != 0o700 {
		return ErrPath
	}
	return nil
}
