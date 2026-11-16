package state

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// User is someone who has signed in through the identity provider.
type User struct {
	ID          int64
	Subject     string // OIDC "sub"
	Username    string
	Name        string
	Email       string
	IsAdmin     bool
	CreatedAt   time.Time
	LastLoginAt time.Time
}

// Display is the name to show and to put in commit trailers.
func (u User) Display() string {
	if u.Username != "" {
		return u.Username
	}
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

const userCols = `id, subject, username, name, email, is_admin, created_at, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created, last sql.NullString
	if err := row.Scan(&u.ID, &u.Subject, &u.Username, &u.Name, &u.Email, &u.IsAdmin, &created, &last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	u.CreatedAt, u.LastLoginAt = parseTime(created), parseTime(last)
	return u, nil
}

// Login records a sign-in: creates the user on first login and refreshes
// their profile and admin flag from the identity provider every time.
func (s *DB) Login(subject, username, name, email string, isAdmin bool) (User, error) {
	now := s.stamp()
	_, err := s.db.Exec(`
		INSERT INTO users (subject, username, name, email, is_admin, created_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(subject) DO UPDATE SET
			username = excluded.username, name = excluded.name, email = excluded.email,
			is_admin = excluded.is_admin, last_login_at = excluded.last_login_at`,
		subject, username, name, email, isAdmin, now, now)
	if err != nil {
		return User{}, err
	}
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE subject = ?`, subject))
}

// UserByID returns one user.
func (s *DB) UserByID(id int64) (User, error) {
	return scanUser(s.db.QueryRow(`SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// Users lists everyone who has signed in, by username.
func (s *DB) Users() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// CreateSession starts a browser session and returns its token.
func (s *DB) CreateSession(userID int64, ttl time.Duration) (string, error) {
	token, hash := newToken("")
	now := s.now().UTC()
	_, err := s.db.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		hash, userID, now.Format(time.RFC3339), now.Add(ttl).Format(time.RFC3339))
	if err != nil {
		return "", err
	}
	// Opportunistic cleanup.
	_, _ = s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Format(time.RFC3339))
	return token, nil
}

// SessionUser returns the user of a live session.
func (s *DB) SessionUser(token string) (User, error) {
	if token == "" {
		return User{}, ErrNotFound
	}
	return scanUser(s.db.QueryRow(`
		SELECT `+prefixed("u.", userCols)+` FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND s.expires_at > ?`, hashToken(token), s.stamp()))
}

// DeleteSession ends a session.
func (s *DB) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ?`, hashToken(token))
	return err
}

// APIToken is a token for scripts and connectors calling the API.
type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// CreateAPIToken makes a token for a user. The token itself is returned
// once; only its hash is stored.
func (s *DB) CreateAPIToken(userID int64, name string) (string, error) {
	token, hash := newToken("ktx_")
	_, err := s.db.Exec(`INSERT INTO api_tokens (user_id, name, token_hash, created_at) VALUES (?, ?, ?, ?)`,
		userID, name, hash, s.stamp())
	return token, err
}

// APITokens lists a user's tokens.
func (s *DB) APITokens(userID int64) ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT id, user_id, name, created_at, last_used_at FROM api_tokens WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var created, last sql.NullString
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &created, &last); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsedAt = parseTime(created), parseTime(last)
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteAPIToken revokes one of a user's tokens.
func (s *DB) DeleteAPIToken(userID, id int64) error {
	res, err := s.db.Exec(`DELETE FROM api_tokens WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// APITokenUser returns the owner of a token and marks it used.
func (s *DB) APITokenUser(token string) (User, error) {
	hash := hashToken(token)
	u, err := scanUser(s.db.QueryRow(`
		SELECT `+prefixed("u.", userCols)+` FROM api_tokens t JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = ?`, hash))
	if err != nil {
		return User{}, err
	}
	_, _ = s.db.Exec(`UPDATE api_tokens SET last_used_at = ? WHERE token_hash = ?`, s.stamp(), hash)
	return u, nil
}

// prefixed turns "a, b" into "p.a, p.b".
func prefixed(p, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = p + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
