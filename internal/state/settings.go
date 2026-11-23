package state

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Setting is a workspace-wide setting: a JSON value, and optionally a
// sealed secret with a hint to recognise it by.
type Setting struct {
	Key        string
	Value      string
	Secret     []byte
	SecretHint string
	UpdatedAt  time.Time
	UpdatedBy  string
}

// Setting returns a setting, or ErrNotFound.
func (s *DB) Setting(key string) (Setting, error) {
	var st Setting
	var updated sql.NullString
	err := s.db.QueryRow(`SELECT key, value, secret, secret_hint, updated_at, updated_by FROM settings WHERE key = ?`, key).
		Scan(&st.Key, &st.Value, &st.Secret, &st.SecretHint, &updated, &st.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Setting{}, ErrNotFound
	}
	st.UpdatedAt = parseTime(updated)
	return st, err
}

// PutSetting creates or replaces a setting.
func (s *DB) PutSetting(st Setting) error {
	_, err := s.db.Exec(`INSERT INTO settings (key, value, secret, secret_hint, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, secret = excluded.secret,
			secret_hint = excluded.secret_hint, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		st.Key, st.Value, st.Secret, st.SecretHint, s.stamp(), st.UpdatedBy)
	return err
}

// DeleteSetting removes a setting.
func (s *DB) DeleteSetting(key string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key = ?`, key)
	return err
}

// PendingNote is a note that was submitted but isn't in git yet: its title
// is being generated, or its commit failed and will be retried.
type PendingNote struct {
	ID        string
	Author    string
	Body      string
	Labels    []string // picked by the author
	Title     string   // given by the writer; "" means generate one
	Via       string   // what wrote it, e.g. "MCP: <app>"; "" for the web UI
	Meta      string   // other fields, as JSON (package notes)
	CreatedAt time.Time
	Attempts  int
	Error     string // the last failure, if any
}

// AddPendingNote queues a note.
func (s *DB) AddPendingNote(n PendingNote) error {
	_, err := s.db.Exec(`INSERT INTO pending_notes (id, author, body, labels, title, via, meta, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.Author, n.Body, strings.Join(n.Labels, ","), n.Title, n.Via, n.Meta, n.CreatedAt.UTC().Format(time.RFC3339))
	return err
}

// PendingNotes lists queued notes, oldest first.
func (s *DB) PendingNotes() ([]PendingNote, error) {
	rows, err := s.db.Query(`SELECT id, author, body, labels, title, via, meta, created_at, attempts, error FROM pending_notes ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingNote
	for rows.Next() {
		var n PendingNote
		var created sql.NullString
		var labels string
		if err := rows.Scan(&n.ID, &n.Author, &n.Body, &labels, &n.Title, &n.Via, &n.Meta, &created, &n.Attempts, &n.Error); err != nil {
			return nil, err
		}
		n.CreatedAt, n.Labels = parseTime(created), splitLabels(labels)
		out = append(out, n)
	}
	return out, rows.Err()
}

// PendingNote returns one queued note, or ErrNotFound.
func (s *DB) PendingNote(id string) (PendingNote, error) {
	var n PendingNote
	var created sql.NullString
	var labels string
	err := s.db.QueryRow(`SELECT id, author, body, labels, title, via, meta, created_at, attempts, error FROM pending_notes WHERE id = ?`, id).
		Scan(&n.ID, &n.Author, &n.Body, &labels, &n.Title, &n.Via, &n.Meta, &created, &n.Attempts, &n.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingNote{}, ErrNotFound
	}
	n.CreatedAt, n.Labels = parseTime(created), splitLabels(labels)
	return n, err
}

// FailPendingNote records a failed attempt.
func (s *DB) FailPendingNote(id, msg string) error {
	_, err := s.db.Exec(`UPDATE pending_notes SET attempts = attempts + 1, error = ? WHERE id = ?`, msg, id)
	return err
}

// DeletePendingNote removes a note from the queue (once it's in git).
func (s *DB) DeletePendingNote(id string) error {
	_, err := s.db.Exec(`DELETE FROM pending_notes WHERE id = ?`, id)
	return err
}

func splitLabels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

const timeZoneKey = "timezone"

// TimeZone returns the workspace's time zone name, or "" if none is set.
func (s *DB) TimeZone() (string, error) {
	st, err := s.Setting(timeZoneKey)
	if errors.Is(err, ErrNotFound) {
		return "", nil
	}
	return st.Value, err
}

// SetTimeZone stores the workspace's time zone name (checked by the caller).
func (s *DB) SetTimeZone(name, user string) error {
	return s.PutSetting(Setting{Key: timeZoneKey, Value: name, UpdatedBy: user})
}
