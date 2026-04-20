package tools

import "testing"

func TestPagination_ListOpts(t *testing.T) {
	var p Pagination
	pg, per := p.ListOpts()
	if pg != 1 || per != 20 {
		t.Fatalf("defaults: got page=%d per=%d", pg, per)
	}
	p = Pagination{Page: 0, PerPage: 0}
	pg, per = p.ListOpts()
	if pg != 1 || per != 20 {
		t.Fatalf("zeros: got page=%d per=%d", pg, per)
	}
	p = Pagination{Page: 3, PerPage: 200}
	pg, per = p.ListOpts()
	if pg != 3 || per != 100 {
		t.Fatalf("clamp per_page: got page=%d per=%d", pg, per)
	}
}

func TestTruncateLines(t *testing.T) {
	s := "a\nb\nc\nd"
	out := TruncateLines(s, 2)
	if out == s {
		t.Fatal("expected truncation")
	}
	if TruncateLines(s, 0) != s {
		t.Fatal("max 0 should not truncate")
	}
}

func TestIntFromAny(t *testing.T) {
	n, err := IntFromAny(float64(42))
	if err != nil || n != 42 {
		t.Fatalf("float64: %v %d", err, n)
	}
	n, err = IntFromAny(int64(7))
	if err != nil || n != 7 {
		t.Fatalf("int64: %v %d", err, n)
	}
	_, err = IntFromAny("x")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestProjectGID(t *testing.T) {
	if g := ProjectGID(123); g != "gid://gitlab/Project/123" {
		t.Fatalf("got %q", g)
	}
}

func TestResolveProjectID(t *testing.T) {
	pid, err := ResolveProjectID("  foo/bar  ", "")
	if err != nil || pid != "foo/bar" {
		t.Fatalf("got %q err=%v", pid, err)
	}
	_, err = ResolveProjectID("", "")
	if err == nil {
		t.Fatal("expected error when empty")
	}
}
