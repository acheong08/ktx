// Package state is ktx's operational database (SQLite): users, sessions,
// API tokens, OAuth apps and grants, connector connections and their run
// history, workspace settings, and notes still being saved.
//
// Knowledge lives in the git repository; this database holds only what is
// needed to run the service. Losing it costs re-entering credentials and
// re-running syncs (which are idempotent), never knowledge.
package state

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go, so builds stay static (CGO_ENABLED=0)
)

// ErrNotFound is returned when a row doesn't exist.
var ErrNotFound = errors.New("not found")

// DB is the state database.
type DB struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer; the load is tiny
	s := &DB{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrating %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *DB) Close() error { return s.db.Close() }

// SetClock replaces time.Now, for tests.
func (s *DB) SetClock(now func() time.Time) { s.now = now }

// Each entry moves the schema up one version (PRAGMA user_version).
var migrations = []string{
	`CREATE TABLE users (
		id            INTEGER PRIMARY KEY,
		subject       TEXT NOT NULL UNIQUE,      -- OIDC "sub"; never changes
		username      TEXT NOT NULL,
		name          TEXT NOT NULL DEFAULT '',
		email         TEXT NOT NULL DEFAULT '',
		is_admin      INTEGER NOT NULL DEFAULT 0, -- from the identity provider's groups, refreshed at login
		created_at    TEXT NOT NULL,
		last_login_at TEXT
	);
	CREATE TABLE sessions (
		token_hash TEXT PRIMARY KEY,
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		created_at TEXT NOT NULL,
		expires_at TEXT NOT NULL
	);
	CREATE TABLE api_tokens (
		id           INTEGER PRIMARY KEY,
		user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		name         TEXT NOT NULL,
		token_hash   TEXT NOT NULL UNIQUE,
		created_at   TEXT NOT NULL,
		last_used_at TEXT
	);
	CREATE TABLE connections (
		id          INTEGER PRIMARY KEY,
		kind        TEXT NOT NULL,
		label       TEXT NOT NULL,
		owner_id    INTEGER NOT NULL REFERENCES users(id),
		settings    TEXT NOT NULL DEFAULT '{}', -- JSON, not secret
		secrets     BLOB,                       -- sealed JSON (see package secrets)
		secret_hint TEXT NOT NULL DEFAULT '',   -- e.g. "…a1f3", to recognise a key without revealing it
		account     TEXT NOT NULL DEFAULT '',   -- whose account the credential belongs to
		enabled     INTEGER NOT NULL DEFAULT 1,
		cursor      TEXT NOT NULL DEFAULT '',   -- connector's bookmark
		created_at  TEXT NOT NULL,
		updated_at  TEXT NOT NULL
	);
	CREATE TABLE runs (
		id            INTEGER PRIMARY KEY,
		connection_id INTEGER NOT NULL REFERENCES connections(id) ON DELETE CASCADE,
		trigger       TEXT NOT NULL,            -- schedule | manual
		started_at    TEXT NOT NULL,
		finished_at   TEXT,
		created       INTEGER NOT NULL DEFAULT 0,
		updated       INTEGER NOT NULL DEFAULT 0,
		unchanged     INTEGER NOT NULL DEFAULT 0,
		commit_sha    TEXT NOT NULL DEFAULT '',
		error         TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX runs_by_connection ON runs(connection_id, id DESC);`,
	// Tokens get a scope: "api" (uploads) or "caldav" (calendar clients).
	`ALTER TABLE api_tokens ADD COLUMN scope TEXT NOT NULL DEFAULT 'api';`,
	// OAuth for the MCP server: apps that registered themselves (dynamic
	// client registration), and what people allowed them (a grant holds the
	// current access and refresh token).
	`CREATE TABLE oauth_clients (
		id            TEXT PRIMARY KEY,
		name          TEXT NOT NULL,
		redirect_uris TEXT NOT NULL,             -- JSON array
		created_at    TEXT NOT NULL
	);
	CREATE TABLE oauth_grants (
		id                 INTEGER PRIMARY KEY,
		user_id            INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		client_id          TEXT NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
		access_hash        TEXT NOT NULL UNIQUE,
		access_expires_at  TEXT NOT NULL,
		refresh_hash       TEXT NOT NULL UNIQUE,
		refresh_expires_at TEXT NOT NULL,
		created_at         TEXT NOT NULL,
		last_used_at       TEXT
	);
	CREATE INDEX oauth_grants_by_user ON oauth_grants(user_id);`,
	// Workspace settings (e.g. the AI provider), with an optional sealed
	// secret; and notes waiting for a title and their commit.
	`CREATE TABLE settings (
		key         TEXT PRIMARY KEY,
		value       TEXT NOT NULL DEFAULT '',   -- JSON, not secret
		secret      BLOB,                       -- sealed (see package secrets)
		secret_hint TEXT NOT NULL DEFAULT '',
		updated_at  TEXT NOT NULL,
		updated_by  TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE pending_notes (
		id         TEXT PRIMARY KEY,
		author     TEXT NOT NULL,
		body       TEXT NOT NULL,
		created_at TEXT NOT NULL,
		attempts   INTEGER NOT NULL DEFAULT 0,
		error      TEXT NOT NULL DEFAULT ''
	);`,
	// Labels the author picked for a queued note, comma-separated.
	`ALTER TABLE pending_notes ADD COLUMN labels TEXT NOT NULL DEFAULT '';`,
}

func (s *DB) migrate() error {
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *DB) stamp() string { return s.now().UTC().Format(time.RFC3339) }

func parseTime(v sql.NullString) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, v.String)
	return t
}

// newToken returns a random token with a recognisable prefix, and its hash.
func newToken(prefix string) (token, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token = prefix + base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
