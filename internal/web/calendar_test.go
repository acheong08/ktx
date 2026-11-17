package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/acheong08/ktx/internal/state"
)

const invite = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//test//EN
BEGIN:VEVENT
UID:acme@google.com
DTSTAMP:20261101T000000Z
DTSTART:20261114T180000Z
DTEND:20261114T183000Z
SUMMARY:Acme <> Melzi
ORGANIZER;CN=Antonio:mailto:ac@melzi.dev
ATTENDEE;CN=Jane Doe:mailto:jane@acme.com
END:VEVENT
BEGIN:VEVENT
UID:standup
DTSTAMP:20261101T000000Z
DTSTART:20261114T180500Z
DTEND:20261114T181500Z
SUMMARY:Standup
END:VEVENT
END:VCALENDAR
`

const acmeCall = `---
title: Acme intro
date: 2026-11-14T10:03:00-08:00
participants:
  - Maxim Iwanskyj <maxim@melzi.dev>
source: granola
source_id: not_acme
---
Notes.
`

func upload(t *testing.T, b *env, calID, ics string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("calendar", calID)
	fw, _ := mw.CreateFormFile("file", "export.ics")
	fw.Write([]byte(ics))
	mw.Close()
	req, _ := http.NewRequest("POST", b.ts.URL+"/settings/calendar/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := b.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out bytes.Buffer
	out.ReadFrom(resp.Body)
	return resp.StatusCode, out.String()
}

func TestCalendarUI(t *testing.T) {
	e := setupAuth(t)
	ant, max := e.browser(), e.browser()
	e.signIn(t, ant, idpUser{sub: "u1", username: "antonio", groups: []string{"melzi"}}, "/")
	e.signIn(t, max, idpUser{sub: "u2", username: "maxim", groups: []string{"melzi"}}, "/")

	// A first visit makes a personal calendar.
	code, body, _ := ant.do(t, "GET", "/settings/calendar", nil)
	if code != 200 || !strings.Contains(body, "/dav/antonio/calendars/personal/") {
		t.Fatalf("calendar page: %d\n%s", code, body)
	}
	id := put(t, e.st, acmeCall) // recorded before the calendar has events

	code, body = upload(t, ant, "personal", invite)
	if code != 200 || !strings.Contains(body, "2 added, 0 updated, 0 unchanged, 0 skipped") || !strings.Contains(body, "Imported, committed and pushed.") {
		t.Fatalf("import: %d\n%s", code, body)
	}
	// The event list links the transcript; the import relinked it.
	if !strings.Contains(body, "Acme &lt;&gt; Melzi") || !strings.Contains(body, `href="/transcripts/`+id+`"`) {
		t.Errorf("event list misses the linked transcript:\n%s", body)
	}
	if code, body = upload(t, ant, "personal", "not a calendar"); code != http.StatusUnprocessableEntity {
		t.Errorf("bad file: %d", code)
	}

	_, body, _ = ant.do(t, "GET", "/transcripts/"+id, nil)
	for _, want := range []string{"Acme &lt;&gt; Melzi", "matched by time", "jane@acme.com", "from the event"} {
		if !strings.Contains(body, want) {
			t.Errorf("detail lacks %q", want)
		}
	}

	// Pick the other event by hand, then "no event", then automatic again.
	opt := regexp.MustCompile(`<option value="(antonio/personal\nstandup\n)"`).FindStringSubmatch(body)
	if opt == nil {
		t.Fatalf("no standup choice:\n%s", body)
	}
	if code, body, _ = ant.do(t, "POST", "/transcripts/"+id+"/event", url.Values{"event": {opt[1]}}); code != 200 || !strings.Contains(body, "Standup") || !strings.Contains(body, "chosen by hand") || strings.Contains(body, "jane@acme.com") {
		t.Errorf("manual pick: %d", code)
	}
	if _, body, _ = max.do(t, "POST", "/transcripts/"+id+"/event", url.Values{"event": {"none"}}); !strings.Contains(body, "no event, set by hand") {
		t.Error("set none")
	}
	if _, body, _ = ant.do(t, "POST", "/transcripts/"+id+"/event", url.Values{"event": {"auto"}}); !strings.Contains(body, "matched by time") || !strings.Contains(body, "jane@acme.com") {
		t.Error("back to automatic")
	}

	// Maxim sees Antonio's calendar, read-only, and can only import into his own.
	_, body, _ = max.do(t, "GET", "/settings/calendar", nil)
	if !strings.Contains(body, "/dav/maxim/calendars/antonio~personal/") || !strings.Contains(body, "read-only for you") {
		t.Errorf("maxim's view:\n%s", body)
	}
	if code, _ = upload(t, max, "work", invite); code != http.StatusUnprocessableEntity {
		t.Errorf("import into a calendar maxim doesn't own: %d", code)
	}

	// Calendar tokens are a separate scope.
	_, body, _ = ant.do(t, "POST", "/settings/tokens", url.Values{"name": {"iPhone"}, "scope": {"caldav"}})
	tok := regexp.MustCompile(`<pre id="new-token-caldav">(ktxcal_[^<]+)</pre>`).FindStringSubmatch(body)
	if tok == nil {
		t.Fatalf("calendar token:\n%s", body)
	}
	if _, err := e.db.TokenUser(tok[1], state.ScopeCalDAV); err != nil {
		t.Error(err)
	}
	if _, err := e.db.TokenUser(tok[1], state.ScopeAPI); err == nil {
		t.Error("calendar token works for the API")
	}
}
