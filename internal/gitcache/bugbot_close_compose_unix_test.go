//go:build linux || darwin

package gitcache

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"golang.org/x/crypto/ssh"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/bounds"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/pack"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshtrust"
)

func TestBugbotClose_CloseOnlyAndBothSuccessIndexed(t *testing.T) {
	ip, head, _ := commitIndexedPack(t)
	packBytes := append([]byte(nil), ip.PackBytes...)

	t.Run("close-only", func(t *testing.T) {
		sess := &closeComposeSess{pack: packBytes, head: head, closeErr: sshtrust.ErrUpdate}
		result, err := finishSSHFetch(context.Background(), sess, FetchOptions{Depth: 2, Wants: []plumbing.Hash{head}}, int64(len(packBytes)+4096))
		if sess.closes.Load() != 1 {
			t.Fatalf("Close count=%d want 1", sess.closes.Load())
		}
		if result.Indexed.PackByteCount != 0 || len(result.Indexed.Objects) != 0 {
			t.Fatalf("close-only must deny content: %#v", result)
		}
		if !errors.Is(err, sshtrust.ErrUpdate) {
			t.Fatalf("close-only trust: %v", err)
		}
	})
	t.Run("both-success", func(t *testing.T) {
		sess := &closeComposeSess{pack: packBytes, head: head, closeErr: nil}
		result, err := finishSSHFetch(context.Background(), sess, FetchOptions{Depth: 2, Wants: []plumbing.Hash{head}}, int64(len(packBytes)+4096))
		if sess.closes.Load() != 1 {
			t.Fatalf("Close count=%d want 1", sess.closes.Load())
		}
		if err != nil {
			t.Fatalf("both-success: %v", err)
		}
		if result.Indexed.PackByteCount == 0 || len(result.Indexed.Objects) == 0 {
			t.Fatalf("both-success empty indexed: %#v", result)
		}
		if _, ok := result.Indexed.Objects[head]; !ok {
			t.Fatalf("both-success missing head object")
		}
	})
}

// Concurrent native SSH: UpdateHostKeys rewrite fails after auth (actual update/close
// stage) while upload/decode also fails. Setup denial cannot stand in.
func TestBugbotClose_NativeSSHAcquireConcurrentUploadAndTrust(t *testing.T) {
	_, head, base := commitIndexedPack(t)
	hostKey := mustEd25519(t)
	rotated := mustEd25519(t)
	idPath, clientKey := writeSSHIdentity(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	known := writeSSHKnownHosts(t, hostKey, addr)

	authed := make(chan struct{})
	releaseUpload := make(chan struct{})
	stageChmod := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, e := ln.Accept()
		if e != nil {
			return
		}
		serveBugbotCloseUpload(raw, hostKey, clientKey, rotated, head, base, authed, releaseUpload)
	}()
	defer func() { _ = ln.Close(); <-done }()

	home := t.TempDir()
	body := fmt.Sprintf("Match host 127.0.0.1\n User ssh-acquire\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys yes\n", port, idPath, known)
	if err := os.WriteFile(filepath.Join(home, "config"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	svc, auth, intent := openF3Acquire(t, home, addr, head, base)
	before, err := svc.Manager().ChargedBytes()
	if err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	var result *AcquireResult
	go func() {
		var e error
		result, e = svc.Acquire(context.Background(), intent, auth)
		errCh <- e
	}()

	select {
	case <-authed:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for SSH auth stage")
	}
	// Deny known_hosts rewrite at the update/finalization stage after successful auth.
	if err := os.Chmod(known, 0o444); err != nil {
		t.Fatal(err)
	}
	close(stageChmod)
	close(releaseUpload)

	select {
	case err = <-errCh:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for Acquire")
	}
	if result != nil {
		t.Fatalf("expected nil acquisition, got %#v", result)
	}
	if err == nil {
		t.Fatal("expected concurrent upload+trust failure")
	}
	// Pack/decode category from corrupt payload.
	if !errors.Is(err, pack.ErrMalformed) && !errors.Is(err, pack.ErrChecksum) && !errors.Is(err, pack.ErrTooManyObjects) && !errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrCapability) {
		t.Fatalf("upload/decode category missing: %v", err)
	}
	// Trust update/finalization must not be discarded (RED on lossy compose).
	if !errors.Is(err, sshtrust.ErrUpdate) && !errors.Is(err, sshtrust.ErrTrustFiles) && !errors.Is(err, sshtrust.ErrUpdateUnsupported) {
		t.Fatalf("trust/finalization category lost on concurrent native SSH failure: %v", err)
	}
	after, err2 := svc.Manager().ChargedBytes()
	if err2 != nil {
		t.Fatal(err2)
	}
	if after != before {
		t.Fatalf("additional charge on failed acquisition: before=%d after=%d", before, after)
	}
	select {
	case <-stageChmod:
	default:
		t.Fatal("chmod stage was not reached")
	}
}

func serveBugbotCloseUpload(raw net.Conn, hostKey, clientKey, rotate ssh.Signer, head, base plumbing.Hash, authed, releaseUpload chan struct{}) {
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(20 * time.Second))
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() == "ssh-acquire" && bytes.Equal(key.Marshal(), clientKey.PublicKey().Marshal()) {
			close(authed)
			select {
			case <-releaseUpload:
			case <-time.After(10 * time.Second):
			}
			return nil, nil
		}
		return nil, fmt.Errorf("reject")
	}}
	cfg.AddHostKey(hostKey)
	server, chans, reqs, e := ssh.NewServerConn(raw, cfg)
	if e != nil {
		return
	}
	defer server.Close()
	go func() {
		for r := range reqs {
			switch r.Type {
			case "hostkeys-prove-00@openssh.com":
				if rotate == nil || !r.WantReply {
					_ = r.Reply(false, nil)
					continue
				}
				blobs, err := f3StringsPacket(r.Payload)
				if err != nil {
					_ = r.Reply(false, nil)
					continue
				}
				var sigs [][]byte
				for _, b := range blobs {
					data := ssh.Marshal(struct {
						Request      string
						Session, Key []byte
					}{"hostkeys-prove-00@openssh.com", server.SessionID(), b})
					sig, err := rotate.Sign(rand.Reader, data)
					if err != nil {
						_ = r.Reply(false, nil)
						return
					}
					sigs = append(sigs, ssh.Marshal(sig))
				}
				_ = r.Reply(true, f3Packet(sigs))
			case "keepalive@openssh.com":
				if r.WantReply {
					_ = r.Reply(true, nil)
				}
			default:
				if r.WantReply {
					_ = r.Reply(false, nil)
				}
			}
		}
	}()
	announced := [][]byte{hostKey.PublicKey().Marshal(), rotate.PublicKey().Marshal()}
	_, _, _ = server.SendRequest("hostkeys-00@openssh.com", false, f3Packet(announced))
	for ch := range chans {
		channel, requests, e := ch.Accept()
		if e != nil {
			continue
		}
		for r := range requests {
			switch r.Type {
			case "env":
				continue
			case "exec":
				_ = r.Reply(true, nil)
				_ = writeShallowAdv(channel, head, base)
				_, _ = io.Copy(io.Discard, io.LimitReader(channel, bounds.MaxPackBytes))
				req := packp.NewUploadPackRequest()
				req.Depth = packp.DepthCommits(2)
				_ = req.Capabilities.Set(capability.Shallow)
				// Corrupt pack: upload/decode fails concurrently with trust update failure.
				bad := []byte("PACK\x00\x00\x00\x02concurrent-close-trust-red")
				resp := packp.NewUploadPackResponseWithPackfile(req, io.NopCloser(bytes.NewReader(bad)))
				resp.Shallows = []plumbing.Hash{base}
				_ = resp.Encode(channel)
				_ = channel.CloseWrite()
				_ = channel.Close()
			}
		}
	}
}
