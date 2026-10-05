package gitcache

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestLedgerEncodeParseRoundTrip(t *testing.T) {
	buf, err := emptyLedger(12345)
	if err != nil {
		t.Fatal(err)
	}
	h, slots, err := parseLedger(buf)
	if err != nil || h.Quota != 12345 || len(slots) != slotCount {
		t.Fatalf("parse empty: %#v %v", h, err)
	}
	slots[0] = Slot{State: SlotCommitted, Charge: 99, PackSize: 1, IndexSize: 2, MetaSize: 3}
	slots[0].ID[0] = 7
	enc, err := encodeLedger(ledgerHeader{Quota: 12345}, slots)
	if err != nil {
		t.Fatal(err)
	}
	h2, slots2, err := parseLedger(enc)
	if err != nil || h2.Quota != 12345 || slots2[0].Charge != 99 || slots2[0].ID[0] != 7 {
		t.Fatalf("roundtrip: %#v %#v %v", h2, slots2[0], err)
	}
	total, err := chargedTotal(slots2)
	if err != nil || total != 99 {
		t.Fatalf("charged: %d %v", total, err)
	}
}

func TestLedgerRejectsCorrupt(t *testing.T) {
	if _, _, err := parseLedger([]byte("short")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("short: %v", err)
	}
	buf, err := emptyLedger(1)
	if err != nil {
		t.Fatal(err)
	}
	buf[len(buf)-1] ^= 0xff
	if _, _, err := parseLedger(buf); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("checksum: %v", err)
	}
	buf, _ = emptyLedger(1)
	copy(buf[:8], []byte("BADMAGIC"))
	sum := hashLedger(buf[:len(buf)-32])
	copy(buf[len(buf)-32:], sum[:])
	if _, _, err := parseLedger(buf); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("magic: %v", err)
	}
	buf, _ = emptyLedger(1)
	binary.LittleEndian.PutUint32(buf[8:12], 99)
	sum = hashLedger(buf[:len(buf)-32])
	copy(buf[len(buf)-32:], sum[:])
	if _, _, err := parseLedger(buf); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("version: %v", err)
	}
	badSlot := make([]byte, slotBytes)
	badSlot[0] = 0xff
	if _, err := parseSlot(badSlot); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("slot state: %v", err)
	}
	badSlot[0] = byte(SlotReserved)
	// charge 0 with non-empty state
	if _, err := parseSlot(badSlot); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("zero charge: %v", err)
	}
	if _, err := encodeLedger(ledgerHeader{}, nil); err == nil {
		t.Fatal("encode nil slots")
	}
}
