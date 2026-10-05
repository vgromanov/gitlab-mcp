package listx

import (
	"errors"
	"golang.org/x/crypto/ssh"
	"slices"
)

var ErrSSHAlgorithms = errors.New("ssh identity signature algorithm is unsupported")

// Explicit secure native baseline for unset origin. Applicable algorithm config
// directives remain unsupported and fail during resolution, never silently ignored.
func secureAlgorithms(c *ssh.ClientConfig) {
	a := ssh.SupportedAlgorithms()
	c.Config.KeyExchanges = a.KeyExchanges
	c.Config.Ciphers = a.Ciphers
	c.Config.MACs = a.MACs
	c.HostKeyAlgorithms = a.HostKeys
}
func secureSigner(s ssh.Signer) (ssh.Signer, error) {
	switch s.PublicKey().Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
		return s, nil
	case ssh.KeyAlgoRSA:
		a, ok := s.(ssh.AlgorithmSigner)
		if !ok {
			return nil, ErrSSHAlgorithms
		}
		allowed := []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
		if multi, ok := s.(ssh.MultiAlgorithmSigner); ok {
			filtered := allowed[:0]
			for _, alg := range allowed {
				if slices.Contains(multi.Algorithms(), alg) {
					filtered = append(filtered, alg)
				}
			}
			allowed = filtered
		}
		out, e := ssh.NewSignerWithAlgorithms(a, allowed)
		if e != nil {
			return nil, ErrSSHAlgorithms
		}
		return out, nil
	default:
		return nil, ErrSSHAlgorithms
	}
}
