package sshconfig

import "testing"

func TestCoverage_HostKeyPolicyContracts(t *testing.T) {
	if err := CheckHostKeyUpdateSupport(Config{UpdateHostKeys: "yes"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckHostKeyUpdateSupport(Config{UpdateHostKeys: "ask"}); err == nil {
		t.Fatal("ask accepted")
	}
	if err := CheckHostKeyUpdateSupport(Config{UpdateHostKeys: "weird"}); err == nil {
		t.Fatal("invalid update mode accepted")
	}
	if _, err := DecideHostKey("bogus", HostKeyTrusted); err == nil {
		t.Fatal("invalid strict mode accepted")
	}
	if _, err := DecideHostKey("yes", HostKeyRevoked); err == nil {
		t.Fatal("revoked accepted in yes")
	}
	d, err := DecideHostKey("no", HostKeyRevoked)
	if err != nil || !d.Accept || !d.DisableHostKeyUpdates {
		t.Fatalf("revoked no-mode: %#v %v", d, err)
	}
	d, err = DecideHostKey("accept-new", HostKeyMissing)
	if err != nil || !d.Enroll || !d.Accept {
		t.Fatalf("accept-new missing: %#v %v", d, err)
	}
	if _, err := DecideHostKey("yes", HostKeyMissing); err == nil {
		t.Fatal("yes missing accepted")
	}
	if _, err := DecideHostKey("ask", HostKeyMissing); err == nil {
		t.Fatal("ask missing without prompt error")
	}
	if _, err := DecideHostKey("yes", HostKeyChanged); err == nil {
		t.Fatal("changed yes accepted")
	}
	d, err = DecideHostKey("no", HostKeyChanged)
	if err != nil || !d.Accept || !d.DisableSensitiveFeatures {
		t.Fatalf("changed no-mode: %#v %v", d, err)
	}
	d, err = DecideHostKey("yes", HostKeyTrusted)
	if err != nil || !d.Verified || !d.Accept {
		t.Fatalf("trusted: %#v %v", d, err)
	}
}
