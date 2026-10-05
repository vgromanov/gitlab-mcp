// Package bounds holds the finite resource contract for the native cache.
//
// Retained inflated pack input and resolved pack output are separate 128 MiB
// counters. Logical disk quota is a third, independent sum. None of these is a
// shared memory pool, a hard process RSS/AS cap, or a forced syscall deadline.
// Cancellation is cooperative via context checkpoints.
package bounds

import (
	"errors"
	"time"
)

var errIndexCount = errors.New("bounds: index object count")

const (
	// MaxRefBytes is the maximum advertised-refs or upload-pack HTTP body.
	MaxRefBytes int64 = 32 << 20

	// MaxPackBytes is the maximum raw pack accepted after the protocol prelude.
	MaxPackBytes int64 = 32 << 20

	// MaxObjectBytes is the maximum inflated size of one object, one delta
	// base, or one delta output.
	MaxObjectBytes int64 = 8 << 20

	// MaxRetainedInput is the maximum sum of inflated pack payloads retained
	// during decode, including delta instruction bodies.
	MaxRetainedInput int64 = 128 << 20

	// MaxRetainedOutput is the maximum sum of resolved undeltified object
	// bytes retained from one pack decode.
	MaxRetainedOutput int64 = 128 << 20

	// MaxObjects is the maximum number of objects in one pack or tree walk.
	MaxObjects = 20000

	// MaxDeltaDepth is the maximum OFS/REF delta chain. The undeltified base
	// is depth 0.
	MaxDeltaDepth = 32

	// Timeout is the cooperative context budget for one acquisition.
	Timeout = 30 * time.Second

	// MaxReaders is the maximum concurrent in-process pinned readers per manager.
	MaxReaders = 8

	// MaxGenerations is the maximum committed/ambiguous generations under one root.
	MaxGenerations = 64

	// MaxMetaBytes is the maximum generation manifest size. Sized for the
	// supported MaxObjects distinct-object JSON map plus grant/root fields
	// (~52 bytes/object worst case plus fixed headers).
	MaxMetaBytes int64 = 2 << 20

	// MaxIndexBytes is the conservative index-v2 ceiling for MaxObjects SHA-1
	// entries including 64-bit offset headroom: 1072 + 36*N. Exact no-ofs64
	// size is IndexV2SHA1Bytes(n) = 1072 + 28*n; writers admit on the exact
	// size before allocation/output.
	MaxIndexBytes int64 = 1072 + 36*MaxObjects

	// MaxPathDepth is the maximum relative path components under the root.
	MaxPathDepth = 16

	// MaxPathBytes is the maximum relative path length under the root.
	MaxPathBytes = 4096

	// MaxScratchBytes is the simultaneous staging/scratch allowance inside the
	// root quota (one pack spool + one index temp + one manifest temp).
	MaxScratchBytes int64 = MaxPackBytes + MaxIndexBytes + MaxMetaBytes

	// LedgerBytes is the durable ledger size (header + fixed slots).
	LedgerBytes int64 = 256 + 64*256 // 16640

	// LockReserveBytes reserves the empty root lock file in quota accounting.
	LockReserveBytes int64 = 0

	// BrootBytes is the root bookkeeping reservation (ledger + equal replacement
	// scratch + lock accounting).
	BrootBytes int64 = LedgerBytes*2 + 64

	// DefaultQuotaBytes is the default logical disk quota when unset.
	DefaultQuotaBytes int64 = BrootBytes + MaxScratchBytes + (MaxPackBytes+MaxIndexBytes+MaxMetaBytes)*4
)

// MaxLooseHeader is retained for compatibility with hermetic fixtures that
// still exercise zlib container sizing; production storage is pack+index only.
const MaxLooseHeader = len("commit ") + len("8388608") + 1

// IndexV2SHA1Bytes is the exact pack index v2 size for n SHA-1 objects when
// every pack offset fits in 31 bits (no offset64 table):
// 8 header + 1024 fanout + 20 pack checksum + 20 idx checksum + 28*n.
func IndexV2SHA1Bytes(n int) (int64, error) {
	if n < 0 || n > MaxObjects {
		return 0, errIndexCount
	}
	return 1072 + 28*int64(n), nil
}

// GenerationCharge is the durable reservation charged before any generation
// output write: pack + index + manifest ceilings.
func GenerationCharge() int64 {
	return MaxPackBytes + MaxIndexBytes + MaxMetaBytes
}

// ReservationFormula documents the root invariant for operators and tests:
// Broot + sum(active|ambiguous generation charges) + inflight scratch <= Q.
func ReservationFormula() string {
	return "Broot + charged_generations + inflight_scratch <= quota"
}
