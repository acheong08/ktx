package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/acheong08/ktx/internal/sshsig"
)

// Namespace is the ssh-keygen -Y namespace for login signatures. It keeps a
// login signature from being valid for anything else (git commits, files).
const Namespace = "brain-server"

const challengeTTL = 5 * time.Minute

// AuthorizedKey is a public key allowed to log in, and the name it logs in as.
type AuthorizedKey struct {
	Key  ssh.PublicKey
	Name string
}

// ParseAuthorizedKeys reads authorized_keys-style lines ("ssh-ed25519 AAAA...
// antonio"). The comment is the user name; without one, a short fingerprint.
func ParseAuthorizedKeys(text string) ([]AuthorizedKey, error) {
	var keys []AuthorizedKey
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("authorized key line %d: %w", i+1, err)
		}
		name := strings.TrimSpace(comment)
		if name == "" {
			name = "key " + strings.TrimPrefix(ssh.FingerprintSHA256(pub), "SHA256:")[:8]
		}
		keys = append(keys, AuthorizedKey{Key: pub, Name: name})
	}
	if len(keys) == 0 {
		return nil, errors.New("no authorized keys found")
	}
	return keys, nil
}

// Auth issues login challenges, checks signed ones and keeps sessions in
// memory. Nothing secret is configured: the challenge MAC key is generated
// at startup, so a restart logs everyone out.
type Auth struct {
	keys   []AuthorizedKey
	ttl    time.Duration
	macKey []byte
	now    func() time.Time

	mu       sync.Mutex
	used     map[string]time.Time // challenge nonce -> expiry, so each is used once
	sessions map[string]session
}

type session struct {
	user    string
	expires time.Time
}

// NewAuth returns an Auth for keys with sessions lasting ttl.
func NewAuth(keys []AuthorizedKey, ttl time.Duration) *Auth {
	return &Auth{
		keys: keys, ttl: ttl, macKey: randomBytes(32), now: time.Now,
		used: map[string]time.Time{}, sessions: map[string]session{},
	}
}

// NewChallenge returns a fresh challenge string. Challenges are
// self-authenticating (MAC'd) so the server stores nothing until one is used.
func (a *Auth) NewChallenge() string {
	exp := strconv.FormatInt(a.now().Add(challengeTTL).Unix(), 10)
	nonce := base64.RawURLEncoding.EncodeToString(randomBytes(16))
	return "brain-login:" + exp + ":" + nonce + ":" + a.mac(exp+":"+nonce)
}

// ChallengeValid reports whether a challenge was issued here, is unexpired
// and unused.
func (a *Auth) ChallengeValid(c string) bool {
	_, err := a.checkChallenge(c)
	return err == nil
}

func (a *Auth) checkChallenge(c string) (nonce string, err error) {
	parts := strings.Split(c, ":")
	if len(parts) != 4 || parts[0] != "brain-login" {
		return "", errors.New("unknown challenge")
	}
	exp, nonce, mac := parts[1], parts[2], parts[3]
	if !hmac.Equal([]byte(mac), []byte(a.mac(exp+":"+nonce))) {
		return "", errors.New("unknown challenge (the server may have restarted); reload the page")
	}
	unix, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || a.now().Unix() > unix {
		return "", errors.New("challenge expired; reload the page for a new one")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, used := a.used[nonce]; used {
		return "", errors.New("challenge already used; reload the page for a new one")
	}
	return nonce, nil
}

// Login checks a signature over a challenge and starts a session. It returns
// the session token and the user name.
func (a *Auth) Login(challenge, signature string) (token, user string, err error) {
	nonce, err := a.checkChallenge(challenge)
	if err != nil {
		return "", "", err
	}

	pub, err := sshsig.Verify(signature, []byte(challenge), Namespace)
	if err != nil {
		// Allow for `echo` adding a newline to the signed text.
		var err2 error
		if pub, err2 = sshsig.Verify(signature, []byte(challenge+"\n"), Namespace); err2 != nil {
			return "", "", err
		}
	}
	for _, k := range a.keys {
		if bytes.Equal(k.Key.Marshal(), pub.Marshal()) {
			user = k.Name
		}
	}
	if user == "" {
		return "", "", fmt.Errorf("key %s is not authorized", ssh.FingerprintSHA256(pub))
	}

	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, used := a.used[nonce]; used {
		return "", "", errors.New("challenge already used; reload the page for a new one")
	}
	for n, exp := range a.used {
		if now.After(exp) {
			delete(a.used, n)
		}
	}
	for t, s := range a.sessions {
		if now.After(s.expires) {
			delete(a.sessions, t)
		}
	}
	a.used[nonce] = now.Add(challengeTTL)
	token = base64.RawURLEncoding.EncodeToString(randomBytes(32))
	a.sessions[token] = session{user: user, expires: now.Add(a.ttl)}
	return token, user, nil
}

// User returns the user of a live session.
func (a *Auth) User(token string) (string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[token]
	if !ok || a.now().After(s.expires) {
		delete(a.sessions, token)
		return "", false
	}
	return s.user, true
}

// Logout ends a session.
func (a *Auth) Logout(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, token)
}

func (a *Auth) mac(s string) string {
	m := hmac.New(sha256.New, a.macKey)
	m.Write([]byte(s))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
