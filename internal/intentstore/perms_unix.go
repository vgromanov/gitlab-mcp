//go:build unix

package intentstore

import "golang.org/x/sys/unix"

func createExclusive(path string) error {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}
