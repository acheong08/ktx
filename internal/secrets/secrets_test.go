package secrets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSealOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte(GenerateKey()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	box, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed := box.Seal("connection:1", []byte(`{"api_key":"grn_secret"}`))
	if got, err := box.Open("connection:1", sealed); err != nil || string(got) != `{"api_key":"grn_secret"}` {
		t.Fatalf("open: %q %v", got, err)
	}
	if _, err := box.Open("connection:2", sealed); err == nil {
		t.Error("opened with the wrong context")
	}
	other, _ := New(make([]byte, 32))
	if _, err := other.Open("connection:1", sealed); err == nil {
		t.Error("opened with the wrong key")
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := box.Open("connection:1", sealed); err == nil {
		t.Error("opened tampered data")
	}
	if a, b := box.Seal("c", []byte("x")), box.Seal("c", []byte("x")); string(a) == string(b) {
		t.Error("nonce reused")
	}
}

func TestKeyPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if got := KeyPath(""); got != "" {
		t.Errorf("no credential: %q", got)
	}
	os.WriteFile(filepath.Join(dir, CredentialName), []byte(GenerateKey()), 0o600)
	if got := KeyPath(""); got != filepath.Join(dir, CredentialName) {
		t.Errorf("credential: %q", got)
	}
	if got := KeyPath("/explicit"); got != "/explicit" {
		t.Errorf("explicit: %q", got)
	}
	if _, err := Load(""); err != ErrNoKey {
		t.Errorf("Load(\"\") = %v", err)
	}
}
