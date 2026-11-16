// Package sshsig verifies signatures made with `ssh-keygen -Y sign`
// (the OpenSSH SSHSIG format, see PROTOCOL.sshsig in the OpenSSH sources).
package sshsig

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"strings"

	"golang.org/x/crypto/ssh"
)

const (
	magic      = "SSHSIG"
	armorBegin = "-----BEGIN SSH SIGNATURE-----"
	armorEnd   = "-----END SSH SIGNATURE-----"
)

// Verify checks an armored signature over message for namespace and returns
// the signing key. It does not decide whether that key is trusted.
func Verify(armored string, message []byte, namespace string) (ssh.PublicKey, error) {
	blob, err := unarmor(armored)
	if err != nil {
		return nil, err
	}
	r := reader{b: blob}
	if string(r.bytes(len(magic))) != magic {
		return nil, errors.New("not an SSH signature (bad magic)")
	}
	if v := r.uint32(); v != 1 {
		return nil, fmt.Errorf("unsupported SSH signature version %d", v)
	}
	pubBytes, ns, reserved, hashAlg, sigBytes := r.str(), r.str(), r.str(), r.str(), r.str()
	if r.err != nil || len(r.b) != 0 {
		return nil, errors.New("malformed SSH signature")
	}
	if string(ns) != namespace {
		return nil, fmt.Errorf("signature is for namespace %q, want %q (use ssh-keygen -Y sign -n %s)", ns, namespace, namespace)
	}

	pub, err := ssh.ParsePublicKey(pubBytes)
	if err != nil {
		return nil, fmt.Errorf("signature public key: %w", err)
	}
	var sig ssh.Signature
	if err := ssh.Unmarshal(sigBytes, &sig); err != nil {
		return nil, fmt.Errorf("signature blob: %w", err)
	}
	if sig.Format == ssh.KeyAlgoRSA {
		return nil, errors.New("ssh-rsa (SHA-1) signatures are not accepted")
	}

	var h hash.Hash
	switch string(hashAlg) {
	case "sha512":
		h = sha512.New()
	case "sha256":
		h = sha256.New()
	default:
		return nil, fmt.Errorf("unsupported hash algorithm %q", hashAlg)
	}
	h.Write(message)

	var signed bytes.Buffer
	signed.WriteString(magic)
	writeStr(&signed, []byte(namespace))
	writeStr(&signed, reserved)
	writeStr(&signed, hashAlg)
	writeStr(&signed, h.Sum(nil))

	if err := pub.Verify(signed.Bytes(), &sig); err != nil {
		return nil, errors.New("signature does not match the message")
	}
	return pub, nil
}

func unarmor(s string) ([]byte, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	begin := strings.Index(s, armorBegin)
	end := strings.Index(s, armorEnd)
	if begin < 0 || end < begin {
		return nil, fmt.Errorf("expected the output of ssh-keygen -Y sign, starting with %s", armorBegin)
	}
	b64 := strings.Join(strings.Fields(s[begin+len(armorBegin):end]), "")
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("signature is not valid base64: %w", err)
	}
	return blob, nil
}

type reader struct {
	b   []byte
	err error
}

func (r *reader) bytes(n int) []byte {
	if r.err != nil || n < 0 || len(r.b) < n {
		r.err = errors.New("short")
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) uint32() uint32 {
	b := r.bytes(4)
	if b == nil {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func (r *reader) str() []byte {
	n := r.uint32()
	if n > 1<<20 {
		r.err = errors.New("too long")
		return nil
	}
	return r.bytes(int(n))
}

func writeStr(w *bytes.Buffer, b []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	w.Write(n[:])
	w.Write(b)
}
