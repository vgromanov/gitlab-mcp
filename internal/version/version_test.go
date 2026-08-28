package version

import "testing"

func TestVersionConstants(t *testing.T) {
	if Name == "" || Version == "" {
		t.Fatal("empty")
	}
}
