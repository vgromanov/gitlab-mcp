package listx

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"golang.org/x/crypto/ssh"
)

func TestConfigDrivenSSHListEnvironmentAndHostkeys(t *testing.T) {
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
	before, _ := os.ReadFile(known)
	var receivedEnv atomic.Int32
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		raw, e := ln.Accept()
		if e != nil {
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() == "configured-git" && bytes.Equal(key.Marshal(), clientKey.PublicKey().Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("fixture rejects credential")
		}}
		cfg.AddHostKey(host)
		server, chans, reqs, e := ssh.NewServerConn(raw, cfg)
		if e != nil {
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(reqs)
		blob := host.PublicKey().Marshal()
		payload := binary.BigEndian.AppendUint32(nil, uint32(len(blob)))
		payload = append(payload, blob...)
		_, _, _ = server.SendRequest("hostkeys-00@openssh.com", false, payload)
		for ch := range chans {
			channel, requests, e := ch.Accept()
			if e != nil {
				continue
			}
			for r := range requests {
				if r.Type == "env" {
					var env struct{ Name, Value string }
					if ssh.Unmarshal(r.Payload, &env) == nil && env.Name == "SYNTHETIC_ENV" && env.Value == "fixture" && !r.WantReply {
						receivedEnv.Add(1)
					}
					continue
				}
				if r.Type == "exec" {
					_ = r.Reply(true, nil)
					_ = writeAdv(channel, "2d132b02f44f8995d58d2909a9e1d8cfe929c37e")
					_ = channel.CloseWrite()
					_ = channel.Close()
				}
			}
		}
	}()
	home := t.TempDir()
	config := filepath.Join(home, "config")
	system := filepath.Join(home, "missing-system")
	body := fmt.Sprintf("Match host 127.0.0.1\n User configured-git\n Port %s\n IdentitiesOnly yes\n IdentityAgent none\n IdentityFile %s\n UserKnownHostsFile %s\n GlobalKnownHostsFile none\n StrictHostKeyChecking no\n UpdateHostKeys yes\n SetEnv SYNTHETIC_ENV=fixture\n", port, identity, known)
	if err := os.WriteFile(config, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: config, SystemConfig: system}
	var diag Diagnostic
	refs, err := List(context.Background(), "ssh://git@"+addr+"/repo.git", Options{SSHConfig: &in, SSHDiagnostics: true, Diag: &diag, Timeout: 5 * time.Second}.withLoopback())
	<-serverDone
	if err != nil || len(refs) != 1 || diag.AuthMethod != "publickey(config)" || diag.HostKeyUpdate != "already_current" || !diag.HostKeyVerified || !diag.SSHConfigResolved || receivedEnv.Load() != 1 {
		t.Fatalf("integrated fixture: error=%v refs=%d env=%d diag=%s", err, len(refs), receivedEnv.Load(), diag.JSON())
	}
	after, _ := os.ReadFile(known)
	if !bytes.Equal(before, after) {
		t.Fatal("no-op hostkey update changed file")
	}
}
func TestConfigResolvedEndpointScopeBeforeDial(t *testing.T) {
	home := t.TempDir()
	config := filepath.Join(home, "config")
	if err := os.WriteFile(config, []byte("Host *\nHostName other.invalid\nPort 7999\nUser git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	in := sshconfig.Input{Home: home, LocalUser: "local", UserConfig: config, SystemConfig: filepath.Join(home, "missing")}
	var diag Diagnostic
	_, err := List(context.Background(), "ssh://git@127.0.0.1:7999/repo.git", Options{SSHConfig: &in, SSHDiagnostics: true, Diag: &diag}.withLoopback())
	if err == nil || diag.TCPConnected {
		t.Fatal("resolved endpoint escaped pin")
	}
}
