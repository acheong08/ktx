// Package connectors pulls documents from outside services (Granola, ...)
// into the store on a schedule.
//
// A Connector only knows its service: which credential fields it needs, how
// to check them, and how to turn "what changed since this bookmark" into
// documents. The Manager does the rest: keeps credentials sealed in the state
// database, runs each connection on its own schedule (or on demand), writes
// each run as one commit, and saves the bookmark only once that commit
// exists. Re-fetching is harmless, because the store overwrites documents by
// id and skips unchanged ones.
package connectors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acheong08/ktx/internal/secrets"
	"github.com/acheong08/ktx/internal/state"
	"github.com/acheong08/ktx/internal/store"
)

// Field is one input on the connection form.
type Field struct {
	Name        string
	Label       string
	Help        string
	Placeholder string
	Secret      bool // stored sealed, never shown again
	Required    bool
}

// Config is what a connector gets to work with.
type Config struct {
	Secrets  map[string]string
	Settings map[string]string
	Owner    Person // whose connection it is (empty in Check)
}

// Person is someone ktx knows, e.g. a connection's owner.
type Person struct{ Name, Email string }

// Connector talks to one outside service.
type Connector interface {
	Kind() string // stable id, e.g. "granola"
	Title() string
	Description() string
	Fields() []Field
	Interval() time.Duration
	// Check tests the credential and returns whose account it is.
	Check(ctx context.Context, cfg Config) (account string, err error)
	// Sync emits every document changed since cursor and returns the next
	// cursor. Emitting something already stored is fine.
	Sync(ctx context.Context, cfg Config, cursor string, emit func(store.Item) error) (next string, err error)
}

// FormError lists problems with submitted connection values.
type FormError struct{ Problems []string }

func (e *FormError) Error() string { return strings.Join(e.Problems, "; ") }

// Manager runs connections.
type Manager struct {
	db    *state.DB
	box   *secrets.Box // nil when no master key is configured
	store *store.Store
	log   *slog.Logger
	kinds []Connector

	mu      sync.Mutex
	ctx     context.Context
	workers map[int64]*worker
}

type worker struct {
	trigger chan string
	cancel  context.CancelFunc
	done    chan struct{}
	syncing bool
}

// batchSize bounds how many documents go into one commit.
const batchSize = 50

// NewManager makes a Manager for the given connector kinds. box may be nil:
// connections can then be viewed but not created or run.
func NewManager(db *state.DB, box *secrets.Box, st *store.Store, log *slog.Logger, kinds ...Connector) *Manager {
	return &Manager{db: db, box: box, store: st, log: log, kinds: kinds, workers: map[int64]*worker{}}
}

// Kinds lists the available connectors.
func (m *Manager) Kinds() []Connector { return m.kinds }

// Kind returns one connector by kind.
func (m *Manager) Kind(kind string) (Connector, bool) {
	for _, k := range m.kinds {
		if k.Kind() == kind {
			return k, true
		}
	}
	return nil, false
}

// CanStoreSecrets reports whether a master key is configured.
func (m *Manager) CanStoreSecrets() bool { return m.box != nil }

// Start runs every enabled connection until ctx ends.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	return m.Reconcile()
}

// Reconcile starts workers for enabled connections and stops the rest.
// Call it after connections change.
func (m *Manager) Reconcile() error {
	conns, err := m.db.Connections()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil {
		return nil // not started
	}
	want := map[int64]bool{}
	for i, c := range conns {
		kind, ok := m.Kind(c.Kind)
		if !c.Enabled || !ok || m.box == nil {
			continue
		}
		want[c.ID] = true
		if _, running := m.workers[c.ID]; running {
			continue
		}
		ctx, cancel := context.WithCancel(m.ctx)
		w := &worker{trigger: make(chan string, 1), cancel: cancel, done: make(chan struct{})}
		m.workers[c.ID] = w
		go m.loop(ctx, c.ID, kind, w, time.Duration(i)*3*time.Second)
	}
	for id, w := range m.workers {
		if !want[id] {
			w.cancel()
			delete(m.workers, id)
		}
	}
	return nil
}

// SyncNow asks a connection to sync as soon as it is free.
func (m *Manager) SyncNow(id int64) bool {
	m.mu.Lock()
	w, ok := m.workers[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case w.trigger <- "manual":
	default: // one is already queued
	}
	return true
}

// Syncing reports whether a connection is syncing right now.
func (m *Manager) Syncing(id int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.workers[id]
	return ok && w.syncing
}

func (m *Manager) loop(ctx context.Context, id int64, kind Connector, w *worker, delay time.Duration) {
	defer close(w.done)
	trigger := "schedule"
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			trigger = "schedule"
		case trigger = <-w.trigger:
		}
		m.setSyncing(w, true)
		m.RunOnce(ctx, id, kind, trigger)
		m.setSyncing(w, false)
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(kind.Interval())
	}
}

func (m *Manager) setSyncing(w *worker, v bool) {
	m.mu.Lock()
	w.syncing = v
	m.mu.Unlock()
}

// RunOnce syncs one connection now and records the run.
func (m *Manager) RunOnce(ctx context.Context, id int64, kind Connector, trigger string) state.Run {
	run := state.Run{ConnectionID: id, Trigger: trigger}
	conn, err := m.db.Connection(id)
	if err != nil {
		m.log.Error("connector: loading connection", "id", id, "err", err)
		return run
	}
	run.ID, err = m.db.StartRun(id, trigger)
	if err != nil {
		m.log.Error("connector: recording run", "id", id, "err", err)
		return run
	}
	defer func() {
		if err := m.db.FinishRun(run); err != nil {
			m.log.Error("connector: recording run", "id", id, "err", err)
		}
	}()

	cfg, err := m.config(conn)
	if err != nil {
		run.Error = err.Error()
		return run
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	var batch []store.Item
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		results, git, err := m.store.PutBatch(ctx, batch, conn.Owner,
			"Sync "+conn.Kind+" ("+conn.Label+")", "Connection: "+conn.Kind+"/"+strconv.FormatInt(conn.ID, 10))
		batch = batch[:0]
		for _, r := range results {
			switch r.Result {
			case store.ResultCreated:
				run.Created++
			case store.ResultUpdated:
				run.Updated++
			case store.ResultUnchanged, store.ResultSkipped:
				run.Unchanged++
			}
		}
		if git.Commit != "" {
			run.Commit = git.Commit
		}
		return err
	}
	emit := func(it store.Item) error {
		it.Sync = true
		batch = append(batch, it)
		if len(batch) >= batchSize {
			return flush()
		}
		return nil
	}

	next, syncErr := kind.Sync(ctx, cfg, conn.Cursor, emit)
	flushErr := flush()
	if err := errors.Join(syncErr, flushErr); err != nil {
		run.Error = err.Error()
		m.log.Warn("connector: sync failed", "connection", conn.Label, "kind", conn.Kind, "err", err)
		return run // bookmark unchanged: the next run fetches the same changes again
	}
	if next != conn.Cursor {
		if err := m.db.SetCursor(conn.ID, next); err != nil {
			run.Error = "saving bookmark: " + err.Error()
		}
	}
	if run.Created+run.Updated > 0 {
		m.log.Info("connector: synced", "connection", conn.Label, "kind", conn.Kind,
			"created", run.Created, "updated", run.Updated, "commit", run.Commit)
	}
	return run
}

func (m *Manager) config(conn state.Connection) (Config, error) {
	cfg := Config{Settings: conn.Settings, Secrets: map[string]string{}}
	if u, err := m.db.UserByID(conn.OwnerID); err == nil {
		cfg.Owner = Person{Name: u.Name, Email: u.Email}
		if cfg.Owner.Name == "" {
			cfg.Owner.Name = u.Display()
		}
	}
	if len(conn.Secrets) == 0 {
		return cfg, nil
	}
	if m.box == nil {
		return cfg, secrets.ErrNoKey
	}
	plain, err := m.box.Open(sealContext(conn.ID), conn.Secrets)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(plain, &cfg.Secrets); err != nil {
		return cfg, fmt.Errorf("reading credential: %w", err)
	}
	return cfg, nil
}

func sealContext(id int64) string { return "connection:" + strconv.FormatInt(id, 10) }

// Create checks the values (including the credential, against the
// service) and stores a new enabled connection owned by owner.
func (m *Manager) Create(ctx context.Context, owner state.User, kind, label string, values map[string]string) (int64, error) {
	k, ok := m.Kind(kind)
	if !ok {
		return 0, &FormError{[]string{"unknown connector " + kind}}
	}
	if m.box == nil {
		return 0, secrets.ErrNoKey
	}
	cfg, problems := split(k, values, nil)
	if strings.TrimSpace(label) == "" {
		problems = append(problems, "Name is required")
	}
	if len(problems) > 0 {
		return 0, &FormError{problems}
	}
	account, err := k.Check(ctx, cfg)
	if err != nil {
		return 0, &FormError{[]string{k.Title() + " rejected the credential: " + err.Error()}}
	}
	id, err := m.db.CreateConnection(state.Connection{
		Kind: kind, Label: strings.TrimSpace(label), OwnerID: owner.ID, Settings: cfg.Settings,
		Account: account, Enabled: true, SecretHint: hint(k, cfg.Secrets),
	})
	if err != nil {
		return 0, err
	}
	conn, err := m.db.Connection(id)
	if err != nil {
		return 0, err
	}
	if err := m.saveSecrets(&conn, cfg.Secrets); err != nil {
		m.db.DeleteConnection(id)
		return 0, err
	}
	return id, m.Reconcile()
}

// Update changes a connection. Empty secret fields keep the stored value; a
// new credential is checked against the service before it is saved.
func (m *Manager) Update(ctx context.Context, id int64, label string, enabled bool, values map[string]string) error {
	conn, err := m.db.Connection(id)
	if err != nil {
		return err
	}
	k, ok := m.Kind(conn.Kind)
	if !ok {
		return &FormError{[]string{"unknown connector " + conn.Kind}}
	}
	old, err := m.config(conn)
	if err != nil {
		return err
	}
	cfg, problems := split(k, values, old.Secrets)
	if strings.TrimSpace(label) == "" {
		problems = append(problems, "Name is required")
	}
	if len(problems) > 0 {
		return &FormError{problems}
	}
	if !sameMap(cfg.Secrets, old.Secrets) || !sameMap(cfg.Settings, old.Settings) {
		account, err := k.Check(ctx, cfg)
		if err != nil {
			return &FormError{[]string{k.Title() + " rejected the credential: " + err.Error()}}
		}
		conn.Account = account
	}
	conn.Label, conn.Enabled, conn.Settings = strings.TrimSpace(label), enabled, cfg.Settings
	conn.SecretHint = hint(k, cfg.Secrets)
	if err := m.saveSecrets(&conn, cfg.Secrets); err != nil {
		return err
	}
	return m.Reconcile()
}

// Delete removes a connection (documents it stored stay in git).
func (m *Manager) Delete(id int64) error {
	if err := m.db.DeleteConnection(id); err != nil {
		return err
	}
	return m.Reconcile()
}

func (m *Manager) saveSecrets(conn *state.Connection, s map[string]string) error {
	plain, _ := json.Marshal(s)
	conn.Secrets = m.box.Seal(sealContext(conn.ID), plain)
	return m.db.UpdateConnection(*conn)
}

// split sorts form values into secrets and settings and checks required
// fields. keep supplies stored secrets for secret fields left empty.
func split(k Connector, values, keep map[string]string) (Config, []string) {
	cfg := Config{Secrets: map[string]string{}, Settings: map[string]string{}}
	var problems []string
	for _, f := range k.Fields() {
		v := strings.TrimSpace(values[f.Name])
		if f.Secret && v == "" {
			v = keep[f.Name]
		}
		if f.Required && v == "" {
			problems = append(problems, f.Label+" is required")
		}
		if v == "" {
			continue
		}
		if f.Secret {
			cfg.Secrets[f.Name] = v
		} else {
			cfg.Settings[f.Name] = v
		}
	}
	return cfg, problems
}

// hint shows the end of the first secret, to recognise it without
// revealing it.
func hint(k Connector, s map[string]string) string {
	for _, f := range k.Fields() {
		if v := s[f.Name]; f.Secret && v != "" {
			if len(v) <= 8 {
				return "set"
			}
			return "…" + v[len(v)-4:]
		}
	}
	return ""
}

func sameMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
