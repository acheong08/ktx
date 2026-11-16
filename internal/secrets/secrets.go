// Package secrets encrypts credentials at rest with a master key.
//
// The master key is a systemd encrypted credential on the server
// (LoadCredentialEncrypted=ktx-master-key), so it is never in a config file
// and ktx starts unattended. What this protects: a copy of state.db (a
// backup, a stray scp) is useless without the key. What it can't protect
// against: root on the server, or ktx itself, which must decrypt to use them.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CredentialName is the systemd credential holding the master key.
const CredentialName = "ktx-master-key"

const version = 1

// ErrNoKey means no master key is configured, so secrets can't be stored.
var ErrNoKey = errors.New("no master key configured (BRAIN_MASTER_KEY_FILE or systemd credential " + CredentialName + ")")

// Box seals and opens secrets with one master key.
type Box struct{ aead cipher.AEAD }

// New makes a Box from a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// KeyPath finds the master key: an explicit path, else the systemd
// credential. It returns "" if neither exists.
func KeyPath(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		p := filepath.Join(dir, CredentialName)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// Load reads a key file holding 32 bytes, raw or base64.
func Load(path string) (*Box, error) {
	if path == "" {
		return nil, ErrNoKey
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil {
			return nil, fmt.Errorf("%s: not 32 raw bytes or base64: %w", path, err)
		}
		b = dec
	}
	return New(b)
}

// GenerateKey returns a new random key, base64-encoded for a key file.
func GenerateKey() string {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(k)
}

// Seal encrypts plaintext. context names what the secret belongs to (e.g.
// "connection:7"); Open must be given the same context, so a sealed value
// can't be moved to another row.
func (b *Box) Seal(context string, plaintext []byte) []byte {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	out := append([]byte{version}, nonce...)
	return b.aead.Seal(out, nonce, plaintext, []byte(context))
}

// Open decrypts what Seal produced for the same context.
func (b *Box) Open(context string, sealed []byte) ([]byte, error) {
	n := b.aead.NonceSize()
	if len(sealed) < 1+n || sealed[0] != version {
		return nil, errors.New("sealed secret has an unknown format")
	}
	pt, err := b.aead.Open(nil, sealed[1:1+n], sealed[1+n:], []byte(context))
	if err != nil {
		return nil, errors.New("can't decrypt secret: wrong master key or it belongs to another record")
	}
	return pt, nil
}
