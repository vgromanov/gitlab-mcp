//go:build !linux && !darwin

package gitcache

// Unsupported platforms keep the package graph closed without Unix descriptor
// helpers. Linux/Darwin provide the real openat/fstat implementations.

func openatDir(int, string) (int, error) { return -1, ErrPlatform }
func openatNoFollow(int, string, int, uint32) (int, error) {
	return -1, ErrPlatform
}
func mkdirat(int, string, uint32) error       { return ErrPlatform }
func unlinkat(int, string, int) error         { return ErrPlatform }
func renameat(int, string, int, string) error { return ErrPlatform }
