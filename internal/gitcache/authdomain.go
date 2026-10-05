package gitcache

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

// AuthDomain is an in-memory, process-local authorization domain key.
// Raw tokens and reusable token digests are never persisted to disk metadata.
type AuthDomain struct {
	mu    sync.Mutex
	key   [32]byte
	ready bool
}

// Bind derives or refreshes the in-memory domain key from actor+token material.
// The raw token is not retained. Cold restart yields a new key; old on-disk
// generations remain charged under recovery/quota until eviction.
func (d *AuthDomain) Bind(actorID, token string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var seed [32]byte
	if !d.ready {
		_, _ = rand.Read(seed[:])
		d.key = seed
		d.ready = true
	}
	h := sha256.New()
	_, _ = h.Write(d.key[:])
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(actorID))
	_, _ = h.Write([]byte{0})
	// Mix token bytes without storing a reusable digest field on disk.
	sum := sha256.Sum256([]byte(token))
	_, _ = h.Write(sum[:])
	out := h.Sum(nil)
	return hex.EncodeToString(out)
}

// NamespaceID returns a non-secret namespace label for an identity under domain.
func NamespaceID(domainKey, originHost, project, fork string) string {
	h := sha256.Sum256([]byte(domainKey + "\n" + originHost + "\n" + project + "\n" + fork + "\n"))
	return hex.EncodeToString(h[:])
}
