//go:build unix

package tlsx

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func readBoundedCA(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, ErrMalformedCA
	}
	if st.Size() <= 0 || st.Size() > maxCABytes {
		return nil, ErrMalformedCA
	}
	b, err := io.ReadAll(io.LimitReader(f, maxCABytes+1))
	if err != nil {
		return nil, err
	}
	after, statErr := f.Stat()
	if statErr != nil || !os.SameFile(st, after) || st.Size() != after.Size() || !st.ModTime().Equal(after.ModTime()) || int64(len(b)) != st.Size() {
		return nil, ErrMalformedCA
	}
	if int64(len(b)) > maxCABytes {
		return nil, ErrMalformedCA
	}
	return b, nil
}
