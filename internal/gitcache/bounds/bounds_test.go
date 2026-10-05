package bounds_test

import (
	"testing"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
)

func TestIndexV2SHA1BytesAndCharges(t *testing.T) {
	n, err := bounds.IndexV2SHA1Bytes(0)
	if err != nil || n != 1072 {
		t.Fatalf("empty: %d %v", n, err)
	}
	n, err = bounds.IndexV2SHA1Bytes(10)
	if err != nil || n != 1072+28*10 {
		t.Fatalf("n=10: %d %v", n, err)
	}
	if _, err := bounds.IndexV2SHA1Bytes(-1); err == nil {
		t.Fatal("negative accepted")
	}
	if _, err := bounds.IndexV2SHA1Bytes(bounds.MaxObjects + 1); err == nil {
		t.Fatal("over max accepted")
	}
	if bounds.GenerationCharge() != bounds.MaxPackBytes+bounds.MaxIndexBytes+bounds.MaxMetaBytes {
		t.Fatal("GenerationCharge")
	}
	if bounds.ReservationFormula() == "" {
		t.Fatal("ReservationFormula empty")
	}
	if bounds.PackResponseLimit(0) != bounds.MaxPackPrelude {
		t.Fatalf("prelude only: %d", bounds.PackResponseLimit(0))
	}
	if bounds.PackResponseLimit(10) != bounds.MaxPackPrelude+10 {
		t.Fatal("pack plus prelude")
	}
	if bounds.MaxPackPrelude <= 8 {
		t.Fatal("prelude does not leave room for a NAK pkt-line")
	}
}
