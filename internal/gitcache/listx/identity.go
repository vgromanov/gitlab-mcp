package listx

import (
	"errors"
	"gitlabci.raiffeisen.ru/skunk-works/tools/gitlab-mcp/internal/gitcache/sshfile"
	"golang.org/x/crypto/ssh"
	"io"
)

var (
	ErrIdentityFile      = errors.New("ssh identity file is unavailable or invalid")
	ErrIdentityEncrypted = errors.New("encrypted ssh identity files are not supported")
	ErrIdentityConflict  = errors.New("ssh identity file and explicit agent socket are mutually exclusive")
)

const maxIdentityBytes = 64 * 1024

// readIdentity reads only the explicitly selected file. It never searches key
// paths, runs helpers, prompts, loads an agent, or returns raw parser errors.
func readIdentity(path string) (ssh.Signer, error) {
	f, st, err := sshfile.OpenRegular(path, maxIdentityBytes)
	if err != nil {
		return nil, ErrIdentityFile
	}
	defer f.Close()
	if st.Size() <= 0 || !sshfile.PrivateKeyOK(st) {
		return nil, ErrIdentityFile
	}
	data, err := io.ReadAll(io.LimitReader(f, maxIdentityBytes+1))
	defer clear(data)
	if err != nil || len(data) > maxIdentityBytes {
		return nil, ErrIdentityFile
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		var encrypted *ssh.PassphraseMissingError
		if errors.As(err, &encrypted) {
			return nil, ErrIdentityEncrypted
		}
		return nil, ErrIdentityFile
	}
	return signer, nil
}
