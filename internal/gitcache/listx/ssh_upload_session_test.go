package listx

import (
	"bytes"
	"context"
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

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/origin"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
)

func TestNewSSHUploadPackSessionAdvertisement(t *testing.T) {
	host, _ := mustKey(t)
	identity, clientKey := identityFixture(t, false)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.Addr().String()
	_, port, _ := net.SplitHostPort(addr)
	known := writeKnown(t, host, addr)
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		raw, e := ln.Accept()
		if e != nil {
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(8 * time.Second))
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() == "upload-git" && bytes.Equal(key.Marshal(), clientKey.PublicKey().Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("reject")
		}}
		cfg.AddHostKey(host)
		server, chans, reqs, e := ssh.NewServerConn(raw, cfg)
		if e != nil {
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(reqs)
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
					_ = writeAdv(channel, "2d132b02f44f8995d58d2909a9e1d8cfe929c37e")
					// Keep stdout open so UploadPack can observe post-adv EOF.
					_, _ = io.Copy(io.Discard, channel)
					_ = channel.Close()
				}
			}
		}
	}()

	home := t.TempDir()
	config := filepath.Join(home, "config")
	body := fmt.Sprintf("Match host 127.0.0.1\n User upload-git\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys no\n", port, identity, known)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: config, SystemConfig: filepath.Join(home, "missing-system")}
	sess, closer, err := NewSSHUploadPackSession(nil, "ssh://git@"+addr+"/repo.git", Options{
		SSHConfig: &in,
		Timeout:   5 * time.Second,
	}.WithLoopback())
	if err != nil {
		t.Fatal(err)
	}
	defer closer()

	adv, err := sess.AdvertisedReferences()
	if err != nil || adv == nil || adv.Head == nil {
		t.Fatalf("adv: %v %#v", err, adv)
	}
	again, err := sess.AdvertisedReferencesContext(context.Background())
	if err != nil || again != adv {
		t.Fatalf("cached adv: %v", err)
	}
	// Post-advertisement UploadPack: this fixture serves no pack payload.
	// Observed EOF semantics — either fail closed as ErrSSH, or return a
	// response whose pack body is empty (close required); non-empty body is a bug.
	req := packp.NewUploadPackRequest()
	_ = req.Capabilities.Set(capability.OFSDelta)
	req.Wants = append(req.Wants, *adv.Head)
	up, upErr := sess.UploadPack(context.Background(), req)
	switch {
	case upErr != nil:
		if !errors.Is(upErr, ErrSSH) {
			t.Fatalf("upload-pack EOF path: %v", upErr)
		}
	case up == nil:
		t.Fatal("upload-pack returned nil response without error")
	default:
		n, rerr := io.Copy(io.Discard, up)
		_ = up.Close()
		if n != 0 {
			t.Fatalf("advertisement-only fixture leaked pack bytes: %d", n)
		}
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			t.Fatalf("pack body read: %v", rerr)
		}
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.AdvertisedReferencesContext(context.Background()); !errors.Is(err, ErrSSH) {
		t.Fatalf("closed adv: %v", err)
	}
	if _, err := sess.UploadPack(context.Background(), req); !errors.Is(err, ErrSSH) {
		t.Fatalf("closed upload: %v", err)
	}
	<-serverDone
}

func TestNewSSHUploadPackSessionRejects(t *testing.T) {
	if _, _, err := NewSSHUploadPackSession(context.Background(), "https://example.com/r.git", Options{}.WithLoopback()); !errors.Is(err, origin.ErrScheme) {
		t.Fatalf("scheme: %v", err)
	}
	if _, _, err := NewSSHUploadPackSession(context.Background(), "ssh://git@127.0.0.1:1/r.git", Options{
		KnownHosts: "kh",
		SSHConfig:  &sshconfig.Input{Home: t.TempDir(), LocalUser: "u"},
	}.WithLoopback()); !errors.Is(err, ErrSSHOverrides) {
		t.Fatalf("overrides: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Canceled ctx should fail before or during dial against unreachable port.
	_, _, _ = NewSSHUploadPackSession(ctx, "ssh://git@127.0.0.1:1/r.git", Options{
		SSHConfig: &sshconfig.Input{
			Home:         t.TempDir(),
			LocalUser:    "local",
			UserConfig:   filepath.Join(t.TempDir(), "missing"),
			SystemConfig: filepath.Join(t.TempDir(), "missing-sys"),
		},
		Timeout: time.Second,
	}.WithLoopback())
}

func TestSSHUploadPackWithoutAdvertisement(t *testing.T) {
	s := &sshUploadSession{}
	req := packp.NewUploadPackRequest()
	req.Wants = append(req.Wants, plumbing.NewHash("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	if _, err := s.UploadPack(context.Background(), req); !errors.Is(err, ErrSSH) {
		t.Fatalf("no adv: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.adv = packp.NewAdvRefs()
	if _, err := s.UploadPack(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}
