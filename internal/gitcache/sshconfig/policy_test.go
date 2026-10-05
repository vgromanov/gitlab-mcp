package sshconfig

import "testing"

func TestConfiguredHostKeyDecisionsWithoutStrictOverride(t *testing.T) {
	for _, mode := range []string{"yes", "ask", "accept-new", "no"} {
		trusted, err := DecideHostKey(mode, HostKeyTrusted)
		if err != nil || !trusted.Accept || !trusted.Verified || trusted.Enroll {
			t.Fatal("trusted host decision wrong")
		}
		revoked, err := DecideHostKey(mode, HostKeyRevoked)
		if mode == "no" {
			if err != nil || !revoked.Accept || !revoked.DisableSensitiveFeatures || !revoked.DisableHostKeyUpdates || revoked.Verified {
				t.Fatal("known_hosts revocation off-mode restrictions wrong")
			}
		} else if err == nil || revoked.Accept {
			t.Fatal("revoked key accepted contrary to strict config")
		}
		missing, err := DecideHostKey(mode, HostKeyMissing)
		if mode == "no" || mode == "accept-new" {
			if err != nil || !missing.Accept || !missing.Enroll || missing.Verified {
				t.Fatal("configured enrollment decision suppressed")
			}
		} else if err == nil || missing.Accept {
			t.Fatal("unknown host accepted without confirmation")
		}
		changed, err := DecideHostKey(mode, HostKeyChanged)
		if mode == "no" {
			if err != nil || !changed.Accept || changed.Verified || changed.Enroll || !changed.DisableSensitiveFeatures {
				t.Fatal("configured changed-key behavior silently overridden")
			}
		} else if err == nil || changed.Accept {
			t.Fatal("changed key accepted contrary to config")
		}
	}
}
func TestHostKeyUpdateCapabilityDoesNotOverrideConfiguration(t *testing.T) {
	c := Config{StrictHostKeyChecking: "no", UpdateHostKeys: "no"}
	if err := CheckHostKeyUpdateSupport(c); err != nil {
		t.Fatal("strict-only policy remained after user approval")
	}
	c.UpdateHostKeys = "yes"
	if err := CheckHostKeyUpdateSupport(c); err != nil {
		t.Fatal(err)
	}
	if c.StrictHostKeyChecking != "no" || c.UpdateHostKeys != "yes" {
		t.Fatal("configuration modified")
	}
}
