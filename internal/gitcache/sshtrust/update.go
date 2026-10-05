//go:build linux || darwin

package sshtrust

import (
	"bytes"
	"context"
	"encoding/binary"
	knownhosts "gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/knownhosts"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

const announcement = "hostkeys-00@openssh.com"
const proofRequest = "hostkeys-prove-00@openssh.com"

type requestConn interface {
	SendRequest(string, bool, []byte) (bool, []byte, error)
	SessionID() []byte
}

// Updates owns authenticated global requests. NewClientConn must have succeeded
// before Start is called. Its caller must not give reqs to a second consumer.
type Updates struct {
	manager     *Manager
	conn        requestConn
	ctx         context.Context
	incoming    <-chan *ssh.Request
	checkpoints chan chan struct{}
	done        chan struct{}
	mu          sync.Mutex
	err         error
	seen        bool
}

func (m *Manager) Start(ctx context.Context, c ssh.Conn, requests <-chan *ssh.Request) *Updates {
	u := &Updates{manager: m, conn: c, ctx: ctx, incoming: requests, checkpoints: make(chan chan struct{}), done: make(chan struct{})}
	go u.run()
	return u
}
func (u *Updates) failure(err error) {
	u.mu.Lock()
	if u.err == nil {
		u.err = err
	}
	u.mu.Unlock()
	u.manager.setStatus("failed")
}
func (u *Updates) Error() error { u.mu.Lock(); defer u.mu.Unlock(); return u.err }
func (u *Updates) run() {
	defer close(u.done)
	for {
		select {
		case r, ok := <-u.incoming:
			if !ok {
				return
			}
			u.handle(r)
		case ack := <-u.checkpoints:
			// After a protocol barrier, process all global requests already delivered
			// before its response. Later server announcements are handled until close.
			draining := true
			for draining {
				select {
				case r, ok := <-u.incoming:
					if !ok {
						close(ack)
						return
					}
					u.handle(r)
				default:
					draining = false
				}
			}
			close(ack)
		case <-u.ctx.Done():
			return
		}
	}
}
func (u *Updates) handle(r *ssh.Request) {
	if r.Type != announcement {
		_ = r.Reply(false, nil)
		return
	}
	m := u.manager
	m.mu.Lock()
	eligible := m.eligible
	m.mu.Unlock()
	if m.cfg.UpdateHostKeys == "no" {
		// Preserve successful accept-new/no initial enrollment; do not clobber it
		// with a later disabled-by-config announcement status.
		if m.UpdateStatus() != "enrolled_initial_key" {
			m.setStatus("disabled_by_config")
		}
		_ = r.Reply(false, nil)
		return
	}
	if !eligible {
		m.setStatus("ineligible_initial_trust")
		_ = r.Reply(false, nil)
		return
	}
	if u.seen {
		u.failure(ErrUpdate)
		_ = r.Reply(false, nil)
		return
	}
	u.seen = true
	if r.WantReply {
		u.failure(ErrUpdate)
		_ = r.Reply(false, nil)
		return
	}
	if err := m.apply(u.ctx, u.conn, r.Payload); err != nil {
		u.failure(err)
	}
}

// Sync issues a harmless global-request barrier, then drains prior requests.
// A peer that does not reply is bounded by the connection deadline/cancellation.
func (u *Updates) Sync() error {
	if _, _, err := u.conn.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		return ErrUpdate
	}
	ack := make(chan struct{})
	select {
	case u.checkpoints <- ack:
	case <-u.done:
		return u.Error()
	case <-u.ctx.Done():
		return u.ctx.Err()
	}
	select {
	case <-ack:
		return u.Error()
	case <-u.done:
		return u.Error()
	case <-u.ctx.Done():
		return u.ctx.Err()
	}
}
func (u *Updates) Wait() { <-u.done }

func stringsPacket(payload []byte) ([][]byte, error) {
	if len(payload) > maxPacket {
		return nil, ErrUpdate
	}
	var out [][]byte
	for len(payload) > 0 {
		if len(payload) < 4 || len(out) >= maxKeys {
			return nil, ErrUpdate
		}
		n := int(binary.BigEndian.Uint32(payload))
		payload = payload[4:]
		if n <= 0 || n > len(payload) {
			return nil, ErrUpdate
		}
		out = append(out, payload[:n])
		payload = payload[n:]
	}
	if len(out) == 0 {
		return nil, ErrUpdate
	}
	return out, nil
}
func packet(items [][]byte) []byte {
	var out []byte
	for _, item := range items {
		out = binary.BigEndian.AppendUint32(out, uint32(len(item)))
		out = append(out, item...)
	}
	return out
}
func proofData(session, key []byte) []byte {
	return ssh.Marshal(struct {
		Request      string
		Session, Key []byte
	}{proofRequest, session, key})
}

func (m *Manager) apply(ctx context.Context, c requestConn, payload []byte) error {
	blobs, err := stringsPacket(payload)
	if err != nil {
		return err
	}
	var keys []ssh.PublicKey
	seen := map[string]bool{}
	for _, blob := range blobs {
		if seen[string(blob)] {
			return ErrUpdate
		}
		seen[string(blob)] = true
		key, err := ssh.ParsePublicKey(blob)
		if err != nil {
			return ErrUpdate
		}
		switch key.Type() {
		case ssh.KeyAlgoED25519, ssh.KeyAlgoRSA, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		default:
			return ErrUpdateUnsupported
		}
		keys = append(keys, key)
	}
	if len(m.files) == 0 {
		return ErrTrustFiles
	}
	// Permit a single atomic target-file update. Other configured files are read
	// for trust; if they contain keys for this endpoint, refuse cross-file rotation
	// rather than partially committing a multi-file transaction.
	for _, s := range m.files[1:] {
		if endpointEntries(s.data, m.addr) {
			return ErrUpdateUnsupported
		}
	}
	before := m.files[0]
	after, err := rewrite(before.data, m.addr, keys, m.cfg.HashKnownHosts)
	if err != nil {
		return err
	}
	if bytes.Equal(after, before.data) {
		m.setStatus("already_current")
		return nil
	}
	var needed [][]byte
	var proofKeys []ssh.PublicKey
	for i, key := range keys {
		// Only exact endpoint entries qualify as already-known for update purposes.
		if !recorded(before.data, m.addr, key) {
			needed = append(needed, blobs[i])
			proofKeys = append(proofKeys, key)
		}
	}
	if len(needed) > 0 {
		ok, reply, err := c.SendRequest(proofRequest, true, packet(needed))
		if err != nil || !ok {
			return ErrUpdate
		}
		proofs, err := stringsPacket(reply)
		if err != nil || len(proofs) != len(proofKeys) {
			return ErrUpdate
		}
		for i, key := range proofKeys {
			var sig ssh.Signature
			if ssh.Unmarshal(proofs[i], &sig) != nil {
				return ErrUpdate
			}
			if key.Type() == ssh.KeyAlgoRSA {
				if sig.Format != ssh.KeyAlgoRSASHA256 && sig.Format != ssh.KeyAlgoRSASHA512 {
					return ErrUpdate
				}
				algorithms, ok := c.(interface {
					Algorithms() ssh.NegotiatedAlgorithms
				})
				if !ok {
					return ErrUpdate
				}
				negotiated := algorithms.Algorithms().HostKey
				if (negotiated == ssh.KeyAlgoRSASHA256 || negotiated == ssh.KeyAlgoRSASHA512) && sig.Format != negotiated {
					return ErrUpdate
				}
				if negotiated == ssh.KeyAlgoRSA {
					return ErrUpdate
				}
			}
			if err := key.Verify(proofData(c.SessionID(), needed[i]), &sig); err != nil {
				return ErrUpdate
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Prepare callback from committed bytes before any mutation. No pathname
	// reread can alter the database; retain all other original bounded snapshots.
	snapshots := [][]byte{after}
	for _, s := range m.files[1:] {
		snapshots = append(snapshots, s.data)
	}
	for _, s := range m.globals {
		snapshots = append(snapshots, s.data)
	}
	cb, err := knownhosts.New(ctx, snapshots...)
	if err != nil {
		return ErrTrustFiles
	}
	written, err := replaceSnapshot(ctx, before, after)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.callback = cb
	m.pending = append(m.pending, Transition{before: before, after: written, owner: m.Provenance()})
	m.mu.Unlock()

	m.setStatus("updated")
	return nil
}
func recorded(data []byte, addr string, key ssh.PublicKey) bool {
	target := knownhosts.Normalize(addr)
	for _, line := range bytes.Split(data, []byte("\n")) {
		marker, hosts, k, _, _, err := ssh.ParseKnownHosts(line)
		if err != nil || marker != "" {
			continue
		}
		for _, h := range hosts {
			if exactHost(h, target) && bytes.Equal(k.Marshal(), key.Marshal()) {
				return true
			}
		}
	}
	return false
}
func endpointEntries(data []byte, addr string) bool {
	target := knownhosts.Normalize(addr)
	for _, line := range bytes.Split(data, []byte("\n")) {
		_, hosts, _, _, _, err := ssh.ParseKnownHosts(line)
		if err != nil {
			continue
		}
		for _, h := range hosts {
			if exactHost(h, target) || strings.ContainsAny(h, "*?!") && hostPatternMatch(strings.TrimPrefix(h, "!"), target) {
				return true
			}
		}
	}
	return false
}
