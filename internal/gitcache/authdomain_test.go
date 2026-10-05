package gitcache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
)

func TestBindLeavesDomainUnusableWhenSeedReadFails(t *testing.T) {
	prev := readDomainSeed
	t.Cleanup(func() { readDomainSeed = prev })

	var d AuthDomain
	readDomainSeed = func(b []byte) (int, error) {
		return 0, errors.New("entropy")
	}
	got, err := d.Bind("actor", "token")
	if got != "" || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("failed read: %q %v", got, err)
	}
	readDomainSeed = func(b []byte) (int, error) {
		for i := range b[:4] {
			b[i] = 7
		}
		return 4, nil
	}
	got, err = d.Bind("actor", "token")
	if got != "" || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("short read: %q %v", got, err)
	}

	key := bytes.Repeat([]byte{0xab}, 32)
	readDomainSeed = func(b []byte) (int, error) {
		copy(b, key)
		return len(b), nil
	}
	got, err = d.Bind("actor", "token")
	want := domainBinding(key, "actor", "token")
	if err != nil || got != want || got == domainBinding(bytes.Repeat([]byte{0}, 32), "actor", "token") {
		t.Fatalf("retry: %q want %q err %v", got, want, err)
	}
	again, err := d.Bind("actor", "other")
	if err != nil || again == got || again != domainBinding(key, "actor", "other") {
		t.Fatalf("ready key: %q %q %v", got, again, err)
	}
}

func domainBinding(key []byte, actor, token string) string {
	h := sha256.New()
	_, _ = h.Write(key)
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(actor))
	_, _ = h.Write([]byte{0})
	sum := sha256.Sum256([]byte(token))
	_, _ = h.Write(sum[:])
	return hex.EncodeToString(h.Sum(nil))
}
