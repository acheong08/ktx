package state

import (
	"database/sql"
	"errors"
	"time"
)

// Subscription is an iCal feed a calendar shows: its sealed URL and how the
// last fetch went.
type Subscription struct {
	Owner, Calendar string
	URL             []byte // sealed
	URLHint         string
	ETag            string
	LastModified    string
	FetchedAt       time.Time // last successful fetch
	CheckedAt       time.Time // last attempt
	Error           string
	Items           int
	CreatedBy       string
}

const subCols = `owner, calendar, url, url_hint, etag, last_modified, fetched_at, checked_at, error, items, created_by`

func scanSub(row interface{ Scan(...any) error }) (Subscription, error) {
	var s Subscription
	var fetched, checked sql.NullString
	err := row.Scan(&s.Owner, &s.Calendar, &s.URL, &s.URLHint, &s.ETag, &s.LastModified, &fetched, &checked, &s.Error, &s.Items, &s.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Subscription{}, ErrNotFound
	}
	s.FetchedAt, s.CheckedAt = parseTime(fetched), parseTime(checked)
	return s, err
}

// PutSubscription stores a subscription's URL, resetting its fetch state.
func (s *DB) PutSubscription(sub Subscription) error {
	_, err := s.db.Exec(`INSERT INTO subscriptions (owner, calendar, url, url_hint, created_by) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(owner, calendar) DO UPDATE SET url = excluded.url, url_hint = excluded.url_hint,
			etag = '', last_modified = '', error = ''`,
		sub.Owner, sub.Calendar, sub.URL, sub.URLHint, sub.CreatedBy)
	return err
}

// Subscription returns one subscription.
func (s *DB) Subscription(owner, calendar string) (Subscription, error) {
	return scanSub(s.db.QueryRow(`SELECT `+subCols+` FROM subscriptions WHERE owner = ? AND calendar = ?`, owner, calendar))
}

// Subscriptions lists every subscription.
func (s *DB) Subscriptions() ([]Subscription, error) {
	rows, err := s.db.Query(`SELECT ` + subCols + ` FROM subscriptions ORDER BY owner, calendar`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// SubscriptionFetched records a fetch: ok with its validators and item
// count, or the error (keeping the last good state).
func (s *DB) SubscriptionFetched(owner, calendar, etag, lastModified string, items int, fetchErr string) error {
	now := s.stamp()
	if fetchErr != "" {
		_, err := s.db.Exec(`UPDATE subscriptions SET checked_at = ?, error = ? WHERE owner = ? AND calendar = ?`,
			now, fetchErr, owner, calendar)
		return err
	}
	_, err := s.db.Exec(`UPDATE subscriptions SET checked_at = ?, fetched_at = ?, error = '', etag = ?, last_modified = ?,
		items = CASE WHEN ? >= 0 THEN ? ELSE items END WHERE owner = ? AND calendar = ?`,
		now, now, etag, lastModified, items, items, owner, calendar)
	return err
}

// DeleteSubscription forgets a subscription.
func (s *DB) DeleteSubscription(owner, calendar string) error {
	_, err := s.db.Exec(`DELETE FROM subscriptions WHERE owner = ? AND calendar = ?`, owner, calendar)
	return err
}
