package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/acheong08/ktx/internal/calendar"
)

func TestTimeZoneSetting(t *testing.T) {
	e := setupAuth(t)
	la, _ := time.LoadLocation("America/Los_Angeles")
	t.Cleanup(func() { calendar.SetDefaultZone(la) })
	b := e.browser()
	e.signIn(t, b, idpUser{sub: "u2", username: "maxim", groups: []string{"melzi"}}, "/")
	// A Granola-style transcript: UTC, no time zone recorded.
	id := put(t, e.st, "---\ntitle: UTC call\ndate: 2026-11-17T18:30:00Z\nparticipants: [Jane <jane@acme.com>]\n---\nHi.\n")

	if _, body, _ := b.do(t, "GET", "/", nil); !strings.Contains(body, "Tue 17 Nov 2026, 10:30 PST") {
		t.Fatalf("default zone not LA: %s", body)
	}
	_, body, _ := b.do(t, "GET", "/settings/general", nil)
	if !strings.Contains(body, `value="America/Los_Angeles"`) {
		t.Fatalf("general page: %s", body)
	}
	if code, body, _ := b.do(t, "POST", "/settings/general", url.Values{"timezone": {"Mars/Olympus"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "isn&#39;t a time zone") {
		t.Fatalf("bad zone: %d", code)
	}
	if code, body, _ := b.do(t, "POST", "/settings/general", url.Values{"timezone": {"Europe/London"}}); code != 200 || !strings.Contains(body, "now shown in Europe/London") {
		t.Fatalf("save: %d %s", code, body)
	}
	if z, _ := e.db.TimeZone(); z != "Europe/London" {
		t.Fatalf("stored = %q", z)
	}
	if _, body, _ := b.do(t, "GET", "/", nil); !strings.Contains(body, "Tue 17 Nov 2026, 18:30 GMT") {
		t.Fatalf("list not in London time: %s", body)
	}
	// Editing a transcript without a zone starts in the workspace's.
	if _, body, _ := b.do(t, "GET", "/transcripts/"+id+"/edit", nil); !strings.Contains(body, `value="2026-11-17T18:30"`) || !strings.Contains(body, `value="Europe/London"`) {
		t.Fatalf("edit form: %s", body)
	}
	// A call with its own zone shows it next to the workspace time.
	id2 := put(t, e.st, "---\ntitle: Tokyo call\ndate: 2026-11-17T09:00:00+09:00\ntimezone: Asia/Tokyo\nparticipants: [Ken <ken@example.jp>]\n---\nHi.\n")
	if _, body, _ := b.do(t, "GET", "/transcripts/"+id2, nil); !strings.Contains(body, "Tue 17 Nov 2026, 00:00 GMT") || !strings.Contains(body, "(09:00 Asia/Tokyo)") {
		t.Fatalf("own zone: %s", body)
	}
}
