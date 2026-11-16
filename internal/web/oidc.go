package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/acheong08/ktx/internal/state"
)

// OIDCConfig points ktx at the identity provider (Pocket ID).
type OIDCConfig struct {
	Issuer       string // e.g. https://id.internal.melzi.dev
	ClientID     string
	ClientSecret string
	PublicURL    string // ktx's own base URL; the callback is PublicURL + "/auth/callback"
	AdminGroup   string // members of this group are ktx admins
	SessionTTL   time.Duration
}

// Auth signs people in through OIDC (authorization code + PKCE) and keeps
// sessions in the state database, so they survive restarts.
type Auth struct {
	cfg OIDCConfig
	db  *state.DB
	log *slog.Logger

	mu       sync.Mutex
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	pending  map[string]pendingLogin // keyed by the OAuth state
}

type pendingLogin struct {
	verifier, nonce, next string
	expires               time.Time
}

const (
	sessionCookie = "ktx_session"
	loginCookie   = "ktx_login" // ties the callback to the browser that started the login
	loginTTL      = 10 * time.Minute
)

// NewAuth makes an Auth. The provider is contacted lazily, so ktx starts
// even while the identity provider is down.
func NewAuth(cfg OIDCConfig, db *state.DB, log *slog.Logger) *Auth {
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 24 * time.Hour
	}
	cfg.PublicURL = strings.TrimSuffix(cfg.PublicURL, "/")
	return &Auth{cfg: cfg, db: db, log: log, pending: map[string]pendingLogin{}}
}

func (a *Auth) setup(ctx context.Context) (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.oauth != nil {
		return a.oauth, a.verifier, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(ctx, a.cfg.Issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("can't reach the identity provider at %s: %w", a.cfg.Issuer, err)
	}
	a.oauth = &oauth2.Config{
		ClientID: a.cfg.ClientID, ClientSecret: a.cfg.ClientSecret,
		Endpoint:    provider.Endpoint(),
		RedirectURL: a.cfg.PublicURL + "/auth/callback",
		Scopes:      []string{oidc.ScopeOpenID, "profile", "email", "groups"},
	}
	a.verifier = provider.Verifier(&oidc.Config{ClientID: a.cfg.ClientID})
	return a.oauth, a.verifier, nil
}

// user returns the signed-in user, if any.
func (a *Auth) user(r *http.Request) (state.User, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return state.User{}, false
	}
	u, err := a.db.SessionUser(c.Value)
	return u, err == nil
}

// login sends the browser to the identity provider.
func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	oauth, _, err := a.setup(r.Context())
	if err != nil {
		a.log.Error("oidc", "err", err)
		http.Error(w, "Sign-in is unavailable: "+err.Error(), http.StatusBadGateway)
		return
	}
	st, nonce, verifier := random(), random(), oauth2.GenerateVerifier()
	next := safeNext(r.URL.Query().Get("next"))

	a.mu.Lock()
	now := time.Now()
	for k, p := range a.pending {
		if now.After(p.expires) {
			delete(a.pending, k)
		}
	}
	if len(a.pending) > 1000 { // unauthenticated callers can start logins; bound the memory
		a.pending = map[string]pendingLogin{}
	}
	a.pending[st] = pendingLogin{verifier: verifier, nonce: nonce, next: next, expires: now.Add(loginTTL)}
	a.mu.Unlock()

	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: st, Path: "/auth/", MaxAge: int(loginTTL.Seconds()),
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, oauth.AuthCodeURL(st, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusSeeOther)
}

// callback finishes the sign-in and starts a session.
func (a *Auth) callback(w http.ResponseWriter, r *http.Request) (state.User, string, error) {
	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		return state.User{}, "", fmt.Errorf("the identity provider refused: %s %s", e, q.Get("error_description"))
	}
	st := q.Get("state")
	c, err := r.Cookie(loginCookie)
	if err != nil || c.Value != st {
		return state.User{}, "", errors.New("this sign-in was started in another browser or has expired; start again")
	}
	a.mu.Lock()
	p, ok := a.pending[st]
	delete(a.pending, st)
	a.mu.Unlock()
	if !ok || time.Now().After(p.expires) {
		return state.User{}, "", errors.New("this sign-in has expired; start again")
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Value: "", Path: "/auth/", MaxAge: -1})

	oauth, verifier, err := a.setup(r.Context())
	if err != nil {
		return state.User{}, "", err
	}
	tok, err := oauth.Exchange(r.Context(), q.Get("code"), oauth2.VerifierOption(p.verifier))
	if err != nil {
		return state.User{}, "", fmt.Errorf("exchanging the code: %w", err)
	}
	raw, _ := tok.Extra("id_token").(string)
	idt, err := verifier.Verify(r.Context(), raw)
	if err != nil {
		return state.User{}, "", fmt.Errorf("checking the ID token: %w", err)
	}
	if idt.Nonce != p.nonce {
		return state.User{}, "", errors.New("the ID token doesn't belong to this sign-in")
	}
	var claims struct {
		Username string   `json:"preferred_username"`
		Name     string   `json:"name"`
		Email    string   `json:"email"`
		Groups   []string `json:"groups"`
	}
	if err := idt.Claims(&claims); err != nil {
		return state.User{}, "", err
	}
	isAdmin := a.cfg.AdminGroup != "" && slices.Contains(claims.Groups, a.cfg.AdminGroup)
	u, err := a.db.Login(idt.Subject, claims.Username, claims.Name, claims.Email, isAdmin)
	if err != nil {
		return state.User{}, "", err
	}
	session, err := a.db.CreateSession(u.ID, a.cfg.SessionTTL)
	if err != nil {
		return state.User{}, "", err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: session, Path: "/", MaxAge: int(a.cfg.SessionTTL.Seconds()),
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode})
	return u, p.next, nil
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = a.db.DeleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

// safeNext only allows paths on this site, so the login can't be used to
// redirect elsewhere.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func secure(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func random() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
