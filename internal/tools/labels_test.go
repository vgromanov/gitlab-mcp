package tools

import "testing"

func TestLabelIDForAPI(t *testing.T) {
	if v := labelIDForAPI("  bug "); v != "bug" {
		t.Fatalf("name: got %#v", v)
	}
	if v := labelIDForAPI("42"); v != int64(42) {
		t.Fatalf("numeric id: got %#v want int64", v)
	}
	if v := labelIDForAPI("4.2"); v != "4.2" {
		t.Fatalf("non-integer string: got %#v", v)
	}
}
