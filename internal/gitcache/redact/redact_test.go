package redact

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestStripsTokenAndUserinfo(t *testing.T) {
	const token = "super-secret-token-value"
	err := Error(errors.New("GET https://oauth2:"+token+"@gitlabci.raiffeisen.ru/repo.git failed token "+token), token)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "oauth2:") {
		t.Fatalf("leaked auth URL: %s", err.Error())
	}
	if unwrap := errors.Unwrap(err); unwrap != nil && strings.Contains(unwrap.Error(), token) {
		t.Fatal("unwrap retained the raw error")
	}
}

func TestChainKeepsAlgorithmsAndDropsSecrets(t *testing.T) {
	blob := strings.Repeat("A", 48)
	pemText := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + blob + "\n-----END OPENSSH PRIVATE KEY-----"
	secret := Chain(errors.New("banner " + pemText + " ssh-ed25519 " + blob + " password=hunter2"))
	if len(secret) != 1 || secret[0].Text != "[redacted]" || strings.Contains(secret[0].Text, blob) || strings.Contains(secret[0].Type, blob) {
		t.Fatalf("secret chain %+v", secret)
	}
	neg := &ssh.AlgorithmNegotiationError{
		What:                "key exchange",
		SupportedAlgorithms: []string{"curve25519-sha256"},
		RequestedAlgorithms: []string{"diffie-hellman-group1-sha1", "ssh-ed25519 " + blob, "curve25519-sha256-name-00"},
	}
	wrapped := fmt.Errorf("ssh: handshake failed: %w", neg)
	frames := Chain(wrapped)
	if len(frames) != 2 || frames[0].Text != "ssh: handshake failed" || frames[1].Type != "*ssh.AlgorithmNegotiationError" {
		t.Fatalf("chain %+v", frames)
	}
	got := frames[1]
	if got.What != "key exchange" || len(got.Supported) != 1 || got.Supported[0] != "curve25519-sha256" || len(got.Requested) != 1 || got.Requested[0] != "diffie-hellman-group1-sha1" || got.Redacted != 2 {
		t.Fatalf("negotiation frame %+v", got)
	}
	encoded := fmt.Sprintf("%+v", frames)
	if strings.Contains(encoded, blob) || strings.Contains(encoded, "curve25519-sha256-name-00") || strings.Contains(encoded, "ssh-ed25519 ") {
		t.Fatalf("algorithm chain kept a secret: %s", encoded)
	}
	badWhat := Chain(&ssh.AlgorithmNegotiationError{What: "key exchange password=hunter2", RequestedAlgorithms: []string{"diffie-hellman-group1-sha1"}})
	if badWhat[0].What != "algorithm" || strings.Contains(fmt.Sprintf("%+v", badWhat), "hunter") || badWhat[0].Requested[0] != "diffie-hellman-group1-sha1" {
		t.Fatalf("unknown stage %+v", badWhat)
	}
	var algos []string
	for i := 0; i < 40; i++ {
		algos = append(algos, "diffie-hellman-group-exchange-sha256")
	}
	long := Chain(&ssh.AlgorithmNegotiationError{
		What:                "key exchange",
		SupportedAlgorithms: algos,
		RequestedAlgorithms: []string{"diffie-hellman-group1-sha1"},
	})
	if len(long) != 1 || len(long[0].Text) > MaxString || long[0].What != "key exchange" || len(long[0].Supported) != maxAlgos || long[0].Omitted != 16 || len(long[0].Requested) != 1 || long[0].Requested[0] != "diffie-hellman-group1-sha1" {
		t.Fatalf("peer list lost %+v", long)
	}
	err := errors.New("ssh: handshake failed")
	for i := 0; i < 10; i++ {
		err = fmt.Errorf("ssh: handshake failed: %w", err)
	}
	if n := len(Chain(err)); n != MaxLayers {
		t.Fatalf("layers %d", n)
	}
}

func TestPublicHTTPSIsFixedCategory(t *testing.T) {
	const token = "local-test-token/value+secret"
	basic := "Basic " + token
	rawText := strings.Join([]string{
		token,
		url.QueryEscape(token),
		url.PathEscape(token),
		"%6Cocal-test-token%2Fvalue%2Bsecret",
		basic,
		"https://oauth2:" + token + "@gitlabci.raiffeisen.ru/repo.git",
		"fatal: protocol error pkt-line",
		strings.Repeat("Z", 256<<10),
	}, "\n")
	raw := fmt.Errorf("unexpected client error: %w", errors.New(rawText))
	got := PublicHTTPS(raw)
	if got == nil || got.Error() != ErrHTTPS.Error() || errors.Unwrap(got) != nil || strings.Contains(got.Error(), token) || strings.Contains(got.Error(), "protocol") || len(got.Error()) > 512 {
		t.Fatal("https boundary retained remote text or a wrapper")
	}
	keep := errors.New("upload-pack body exceeds the byte limit")
	if PublicHTTPS(fmt.Errorf("seen %s: %w", rawText, keep), keep) != keep {
		t.Fatal("local sentinel was replaced")
	}
	if PublicHTTPS(timeoutErr{}) != ErrHTTPSTimeout || strings.Contains(ErrHTTPSTimeout.Error(), "10.1.2.3") {
		t.Fatal("timeout was not a fixed category")
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "dial tcp 10.1.2.3:7999: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
