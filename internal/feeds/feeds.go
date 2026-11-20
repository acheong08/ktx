// Package feeds keeps subscribed calendars (iCal links from Luma, Google,
// Outlook…) up to date. The calendar is in git (package calendar marks it
// with "subscription"); its URL, which holds the feed's secret, is sealed
// in the state database; the last fetched copy is a file in a cache
// directory outside git, which package calendar reads. Feeds are fetched
// on a schedule with conditional requests; a failed fetch keeps the last
// good copy.
package feeds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/secrets"
	"github.com/acheong08/ktx/internal/state"
)

const (
	// DefaultRefresh is how often a feed is fetched unless its calendar says.
	DefaultRefresh = 30 * time.Minute
	minRefresh     = 5 * time.Minute
	maxFeedBytes   = 20 << 20
)

// Manager fetches and stores subscriptions.
type Manager struct {
	db     *state.DB
	box    *secrets.Box // nil: no master key, so no URLs can be stored
	cal    *calendar.Service
	dir    string
	log    *slog.Logger
	client *http.Client
	now    func() time.Time
	wake   chan struct{}

	mu   sync.Mutex // one fetch per calendar at a time
	busy map[string]*sync.Mutex
}

// Option changes a Manager, for tests.
type Option func(*Manager)

// AllowPrivateAddresses lets feeds be fetched from private and loopback
// addresses (tests use a local server).
func AllowPrivateAddresses() Option {
	return func(m *Manager) { m.client = newClient(true) }
}

// WithHTTPClient replaces the HTTP client, private-address guard included
// (tests: a local TLS server).
func WithHTTPClient(c *http.Client) Option {
	return func(m *Manager) { m.client = c }
}

// New makes a Manager keeping feeds in dir, and tells package calendar
// where to read them.
func New(db *state.DB, box *secrets.Box, cal *calendar.Service, dir string, log *slog.Logger, opts ...Option) *Manager {
	m := &Manager{db: db, box: box, cal: cal, dir: dir, log: log, client: newClient(false), now: time.Now,
		wake: make(chan struct{}, 1), busy: map[string]*sync.Mutex{}}
	for _, o := range opts {
		o(m)
	}
	calendar.SetFeedDir(dir)
	return m
}

// CanStoreURLs reports whether there's a master key to seal URLs with.
func (m *Manager) CanStoreURLs() bool { return m.box != nil }

// Start fetches feeds that are due, every minute, until ctx ends.
func (m *Manager) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			m.refreshDue(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-m.wake:
			}
		}
	}()
}

func (m *Manager) refreshDue(ctx context.Context) {
	subs, err := m.db.Subscriptions()
	if err != nil {
		m.log.Error("listing subscriptions", "err", err)
		return
	}
	changed := false
	for _, sub := range subs {
		if ctx.Err() != nil {
			return
		}
		c, err := m.cal.Calendar(sub.Owner, sub.Calendar)
		if err != nil || !c.Subscribed() {
			continue // removed (or no longer a subscription) in git
		}
		if !sub.CheckedAt.IsZero() && m.now().Sub(sub.CheckedAt) < refreshOf(c) {
			continue
		}
		ch, err := m.fetch(ctx, c, sub)
		if err != nil {
			m.log.Warn("fetching subscription failed", "calendar", c.Key(), "err", err)
		}
		changed = changed || ch
	}
	if changed {
		m.relink(ctx)
	}
}

func refreshOf(c calendar.Calendar) time.Duration {
	if c.Subscription != nil {
		if d, err := time.ParseDuration(c.Subscription.Refresh); err == nil {
			return max(d, minRefresh)
		}
	}
	return DefaultRefresh
}

// --- adding, refreshing, removing ------------------------------------------

// ErrNoKey means there's no master key to seal the URL with.
var ErrNoKey = errors.New("there's no master key on the server (BRAIN_MASTER_KEY_FILE), so a feed URL can't be stored")

// NormalizeURL checks a feed URL: https, or webcal (read as https).
func NormalizeURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("%w: that isn't a link", calendar.ErrInvalid)
	}
	switch strings.ToLower(u.Scheme) {
	case "webcal", "webcals", "https":
		u.Scheme = "https"
	default:
		return nil, fmt.Errorf("%w: use an https:// or webcal:// link", calendar.ErrInvalid)
	}
	u.Fragment = ""
	return u, nil
}

// Subscribe adds a subscribed calendar owner/id named name, or gives an
// existing subscription a new URL. The feed is fetched first, so a link
// that doesn't work is refused and nothing is created.
func (m *Manager) Subscribe(ctx context.Context, owner, id, name, rawURL, user string) error {
	if m.box == nil {
		return ErrNoKey
	}
	u, err := NormalizeURL(rawURL)
	if err != nil {
		return err
	}
	if !calendar.ValidID(id) {
		return fmt.Errorf("%w: calendar ids are lowercase letters, digits, '-' and '_'", calendar.ErrInvalid)
	}
	c := calendar.Calendar{Owner: owner, ID: id, Name: strings.TrimSpace(name), Subscription: &calendar.Subscription{}}
	if old, err := m.cal.Calendar(owner, id); err == nil {
		if !old.Subscribed() {
			return fmt.Errorf("%w: you already have a calendar called %s", calendar.ErrExists, id)
		}
		c.Subscription = old.Subscription
		if c.Name == "" {
			c.Name = old.Name
		}
	}
	if c.Name == "" {
		c.Name = u.Hostname()
	}
	data, etag, lastMod, err := m.download(ctx, u, state.Subscription{})
	if err != nil {
		return fmt.Errorf("fetching the feed: %w", err)
	}
	parts, _, err := calendar.SplitFeed(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("the link doesn't give a calendar: %w", err)
	}
	if err := m.cal.SetSubscription(ctx, c, user); err != nil {
		return err
	}
	if err := m.db.PutSubscription(state.Subscription{Owner: owner, Calendar: id, URL: m.box.Seal(sealContext(owner, id), []byte(u.String())),
		URLHint: u.Hostname(), CreatedBy: user}); err != nil {
		return err
	}
	if err := m.store(c, data); err != nil {
		return err
	}
	if err := m.db.SubscriptionFetched(owner, id, etag, lastMod, len(parts), ""); err != nil {
		return err
	}
	m.log.Info("subscribed", "calendar", c.Key(), "host", u.Hostname(), "items", len(parts), "user", user)
	m.relink(ctx)
	return nil
}

// Refresh fetches one subscription now. It reports whether the feed
// changed; transcripts are relinked if so.
func (m *Manager) Refresh(ctx context.Context, owner, id string) (bool, error) {
	c, err := m.cal.Calendar(owner, id)
	if err != nil {
		return false, err
	}
	if !c.Subscribed() {
		return false, fmt.Errorf("%w: %s isn't a subscription", calendar.ErrInvalid, c.Key())
	}
	sub, err := m.db.Subscription(owner, id)
	if err != nil {
		return false, err
	}
	changed, err := m.fetch(ctx, c, sub)
	if changed {
		m.relink(ctx)
	}
	return changed, err
}

// Unsubscribe removes a subscribed calendar: from git, its URL, and its
// cached feed. Links from transcripts to its events are re-matched.
func (m *Manager) Unsubscribe(ctx context.Context, owner, id, user string) error {
	c, err := m.cal.Calendar(owner, id)
	if err != nil {
		return err
	}
	if err := m.cal.DeleteCalendar(ctx, c, user); err != nil {
		return err
	}
	if err := m.db.DeleteSubscription(owner, id); err != nil {
		return err
	}
	if p := calendar.FeedPath(c); p != "" {
		_ = os.Remove(p)
	}
	m.log.Info("unsubscribed", "calendar", c.Key(), "user", user)
	m.relink(ctx)
	return nil
}

// Status is a subscription's fetch state; NeedsURL when the calendar is in
// git but its URL isn't stored here (e.g. a new state database).
type Status struct {
	state.Subscription
	NeedsURL bool
}

// Status reports on a subscribed calendar.
func (m *Manager) Status(c calendar.Calendar) Status {
	sub, err := m.db.Subscription(c.Owner, c.ID)
	if err != nil {
		return Status{NeedsURL: true}
	}
	return Status{Subscription: sub}
}

// --- fetching ----------------------------------------------------------------

func sealContext(owner, id string) string { return "subscription:" + owner + "/" + id }

// fetch downloads a feed if it changed, stores it and records the result.
func (m *Manager) fetch(ctx context.Context, c calendar.Calendar, sub state.Subscription) (changed bool, err error) {
	lock := m.lock(c.Key())
	lock.Lock()
	defer lock.Unlock()
	defer func() {
		if err != nil {
			_ = m.db.SubscriptionFetched(c.Owner, c.ID, "", "", -1, err.Error())
		}
	}()
	if m.box == nil {
		return false, ErrNoKey
	}
	raw, err := m.box.Open(sealContext(c.Owner, c.ID), sub.URL)
	if err != nil {
		return false, fmt.Errorf("unsealing the URL: %w", err)
	}
	u, err := NormalizeURL(string(raw))
	if err != nil {
		return false, err
	}
	data, etag, lastMod, err := m.download(ctx, u, sub)
	if err != nil {
		return false, err
	}
	if data == nil { // 304 Not Modified
		return false, m.db.SubscriptionFetched(c.Owner, c.ID, etag, lastMod, -1, "")
	}
	parts, _, err := calendar.SplitFeed(bytes.NewReader(data))
	if err != nil {
		return false, fmt.Errorf("the feed isn't a calendar any more: %w", err)
	}
	old, _ := os.ReadFile(calendar.FeedPath(c))
	if !bytes.Equal(old, data) {
		if err := m.store(c, data); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, m.db.SubscriptionFetched(c.Owner, c.ID, etag, lastMod, len(parts), "")
}

func (m *Manager) lock(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.busy[key]
	if !ok {
		l = &sync.Mutex{}
		m.busy[key] = l
	}
	return l
}

// download GETs a feed, conditionally on sub's validators. data is nil for
// 304 Not Modified.
func (m *Manager) download(ctx context.Context, u *url.URL, sub state.Subscription) (data []byte, etag, lastMod string, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("Accept", "text/calendar, */*;q=0.5")
	req.Header.Set("User-Agent", "ktx-calendar/1 (+https://github.com/acheong08/ktx)")
	if sub.ETag != "" {
		req.Header.Set("If-None-Match", sub.ETag)
	}
	if sub.LastModified != "" {
		req.Header.Set("If-Modified-Since", sub.LastModified)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, "", "", scrub(err, u)
	}
	defer resp.Body.Close()
	etag, lastMod = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return nil, sub.ETag, sub.LastModified, nil
	case resp.StatusCode != http.StatusOK:
		return nil, "", "", fmt.Errorf("the server answered %s", resp.Status)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return nil, "", "", scrub(err, u)
	}
	if len(data) > maxFeedBytes {
		return nil, "", "", fmt.Errorf("the feed is larger than %d MB", maxFeedBytes>>20)
	}
	return data, etag, lastMod, nil
}

// scrub removes the URL (and its secret) from an error message.
func scrub(err error, u *url.URL) error {
	msg := strings.ReplaceAll(err.Error(), u.String(), u.Scheme+"://"+u.Host+"/…")
	return errors.New(msg)
}

// store writes a feed atomically, so readers never see half a file.
func (m *Manager) store(c calendar.Calendar, data []byte) error {
	p := calendar.FeedPath(c)
	if p == "" {
		return errors.New("no feed directory")
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".feed-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}

func (m *Manager) relink(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	defer cancel()
	if n, err := m.cal.RelinkAll(ctx); err != nil {
		m.log.Error("relinking after a feed change", "err", err)
	} else if n > 0 {
		m.log.Info("relinked transcripts after a feed change", "changed", n)
	}
}

// --- a client that only reaches the internet -----------------------------------

// newClient makes an HTTP client that refuses to connect to loopback,
// private, link-local and tailnet (100.64.0.0/10) addresses, so a feed URL
// can't be used to reach services on the box or the tailnet. The check is
// on the address actually dialled, so redirects and DNS tricks are covered.
func newClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if !Public(ap.Addr()) {
				return fmt.Errorf("refusing to fetch from a private address (%s)", ap.Addr())
			}
			return nil
		}
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       90 * time.Second,
	}
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "https" && !allowPrivate {
				return errors.New("the feed redirects to a non-https address")
			}
			return nil
		},
	}
}

var tailnet = netip.MustParsePrefix("100.64.0.0/10")

// Public reports whether an address is on the public internet.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsGlobalUnicast() && !a.IsPrivate() && !tailnet.Contains(a)
}
