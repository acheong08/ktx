package sshsig

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// sign makes a key of the given type with ssh-keygen and signs msg with it.
func sign(t *testing.T, keyType, namespace, msg string) (sig string, pub ssh.PublicKey) {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	key := filepath.Join(t.TempDir(), "key")
	args := []string{"-q", "-t", keyType, "-N", "", "-C", "tester", "-f", key}
	if keyType == "rsa" {
		args = append(args, "-b", "2048")
	}
	if out, err := exec.Command("ssh-keygen", args...).CombinedOutput(); err != nil {
		t.Fatalf("keygen: %v %s", err, out)
	}
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-n", namespace, "-f", key)
	cmd.Stdin = strings.NewReader(msg)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("sign: %v %s", err, stderr.String())
	}
	pubRaw, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	pub, _, _, _, err = ssh.ParseAuthorizedKey(pubRaw)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), pub
}

func TestVerifyRealSignatures(t *testing.T) {
	for _, kt := range []string{"ed25519", "ecdsa", "rsa"} {
		t.Run(kt, func(t *testing.T) {
			sig, pub := sign(t, kt, "brain-server", "challenge-123")
			got, err := Verify(sig, []byte("challenge-123"), "brain-server")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Marshal(), pub.Marshal()) {
				t.Error("returned key differs from signer")
			}
		})
	}
}

func TestVerifyRejects(t *testing.T) {
	sig, _ := sign(t, "ed25519", "brain-server", "challenge-123")

	if _, err := Verify(sig, []byte("challenge-124"), "brain-server"); err == nil {
		t.Error("wrong message accepted")
	}
	if _, err := Verify(sig, []byte("challenge-123"), "git"); err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("wrong namespace: %v", err)
	}
	if _, err := Verify("hello", []byte("x"), "brain-server"); err == nil {
		t.Error("garbage accepted")
	}

	// Flip one base64 character inside the armor.
	lines := strings.Split(sig, "\n")
	b := []byte(lines[3])
	b[10] ^= 1
	lines[3] = string(b)
	if _, err := Verify(strings.Join(lines, "\n"), []byte("challenge-123"), "brain-server"); err == nil {
		t.Error("tampered signature accepted")
	}

	// Pasted with Windows line endings and surrounding whitespace still works.
	if _, err := Verify("\n  "+strings.ReplaceAll(sig, "\n", "\r\n")+"  ", []byte("challenge-123"), "brain-server"); err != nil {
		t.Errorf("whitespace variant: %v", err)
	}
}
