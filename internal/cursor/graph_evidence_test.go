package cursor

import (
	"strconv"
	"strings"
	"testing"
)

func TestGraphEvidenceRoundTrip(t *testing.T) {
	jd := strings.Repeat("a", 32)
	item, err := FormatGraphEvidence("42:100", jd, "", jd)
	if err != nil {
		t.Fatal(err)
	}
	key, gotJD, gotBD, gotMD, ok := ParseGraphEvidence(item)
	if !ok || key != "42:100" || gotJD != jd || gotBD != "" || gotMD != jd {
		t.Fatalf("round trip %q %q %q %q %v", key, gotJD, gotBD, gotMD, ok)
	}
}

func TestValidateGraphReachWithinNodeBound(t *testing.T) {
	rg := make([]string, 0, 33)
	for i := 0; i < 33; i++ {
		item, err := FormatGraphReachEdge("42:100", "42:"+strconv.Itoa(200+i), "bridge")
		if err != nil {
			t.Fatal(err)
		}
		rg = append(rg, item)
	}
	if err := validateGraphReach(rg); err != nil {
		t.Fatalf("dense reach within node bound: %v", err)
	}
}

func TestGraphEvidenceRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "42:100", "42:100==", "42:100=zz==", "42:100=" + strings.Repeat("a", 31) + "==", "nope===", "42:0===", "42:100=="} {
		if _, _, _, _, ok := ParseGraphEvidence(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := FormatGraphEvidence("42:100", "XYZ", "", ""); err == nil {
		t.Fatal("format accepted bad digest")
	}
	dup := &GraphCont{Ev: []string{"42:100===", "42:100=" + strings.Repeat("b", 32) + "==" + "="}}
	if validateGraphEvidence(dup) == nil {
		t.Fatal("duplicate evidence key accepted")
	}
	if validateGraphEvidence(&GraphCont{JD: "not-hex"}) == nil {
		t.Fatal("bad running digest accepted")
	}
}
