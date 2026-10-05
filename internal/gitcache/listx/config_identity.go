package listx

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"

	"golang.org/x/crypto/ssh"

	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshconfig"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshfile"
)

var (
	ErrConfigNoIdentities      = errors.New("ssh config: no usable configured public-key identity")
	ErrConfigCertificate       = errors.New("ssh config: certificate identities are not supported by the native selector")
	ErrConfigPublicKeyDisabled = errors.New("ssh config: public-key authentication is disabled; other authentication methods are not implemented")
)

type identityCandidate struct {
	path    string
	key     ssh.PublicKey
	matched bool
}

// configSigners resolves configuration-derived identities against an already
// authorized agent inventory. It never opens an agent, writes it, or dials.
// RSA signers retain algorithm signing while forbidding SHA-1 fallback.
func configSigners(c sshconfig.Config, available []ssh.Signer) ([]ssh.Signer, error) {
	candidates, err := configuredCandidates(c)
	if err != nil {
		return nil, err
	}
	if c.IdentityAgent == "" {
		available = nil
	}
	var matched, other []ssh.Signer
	for _, signer := range available {
		if _, cert := signer.PublicKey().(*ssh.Certificate); cert {
			if !c.IdentitiesOnly {
				return nil, ErrConfigCertificate
			}
			continue
		}
		found := false
		for i := range candidates {
			if bytes.Equal(signer.PublicKey().Marshal(), candidates[i].key.Marshal()) {
				candidates[i].matched = true
				found = true
			}
		}
		if found {
			matched = append(matched, signer)
		} else if !c.IdentitiesOnly {
			other = append(other, signer)
		}
	}
	result := append(matched, other...)
	for _, candidate := range candidates {
		if candidate.matched {
			continue
		}
		signer, err := readIdentity(candidate.path)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(signer.PublicKey().Marshal(), candidate.key.Marshal()) {
			return nil, ErrIdentityFile
		}
		result = append(result, signer)
	}
	if len(result) == 0 {
		return nil, ErrConfigNoIdentities
	}
	for i, s := range result {
		var err error
		result[i], err = secureSigner(s)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

// authSelectionFingerprint is a process-local, non-secret binding of effective
// public-key authentication policy and selected public identities. It never
// includes private keys, passphrases, or reusable secret digests.
func authSelectionFingerprint(c sshconfig.Config, signers []ssh.Signer) string {
	h := sha256.New()
	write := func(s string) {
		_, _ = h.Write([]byte(s))
		_, _ = h.Write([]byte{0})
	}
	if c.PublicKeyAuthentication {
		write("pubkey=1")
	} else {
		write("pubkey=0")
	}
	if c.IdentitiesOnly {
		write("identities_only=1")
	} else {
		write("identities_only=0")
	}
	if c.IdentityAgent == "" {
		write("agent=none")
	} else {
		write("agent=set")
		write(c.IdentityAgent)
	}
	for _, path := range c.IdentityFiles {
		write("idfile")
		write(path)
	}
	for _, s := range signers {
		if s == nil {
			continue
		}
		sum := sha256.Sum256(s.PublicKey().Marshal())
		_, _ = h.Write(sum[:])
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Prefer public metadata, allowing agent use without opening a private file.
// Without a sidecar, OpenSSH private keys may expose their public half even
// when encrypted; the parser returns that metadata in PassphraseMissingError.
func configuredPublic(path string) (ssh.PublicKey, error) {
	data, err := readIdentityMetadata(path + ".pub")
	if errors.Is(err, os.ErrNotExist) {
		data, err = readIdentityMetadata(path)
	}
	if err != nil {
		return nil, err
	}
	defer clear(data)
	if pub, _, opts, rest, err := ssh.ParseAuthorizedKey(data); err == nil {
		if len(opts) > 0 || len(bytes.TrimSpace(rest)) != 0 {
			return nil, ErrIdentityFile
		}
		return pub, nil
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		var encrypted *ssh.PassphraseMissingError
		if errors.As(err, &encrypted) && encrypted.PublicKey != nil {
			return encrypted.PublicKey, nil
		}
		return nil, ErrIdentityFile
	}
	return signer.PublicKey(), nil
}
func readIdentityMetadata(path string) ([]byte, error) {
	f, st, err := sshfile.OpenRegular(path, maxIdentityBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, ErrIdentityFile
	}
	defer f.Close()
	// Read only a bounded first line for public-only candidates. If it is
	// not a complete valid authorized key, apply private permissions before
	// reading further or invoking a private-key parser. Never trust a suffix.
	reader := bufio.NewReader(io.LimitReader(singleByteReader{f}, maxIdentityBytes+1))
	first, readErr := reader.ReadBytes('\n')
	if readErr != nil && readErr != io.EOF || len(first) > maxIdentityBytes {
		clear(first)
		return nil, ErrIdentityFile
	}
	if pub, _, opts, rest, e := ssh.ParseAuthorizedKey(first); e == nil && pub != nil && len(opts) == 0 && len(bytes.TrimSpace(rest)) == 0 {
		tail, e := io.ReadAll(reader)
		if e != nil || len(first)+len(tail) > maxIdentityBytes || len(bytes.TrimSpace(tail)) != 0 {
			clear(first)
			clear(tail)
			return nil, ErrIdentityFile
		}
		return first, nil
	}
	clear(first)
	if !sshfile.PrivateKeyOK(st) {
		return nil, ErrIdentityFile
	}
	if _, err = f.Seek(0, 0); err != nil {
		return nil, ErrIdentityFile
	}

	data, err := io.ReadAll(io.LimitReader(f, maxIdentityBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxIdentityBytes {
		clear(data)
		return nil, ErrIdentityFile
	}
	return data, nil
}

// Avoid buffered read-ahead into private bodies before descriptor permission checks.
type singleByteReader struct{ io.Reader }

func (r singleByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

func configuredCandidates(c sshconfig.Config) ([]identityCandidate, error) {
	if !c.PublicKeyAuthentication {
		return nil, ErrConfigPublicKeyDisabled
	}
	var candidates []identityCandidate
	for _, path := range c.IdentityFiles {
		if _, err := os.Stat(path + "-cert.pub"); err == nil {
			return nil, ErrConfigCertificate
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, ErrIdentityFile
		}
		pub, err := configuredPublic(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, ok := pub.(*ssh.Certificate); ok {
			return nil, ErrConfigCertificate
		}
		candidates = append(candidates, identityCandidate{path: path, key: pub})
	}
	return candidates, nil
}
