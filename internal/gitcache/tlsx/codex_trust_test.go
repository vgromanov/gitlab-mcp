package tlsx

import (
	"context"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexTrustContentChangesAtSamePath(t *testing.T) {
	a := httptest.NewTLSServer(nil)
	defer a.Close()
	b := httptest.NewTLSServer(nil)
	defer b.Close()
	path := filepath.Join(t.TempDir(), "ca.pem")
	write := func(der []byte) {
		t.Helper()
		if e := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); e != nil {
			t.Fatal(e)
		}
	}
	write(a.Certificate().Raw)
	_, first, e := Prepare(context.Background(), Input{ServerName: "example.com", CAPath: path})
	if e != nil {
		t.Fatal(e)
	}
	// A second certificate may be identical for httptest; adding a valid duplicate
	// changes the actual configured certificate sequence and must bind differently.
	data, _ := os.ReadFile(path)
	if e := os.WriteFile(path, append(data, data...), 0600); e != nil {
		t.Fatal(e)
	}
	_, second, e := Prepare(context.Background(), Input{ServerName: "example.com", CAPath: path})
	if e != nil || first == second {
		t.Fatal("same-path trust contents not bound")
	}
	if e := os.WriteFile(path, []byte("bad CA"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e := Prepare(context.Background(), Input{ServerName: "example.com", CAPath: path}); e == nil {
		t.Fatal("changed malformed CA accepted")
	}
	if _, _, e := Prepare(context.Background(), Input{ServerName: "example.com", CAPath: path, Insecure: true, AllowedInsecureHost: "example.com"}); e == nil {
		t.Fatal("insecure ignored invalid configured CA")
	}
}
