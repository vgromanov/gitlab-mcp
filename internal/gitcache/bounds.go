// Package gitcache is the RVG-131 PR-A resource foundation.
// Production startup does not import it. Dispatch runs only from TestMain.
package gitcache

// Disk bounds from approved-design-v4. Quota counts logical regular-file
// lengths, including inflight, metadata, scratch, and the lock file.
const (
	PackMax   uint64 = 67_108_864
	IndexMax  uint64 = 36_001_072 // 1072 + 36*1_000_000
	MetaMax   uint64 = 8192
	Reserve   uint64 = PackMax + IndexMax + MetaMax // 103_118_128
	RootBytes uint64 = 33_280                       // ledger + one scratch
	QuotaMin  uint64 = RootBytes + Reserve
	QuotaMax  uint64 = 1 << 40

	MaxSlots  = 64
	MaxLeases = 8
	LedgerLen = 256 + MaxSlots*256 // 16_640

	MetaConfig     = 512
	MetaHEAD       = 64
	MetaRef        = 64
	MetaProvenance = 2048
	MetaManifest   = 1024
	MetaSimul      = (MetaConfig + MetaHEAD + MetaRef + MetaProvenance + MetaManifest) * 2 // 7424
)

// Add returns a+b or ErrOverflow.
func Add(a, b uint64) (uint64, error) {
	if a > ^uint64(0)-b {
		return 0, ErrOverflow
	}
	return a + b, nil
}

// QuotaOK reports whether q can fund the root budget and one reservation.
func QuotaOK(q uint64) error {
	if q < QuotaMin || q > QuotaMax {
		return ErrQuota
	}
	return nil
}

// CommittedCharge is actual pack bytes + actual index bytes + the full metadata budget.
func CommittedCharge(pack, index uint64) (uint64, error) {
	if pack > PackMax || index > IndexMax {
		return 0, ErrQuota
	}
	v, err := Add(pack, index)
	if err != nil {
		return 0, err
	}
	return Add(v, MetaMax)
}
