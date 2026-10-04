//go:build !linux && !darwin

package gitcache

// Root is unavailable outside Linux and Darwin. Calls fail closed.
type Root struct{}

// Open refuses to create a cache root on an unaudited platform.
func Open(string, string, uint64) (*Root, error) { return nil, ErrUnsupported }
