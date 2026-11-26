package granolaacct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/store"
)

// fake is Granola's sign-in service and internal API.
type fake struct {
	*httptest.Server
	mu        sync.Mutex
	approved  bool
	refresh   string // the one valid refresh token
	access    string
	refreshes int
	docs      []map[string]any
	uas       []string
	expireNow bool // next API call answers 401 once
}

func newFake(t *testing.T) *fake {
	f := &fake{refresh: "r0", access: "a0"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/user_management/authorize/device":
			if r.PostFormValue("client_id") != ClientID {
				w.WriteHeader(400)
				return
			}
			fmt.Fprint(w, `{"device_code":"dc1","user_code":"ABCD-EFGH","verification_uri":"https://auth.example/device","verification_uri_complete":"https://auth.example/device?code=ABCD-EFGH","expires_in":300,"interval":1}`)
		case "/user_management/authenticate":
			if r.PostFormValue("device_code") != "dc1" {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
			if !f.approved {
				w.WriteHeader(400)
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
				return
			}
			fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":3600,"user":{"email":"antonio@melzi.dev","first_name":"Antonio","last_name":"Cheong"}}`, f.access, f.refresh)
		case "/v1/refresh-access-token":
			var in struct {
				RefreshToken string `json:"refresh_token"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			if in.RefreshToken != f.refresh {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":"invalid_grant"}`)
				return
			}
			f.refreshes++
			f.refresh = fmt.Sprintf("r%d", f.refreshes)
			f.access = fmt.Sprintf("a%d", f.refreshes)
			fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":3600}`, f.access, f.refresh)
		default:
			f.uas = append(f.uas, r.Header.Get("User-Agent"))
			if f.expireNow || r.Header.Get("Authorization") != "Bearer "+f.access {
				f.expireNow = false
				w.WriteHeader(401)
				return
			}
			switch r.URL.Path {
			case "/v2/get-documents":
				var in struct{ Limit, Offset int }
				json.NewDecoder(r.Body).Decode(&in)
				end := min(in.Offset+in.Limit, len(f.docs))
				page := []map[string]any{}
				if in.Offset < len(f.docs) {
					page = f.docs[in.Offset:end]
				}
				json.NewEncoder(w).Encode(map[string]any{"docs": page})
			case "/v1/get-document-transcript":
				var in struct {
					DocumentID string `json:"document_id"`
				}
				json.NewDecoder(r.Body).Decode(&in)
				if in.DocumentID == "no-transcript" {
					w.WriteHeader(404)
					return
				}
				fmt.Fprint(w, `[{"text":"Hi, can you hear me?","source":"microphone","is_final":true},{"text":"Yes.","source":"system","is_final":true},{"text":"Great.","source":"system","is_final":true}]`)
			default:
				w.WriteHeader(404)
			}
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func doc(id, title string, updated time.Time, summary bool) map[string]any {
	d := map[string]any{"id": id, "title": title, "created_at": updated.Add(-time.Hour), "updated_at": updated, "type": "meeting"}
	if summary {
		d["last_viewed_panel"] = map[string]any{"content": map[string]any{"type": "doc", "content": []any{
			map[string]any{"type": "heading", "attrs": map[string]any{"level": 1}, "content": []any{map[string]any{"type": "text", "text": "Pricing"}}},
			map[string]any{"type": "bulletList", "content": []any{map[string]any{"type": "listItem", "content": []any{
				map[string]any{"type": "paragraph", "content": []any{
					map[string]any{"type": "text", "text": "Wants "}, map[string]any{"type": "text", "text": "SSO", "marks": []any{map[string]any{"type": "bold"}}}}}}}}},
		}}}
	}
	return d
}

func connector(f *fake, now time.Time) *Connector {
	return &Connector{API: f.URL, Auth: f.URL, HTTP: f.Client(), Now: func() time.Time { return now }}
}

func TestSignIn(t *testing.T) {
	f := newFake(t)
	c := connector(f, time.Now())
	ctx := context.Background()
	p, err := c.StartSignIn(ctx)
	if err != nil || p.Code != "ABCD-EFGH" || !strings.Contains(p.URL, "code=ABCD-EFGH") || p.State != "dc1" || p.Interval != 5*time.Second { // never faster than every 5s
		t.Fatalf("start: %+v %v", p, err)
	}
	if _, _, err := c.PollSignIn(ctx, p); !errors.Is(err, connectors.ErrPending) {
		t.Fatalf("pending: %v", err)
	}
	f.approved = true
	sec, account, err := c.PollSignIn(ctx, p)
	if err != nil || account != "antonio@melzi.dev" || sec["refresh_token"] != "r0" || sec["access_token"] != "a0" || sec["name"] != "Antonio Cheong" || sec["expires_at"] == "" {
		t.Fatalf("approved: %v %q %v", sec, account, err)
	}
}

func TestSyncRotatesAndStores(t *testing.T) {
	f := newFake(t)
	now := time.Date(2026, 11, 26, 20, 0, 0, 0, time.UTC)
	c := connector(f, now)
	ctx := context.Background()
	// Newest first, as Granola returns them.
	f.docs = []map[string]any{
		doc("in-progress", "Recording now", now.Add(-10*time.Minute), false),
		doc("d2", "Acme intro", now.Add(-3*time.Hour), true),
		doc("no-transcript", "Notes only", now.Add(-4*time.Hour), true),
		doc("d1", "Old call", now.Add(-48*time.Hour), true),
	}
	ev := map[string]any{"iCalUID": "acme@google.com", "id": "acme", "summary": "Acme <> Melzi",
		"start":     map[string]any{"dateTime": "2026-11-26T09:00:00-08:00", "timeZone": "America/Los_Angeles"},
		"end":       map[string]any{"dateTime": "2026-11-26T09:30:00-08:00"},
		"attendees": []any{map[string]any{"email": "Jane@Acme.com", "responseStatus": "accepted"}, map[string]any{"email": "room@x", "resource": true}}}
	f.docs[1]["google_calendar_event"] = ev
	f.docs[1]["people"] = map[string]any{"attendees": []any{map[string]any{"email": "jane@acme.com", "details": map[string]any{"person": map[string]any{"name": map[string]any{"fullName": "Jane Doe"}}}}}}

	// The stored session's access token has expired: the first call refreshes.
	var saved []map[string]string
	cfg := connectors.Config{Settings: map[string]string{}, Owner: connectors.Person{Name: "Antonio", Email: "antonio@melzi.dev"},
		Secrets: map[string]string{"access_token": "stale", "refresh_token": "r0", "expires_at": now.Add(-time.Minute).Format(time.RFC3339), "name": "Antonio"}}
	cfg.SaveSecrets = func(s map[string]string) error {
		// Saved before the new access token is used.
		if s["access_token"] != f.access {
			t.Errorf("saved %v, server now has %s", s, f.access)
		}
		saved = append(saved, s)
		return nil
	}
	var got []store.Item
	cursor, err := c.Sync(ctx, cfg, "", func(it store.Item) error { got = append(got, it); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0]["refresh_token"] != "r1" || saved[0]["name"] != "Antonio" {
		t.Fatalf("saved: %v", saved)
	}
	var titles []string
	for _, it := range got {
		titles = append(titles, it.Doc.Meta.Title)
	}
	if strings.Join(titles, ",") != "Acme intro,Notes only,Old call" {
		t.Fatalf("stored: %v", titles)
	}
	acme := got[0].Doc
	if acme.Field("calendar_event_id") != "acme@google.com" || acme.Meta.Timezone != "America/Los_Angeles" || acme.Meta.DurationMinutes != 30 ||
		len(acme.Meta.Participants) != 1 || acme.Meta.Participants[0].Name != "Jane Doe" || acme.Meta.Participants[0].Email != "jane@acme.com" ||
		acme.Meta.Source != "granola" || acme.Meta.SourceID != "d2" || got[0].DefaultType != "meetings" {
		t.Fatalf("acme: %+v %q", acme.Meta, acme.Field("calendar_event_id"))
	}
	for _, want := range []string{"## Summary\n\n## Pricing\n\n- Wants **SSO**", "## Transcript\n\n**Antonio:** Hi, can you hear me?\n\n**Them:** Yes. Great."} {
		if !strings.Contains(acme.Body, want) {
			t.Errorf("body lacks %q:\n%s", want, acme.Body)
		}
	}
	if strings.Contains(got[1].Doc.Body, "## Transcript") {
		t.Error("a note without a transcript got one")
	}
	if got[2].Doc.Meta.Participants[0].Email != "antonio@melzi.dev" {
		t.Errorf("solo note participants: %+v", got[2].Doc.Meta.Participants)
	}
	// The in-progress note doesn't move the bookmark, so it's fetched later.
	if want := now.Add(-3 * time.Hour).UTC().Format(time.RFC3339); cursor != want {
		t.Fatalf("cursor %s, want %s", cursor, want)
	}
	for _, ua := range f.uas {
		if !strings.HasPrefix(ua, "Granola/"+DefaultClientVersion+" (macOS; ktx)") {
			t.Fatalf("user agent %q", ua)
		}
	}

	// Next run: only what changed since the bookmark; a 401 refreshes once.
	cfg.Secrets = saved[0]
	f.docs[0] = doc("in-progress", "Recording now", now.Add(-5*time.Minute), true)
	f.expireNow = true
	got = nil
	if _, err := c.Sync(ctx, cfg, cursor, func(it store.Item) error { got = append(got, it); return nil }); err != nil {
		t.Fatal(err)
	}
	// (the overlap re-reads the note at the bookmark; storing it again is a no-op)
	if len(got) != 2 || got[0].Doc.Meta.Title != "Recording now" || got[1].Doc.Meta.Title != "Acme intro" || len(saved) != 2 {
		t.Fatalf("second run: %d items, %d saves", len(got), len(saved))
	}

	// A spent refresh token means signing in again.
	cfg.Secrets = map[string]string{"access_token": "gone", "refresh_token": "r0"}
	if _, err := c.Sync(ctx, cfg, "", func(store.Item) error { return nil }); !errors.Is(err, connectors.ErrSignInAgain) {
		t.Fatalf("spent token: %v", err)
	}
}

func TestCheck(t *testing.T) {
	c := New()
	for settings, ok := range map[string]bool{"": true, "type=customer_discovery": true, "type=Bad Type!": false, "timezone=Mars/Base": false, "client_version=7.300.1": true, "client_version=7 (x)": false} {
		s := map[string]string{}
		if k, v, found := strings.Cut(settings, "="); found {
			s[k] = v
		}
		if _, err := c.Check(context.Background(), connectors.Config{Settings: s}); (err == nil) != ok {
			t.Errorf("%q: %v", settings, err)
		}
	}
}
