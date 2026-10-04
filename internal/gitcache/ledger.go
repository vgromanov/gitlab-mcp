package gitcache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
)

const (
	stateEmpty uint8 = iota
	stateReserved
	stateActive
	stateQuiescent
	stateVerified
	stateCommitted
)

const magic = "GITCACHE"

// Slot is one fixed ledger record. Frozen slots were not committed when the
// process that reserved them died. They keep a full reservation and are not modified.
type Slot struct {
	ID        string
	Domain    string
	Tip       string
	DestID    string
	State     uint8
	Frozen    bool
	Pack      uint64
	Index     uint64
	Access    uint64
	ReadCount uint32
	PinSync   bool
}

func (s Slot) activeReservation() bool {
	return s.State != stateEmpty && s.State != stateCommitted
}

func (s Slot) charge() (uint64, error) {
	if s.State == stateEmpty {
		return 0, nil
	}
	if s.activeReservation() || s.Frozen {
		return Reserve, nil
	}
	return CommittedCharge(s.Pack, s.Index)
}

type header struct {
	Quota uint64
}

func emptyLedger(quota uint64) ([]byte, error) {
	if err := QuotaOK(quota); err != nil {
		return nil, err
	}
	buf := make([]byte, LedgerLen)
	copy(buf[:8], magic)
	binary.LittleEndian.PutUint32(buf[8:12], 1)
	binary.LittleEndian.PutUint64(buf[16:24], quota)
	binary.LittleEndian.PutUint32(buf[24:28], MaxSlots)
	binary.LittleEndian.PutUint32(buf[28:32], 256)
	sum := hashLedger(buf)
	copy(buf[32:64], sum[:])
	return buf, nil
}

func hashLedger(buf []byte) [32]byte {
	tmp := make([]byte, len(buf))
	copy(tmp, buf)
	for i := 32; i < 64 && i < len(tmp); i++ {
		tmp[i] = 0
	}
	return sha256.Sum256(tmp)
}

func parseLedger(buf []byte) (header, []Slot, error) {
	if len(buf) != LedgerLen {
		return header{}, nil, ErrCorrupt
	}
	if string(buf[:8]) != magic {
		return header{}, nil, ErrCorrupt
	}
	if binary.LittleEndian.Uint32(buf[8:12]) != 1 {
		return header{}, nil, ErrCorrupt
	}
	sum := hashLedger(buf)
	var got [32]byte
	copy(got[:], buf[32:64])
	if got != sum {
		return header{}, nil, ErrCorrupt
	}
	q := binary.LittleEndian.Uint64(buf[16:24])
	if err := QuotaOK(q); err != nil {
		return header{}, nil, ErrCorrupt
	}
	if binary.LittleEndian.Uint32(buf[24:28]) != MaxSlots {
		return header{}, nil, ErrCorrupt
	}
	slots := make([]Slot, MaxSlots)
	for i := 0; i < MaxSlots; i++ {
		s, err := parseSlot(buf[256+i*256 : 256+(i+1)*256])
		if err != nil {
			return header{}, nil, err
		}
		if s.State != stateEmpty && s.State != stateCommitted {
			s.Frozen = true
		}
		slots[i] = s
	}
	if _, err := accounted(q, slots); err != nil {
		return header{}, nil, err
	}
	return header{Quota: q}, slots, nil
}

func parseSlot(rec []byte) (Slot, error) {
	var s Slot
	if len(rec) != 256 {
		return s, ErrCorrupt
	}
	var sum, got [32]byte
	copy(got[:], rec[176:208])
	zero := append([]byte(nil), rec...)
	for i := 176; i < 208; i++ {
		zero[i] = 0
	}
	sum = sha256.Sum256(zero)
	if got != sum {
		return s, ErrCorrupt
	}
	id := cString(rec[0:32])
	dom := cString(rec[32:64])
	tip := cString(rec[64:104])
	dest := cString(rec[104:136])
	s.State = rec[136]
	s.ID, s.Domain, s.Tip, s.DestID = id, dom, tip, dest
	s.Pack = binary.LittleEndian.Uint64(rec[144:152])
	s.Index = binary.LittleEndian.Uint64(rec[152:160])
	s.Access = binary.LittleEndian.Uint64(rec[160:168])
	s.ReadCount = binary.LittleEndian.Uint32(rec[168:172])
	switch s.State {
	case stateEmpty:
		if id != "" || s.Pack != 0 || s.Index != 0 || s.ReadCount != 0 {
			return Slot{}, ErrCorrupt
		}
	case stateReserved, stateActive, stateQuiescent, stateVerified:
		if !hex32(id) {
			return Slot{}, ErrCorrupt
		}
	case stateCommitted:
		if !hex32(id) {
			return Slot{}, ErrCorrupt
		}
		if _, err := CommittedCharge(s.Pack, s.Index); err != nil {
			return Slot{}, ErrCorrupt
		}
	default:
		return Slot{}, ErrCorrupt
	}
	return s, nil
}

func putSlot(rec []byte, s Slot) error {
	for i := range rec {
		rec[i] = 0
	}
	if len(s.ID) > 32 || len(s.Domain) > 32 || len(s.Tip) > 40 || len(s.DestID) > 32 {
		return ErrCorrupt
	}
	copy(rec[0:32], s.ID)
	copy(rec[32:64], s.Domain)
	copy(rec[64:104], s.Tip)
	copy(rec[104:136], s.DestID)
	rec[136] = s.State
	binary.LittleEndian.PutUint64(rec[144:152], s.Pack)
	binary.LittleEndian.PutUint64(rec[152:160], s.Index)
	binary.LittleEndian.PutUint64(rec[160:168], s.Access)
	binary.LittleEndian.PutUint32(rec[168:172], s.ReadCount)
	sum := sha256.Sum256(rec)
	copy(rec[176:208], sum[:])
	return nil
}

func writeLedger(quota uint64, slots []Slot) ([]byte, error) {
	buf, err := emptyLedger(quota)
	if err != nil {
		return nil, err
	}
	if len(slots) != MaxSlots {
		return nil, ErrCorrupt
	}
	for i := range slots {
		if err := putSlot(buf[256+i*256:256+(i+1)*256], slots[i]); err != nil {
			return nil, err
		}
	}
	sum := hashLedger(buf)
	copy(buf[32:64], sum[:])
	if _, _, err := parseLedger(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func accounted(quota uint64, slots []Slot) (uint64, error) {
	sum := RootBytes
	var err error
	for i := range slots {
		c, e := slots[i].charge()
		if e != nil {
			return 0, e
		}
		sum, err = Add(sum, c)
		if err != nil {
			return 0, err
		}
	}
	if sum > quota {
		return sum, ErrQuota
	}
	return sum, nil
}

func hex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
