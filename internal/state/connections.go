package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Connection is one configured connector: a kind (e.g. "granola"), whose
// credential it is, its settings and its sync bookmark.
type Connection struct {
	ID         int64
	Kind       string
	Label      string
	OwnerID    int64
	Owner      string // owner's display name, filled in by reads
	Settings   map[string]string
	Secrets    []byte // sealed; only the secrets package can open it
	SecretHint string
	Account    string
	Enabled    bool
	Cursor     string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const connCols = `c.id, c.kind, c.label, c.owner_id, u.username, c.settings, c.secrets, c.secret_hint,
	c.account, c.enabled, c.cursor, c.created_at, c.updated_at`

func scanConn(row interface{ Scan(...any) error }) (Connection, error) {
	var c Connection
	var settings string
	var created, updated sql.NullString
	err := row.Scan(&c.ID, &c.Kind, &c.Label, &c.OwnerID, &c.Owner, &settings, &c.Secrets, &c.SecretHint,
		&c.Account, &c.Enabled, &c.Cursor, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Connection{}, ErrNotFound
	}
	if err != nil {
		return Connection{}, err
	}
	c.Settings = map[string]string{}
	_ = json.Unmarshal([]byte(settings), &c.Settings)
	c.CreatedAt, c.UpdatedAt = parseTime(created), parseTime(updated)
	return c, nil
}

// CreateConnection stores a new connection and returns its ID.
func (s *DB) CreateConnection(c Connection) (int64, error) {
	settings, _ := json.Marshal(nonNil(c.Settings))
	now := s.stamp()
	res, err := s.db.Exec(`INSERT INTO connections
		(kind, label, owner_id, settings, secrets, secret_hint, account, enabled, cursor, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		c.Kind, c.Label, c.OwnerID, string(settings), c.Secrets, c.SecretHint, c.Account, c.Enabled, c.Cursor, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateConnection saves label, settings, credential, account and enabled.
// It leaves the cursor alone (see SetCursor).
// A nil c.Secrets keeps the stored secrets (a sync may have rotated them
// meanwhile, e.g. a single-use refresh token).
func (s *DB) UpdateConnection(c Connection) error {
	settings, _ := json.Marshal(nonNil(c.Settings))
	var sealed any // NULL: keep
	if c.Secrets != nil {
		sealed = c.Secrets
	}
	res, err := s.db.Exec(`UPDATE connections SET label = ?, settings = ?, secrets = COALESCE(?, secrets), secret_hint = ?,
		account = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		c.Label, string(settings), sealed, c.SecretHint, c.Account, c.Enabled, s.stamp(), c.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetConnectionSecrets replaces only a connection's sealed secrets.
func (s *DB) SetConnectionSecrets(id int64, sealed []byte) error {
	res, err := s.db.Exec(`UPDATE connections SET secrets = ? WHERE id = ?`, sealed, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCursor saves a connection's sync bookmark.
func (s *DB) SetCursor(id int64, cursor string) error {
	_, err := s.db.Exec(`UPDATE connections SET cursor = ? WHERE id = ?`, cursor, id)
	return err
}

// Connection returns one connection.
func (s *DB) Connection(id int64) (Connection, error) {
	return scanConn(s.db.QueryRow(`SELECT `+connCols+` FROM connections c JOIN users u ON u.id = c.owner_id WHERE c.id = ?`, id))
}

// Connections lists all connections.
func (s *DB) Connections() ([]Connection, error) {
	rows, err := s.db.Query(`SELECT ` + connCols + ` FROM connections c JOIN users u ON u.id = c.owner_id ORDER BY c.kind, c.label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		c, err := scanConn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteConnection removes a connection and its run history.
func (s *DB) DeleteConnection(id int64) error {
	_, err := s.db.Exec(`DELETE FROM connections WHERE id = ?`, id)
	return err
}

// Run is one sync of a connection.
type Run struct {
	ID                          int64
	ConnectionID                int64
	Trigger                     string
	StartedAt, FinishedAt       time.Time
	Created, Updated, Unchanged int
	Commit                      string
	Error                       string
}

// StartRun records the start of a sync.
func (s *DB) StartRun(connectionID int64, trigger string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO runs (connection_id, trigger, started_at) VALUES (?, ?, ?)`, connectionID, trigger, s.stamp())
	if err != nil {
		return 0, err
	}
	// Keep the last 50 runs per connection.
	_, _ = s.db.Exec(`DELETE FROM runs WHERE connection_id = ? AND id NOT IN
		(SELECT id FROM runs WHERE connection_id = ? ORDER BY id DESC LIMIT 50)`, connectionID, connectionID)
	return res.LastInsertId()
}

// FinishRun records how a sync ended.
func (s *DB) FinishRun(r Run) error {
	_, err := s.db.Exec(`UPDATE runs SET finished_at = ?, created = ?, updated = ?, unchanged = ?, commit_sha = ?, error = ? WHERE id = ?`,
		s.stamp(), r.Created, r.Updated, r.Unchanged, r.Commit, r.Error, r.ID)
	return err
}

// Runs returns a connection's most recent runs, newest first.
func (s *DB) Runs(connectionID int64, limit int) ([]Run, error) {
	rows, err := s.db.Query(`SELECT id, connection_id, trigger, started_at, finished_at, created, updated, unchanged, commit_sha, error
		FROM runs WHERE connection_id = ? ORDER BY id DESC LIMIT ?`, connectionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var started, finished sql.NullString
		if err := rows.Scan(&r.ID, &r.ConnectionID, &r.Trigger, &started, &finished, &r.Created, &r.Updated, &r.Unchanged, &r.Commit, &r.Error); err != nil {
			return nil, err
		}
		r.StartedAt, r.FinishedAt = parseTime(started), parseTime(finished)
		out = append(out, r)
	}
	return out, rows.Err()
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}
