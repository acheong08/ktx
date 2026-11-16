package connectors

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/secrets"
	"github.com/acheong08/ktx/internal/state"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

// fake emits n meetings with ids after the cursor, or fails when told to.
type fake struct {
	n       int
	fail    error
	checked map[string]string
}

func (f *fake) Kind() string            { return "fake" }
func (f *fake) Title() string           { return "Fake" }
func (f *fake) Description() string     { return "test" }
func (f *fake) Interval() time.Duration { return time.Hour }
func (f *fake) Fields() []Field {
	return []Field{{Name: "api_key", Label: "API key", Secret: true, Required: true}, {Name: "timezone", Label: "Timezone"}}
}
func (f *fake) Check(_ context.Context, cfg Config) (string, error) {
	f.checked = cfg.Secrets
	if cfg.Secrets["api_key"] == "bad" {
		return "", errors.New("invalid key")
	}
	return "owner@example.com", nil
}
func (f *fake) Sync(_ context.Context, cfg Config, cursor string, emit func(store.Item) error) (string, error) {
	if cfg.Secrets["api_key"] == "" {
		return cursor, errors.New("no key")
	}
	for i := 0; i < f.n; i++ {
		doc, err := transcript.Parse([]byte(fmt.Sprintf("---\ntitle: Meeting %d\ndate: 2026-11-15T17:00:00Z\nparticipants: [A]\nsource: fake\nsource_id: n%d\n---\nbody %d\n", i, i, i)))
		if err != nil {
			return cursor, err
		}
		if err := emit(store.Item{Doc: doc, Collection: store.Meetings}); err != nil {
			return cursor, err
		}
	}
	if f.fail != nil {
		return cursor, f.fail
	}
	return fmt.Sprintf("after-%d", f.n), nil
}

type env struct {
	m      *Manager
	db     *state.DB
	f      *fake
	remote string
	owner  state.User
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo, err := gitrepo.Open(context.Background(), gitrepo.Options{RemoteURL: remote, Branch: "main", Dir: filepath.Join(dir, "work"), AuthorName: "t", AuthorEmail: "t@e", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	box, _ := secrets.New(make([]byte, 32))
	f := &fake{}
	owner, _ := db.Login("sub-1", "antonio", "", "", true)
	return &env{m: NewManager(db, box, store.New(repo), quiet, f), db: db, f: f, remote: remote, owner: owner}
}

func TestCreateCheckAndSeal(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	_, err := e.m.Create(ctx, e.owner, "fake", "", map[string]string{})
	var fe *FormError
	if !errors.As(err, &fe) || len(fe.Problems) != 2 {
		t.Fatalf("missing fields: %v", err)
	}
	if _, err := e.m.Create(ctx, e.owner, "fake", "Mine", map[string]string{"api_key": "bad"}); !errors.As(err, &fe) || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("bad key: %v", err)
	}

	id, err := e.m.Create(ctx, e.owner, "fake", "Mine", map[string]string{"api_key": "grn_supersecretvalue", "timezone": "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := e.db.Connection(id)
	if c.Account != "owner@example.com" || c.SecretHint != "…alue" || c.Settings["timezone"] != "UTC" || !c.Enabled {
		t.Fatalf("stored: %+v", c)
	}
	if strings.Contains(string(c.Secrets), "supersecret") {
		t.Fatal("secret stored in plain text")
	}

	// Updating without a key keeps the old one; a new key is checked first.
	if err := e.m.Update(ctx, id, "Renamed", false, map[string]string{"timezone": "UTC"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ := e.m.config(mustConn(t, e.db, id))
	if cfg.Secrets["api_key"] != "grn_supersecretvalue" {
		t.Errorf("key lost on update: %v", cfg.Secrets)
	}
	if err := e.m.Update(ctx, id, "Renamed", false, map[string]string{"api_key": "bad"}); !errors.As(err, &fe) {
		t.Errorf("bad new key accepted: %v", err)
	}
	if c := mustConn(t, e.db, id); c.Label != "Renamed" || c.Enabled {
		t.Errorf("after update: %+v", c)
	}
}

func TestRunOnceBatchesAndBookmarks(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, err := e.m.Create(ctx, e.owner, "fake", "Mine", map[string]string{"api_key": "grn_supersecretvalue"})
	if err != nil {
		t.Fatal(err)
	}

	e.f.n = 60 // more than one batch
	run := e.m.RunOnce(ctx, id, e.f, "manual")
	if run.Error != "" || run.Created != 60 || run.Commit == "" {
		t.Fatalf("run: %+v", run)
	}
	if c := mustConn(t, e.db, id); c.Cursor != "after-60" {
		t.Errorf("cursor = %q", c.Cursor)
	}
	if n := gitOut(t, "--git-dir", e.remote, "rev-list", "--count", "main"); n != "2" {
		t.Errorf("want 2 commits (batches of %d), got %s", batchSize, n)
	}
	if msg := gitOut(t, "--git-dir", e.remote, "log", "-1", "--format=%B", "main"); !strings.Contains(msg, "Sync fake (Mine)") || !strings.Contains(msg, "Uploaded-By: antonio") {
		t.Errorf("message:\n%s", msg)
	}

	// Failure after emitting: documents are kept, the bookmark isn't moved.
	e.f.n, e.f.fail = 61, errors.New("boom")
	run = e.m.RunOnce(ctx, id, e.f, "schedule")
	if run.Error == "" || run.Created != 1 || run.Unchanged != 60 {
		t.Fatalf("failed run: %+v", run)
	}
	if c := mustConn(t, e.db, id); c.Cursor != "after-60" {
		t.Errorf("cursor moved on failure: %q", c.Cursor)
	}
	runs, _ := e.db.Runs(id, 10)
	if len(runs) != 2 || runs[0].Error == "" || runs[1].Created != 60 {
		t.Errorf("recorded runs: %+v", runs)
	}
}

func TestWorkersFollowConnections(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := e.m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := e.m.Create(ctx, e.owner, "fake", "Mine", map[string]string{"api_key": "grn_supersecretvalue"})
	if err != nil {
		t.Fatal(err)
	}
	// The first scheduled run starts right away.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if runs, _ := e.db.Runs(id, 1); len(runs) == 1 && !runs[0].FinishedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no run happened")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !e.m.SyncNow(id) {
		t.Error("SyncNow on a running connection")
	}
	if err := e.m.Update(ctx, id, "Mine", false, nil); err != nil {
		t.Fatal(err)
	}
	if e.m.SyncNow(id) {
		t.Error("disabled connection still has a worker")
	}
}

func mustConn(t *testing.T, db *state.DB, id int64) state.Connection {
	t.Helper()
	c, err := db.Connection(id)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func gitOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
