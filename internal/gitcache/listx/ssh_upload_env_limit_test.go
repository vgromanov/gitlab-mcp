package listx

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
)

// Oversized SetEnv must fail closed before session start completes — bounds
// negative, not a coverage-only stub.
func TestNewSSHUploadPackSessionRejectsOversizedEnv(t *testing.T) {
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
	go func() {
		raw, e := ln.Accept()
		if e != nil {
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if bytes.Equal(key.Marshal(), clientKey.PublicKey().Marshal()) {
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
				if r.WantReply {
					_ = r.Reply(true, nil)
				}
				_ = channel.Close()
			}
		}
	}()

	home := t.TempDir()
	config := filepath.Join(home, "config")
	huge := strings.Repeat("X", 33*1024)
	body := fmt.Sprintf("Match host 127.0.0.1\n User git\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking yes\n UpdateHostKeys no\n SetEnv OVERSIZE=%s\n", port, identity, known, huge)
	if err := os.WriteFile(config, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: config, SystemConfig: filepath.Join(home, "missing-system")}
	_, _, err = NewSSHUploadPackSession(context.Background(), "ssh://git@"+addr+"/repo.git", Options{
		SSHConfig: &in,
		Timeout:   5 * time.Second,
	}.WithLoopback())
	if err == nil || !strings.Contains(err.Error(), "environment exceeds request size limit") {
		t.Fatalf("oversized env: %v", err)
	}
}
