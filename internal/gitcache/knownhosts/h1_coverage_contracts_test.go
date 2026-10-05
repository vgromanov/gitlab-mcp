package knownhosts

import (
	"testing"
)

func TestCoverage_NormalizeAndHashHostContracts(t *testing.T) {
	if Normalize("example.com") != "example.com" {
		t.Fatal("default port host")
	}
	if Normalize("example.com:22") != "example.com" {
		t.Fatal("explicit 22")
	}
	if Normalize("example.com:2222") != "[example.com]:2222" {
		t.Fatal("nondefault port")
	}
	if Normalize("[::1]:22") != "::1" {
		t.Fatal("bracket ipv6 default")
	}
	h := HashHostname("example.com")
	if h == "" || h[0] != '|' {
		t.Fatalf("hash hostname: %q", h)
	}
	if _, _, _, err := decodeHash("not-hashed"); err == nil {
		t.Fatal("decode non-hash")
	}
	if _, _, _, err := decodeHash("|a|b"); err == nil {
		t.Fatal("decode short")
	}
}
