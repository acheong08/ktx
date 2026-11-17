package state

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// OAuthClient is an app that registered itself to use ktx over OAuth
// (dynamic client registration). Clients are public: they have no secret
// and must use PKCE.
type OAuthClient struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// CreateOAuthClient registers an app. Clients that never got a grant are
// removed after a day, so anonymous registrations can't pile up.
func (s *DB) CreateOAuthClient(name string, redirectURIs []string) (OAuthClient, error) {
	id, _ := newToken("ktxapp_")
	uris, err := json.Marshal(redirectURIs)
	if err != nil {
		return OAuthClient{}, err
	}
	now := s.now().UTC()
	_, _ = s.db.Exec(`DELETE FROM oauth_clients WHERE created_at < ?
		AND NOT EXISTS (SELECT 1 FROM oauth_grants g WHERE g.client_id = oauth_clients.id)`,
		now.Add(-24*time.Hour).Format(time.RFC3339))
	if _, err := s.db.Exec(`INSERT INTO oauth_clients (id, name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		id, name, string(uris), now.Format(time.RFC3339)); err != nil {
		return OAuthClient{}, err
	}
	return OAuthClient{ID: id, Name: name, RedirectURIs: redirectURIs, CreatedAt: now}, nil
}

// OAuthClientByID returns a registered app.
func (s *DB) OAuthClientByID(id string) (OAuthClient, error) {
	var c OAuthClient
	var uris string
	var created sql.NullString
	err := s.db.QueryRow(`SELECT id, name, redirect_uris, created_at FROM oauth_clients WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &uris, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return OAuthClient{}, ErrNotFound
	}
	if err != nil {
		return OAuthClient{}, err
	}
	c.CreatedAt = parseTime(created)
	return c, json.Unmarshal([]byte(uris), &c.RedirectURIs)
}

// OAuthGrant is a person's permission for an app, with the app's current
// access and refresh token (only their hashes are stored).
type OAuthGrant struct {
	ID         int64
	UserID     int64
	ClientID   string
	ClientName string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// OAuthTokens are the tokens an app receives.
type OAuthTokens struct {
	Access, Refresh string
	AccessExpires   time.Time
}

// CreateOAuthGrant records that a user allowed an app and returns its first
// tokens.
func (s *DB) CreateOAuthGrant(userID int64, clientID string, accessTTL, refreshTTL time.Duration) (OAuthTokens, error) {
	now := s.now().UTC()
	_, _ = s.db.Exec(`DELETE FROM oauth_grants WHERE refresh_expires_at < ?`, now.Format(time.RFC3339))
	access, accessHash := newToken("ktxmcp_")
	refresh, refreshHash := newToken("ktxmcpr_")
	t := OAuthTokens{Access: access, Refresh: refresh, AccessExpires: now.Add(accessTTL)}
	_, err := s.db.Exec(`INSERT INTO oauth_grants (user_id, client_id, access_hash, access_expires_at,
		refresh_hash, refresh_expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		userID, clientID, accessHash, t.AccessExpires.Format(time.RFC3339),
		refreshHash, now.Add(refreshTTL).Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return OAuthTokens{}, err
	}
	return t, nil
}

// RefreshOAuthGrant swaps a refresh token for new tokens. The old access
// and refresh tokens stop working. A refresh token that is unknown,
// expired, or belongs to another app is not found.
func (s *DB) RefreshOAuthGrant(refreshToken, clientID string, accessTTL, refreshTTL time.Duration) (OAuthTokens, error) {
	now := s.now().UTC()
	access, accessHash := newToken("ktxmcp_")
	refresh, refreshHash := newToken("ktxmcpr_")
	t := OAuthTokens{Access: access, Refresh: refresh, AccessExpires: now.Add(accessTTL)}
	res, err := s.db.Exec(`UPDATE oauth_grants SET access_hash = ?, access_expires_at = ?,
		refresh_hash = ?, refresh_expires_at = ?
		WHERE refresh_hash = ? AND client_id = ? AND refresh_expires_at > ?`,
		accessHash, t.AccessExpires.Format(time.RFC3339), refreshHash, now.Add(refreshTTL).Format(time.RFC3339),
		hashToken(refreshToken), clientID, now.Format(time.RFC3339))
	if err != nil {
		return OAuthTokens{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return OAuthTokens{}, ErrNotFound
	}
	return t, nil
}

// OAuthAccessUser returns the user an unexpired access token acts for, and
// marks the grant used.
func (s *DB) OAuthAccessUser(accessToken string) (User, OAuthGrant, error) {
	if accessToken == "" {
		return User{}, OAuthGrant{}, ErrNotFound
	}
	hash := hashToken(accessToken)
	var g OAuthGrant
	var created, last sql.NullString
	err := s.db.QueryRow(`SELECT g.id, g.user_id, g.client_id, c.name, g.created_at, g.last_used_at
		FROM oauth_grants g JOIN oauth_clients c ON c.id = g.client_id
		WHERE g.access_hash = ? AND g.access_expires_at > ?`, hash, s.stamp()).
		Scan(&g.ID, &g.UserID, &g.ClientID, &g.ClientName, &created, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, OAuthGrant{}, ErrNotFound
	}
	if err != nil {
		return User{}, OAuthGrant{}, err
	}
	g.CreatedAt, g.LastUsedAt = parseTime(created), parseTime(last)
	u, err := s.UserByID(g.UserID)
	if err != nil {
		return User{}, OAuthGrant{}, err
	}
	// Once a minute is plenty for "last used", and spares a write per call.
	if s.now().Sub(g.LastUsedAt) > time.Minute {
		_, _ = s.db.Exec(`UPDATE oauth_grants SET last_used_at = ? WHERE id = ?`, s.stamp(), g.ID)
	}
	return u, g, nil
}

// OAuthGrants lists the apps a user has allowed, oldest first.
func (s *DB) OAuthGrants(userID int64) ([]OAuthGrant, error) {
	rows, err := s.db.Query(`SELECT g.id, g.user_id, g.client_id, c.name, g.created_at, g.last_used_at
		FROM oauth_grants g JOIN oauth_clients c ON c.id = g.client_id
		WHERE g.user_id = ? AND g.refresh_expires_at > ? ORDER BY g.id`, userID, s.stamp())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OAuthGrant
	for rows.Next() {
		var g OAuthGrant
		var created, last sql.NullString
		if err := rows.Scan(&g.ID, &g.UserID, &g.ClientID, &g.ClientName, &created, &last); err != nil {
			return nil, err
		}
		g.CreatedAt, g.LastUsedAt = parseTime(created), parseTime(last)
		out = append(out, g)
	}
	return out, rows.Err()
}

// DeleteOAuthGrant revokes one of a user's grants; its tokens stop working.
func (s *DB) DeleteOAuthGrant(userID, id int64) error {
	res, err := s.db.Exec(`DELETE FROM oauth_grants WHERE id = ? AND user_id = ?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
