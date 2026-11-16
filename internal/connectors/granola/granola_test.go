package granola

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/store"
)

// fakeAPI serves two notes shaped like Granola's API reference. The second
// note's transcript is "too large" and must be paged.
func fakeAPI(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var throttled atomic.Int32
	mux := http.NewServeMux()
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer grn_test" {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"code": "INVALID_API_KEY", "message": "The key is unknown"})
			return false
		}
		return true
	}
	mux.HandleFunc("GET /v1/notes", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if throttled.Add(1) == 1 { // first call: rate limited once
			w.WriteHeader(429)
			return
		}
		notes := []map[string]any{
			{"id": "not_aaaaaaaaaaaaaa", "owner": map[string]any{"name": "Antonio", "email": "ac@melzi.dev"}, "updated_at": "2026-11-15T18:00:00Z", "deleted_at": nil},
			{"id": "not_bbbbbbbbbbbbbb", "owner": map[string]any{"name": "Antonio", "email": "ac@melzi.dev"}, "updated_at": "2026-11-16T09:00:00Z", "deleted_at": nil},
			{"id": "not_deleteddddddd", "owner": map[string]any{"name": "Antonio", "email": "ac@melzi.dev"}, "updated_at": "2026-11-16T10:00:00Z", "deleted_at": "2026-11-16T10:00:00Z"},
		}
		if r.URL.Query().Get("cursor") == "" {
			json.NewEncoder(w).Encode(map[string]any{"notes": notes[:1], "hasMore": true, "cursor": "page2"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"notes": notes[1:], "hasMore": false, "cursor": nil})
	})
	mux.HandleFunc("GET /v1/notes/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		switch r.PathValue("id") {
		case "not_aaaaaaaaaaaaaa":
			json.NewEncoder(w).Encode(map[string]any{
				"id": "not_aaaaaaaaaaaaaa", "title": "Acme discovery", "owner": map[string]any{"name": "Antonio", "email": "ac@melzi.dev"},
				"created_at": "2026-11-15T17:00:00Z", "updated_at": "2026-11-15T18:00:00Z", "web_url": "https://notes.granola.ai/d/x",
				"calendar_event": map[string]any{"event_title": "Acme <> Melzi", "calendar_event_id": "evt1",
					"scheduled_start_time": "2026-11-15T17:00:00Z", "scheduled_end_time": "2026-11-15T17:45:00Z", "invitees": []any{}, "organiser": nil},
				"attendees":         []any{map[string]any{"name": "Jane Doe", "email": "jane@acme.com"}, map[string]any{"name": "Antonio", "email": "ac@melzi.dev"}},
				"folder_membership": []any{map[string]any{"name": "Customers"}},
				"summary_text":      "plain", "summary_markdown": "### Key points\n- They want **tenant isolation**",
				"private_notes_markdown": "- ask about pricing",
				"transcript": []any{
					map[string]any{"speaker": map[string]any{"source": "microphone", "attribution": "me"}, "text": "Thanks for joining."},
					map[string]any{"speaker": map[string]any{"source": "microphone", "attribution": "me"}, "text": "How do you isolate tenants?"},
					map[string]any{"speaker": map[string]any{"source": "speaker", "attribution": "them"}, "text": "Row-level security."},
				},
			})
		case "not_bbbbbbbbbbbbbb":
			if r.URL.Query().Get("include") == "transcript" {
				w.WriteHeader(413)
				json.NewEncoder(w).Encode(map[string]string{"code": "TRANSCRIPT_TOO_LARGE", "message": "use the transcript endpoint"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id": "not_bbbbbbbbbbbbbb", "title": nil, "owner": map[string]any{"name": "Antonio", "email": "ac@melzi.dev"},
				"created_at": "2026-11-16T08:00:00Z", "updated_at": "2026-11-16T09:00:00Z", "web_url": "https://notes.granola.ai/d/y",
				"calendar_event": nil, "attendees": []any{}, "folder_membership": []any{},
				"summary_text": "Voice memo about hiring.", "summary_markdown": nil, "private_notes_markdown": nil, "transcript": nil,
			})
		default:
			w.WriteHeader(404)
		}
	})
	mux.HandleFunc("GET /v1/notes/{id}/transcript", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.URL.Query().Get("cursor") == "" {
			json.NewEncoder(w).Encode(map[string]any{"transcript": []any{
				map[string]any{"speaker": map[string]any{"source": "microphone", "diarization_label": "Speaker A"}, "text": "First half."},
			}, "hasMore": true, "cursor": "t2"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"transcript": []any{
			map[string]any{"speaker": map[string]any{"source": "microphone", "diarization_label": "Speaker B"}, "text": "Second half."},
		}, "hasMore": false, "cursor": nil})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &throttled
}

func newConn(ts *httptest.Server) *Connector {
	return &Connector{BaseURL: ts.URL + "/v1", HTTP: ts.Client()}
}

var cfg = connectors.Config{Secrets: map[string]string{"api_key": "grn_test"}, Settings: map[string]string{"timezone": "America/Los_Angeles"}}

func TestCheck(t *testing.T) {
	ts, _ := fakeAPI(t)
	c := newConn(ts)
	if acct, err := c.Check(context.Background(), cfg); err != nil || acct != "ac@melzi.dev" {
		t.Fatalf("check: %q %v", acct, err)
	}
	bad := connectors.Config{Secrets: map[string]string{"api_key": "grn_wrong"}}
	if _, err := c.Check(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "INVALID_API_KEY") {
		t.Errorf("bad key: %v", err)
	}
	badTZ := connectors.Config{Secrets: cfg.Secrets, Settings: map[string]string{"timezone": "PST"}}
	if _, err := c.Check(context.Background(), badTZ); err == nil {
		t.Error("bad time zone accepted")
	}
}

func TestSync(t *testing.T) {
	ts, _ := fakeAPI(t)
	c := newConn(ts)
	var items []store.Item
	next, err := c.Sync(context.Background(), cfg, "", func(it store.Item) error { items = append(items, it); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if next != "2026-11-16T09:00:00Z" {
		t.Errorf("cursor = %q (deleted notes must not move it)", next)
	}
	if len(items) != 2 || items[0].Collection != store.Meetings {
		t.Fatalf("items: %d", len(items))
	}

	acme := items[0].Doc
	out, _ := acme.Render()
	file := string(out)
	for _, want := range []string{
		"title: Acme discovery\n", "date: \"2026-11-15T09:00:00-08:00\"", "timezone: America/Los_Angeles", "duration_minutes: 45",
		"email: jane@acme.com", "source: granola", "source_id: not_aaaaaaaaaaaaaa", "calendar_event_id: evt1",
		"granola_folders:\n  - Customers", "## Summary\n\n### Key points\n- They want **tenant isolation**",
		"## My notes\n\n- ask about pricing",
		"## Transcript\n\n**Antonio:** Thanks for joining. How do you isolate tenants?\n\n**Them:** Row-level security.",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("acme note lacks %q:\n%s", want, file)
		}
	}
	if acme.Start.UTC().Format(time.RFC3339) != "2026-11-15T17:00:00Z" {
		t.Errorf("start = %v", acme.Start)
	}

	memo, _ := items[1].Doc.Render()
	for _, want := range []string{"title: Untitled meeting", "name: Antonio", "Voice memo about hiring.", "**Speaker A:** First half.\n\n**Speaker B:** Second half."} {
		if !strings.Contains(string(memo), want) {
			t.Errorf("memo lacks %q:\n%s", want, memo)
		}
	}
	if strings.Contains(string(memo), "## My notes") || strings.Contains(string(memo), "duration_minutes") {
		t.Errorf("memo has empty parts:\n%s", memo)
	}

	// Same documents on a second run: identical ids, so the store sees no change.
	var again []store.Item
	if _, err := c.Sync(context.Background(), cfg, next, func(it store.Item) error { again = append(again, it); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || again[0].Doc.ID != acme.ID {
		t.Errorf("second run: %d items", len(again))
	}
}
