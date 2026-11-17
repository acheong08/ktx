package state

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestUsersAndSessions(t *testing.T) {
	db := open(t)
	u, err := db.Login("sub-1", "antonio", "Antonio Cheong", "ac@melzi.dev", true)
	if err != nil || u.ID == 0 || !u.IsAdmin || u.Display() != "antonio" {
		t.Fatalf("login: %+v %v", u, err)
	}
	// A later login refreshes the profile and admin flag but keeps the ID.
	u2, err := db.Login("sub-1", "antonio", "Antonio C.", "ac@melzi.dev", false)
	if err != nil || u2.ID != u.ID || u2.IsAdmin || u2.Name != "Antonio C." {
		t.Fatalf("relogin: %+v %v", u2, err)
	}

	tok, err := db.CreateSession(u.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := db.SessionUser(tok); err != nil || got.ID != u.ID {
		t.Fatalf("session: %+v %v", got, err)
	}
	if _, err := db.SessionUser("nope"); err != ErrNotFound {
		t.Errorf("unknown session: %v", err)
	}
	db.SetClock(func() time.Time { return time.Now().Add(2 * time.Hour) })
	if _, err := db.SessionUser(tok); err != ErrNotFound {
		t.Errorf("expired session still valid: %v", err)
	}
	db.SetClock(time.Now)
	if err := db.DeleteSession(tok); err != nil {
		t.Fatal(err)
	}
}

func TestAPITokens(t *testing.T) {
	db := open(t)
	u, _ := db.Login("sub-1", "maxim", "", "", false)
	tok, err := db.CreateAPIToken(u.ID, "granola script")
	if err != nil || !strings.HasPrefix(tok, "ktx_") {
		t.Fatalf("create: %q %v", tok, err)
	}
	if got, err := db.APITokenUser(tok); err != nil || got.Username != "maxim" {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	list, _ := db.APITokens(u.ID)
	if len(list) != 1 || list[0].LastUsedAt.IsZero() {
		t.Fatalf("list: %+v", list)
	}
	other, _ := db.Login("sub-2", "antonio", "", "", true)
	if err := db.DeleteAPIToken(other.ID, list[0].ID); err != ErrNotFound {
		t.Errorf("deleting someone else's token: %v", err)
	}
	if err := db.DeleteAPIToken(u.ID, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.APITokenUser(tok); err != ErrNotFound {
		t.Errorf("revoked token still works: %v", err)
	}
}

func TestConnectionsAndRuns(t *testing.T) {
	db := open(t)
	u, _ := db.Login("sub-1", "antonio", "", "", true)
	id, err := db.CreateConnection(Connection{Kind: "granola", Label: "Antonio's Granola", OwnerID: u.ID,
		Settings: map[string]string{"x": "y"}, Secrets: []byte("sealed"), SecretHint: "…abcd", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	c, err := db.Connection(id)
	if err != nil || c.Owner != "antonio" || c.Settings["x"] != "y" || string(c.Secrets) != "sealed" || !c.Enabled {
		t.Fatalf("read: %+v %v", c, err)
	}
	c.Enabled = false
	c.Label = "renamed"
	if err := db.UpdateConnection(c); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCursor(id, "2026-11-16T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	all, _ := db.Connections()
	if len(all) != 1 || all[0].Label != "renamed" || all[0].Enabled || all[0].Cursor == "" {
		t.Fatalf("list: %+v", all)
	}

	for i := 0; i < 55; i++ {
		rid, err := db.StartRun(id, "schedule")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.FinishRun(Run{ID: rid, Created: i}); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := db.Runs(id, 100)
	if len(runs) != 50 || runs[0].Created != 54 || runs[0].FinishedAt.IsZero() {
		t.Fatalf("runs: %d, newest %+v", len(runs), runs[0])
	}
	if err := db.DeleteConnection(id); err != nil {
		t.Fatal(err)
	}
	if runs, _ := db.Runs(id, 10); len(runs) != 0 {
		t.Error("runs not deleted with their connection")
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Login("sub-1", "antonio", "", "", true)
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if users, _ := db.Users(); len(users) != 1 {
		t.Fatalf("users after reopen: %d", len(users))
	}
}

func TestTokenScopes(t *testing.T) {
	db := open(t)
	u, _ := db.Login("sub-1", "antonio", "", "", true)
	api, _ := db.CreateAPIToken(u.ID, "script")
	cal, err := db.CreateToken(u.ID, "iPhone", ScopeCalDAV)
	if err != nil || !strings.HasPrefix(cal, "ktxcal_") {
		t.Fatalf("caldav token: %q %v", cal, err)
	}
	if _, err := db.TokenUser(cal, ScopeAPI); err != ErrNotFound {
		t.Error("calendar token accepted for the API")
	}
	if _, err := db.TokenUser(api, ScopeCalDAV); err != ErrNotFound {
		t.Error("API token accepted for CalDAV")
	}
	if got, err := db.TokenUser(cal, ScopeCalDAV); err != nil || got.ID != u.ID {
		t.Errorf("caldav lookup: %v", err)
	}
	if l, _ := db.Tokens(u.ID, ScopeCalDAV); len(l) != 1 || l[0].Name != "iPhone" {
		t.Errorf("list: %+v", l)
	}
	if l, _ := db.APITokens(u.ID); len(l) != 1 || l[0].Name != "script" {
		t.Errorf("api list: %+v", l)
	}
	if got, err := db.UserByUsername("ANTONIO"); err != nil || got.ID != u.ID {
		t.Errorf("by username: %v", err)
	}
}
