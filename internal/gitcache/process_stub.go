//go:build !linux && !darwin

package gitcache

// ApplyLimits is unsupported outside the audited Linux and Darwin profiles.
func ApplyLimits(uint64) error { return ErrUnsupported }

// ReadToken fails closed where the core-limit check cannot run.
func ReadToken(_ interface{ Read([]byte) (int, error) }) ([]byte, error) {
	return nil, ErrUnsupported
}

// QuiesceGroup fails closed outside the audited profiles.
func QuiesceGroup(int, func() error) error { return ErrNotQuiescent }
