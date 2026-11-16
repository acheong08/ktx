// Package config reads settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every setting. See README.md for the variables.
type Config struct {
	ListenAddr     string
	RepoURL        string
	Branch         string
	WorkDir        string
	SSHKeyPath     string
	KnownHostsPath string
	AuthorName     string
	AuthorEmail    string
	Tokens         map[string]string // bearer token -> user name
	SyncInterval   time.Duration
	MaxUploadBytes int64

	Types []string // type suggestions in the web UI

	StatePath     string // SQLite: users, sessions, API tokens, connections
	MasterKeyFile string // "" = the systemd credential ktx-master-key, if any

	// Sign-in through an OIDC provider (Pocket ID). Off when OIDCIssuer is "".
	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	PublicURL        string // ktx's own URL, for the OIDC callback
	AdminGroup       string
	SessionTTL       time.Duration
}

// Load reads the environment and reports every missing or invalid value.
func Load() (Config, error) {
	c := Config{
		ListenAddr:     env("BRAIN_LISTEN_ADDR", ":8080"),
		RepoURL:        os.Getenv("BRAIN_REPO_URL"),
		Branch:         env("BRAIN_BRANCH", "main"),
		WorkDir:        env("BRAIN_WORKDIR", "./data/repo"),
		SSHKeyPath:     os.Getenv("BRAIN_SSH_KEY_PATH"),
		KnownHostsPath: os.Getenv("BRAIN_SSH_KNOWN_HOSTS"),
		AuthorName:     env("BRAIN_GIT_AUTHOR_NAME", "brain-server"),
		AuthorEmail:    env("BRAIN_GIT_AUTHOR_EMAIL", "brain-server@users.noreply.github.com"),
	}
	var errs []error

	if c.RepoURL == "" {
		errs = append(errs, errors.New("BRAIN_REPO_URL is required (e.g. git@github.com:org/brain.git)"))
	}

	tokens, err := ParseTokens(os.Getenv("BRAIN_API_TOKENS"))
	if err != nil {
		errs = append(errs, err)
	}
	c.Tokens = tokens

	if c.SyncInterval, err = time.ParseDuration(env("BRAIN_SYNC_INTERVAL", "5m")); err != nil {
		errs = append(errs, fmt.Errorf("BRAIN_SYNC_INTERVAL: %w", err))
	}

	mb, err := strconv.ParseInt(env("BRAIN_MAX_UPLOAD_MB", "10"), 10, 64)
	if err != nil || mb <= 0 {
		errs = append(errs, errors.New("BRAIN_MAX_UPLOAD_MB must be a positive integer"))
	}
	c.MaxUploadBytes = mb << 20

	for _, t := range strings.Split(env("BRAIN_TRANSCRIPT_TYPES", "advisor,customer_discovery,investor,internal"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			c.Types = append(c.Types, t)
		}
	}

	c.StatePath = env("BRAIN_STATE_DB", "./data/state.db")
	c.MasterKeyFile = os.Getenv("BRAIN_MASTER_KEY_FILE")

	c.OIDCIssuer = os.Getenv("BRAIN_OIDC_ISSUER")
	c.OIDCClientID = os.Getenv("BRAIN_OIDC_CLIENT_ID")
	c.OIDCClientSecret = os.Getenv("BRAIN_OIDC_CLIENT_SECRET")
	if f := os.Getenv("BRAIN_OIDC_CLIENT_SECRET_FILE"); f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, fmt.Errorf("BRAIN_OIDC_CLIENT_SECRET_FILE: %w", err))
		}
		c.OIDCClientSecret = strings.TrimSpace(string(b))
	}
	c.PublicURL = strings.TrimSuffix(os.Getenv("BRAIN_PUBLIC_URL"), "/")
	c.AdminGroup = env("BRAIN_OIDC_ADMIN_GROUP", "ktx-admins")
	if c.SessionTTL, err = time.ParseDuration(env("BRAIN_SESSION_TTL", "24h")); err != nil || c.SessionTTL <= 0 {
		errs = append(errs, errors.New("BRAIN_SESSION_TTL must be a positive duration like 24h"))
	}
	if c.OIDCIssuer != "" {
		for name, v := range map[string]string{"BRAIN_OIDC_CLIENT_ID": c.OIDCClientID, "BRAIN_OIDC_CLIENT_SECRET": c.OIDCClientSecret, "BRAIN_PUBLIC_URL": c.PublicURL} {
			if v == "" {
				errs = append(errs, fmt.Errorf("%s is required when BRAIN_OIDC_ISSUER is set", name))
			}
		}
	}

	for _, p := range []struct{ name, path string }{
		{"BRAIN_SSH_KEY_PATH", c.SSHKeyPath},
		{"BRAIN_SSH_KNOWN_HOSTS", c.KnownHostsPath},
	} {
		if p.path != "" {
			if _, err := os.Stat(p.path); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p.name, err))
			}
		}
	}
	return c, errors.Join(errs...)
}

// ParseTokens reads "user:token,user:token". Each user name ends up in
// commit trailers and in the uploaded_by field. Empty is fine: people can
// create their own tokens in the web UI instead.
func ParseTokens(s string) (map[string]string, error) {
	tokens := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		user, token, ok := strings.Cut(pair, ":")
		user, token = strings.TrimSpace(user), strings.TrimSpace(token)
		if !ok || user == "" || token == "" {
			return nil, errors.New(`BRAIN_API_TOKENS entries must look like "user:token"`)
		}
		if len(token) < 24 {
			return nil, fmt.Errorf("BRAIN_API_TOKENS: token for %q is shorter than 24 characters (try: openssl rand -hex 32)", user)
		}
		if _, dup := tokens[token]; dup {
			return nil, fmt.Errorf("BRAIN_API_TOKENS: token for %q is used twice", user)
		}
		tokens[token] = user
	}
	return tokens, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
