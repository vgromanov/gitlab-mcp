package cursor

import (
	"strings"
	"testing"
)

func TestGraphEvidenceRoundTrip(t *testing.T) {
	jd := strings.Repeat("a", 32)
	item, err := FormatGraphEvidence("42:100", jd, "")
	if err != nil {
		t.Fatal(err)
	}
	key, gotJD, gotBD, ok := ParseGraphEvidence(item)
	if !ok || key != "42:100" || gotJD != jd || gotBD != "" {
		t.Fatalf("round trip %q %q %q %v", key, gotJD, gotBD, ok)
	}
}

func TestGraphEvidenceRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "42:100", "42:100=zz=", "42:100=" + strings.Repeat("a", 31) + "=", "nope==", "42:0=="} {
		if _, _, _, ok := ParseGraphEvidence(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := FormatGraphEvidence("42:100", "XYZ", ""); err == nil {
		t.Fatal("format accepted bad digest")
	}
	dup := &GraphCont{Ev: []string{"42:100==", "42:100=" + strings.Repeat("b", 32) + "="}}
	if validateGraphEvidence(dup) == nil {
		t.Fatal("duplicate evidence key accepted")
	}
	if validateGraphEvidence(&GraphCont{JD: "not-hex"}) == nil {
		t.Fatal("bad running digest accepted")
	}
}
