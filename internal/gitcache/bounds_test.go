package gitcache

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackV2Fixture(t *testing.T) {
	pack, err := PackV2EmptyBlob()
	if err != nil {
		t.Fatal(err)
	}
	if len(pack) < 32 || string(pack[:4]) != "PACK" || pack[7] != 2 {
		t.Fatalf("pack header %x", pack[:8])
	}
}

func TestBounds(t *testing.T) {
	if Reserve != PackMax+IndexMax+MetaMax {
		t.Fatalf("reserve %d", Reserve)
	}
	if IndexMax != 1072+36*1_000_000 {
		t.Fatalf("index max %d", IndexMax)
	}
	if MetaSimul != 7424 || MetaSimul > MetaMax {
		t.Fatalf("meta simul %d", MetaSimul)
	}
	if QuotaMin != RootBytes+Reserve {
		t.Fatalf("quota min")
	}
	if _, err := Add(^uint64(0), 1); err != ErrOverflow {
		t.Fatalf("overflow %v", err)
	}
	if err := QuotaOK(QuotaMin - 1); err != ErrQuota {
		t.Fatalf("low quota %v", err)
	}
	if err := QuotaOK(QuotaMax + 1); err != ErrQuota {
		t.Fatalf("high quota %v", err)
	}
	if err := QuotaOK(QuotaMin); err != nil {
		t.Fatal(err)
	}
	if _, err := CommittedCharge(PackMax+1, 0); err != ErrQuota {
		t.Fatal(err)
	}
	got, err := CommittedCharge(10, 20)
	if err != nil || got != 10+20+MetaMax {
		t.Fatalf("charge %d %v", got, err)
	}
	if err := AdmitPack(0, PackMax+1); err != ErrQuota {
		t.Fatal("P+1 must be rejected before a write")
	}
	if err := AdmitPack(PackMax, 1); err != ErrQuota {
		t.Fatal(err)
	}
	if err := AdmitPack(3, 4); err != nil {
		t.Fatal(err)
	}
	if err := AdmitMeta("config", MetaConfig+1, 0); err != ErrQuota {
		t.Fatal(err)
	}
	if err := AdmitMeta("nope", 1, 1); err != ErrQuota {
		t.Fatal(err)
	}
	if err := AdmitMeta("config", 1, MetaSimul+1); err != ErrQuota {
		t.Fatal(err)
	}
	if err := AdmitMeta("provenance", 10, 20); err != nil {
		t.Fatal(err)
	}
}

func TestArgvClosed(t *testing.T) {
	argv, err := IndexArgv("/usr/bin/git")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, ban := range []string{"--stdin", "--promisor", "--fix-thin", "--keep", "tmp_idx", "tmp_pack", "--rev-index"} {
		if strings.Contains(joined, ban) {
			t.Fatalf("banned %s in %s", ban, joined)
		}
	}
	if !strings.Contains(joined, "-o objects/pack/input.idx objects/pack/input.pack") {
		t.Fatalf("index path: %s", joined)
	}
	if !strings.Contains(joined, "--no-rev-index") || !strings.Contains(joined, "--threads=1") {
		t.Fatal(joined)
	}
	if _, err := IndexArgv("git"); err != ErrPath {
		t.Fatal(err)
	}
	if _, err := VersionArgv("/usr/bin/git"); err != nil {
		t.Fatal(err)
	}
	tip := strings.Repeat("ab", 20)
	rev, err := RevListArgv("/usr/bin/git", tip)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(rev, " "), "--objects --no-object-names --missing=error "+tip) {
		t.Fatal(rev)
	}
	if _, err := RevListArgv("/usr/bin/git", "HEAD"); err != ErrPath {
		t.Fatal(err)
	}
	cat, err := CatFileArgv("/usr/bin/git")
	if err != nil {
		t.Fatal(err)
	}
	if cat[len(cat)-1] != CatFileCheckArg {
		t.Fatalf("cat-file argv %q", cat)
	}
	for _, a := range cat {
		if a == "--batch" || a == "--batch-command" || a == "--textconv" || a == "--filters" {
			t.Fatalf("content-reading argv %s", a)
		}
	}
	if string([]byte(AllowedConfig)) == "" || AdmitMetaBody("config", []byte("trace2.eventTarget=1\n")) != ErrAudit {
		t.Fatal("config allowlist")
	}
	if err := AdmitMetaBody("config", []byte(AllowedConfig)); err != nil {
		t.Fatal(err)
	}
	if err := AdmitMetaBody("HEAD", []byte(AllowedHEAD)); err != nil {
		t.Fatal(err)
	}
	oids := make([]string, 33)
	for i := range oids {
		oids[i] = strings.Repeat("ab", 20)
	}
	buf, err := CatFileInput(oids)
	if err != nil || len(buf) != 1353 {
		t.Fatalf("cat %d %v", len(buf), err)
	}
	if _, err := CatFileInput(append(oids, oids[0])); err != ErrQuota {
		t.Fatal(err)
	}
	if _, err := CatFileInput([]string{"zz"}); err != ErrPath {
		t.Fatal(err)
	}
}

func TestAuditIdentity(t *testing.T) {
	if !strings.Contains(AuditNote, "repack_local_links") || !strings.Contains(AuditNote, "setsid") || !strings.Contains(AuditNote, "BATCH_MODE_INFO") || !strings.Contains(AuditNote, "PROT_NONE") || !strings.Contains(AuditNote, "precompose") {
		t.Fatal("audit note missing closure")
	}
	darwinFlags, err := FlagsForOS("darwin")
	if err != nil || len(darwinFlags) != 6 {
		t.Fatal(err)
	}
	id := BuildIdentity{
		SourceSHA256: GitSourceSHA256,
		Flags:        darwinFlags,
		OS:           "darwin",
		Arch:         "arm64",
		Compiler:     "clang",
		BinarySHA256: "abc",
		VersionLine:  "git version 2.50.1",
	}
	if err := IdentityOK(id, "", 25); err != ErrUnsupported {
		t.Fatalf("darwin 25 must fail closed, got %v", err)
	}
	if err := IdentityOK(id, "", 24); err != nil {
		t.Fatal(err)
	}
	id.Flags = append([]string{}, GitBuildFlags...)
	if err := IdentityOK(id, "", 24); err != ErrAudit {
		t.Fatal("linux flags on darwin")
	}
	id.OS = "linux"
	id.Flags = append([]string{}, GitBuildFlags...)
	if err := IdentityOK(id, "5.15.0", 0); err != nil {
		t.Fatal(err)
	}
	id.Flags = darwinFlags
	if err := IdentityOK(id, "5.15.0", 0); err != ErrAudit {
		t.Fatal("darwin flags on linux")
	}
	id.Flags = append([]string{}, GitBuildFlags...)
	if err := IdentityOK(id, "5.4.0", 0); err != ErrUnsupported {
		t.Fatal(err)
	}
	if err := IdentityOK(id, "", 0); err != ErrUnsupported {
		t.Fatal(err)
	}
	id.SourceSHA256 = "nope"
	if err := IdentityOK(id, "6.8.0", 0); err != ErrAudit {
		t.Fatal(err)
	}
}

func TestProductionUnreferenced(t *testing.T) {
	roots := []string{"../../cmd", "../../internal/mcpsrv", "../../internal/config", "../../internal/tools", "../../internal/gitlab"}
	needle := "internal/gitcache"
	for _, root := range roots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if bytes.Contains(b, []byte(needle)) {
				t.Errorf("%s references gitcache", path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestLedgerRoundTrip(t *testing.T) {
	slots := make([]Slot, MaxSlots)
	buf, err := writeLedger(QuotaMin, slots)
	if err != nil || len(buf) != LedgerLen {
		t.Fatal(err)
	}
	if _, _, err := parseLedger(buf); err != nil {
		t.Fatal(err)
	}
	buf[40] ^= 0xff
	if _, _, err := parseLedger(buf); err != ErrCorrupt {
		t.Fatal(err)
	}
	slots[0] = Slot{ID: strings.Repeat("ab", 16), Domain: "d", Tip: "t", State: stateReserved}
	buf, err = writeLedger(QuotaMin, slots)
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := parseLedger(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Frozen || got[0].State != stateReserved {
		t.Fatalf("frozen %+v", got[0])
	}
	used, err := accounted(QuotaMin, got)
	if err != nil || used != RootBytes+Reserve {
		t.Fatalf("used %d %v", used, err)
	}
	slots[0].State = stateCommitted
	slots[0].Pack = 10
	slots[0].Index = 20
	buf, err = writeLedger(QuotaMin, slots)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseLedger(buf); err != nil {
		t.Fatal(err)
	}
	if err := QuotaOK(0); err != ErrQuota {
		t.Fatal(err)
	}
	bad := append([]byte(nil), buf...)
	copy(bad[:8], "NOPE1234")
	if _, _, err := parseLedger(bad); err != ErrCorrupt {
		t.Fatal(err)
	}
	short := buf[:10]
	if _, _, err := parseLedger(short); err != ErrCorrupt {
		t.Fatal(err)
	}
	slots[0].State = 9
	if _, err := writeLedger(QuotaMin, slots); err == nil {
		t.Fatal("bad state")
	}
	slots[0] = Slot{}
	if _, err := writeLedger(QuotaMin, slots[:1]); err == nil {
		t.Fatal("short slot list")
	}
	if _, err := emptyLedger(0); err != ErrQuota {
		t.Fatal(err)
	}
	ver := append([]byte(nil), buf...)
	ver[8] = 9
	if _, _, err := parseLedger(ver); err != ErrCorrupt {
		t.Fatal("version")
	}
	q0 := append([]byte(nil), buf...)
	for i := 16; i < 24; i++ {
		q0[i] = 0
	}
	sum := hashLedger(q0)
	copy(q0[32:64], sum[:])
	if _, _, err := parseLedger(q0); err != ErrCorrupt {
		t.Fatal("quota header")
	}
	slots[0] = Slot{State: stateEmpty, Pack: 1}
	if _, err := writeLedger(QuotaMin, slots); err == nil {
		t.Fatal("empty with pack")
	}
	slots[0] = Slot{ID: "not-a-generation-id-32-chars!!", State: stateReserved}
	if _, err := writeLedger(QuotaMin, slots); err == nil {
		t.Fatal("bad reserved id")
	}
	slots[0] = Slot{ID: strings.Repeat("ab", 16), State: stateCommitted, Pack: PackMax + 1}
	if _, err := writeLedger(QuotaMin, slots); err == nil {
		t.Fatal("committed oversize")
	}
	if err := putSlot(make([]byte, 256), Slot{Domain: strings.Repeat("d", 33)}); err != ErrCorrupt {
		t.Fatal(err)
	}
	two := make([]Slot, MaxSlots)
	two[0] = Slot{ID: strings.Repeat("ab", 16), State: stateReserved}
	two[1] = Slot{ID: strings.Repeat("cd", 16), State: stateReserved}
	if _, err := writeLedger(QuotaMin, two); err == nil {
		t.Fatal("accounted over quota")
	}
	if _, err := VersionArgv("git"); err == nil {
		t.Fatal("relative version")
	}
	if _, err := RevListArgv("/usr/bin/git", "HEAD"); err == nil {
		t.Fatal("rev syntax")
	}
	if _, err := CatFileArgv("git"); err == nil {
		t.Fatal("relative cat")
	}
	if _, err := CatFileInput([]string{strings.Repeat("a", 39)}); err == nil {
		t.Fatal("short oid")
	}
	if _, err := CatFileInput([]string{strings.Repeat("zz", 20)}); err == nil {
		t.Fatal("bad hex")
	}
	many := make([]string, 34)
	for i := range many {
		many[i] = strings.Repeat("ab", 20)
	}
	if _, err := CatFileInput(many); err != ErrQuota {
		t.Fatal(err)
	}
	if err := AdmitMetaBody("HEAD", []byte("ref: refs/heads/other\n")); err != ErrAudit {
		t.Fatal(err)
	}
	id := BuildIdentity{SourceSHA256: GitSourceSHA256, Flags: append([]string(nil), GitBuildFlags...), OS: "linux", Arch: "amd64", Compiler: "cc", BinarySHA256: "abc", VersionLine: "git version 2.50.1"}
	id.Flags[0] = "NO_CURL=No"
	if err := IdentityOK(id, "6.1.0", 0); err != ErrAudit {
		t.Fatal(err)
	}
	id.Flags = []string{"only-one"}
	if err := IdentityOK(id, "6.1.0", 0); err != ErrAudit {
		t.Fatal(err)
	}
	id.Flags = append([]string(nil), GitBuildFlags...)
	id.Compiler = ""
	if err := IdentityOK(id, "6.1.0", 0); err != ErrAudit {
		t.Fatal(err)
	}
	id.Compiler = "cc"
	id.OS = "freebsd"
	if err := IdentityOK(id, "6.1.0", 0); err != ErrUnsupported {
		t.Fatal(err)
	}
	id.OS = "linux"
	id.Arch = "386"
	if err := IdentityOK(id, "6.1.0", 0); err != ErrUnsupported {
		t.Fatal(err)
	}
	if linuxKernelOK("5") || linuxKernelOK("5.14") || linuxKernelOK("nope") {
		t.Fatal("kernel")
	}
}
