package pocket

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/store"
)

// detail is shaped like a real Pocket recording (summary next to a mind map,
// generic speaker labels, duration in seconds).
func detail(id, title string) map[string]any {
	return map[string]any{
		"id": id, "title": title, "duration": 534, "state": "completed",
		"recording_at": "2026-11-14T19:17:28Z", "created_at": "2026-11-14T19:27:03Z", "updated_at": "2026-11-14T19:28:23Z",
		"tags": []any{map[string]any{"name": "strategy"}}, "speakers": map[string]any{},
		"transcript": map[string]any{"segments": []any{
			map[string]any{"start": 2.92, "speaker": "SPEAKER_00", "text": " Starting from the beginning. "},
			map[string]any{"start": 70.5, "speaker": "SPEAKER_01", "text": "Okay."},
		}},
		"summarizations": map[string]any{
			"old": map[string]any{"createdAt": "2026-11-14T19:27:32Z", "v2": map[string]any{
				"summary": map[string]any{"markdown": "Old summary."}}},
			"new": map[string]any{"createdAt": "2026-11-15T10:00:00Z", "v2": map[string]any{
				"summary": map[string]any{"markdown": "### Premise\n- Software factories"},
				"mindMap": map[string]any{"nodes": []any{map[string]any{"label": "</pocket:decision-tree>"}}}}},
		},
	}
}

type fakeAPI struct {
	*httptest.Server
	mu         sync.Mutex
	startDates []string
}

func newFake(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/public/recordings", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer pk_test" {
			w.WriteHeader(401)
			w.Write([]byte(`{"success":false,"error":"Invalid API key"}`))
			return
		}
		f.mu.Lock()
		f.startDates = append(f.startDates, r.URL.Query().Get("start_date"))
		f.mu.Unlock()
		page1 := []any{
			map[string]any{"id": "abc_getting_started", "title": "Getting Started with Pocket", "state": "completed"},
			map[string]any{"id": "rec-1", "title": "AI Software Factories", "state": "completed"},
		}
		page2 := []any{map[string]any{"id": "rec-2", "title": "Still processing", "state": "transcribing"}}
		if r.URL.Query().Get("page") == "2" {
			json.NewEncoder(w).Encode(map[string]any{"data": page2, "pagination": map[string]any{"has_more": false, "total": 3}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": page1, "pagination": map[string]any{"has_more": r.URL.Query().Get("limit") != "1", "total": 3}})
	})
	mux.HandleFunc("GET /api/v1/public/recordings/{id}", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if r.PathValue("id") != "rec-1" || q.Get("include_transcript") != "true" || q.Get("include_summarizations") != "true" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "data": detail("rec-1", "AI Software Factories")})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func cfg(settings map[string]string) connectors.Config {
	return connectors.Config{Secrets: map[string]string{"api_key": "pk_test"}, Settings: settings,
		Owner: connectors.Person{Name: "Maxim Iwanskyj", Email: "maxim@melzi.dev"}}
}

func TestCheck(t *testing.T) {
	f := newFake(t)
	c := &Connector{BaseURL: f.URL + "/api/v1", HTTP: f.Client()}
	if acct, err := c.Check(context.Background(), cfg(nil)); err != nil || acct != "key works (3 recordings)" {
		t.Fatalf("check: %q %v", acct, err)
	}
	bad := cfg(nil)
	bad.Secrets["api_key"] = "pk_wrong"
	if _, err := c.Check(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("bad key: %v", err)
	}
	for _, s := range []map[string]string{{"type": "a b"}, {"timezone": "PST"}, {"participants": "not an address"}} {
		if _, err := c.Check(context.Background(), cfg(s)); err == nil {
			t.Errorf("bad settings %v accepted", s)
		}
	}
}

func TestSync(t *testing.T) {
	f := newFake(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	c := &Connector{BaseURL: f.URL + "/api/v1", HTTP: f.Client(), Now: func() time.Time { return now }}
	settings := map[string]string{"timezone": "America/Los_Angeles",
		"participants": "Maxim Iwanskyj <maxim@melzi.dev>, Antonio Cheong <AC@melzi.dev>"}

	var items []store.Item
	next, err := c.Sync(context.Background(), cfg(settings), "", func(it store.Item) error { items = append(items, it); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if next != "2026-11-16T12:00:00Z" || len(items) != 1 || items[0].DefaultType != "internal" {
		t.Fatalf("next=%q items=%d (sample and unfinished recordings must be skipped)", next, len(items))
	}
	out, _ := items[0].Doc.Render()
	file := string(out)
	for _, want := range []string{
		"title: AI Software Factories\n", `date: "2026-11-14T11:17:28-08:00"`, "timezone: America/Los_Angeles", "duration_minutes: 9\n",
		"name: Maxim Iwanskyj\n    email: maxim@melzi.dev", "name: Antonio Cheong\n    email: ac@melzi.dev",
		"source: pocket\n", "source_id: rec-1\n", "tags:\n  - strategy",
		"## Summary\n\n### Premise\n- Software factories\n\n## Transcript\n\n[00:02] SPEAKER_00: Starting from the beginning.\n[01:10] SPEAKER_01: Okay.",
	} {
		if !strings.Contains(file, want) {
			t.Errorf("document lacks %q:\n%s", want, file)
		}
	}
	if strings.Contains(file, "Old summary") || strings.Contains(file, "decision-tree") || strings.Contains(file, "SPEAKER_00\n") {
		t.Errorf("old summary, mind map or generic speakers leaked in:\n%s", file)
	}

	// Later runs re-read the last week of recordings.
	if _, err := c.Sync(context.Background(), cfg(map[string]string{"type": "Customer_Discovery"}), next, func(it store.Item) error {
		if it.DefaultType != "customer_discovery" {
			t.Errorf("default type = %q", it.DefaultType)
		}
		if out, _ := it.Doc.Render(); !strings.Contains(string(out), "name: Maxim Iwanskyj\n    email: maxim@melzi.dev") {
			t.Errorf("owner should be the participant when none are configured:\n%s", out)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.startDates[len(f.startDates)-1]; got != "2026-11-09" {
		t.Errorf("start_date = %q, want a week before the last run", got)
	}
	if f.startDates[0] != "" {
		t.Errorf("first run should list everything, got start_date %q", f.startDates[0])
	}
}

// TestRealRecording converts a real recording's JSON when POCKET_SAMPLE
// points at one (kept out of the repository).
func TestRealRecording(t *testing.T) {
	path := os.Getenv("POCKET_SAMPLE")
	if path == "" {
		t.Skip("POCKET_SAMPLE not set")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r recording
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	o, _ := settings(cfg(map[string]string{"timezone": "America/Los_Angeles"}))
	doc, err := toDoc(r, o, connectors.Person{Name: "Maxim Iwanskyj", Email: "maxim@melzi.dev"})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := doc.Render()
	t.Logf("%d bytes, id %s\n%.1500s", len(out), doc.ID, out)
}
