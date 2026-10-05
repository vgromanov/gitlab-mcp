package sshconfig

import "errors"

// CheckHostKeyUpdateSupport validates noninteractive update policy. Native
// authenticated yes-mode handling lives in sshtrust; ask-mode is explicit.
func CheckHostKeyUpdateSupport(c Config) error {
	switch c.UpdateHostKeys {
	case "no", "yes":
		return nil
	case "ask":
		return errors.New("ssh config: UpdateHostKeys=ask requires interactive confirmation; native transport is noninteractive")
	default:
		return errors.New("ssh config: invalid UpdateHostKeys mode")
	}
}

// HostKeyState must come from configured known-host databases for the exact
// resolved endpoint. These decisions are pure: they never enroll a key or
// modify trust. Persistence and callbacks must be reviewed before integration.
type HostKeyState int

const (
	HostKeyTrusted HostKeyState = iota
	HostKeyMissing
	HostKeyChanged
	HostKeyRevoked
)

type HostKeyDecision struct {
	Accept                   bool
	Verified                 bool
	Enroll                   bool
	Prompt                   bool
	DisableSensitiveFeatures bool
	DisableHostKeyUpdates    bool
}

func DecideHostKey(mode string, state HostKeyState) (HostKeyDecision, error) {
	switch mode {
	case "yes", "ask", "accept-new", "no":
	default:
		return HostKeyDecision{}, errors.New("ssh config: invalid StrictHostKeyChecking mode")
	}
	if state == HostKeyRevoked {
		// This state denotes a known_hosts @revoked marker, not a separate
		// RevokedHostKeys file/KRL (that directive is not implemented).
		if mode == "no" {
			return HostKeyDecision{Accept: true, DisableSensitiveFeatures: true, DisableHostKeyUpdates: true}, nil
		}
		return HostKeyDecision{}, errors.New("ssh host key is revoked")
	}
	if state == HostKeyTrusted {
		return HostKeyDecision{Accept: true, Verified: true}, nil
	}
	if state == HostKeyMissing {
		switch mode {
		case "no", "accept-new":
			return HostKeyDecision{Accept: true, Enroll: true, DisableHostKeyUpdates: true}, nil
		case "ask":
			return HostKeyDecision{Prompt: true}, errors.New("ssh config: unknown host key requires confirmation in ask mode")
		default:
			return HostKeyDecision{}, errors.New("ssh host key is not trusted")
		}
	}
	if state == HostKeyChanged {
		if mode == "no" {
			return HostKeyDecision{Accept: true, DisableSensitiveFeatures: true, DisableHostKeyUpdates: true}, nil
		}
		return HostKeyDecision{}, errors.New("ssh host key has changed")
	}
	return HostKeyDecision{}, errors.New("ssh host-key state unavailable")
}
