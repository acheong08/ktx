// Package granolaacct is the connector for a personal Granola account,
// without a Business plan: you sign in to Granola from ktx (an OAuth device
// flow, approved in a browser), and ktx reads your notes and transcripts
// through the internal API the Granola desktop app uses.
//
// This API is unofficial and undocumented: it can change or stop working
// whenever Granola updates its app, and it expects a desktop-app User-Agent.
// Ported from pp-granola (Apache-2.0) by way of preambient's granola_sync.mjs.
//
// Granola's refresh tokens are single use: each refresh returns a new one
// and spends the old. The new one is saved (Config.SaveSecrets) before the
// access token it came with is used, so a crash can't strand the session.
package granolaacct

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

const (
	// DefaultType is where new notes are filed.
	DefaultType = "meetings"
	// DefaultAPI is Granola's internal API.
	DefaultAPI = "https://api.granola.ai"
	// DefaultAuth is Granola's sign-in service (WorkOS).
	DefaultAuth = "https://auth.granola.ai"
	// ClientID is Granola's public device-sign-in client (not a secret).
	ClientID = "client_01JZJ0XBDAT8PHJWQY09Y0VD61"
	// DefaultClientVersion is the desktop app version ktx claims to be; the
	// API refuses clients it doesn't recognise.
	DefaultClientVersion = "7.299.0"

	pageSize = 100
	maxPages = 200
	overlap  = 10 * time.Minute
	// settle: a note without a summary that changed this recently is
	// probably still being recorded; it's picked up on a later run.
	settle = 2 * time.Hour
)

// Connector reads a personal Granola account.
type Connector struct {
	API, Auth   string // DefaultAPI, DefaultAuth when empty
	HTTP        *http.Client
	MinInterval time.Duration // between API requests
	Now         func() time.Time

	mu   sync.Mutex
	last time.Time
}

// New returns a connector for the real service.
func New() *Connector {
	return &Connector{HTTP: &http.Client{Timeout: 60 * time.Second}, MinInterval: 550 * time.Millisecond}
}

func (c *Connector) Kind() string  { return "granola_account" }
func (c *Connector) Title() string { return "Granola (personal account)" }
func (c *Connector) Description() string {
	return "Your own Granola notes and transcripts, without a Business plan: sign in to Granola, and ktx reads them " +
		"the way the desktop app does. Unofficial: it may break when Granola changes its app."
}
func (c *Connector) Interval() time.Duration { return 15 * time.Minute }

func (c *Connector) Fields() []connectors.Field {
	return []connectors.Field{
		{Name: "type", Label: "Type for new notes", Placeholder: DefaultType,
			Help: "The transcripts/<type>/ folder new notes go to. Notes already stored stay where they are. Optional; default meetings."},
		{Name: "timezone", Label: "Time zone", Placeholder: "America/Los_Angeles",
			Help: "For notes without a calendar event (Granola reports UTC). Optional; the event's own zone is used when there is one."},
		{Name: "client_version", Label: "Granola app version", Placeholder: DefaultClientVersion,
			Help: "Only change this if syncs fail with “Unsupported client”: set it to the version of a current Granola desktop app."},
	}
}

func (c *Connector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func newType(cfg connectors.Config) (string, error) {
	if t := cfg.Settings["type"]; t != "" {
		return transcript.NormalizeType(t)
	}
	return DefaultType, nil
}

// Check checks the settings (the session comes from signing in).
func (c *Connector) Check(_ context.Context, cfg connectors.Config) (string, error) {
	if _, err := newType(cfg); err != nil {
		return "", err
	}
	if tz := cfg.Settings["timezone"]; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return "", fmt.Errorf("time zone %q is not an IANA name like America/Los_Angeles", tz)
		}
	}
	if v := cfg.Settings["client_version"]; v != "" && strings.ContainsAny(v, " ()\r\n") {
		return "", fmt.Errorf("the app version looks like 7.299.0")
	}
	return "settings ok", nil
}

// --- sign-in (OAuth 2.0 device flow, RFC 8628) ------------------------------------

func (c *Connector) authURL(p string) string {
	base := c.Auth
	if base == "" {
		base = DefaultAuth
	}
	return base + p
}

// StartSignIn asks Granola for a device code.
func (c *Connector) StartSignIn(ctx context.Context) (connectors.Prompt, error) {
	var dc struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	status, body, err := c.postForm(ctx, c.authURL("/user_management/authorize/device"), url.Values{"client_id": {ClientID}})
	if err != nil {
		return connectors.Prompt{}, err
	}
	if status != http.StatusOK {
		return connectors.Prompt{}, fmt.Errorf("starting the sign-in: HTTP %d: %s", status, short(body))
	}
	if err := json.Unmarshal(body, &dc); err != nil || dc.DeviceCode == "" || dc.UserCode == "" {
		return connectors.Prompt{}, fmt.Errorf("starting the sign-in: unexpected answer: %s", short(body))
	}
	p := connectors.Prompt{URL: dc.VerificationURIComplete, Code: dc.UserCode, State: dc.DeviceCode,
		Expires: c.now().Add(time.Duration(max(dc.ExpiresIn, 60)) * time.Second), Interval: time.Duration(max(dc.Interval, 5)) * time.Second}
	if p.URL == "" {
		p.URL = dc.VerificationURI
	}
	return p, nil
}

type tokenAnswer struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	User         struct {
		Email     string `json:"email"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	} `json:"user"`
	Error string `json:"error"`
}

// PollSignIn checks whether the code was approved.
func (c *Connector) PollSignIn(ctx context.Context, p connectors.Prompt) (map[string]string, string, error) {
	status, body, err := c.postForm(ctx, c.authURL("/user_management/authenticate"), url.Values{
		"client_id": {ClientID}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {p.State}})
	if err != nil {
		return nil, "", err
	}
	var tr tokenAnswer
	_ = json.Unmarshal(body, &tr)
	if status != http.StatusOK {
		switch tr.Error {
		case "authorization_pending", "slow_down":
			return nil, "", connectors.ErrPending
		case "expired_token":
			return nil, "", errors.New("the code expired before it was approved: start again")
		case "access_denied":
			return nil, "", errors.New("the sign-in was declined")
		}
		return nil, "", fmt.Errorf("finishing the sign-in: HTTP %d: %s", status, short(body))
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" {
		return nil, "", errors.New("finishing the sign-in: Granola returned no session")
	}
	name := strings.TrimSpace(tr.User.FirstName + " " + tr.User.LastName)
	sec := c.session(tr, "")
	sec["name"] = name
	account := tr.User.Email
	if account == "" {
		account = "signed in"
	}
	return sec, account, nil
}

// session turns a token answer into the secrets kept for the connection.
func (c *Connector) session(tr tokenAnswer, oldRefresh string) map[string]string {
	refresh := tr.RefreshToken
	if refresh == "" { // Granola sometimes keeps the same one
		refresh = oldRefresh
	}
	sec := map[string]string{"access_token": tr.AccessToken, "refresh_token": refresh}
	if exp := expiry(tr.AccessToken, tr.ExpiresIn, c.now()); !exp.IsZero() {
		sec["expires_at"] = exp.UTC().Format(time.RFC3339)
	}
	return sec
}

// expiry is when an access token expires: from expires_in, else the JWT's
// exp claim.
func expiry(token string, expiresIn int, now time.Time) time.Time {
	if expiresIn > 0 {
		return now.Add(time.Duration(expiresIn) * time.Second)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

// --- the API ----------------------------------------------------------------------

// client is one sync's view of the session.
type client struct {
	c   *Connector
	cfg connectors.Config
}

// refresh swaps the refresh token for a new session and saves it before
// anything uses it (the old refresh token is spent).
func (k *client) refresh(ctx context.Context) error {
	old := k.cfg.Secrets["refresh_token"]
	if old == "" {
		return connectors.ErrSignInAgain
	}
	b, _ := json.Marshal(map[string]string{"refresh_token": old})
	status, body, err := k.c.do(ctx, k.apiURL("/v1/refresh-access-token"), b, "", k.version())
	if err != nil {
		return err
	}
	var tr tokenAnswer
	_ = json.Unmarshal(body, &tr)
	if status == http.StatusBadRequest || status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("%w (Granola said: %s)", connectors.ErrSignInAgain, short(body))
	}
	if status != http.StatusOK || tr.AccessToken == "" {
		return fmt.Errorf("refreshing the session: HTTP %d: %s", status, short(body))
	}
	sec := k.c.session(tr, old)
	sec["name"] = k.cfg.Secrets["name"]
	if k.cfg.SaveSecrets == nil {
		return errors.New("refreshing the session: nowhere to save it")
	}
	if err := k.cfg.SaveSecrets(sec); err != nil {
		return fmt.Errorf("saving the refreshed session: %w", err)
	}
	k.cfg.Secrets = sec
	return nil
}

func (k *client) apiURL(p string) string {
	base := k.c.API
	if base == "" {
		base = DefaultAPI
	}
	return base + p
}

func (k *client) version() string {
	if v := k.cfg.Settings["client_version"]; v != "" {
		return v
	}
	return DefaultClientVersion
}

// post calls the API, refreshing the session first when it's about to
// expire and once more on a 401. A 404 gives found=false.
func (k *client) post(ctx context.Context, p string, in, out any) (found bool, err error) {
	if exp, _ := time.Parse(time.RFC3339, k.cfg.Secrets["expires_at"]); k.cfg.Secrets["access_token"] == "" || (!exp.IsZero() && k.c.now().After(exp.Add(-2*time.Minute))) {
		if err := k.refresh(ctx); err != nil {
			return false, err
		}
	}
	b, _ := json.Marshal(in)
	for attempt := 0; ; attempt++ {
		k.c.pace()
		status, body, err := k.c.do(ctx, k.apiURL(p), b, k.cfg.Secrets["access_token"], k.version())
		if err != nil {
			return false, err
		}
		switch {
		case status == http.StatusUnauthorized && attempt == 0:
			if err := k.refresh(ctx); err != nil {
				return false, err
			}
			continue
		case status == http.StatusTooManyRequests && attempt < 5, status >= 500 && attempt < 2:
			wait := time.Duration(attempt+1) * 2 * time.Second
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(wait):
			}
			continue
		case status == http.StatusNotFound:
			return false, nil
		case status != http.StatusOK:
			return false, fmt.Errorf("%s: HTTP %d: %s", p, status, short(body))
		}
		if out != nil && len(bytes.TrimSpace(body)) > 0 {
			if err := json.Unmarshal(body, out); err != nil {
				return false, fmt.Errorf("%s: unexpected answer: %w", p, err)
			}
		}
		return true, nil
	}
}

func (c *Connector) do(ctx context.Context, u string, body []byte, token, version string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// The API refuses User-Agents it doesn't recognise.
	req.Header.Set("User-Agent", "Granola/"+version+" (macOS; ktx)")
	req.Header.Set("X-Client-Version", version)
	req.Header.Set("X-Granola-Platform", "darwin")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.send(req)
}

func (c *Connector) postForm(ctx context.Context, u string, v url.Values) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(v.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.send(req)
}

func (c *Connector) send(req *http.Request) (int, []byte, error) {
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return resp.StatusCode, body, err
}

func (c *Connector) pace() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.MinInterval - time.Since(c.last); wait > 0 {
		time.Sleep(wait)
	}
	c.last = time.Now()
}

// short is the start of a response body, for errors (bodies here never
// hold tokens: errors are JSON messages).
func short(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// --- sync -------------------------------------------------------------------------

// Sync emits every note updated since cursor (RFC 3339) and returns the
// newest update time it can vouch for.
func (c *Connector) Sync(ctx context.Context, cfg connectors.Config, cursor string, emit func(store.Item) error) (string, error) {
	typ, err := newType(cfg)
	if err != nil {
		return cursor, err
	}
	loc := time.UTC
	if tz := cfg.Settings["timezone"]; tz != "" {
		if loc, err = time.LoadLocation(tz); err != nil {
			return cursor, err
		}
	}
	k := &client{c: c, cfg: cfg}
	since, _ := time.Parse(time.RFC3339, cursor)
	from := time.Time{}
	if !since.IsZero() {
		from = since.Add(-overlap)
	}

	// Documents come newest-updated first; stop paging past the bookmark.
	var docs []document
	for page := 0; page < maxPages; page++ {
		var raw json.RawMessage
		if _, err := k.post(ctx, "/v2/get-documents", map[string]any{"limit": pageSize, "offset": page * pageSize, "include_last_viewed_panel": true}, &raw); err != nil {
			return cursor, fmt.Errorf("listing notes: %w", err)
		}
		batch, err := decodeDocs(raw)
		if err != nil {
			return cursor, fmt.Errorf("listing notes: %w", err)
		}
		older := 0
		for _, d := range batch {
			if !from.IsZero() && d.UpdatedAt.Before(from) {
				older++
				continue
			}
			docs = append(docs, d)
		}
		if len(batch) < pageSize || (len(batch) > 0 && older == len(batch)) {
			break
		}
	}

	newest := since
	var hold time.Time // the oldest note left for later
	owner := cfg.Secrets["name"]
	if owner == "" {
		owner = cfg.Owner.Name
	}
	for _, d := range docs {
		if d.DeletedAt != nil || d.WasTrashed {
			continue
		}
		summary := d.summary()
		if summary == "" && c.now().Sub(d.UpdatedAt) < settle {
			if hold.IsZero() || d.UpdatedAt.Before(hold) {
				hold = d.UpdatedAt
			}
			continue // probably still recording; its summary will update it
		}
		var segs []segment
		if _, err := k.post(ctx, "/v1/get-document-transcript", map[string]string{"document_id": d.ID}, &segs); err != nil {
			return cursor, fmt.Errorf("transcript of %q: %w", d.Title, err)
		}
		doc, ok, err := toDoc(d, segs, summary, owner, cfg.Owner, loc)
		if err != nil {
			return cursor, fmt.Errorf("note %q: %w", d.Title, err)
		}
		if ok {
			if err := emit(store.Item{Doc: doc, DefaultType: typ}); err != nil {
				return cursor, err
			}
		}
		if d.UpdatedAt.After(newest) {
			newest = d.UpdatedAt
		}
	}
	if !hold.IsZero() && (newest.IsZero() || !hold.After(newest)) {
		newest = hold.Add(-time.Second) // come back for it
	}
	if newest.IsZero() {
		return cursor, nil
	}
	return newest.UTC().Format(time.RFC3339), nil
}

func decodeDocs(raw json.RawMessage) ([]document, error) {
	var docs []document
	if err := json.Unmarshal(raw, &docs); err == nil {
		return docs, nil
	}
	var env struct {
		Docs []document `json:"docs"`
	}
	err := json.Unmarshal(raw, &env)
	return env.Docs, err
}
