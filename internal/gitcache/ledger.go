package gitcache

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

const (
	// Exactly 8 bytes; longer literals truncate on copy and fail parse.
	ledgerMagic   = "GITCACHE"
	ledgerVersion = uint32(2)
	slotBytes     = 256
	slotCount     = 64
	headerBytes   = 256
)

// SlotState is the durable generation lifecycle.
type SlotState uint8

const (
	SlotEmpty SlotState = iota
	SlotReserved
	SlotStaging
	SlotActive
	SlotCommitted
	SlotDeleting
	SlotAmbiguous
)

// Slot is one fixed ledger record.
type Slot struct {
	State          SlotState
	ID             [32]byte
	Charge         uint64
	PackSize       uint64
	IndexSize      uint64
	MetaSize       uint64
	ReadCount      uint32
	Flags          uint32
	ManifestDigest [32]byte
}

type ledgerHeader struct {
	Magic   [8]byte
	Version uint32
	Quota   uint64
}

func emptyLedger(quota uint64) ([]byte, error) {
	buf := make([]byte, headerBytes+slotCount*slotBytes)
	copy(buf[:8], ledgerMagic)
	binary.LittleEndian.PutUint32(buf[8:12], ledgerVersion)
	binary.LittleEndian.PutUint64(buf[12:20], quota)
	sum := hashLedger(buf[:len(buf)-32])
	copy(buf[len(buf)-32:], sum[:])
	return buf, nil
}

func hashLedger(buf []byte) [32]byte {
	return sha256.Sum256(buf)
}

func parseLedger(buf []byte) (ledgerHeader, []Slot, error) {
	if len(buf) != headerBytes+slotCount*slotBytes {
		return ledgerHeader{}, nil, ErrCorrupt
	}
	sum := hashLedger(buf[:len(buf)-32])
	if string(buf[len(buf)-32:]) != string(sum[:]) {
		// compare correctly
		var got [32]byte
		copy(got[:], buf[len(buf)-32:])
		if got != sum {
			return ledgerHeader{}, nil, ErrCorrupt
		}
	}
	var h ledgerHeader
	copy(h.Magic[:], buf[:8])
	if string(h.Magic[:]) != ledgerMagic {
		return ledgerHeader{}, nil, ErrCorrupt
	}
	h.Version = binary.LittleEndian.Uint32(buf[8:12])
	if h.Version != ledgerVersion {
		return ledgerHeader{}, nil, ErrCorrupt
	}
	h.Quota = binary.LittleEndian.Uint64(buf[12:20])
	slots := make([]Slot, slotCount)
	off := headerBytes
	for i := range slots {
		s, err := parseSlot(buf[off : off+slotBytes])
		if err != nil {
			return ledgerHeader{}, nil, err
		}
		slots[i] = s
		off += slotBytes
	}
	return h, slots, nil
}

func parseSlot(b []byte) (Slot, error) {
	if len(b) != slotBytes {
		return Slot{}, ErrCorrupt
	}
	st := SlotState(b[0])
	switch st {
	case SlotEmpty, SlotReserved, SlotStaging, SlotActive, SlotCommitted, SlotDeleting, SlotAmbiguous:
	default:
		return Slot{}, ErrCorrupt
	}
	var s Slot
	s.State = st
	copy(s.ID[:], b[1:33])
	s.Charge = binary.LittleEndian.Uint64(b[33:41])
	s.PackSize = binary.LittleEndian.Uint64(b[41:49])
	s.IndexSize = binary.LittleEndian.Uint64(b[49:57])
	s.MetaSize = binary.LittleEndian.Uint64(b[57:65])
	s.ReadCount = binary.LittleEndian.Uint32(b[65:69])
	s.Flags = binary.LittleEndian.Uint32(b[69:73])
	copy(s.ManifestDigest[:], b[73:105])
	if st != SlotEmpty && s.Charge == 0 {
		return Slot{}, ErrCorrupt
	}
	return s, nil
}

func encodeLedger(h ledgerHeader, slots []Slot) ([]byte, error) {
	if len(slots) != slotCount {
		return nil, errors.New("gitcache: slot count")
	}
	buf := make([]byte, headerBytes+slotCount*slotBytes)
	copy(buf[:8], ledgerMagic)
	binary.LittleEndian.PutUint32(buf[8:12], ledgerVersion)
	binary.LittleEndian.PutUint64(buf[12:20], h.Quota)
	off := headerBytes
	for _, s := range slots {
		encodeSlot(buf[off:off+slotBytes], s)
		off += slotBytes
	}
	sum := hashLedger(buf[:len(buf)-32])
	copy(buf[len(buf)-32:], sum[:])
	return buf, nil
}

func encodeSlot(b []byte, s Slot) {
	for i := range b {
		b[i] = 0
	}
	b[0] = byte(s.State)
	copy(b[1:33], s.ID[:])
	binary.LittleEndian.PutUint64(b[33:41], s.Charge)
	binary.LittleEndian.PutUint64(b[41:49], s.PackSize)
	binary.LittleEndian.PutUint64(b[49:57], s.IndexSize)
	binary.LittleEndian.PutUint64(b[57:65], s.MetaSize)
	binary.LittleEndian.PutUint32(b[65:69], s.ReadCount)
	binary.LittleEndian.PutUint32(b[69:73], s.Flags)
	copy(b[73:105], s.ManifestDigest[:])
}

func chargedTotal(slots []Slot) (uint64, error) {
	var total uint64
	for _, s := range slots {
		if s.State == SlotEmpty {
			continue
		}
		if s.Charge == 0 {
			return 0, ErrCorrupt
		}
		n := total + s.Charge
		if n < total {
			return 0, ErrOverflow
		}
		total = n
	}
	return total, nil
}

// validateLedgerSemantics rejects checksum-valid but accounting-invalid ledgers.
func validateLedgerSemantics(h ledgerHeader, slots []Slot) error {
	if h.Quota < uint64(bounds.BrootBytes) || h.Quota > uint64(^uint64(0)>>1) {
		return ErrCorrupt
	}
	seen := make(map[[32]byte]struct{}, slotCount)
	var zero [32]byte
	var total uint64
	for _, s := range slots {
		switch s.State {
		case SlotEmpty:
			if s.Flags != 0 || s.ManifestDigest != zero || s.Charge != 0 || s.PackSize != 0 || s.IndexSize != 0 || s.MetaSize != 0 || s.ReadCount != 0 || s.ID != zero {
				return ErrCorrupt
			}
			continue
		case SlotReserved, SlotStaging, SlotActive, SlotCommitted, SlotDeleting, SlotAmbiguous:
		default:
			return ErrCorrupt
		}
		if s.Charge == 0 || s.Flags != 0 || s.ReadCount > bounds.MaxReaders || s.Charge > uint64(bounds.GenerationCharge()) {
			return ErrCorrupt
		}
		if s.ID == zero {
			return ErrCorrupt
		}
		if _, dup := seen[s.ID]; dup {
			return ErrCorrupt
		}
		seen[s.ID] = struct{}{}
		if s.State == SlotCommitted && (s.ManifestDigest == zero || s.PackSize < 32 || s.IndexSize < 1072 || s.MetaSize == 0) {
			return ErrCorrupt
		}
		sum := s.PackSize + s.IndexSize
		if sum < s.PackSize {
			return ErrOverflow
		}
		sum2 := sum + s.MetaSize
		if sum2 < sum {
			return ErrOverflow
		}
		if sum2 > s.Charge {
			return ErrCorrupt
		}
		if s.PackSize > uint64(bounds.MaxPackBytes) || s.IndexSize > uint64(bounds.MaxIndexBytes) || s.MetaSize > uint64(bounds.MaxMetaBytes) {
			return ErrCorrupt
		}
		n := total + s.Charge
		if n < total {
			return ErrOverflow
		}
		total = n
	}
	broot := uint64(bounds.BrootBytes)
	if broot+total < broot || broot+total > h.Quota {
		return ErrCorrupt
	}
	return nil
}
