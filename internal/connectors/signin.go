package connectors

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/acheong08/ktx/internal/secrets"
	"github.com/acheong08/ktx/internal/state"
)

// SignIn is implemented by connectors that connect by signing in to the
// service in a browser (an OAuth device flow) instead of with a pasted key.
// Their Fields are settings only; the secrets come from the sign-in.
type SignIn interface {
	// StartSignIn asks the service for a sign-in: a page and code for the
	// person to approve.
	StartSignIn(ctx context.Context) (Prompt, error)
	// PollSignIn checks once whether the person approved. It returns
	// ErrPending while they haven't, and the session's secrets and the
	// account once they have.
	PollSignIn(ctx context.Context, p Prompt) (secrets map[string]string, account string, err error)
}

// Prompt is a sign-in waiting for approval.
type Prompt struct {
	URL      string // page to open (with the code filled in, if the service supports it)
	Code     string // code to check or type there
	Expires  time.Time
	Interval time.Duration // how often to poll
	State    string        // the connector's own handle (e.g. the device code); never shown
}

// ErrPending means a sign-in hasn't been approved yet.
var ErrPending = errors.New("waiting for approval")

// ErrSignInAgain means a connection's session is gone (e.g. revoked, or a
// spent refresh token): someone has to sign in again.
var ErrSignInAgain = errors.New("the sign-in has expired: sign in again")

// PendingSignIn is a sign-in a person started from ktx.
type PendingSignIn struct {
	ID      string
	Prompt  Prompt
	Kind    string
	Label   string
	OwnerID int64
	ConnID  int64 // 0: a new connection; else signing in again
	cfg     Config
	next    time.Time
}

// StartSignIn begins a sign-in for a new connection (connID 0) owned by
// owner, or for signing connection connID in again. Settings are checked
// first.
func (m *Manager) StartSignIn(ctx context.Context, owner state.User, kind, label string, values map[string]string, connID int64) (PendingSignIn, error) {
	k, ok := m.Kind(kind)
	if !ok {
		return PendingSignIn{}, &FormError{[]string{"unknown connector " + kind}}
	}
	si, ok := k.(SignIn)
	if !ok {
		return PendingSignIn{}, &FormError{[]string{k.Title() + " doesn't connect by signing in"}}
	}
	if m.box == nil {
		return PendingSignIn{}, secrets.ErrNoKey
	}
	var cfg Config
	if connID == 0 {
		var problems []string
		cfg, problems = split(k, values, nil)
		if strings.TrimSpace(label) == "" {
			problems = append(problems, "Name is required")
		}
		if len(problems) > 0 {
			return PendingSignIn{}, &FormError{problems}
		}
		if _, err := k.Check(ctx, cfg); err != nil {
			return PendingSignIn{}, &FormError{[]string{err.Error()}}
		}
	}
	p, err := si.StartSignIn(ctx)
	if err != nil {
		return PendingSignIn{}, &FormError{[]string{k.Title() + ": " + err.Error()}}
	}
	if p.Interval <= 0 {
		p.Interval = 5 * time.Second
	}
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return PendingSignIn{}, err
	}
	ps := PendingSignIn{ID: hex.EncodeToString(b), Prompt: p, Kind: kind, Label: strings.TrimSpace(label),
		OwnerID: owner.ID, ConnID: connID, cfg: cfg, next: time.Now().Add(p.Interval)}
	m.mu.Lock()
	if m.signIns == nil {
		m.signIns = map[string]*PendingSignIn{}
	}
	for id, old := range m.signIns {
		if time.Now().After(old.Prompt.Expires.Add(time.Minute)) {
			delete(m.signIns, id)
		}
	}
	m.signIns[ps.ID] = &ps
	m.mu.Unlock()
	return ps, nil
}

// PendingSignInFor returns a pending sign-in of userID.
func (m *Manager) PendingSignInFor(id string, userID int64) (PendingSignIn, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.signIns[id]
	if !ok || p.OwnerID != userID {
		return PendingSignIn{}, false
	}
	return *p, true
}

// PollSignIn checks a pending sign-in of userID once (no more often than
// the service allows). When it's approved, the connection is created (or
// its session replaced) and its id returned with done true.
func (m *Manager) PollSignIn(ctx context.Context, id string, userID int64) (connID int64, done bool, err error) {
	m.mu.Lock()
	p, ok := m.signIns[id]
	if !ok || p.OwnerID != userID {
		m.mu.Unlock()
		return 0, false, errors.New("this sign-in isn't known any more (ktx may have restarted): start again")
	}
	if time.Now().After(p.Prompt.Expires) {
		delete(m.signIns, id)
		m.mu.Unlock()
		return 0, false, errors.New("the code expired before it was approved: start again")
	}
	if time.Now().Before(p.next) {
		m.mu.Unlock()
		return 0, false, nil
	}
	p.next = time.Now().Add(p.Prompt.Interval)
	ps := *p
	m.mu.Unlock()

	k, ok := m.Kind(ps.Kind)
	if !ok {
		return 0, false, errors.New("unknown connector " + ps.Kind)
	}
	sec, account, err := k.(SignIn).PollSignIn(ctx, ps.Prompt)
	if errors.Is(err, ErrPending) {
		return 0, false, nil
	}
	m.mu.Lock()
	delete(m.signIns, id)
	m.mu.Unlock()
	if err != nil {
		return 0, false, err
	}
	plain, _ := json.Marshal(sec)
	if ps.ConnID != 0 {
		conn, err := m.db.Connection(ps.ConnID)
		if err != nil {
			return 0, false, err
		}
		if err := m.db.SetConnectionSecrets(conn.ID, m.box.Seal(sealContext(conn.ID), plain)); err != nil {
			return 0, false, err
		}
		conn.Account, conn.Secrets = account, nil
		if err := m.db.UpdateConnection(conn); err != nil {
			return 0, false, err
		}
		return conn.ID, true, m.Reconcile()
	}
	cid, err := m.db.CreateConnection(state.Connection{Kind: ps.Kind, Label: ps.Label, OwnerID: ps.OwnerID,
		Settings: ps.cfg.Settings, Account: account, Enabled: true})
	if err != nil {
		return 0, false, err
	}
	if err := m.db.SetConnectionSecrets(cid, m.box.Seal(sealContext(cid), plain)); err != nil {
		m.db.DeleteConnection(cid)
		return 0, false, err
	}
	return cid, true, m.Reconcile()
}
