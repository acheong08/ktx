package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestNotesUI(t *testing.T) {
	e := setupAuth(t)
	admin, maxim := e.browser(), e.browser()
	e.signIn(t, admin, idpUser{sub: "u1", username: "antonio", groups: []string{"melzi", "ktx-admins"}}, "/")
	e.signIn(t, maxim, idpUser{sub: "u2", username: "maxim", groups: []string{"melzi"}}, "/")

	// Only admins can set the AI key.
	if _, body, _ := maxim.do(t, "GET", "/settings/ai", nil); !strings.Contains(body, "Only admins can change this") {
		t.Fatalf("non-admin AI page: %s", body)
	}
	if code, _, _ := maxim.do(t, "POST", "/settings/ai", url.Values{"key": {"k-12345678"}}); code != http.StatusForbidden {
		t.Fatalf("non-admin save: %d", code)
	}
	code, body, _ := admin.do(t, "POST", "/settings/ai", url.Values{"key": {"nw-key-12345678"}, "model": {"nw-flash"}})
	if code != 200 || !strings.Contains(body, "Saved, and checked") || !strings.Contains(body, "…5678") || strings.Contains(body, "nw-key-12345678") {
		t.Fatalf("admin save: %d %s", code, body)
	}

	// The scratchpad: save, then the title arrives.
	if code, body, _ := maxim.do(t, "POST", "/notes", url.Values{"body": {"  "}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "the note is empty") {
		t.Fatalf("empty note: %d", code)
	}
	code, body, _ = maxim.do(t, "POST", "/notes", url.Values{"body": {"Acme wants usage-based pricing.\r\nFollow up Friday."}})
	if code != 200 || !strings.Contains(body, "<h1>Notes</h1>") {
		t.Fatalf("after save: %d %s", code, body)
	}
	id := regexp.MustCompile(`href="/notes/([0-9a-f]{12})"`).FindStringSubmatch(body)[1]
	var title string
	for i := 0; i < 200; i++ {
		_, title, _ = maxim.do(t, "GET", "/notes/"+id+"/title", nil, "HX-Request", "true")
		if !strings.Contains(title, "Generating title") {
			break
		}
		if !strings.Contains(title, `hx-trigger="every 2s"`) {
			t.Fatalf("pending fragment doesn't poll: %s", title)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if title != "<span>Acme pricing follow-up</span>" {
		t.Fatalf("title fragment: %q", title)
	}
	// On the note's own page the fragment reloads the page once ready.
	_, _, resp := maxim.do(t, "GET", "/notes/"+id+"/title?reload=1", nil, "HX-Request", "true")
	if resp.Header.Get("HX-Redirect") != "/notes/"+id {
		t.Fatalf("reload: %v", resp.Header)
	}

	// List, search, read.
	if _, body, _ := admin.do(t, "GET", "/notes?q=usage", nil); !strings.Contains(body, "Acme pricing follow-up") || !strings.Contains(body, `data-label="By">maxim`) {
		t.Fatalf("search: %s", body)
	}
	if _, body, _ := admin.do(t, "GET", "/notes?q=nothing-like-this", nil, "HX-Request", "true", "HX-Request-Type", "partial"); !strings.Contains(body, "No note matches") || strings.Contains(body, "<html") {
		t.Fatalf("partial search: %s", body)
	}
	_, body, _ = admin.do(t, "GET", "/notes/"+id, nil)
	if !strings.Contains(body, "<p>Acme wants usage-based pricing.<br>\nFollow up Friday.</p>") || !strings.Contains(body, "/notes/"+id+"/edit") {
		t.Fatalf("detail: %s", body)
	}

	// Edit and delete.
	code, body, _ = admin.do(t, "POST", "/notes/"+id, url.Values{"title": {"Acme: pricing"}, "body": {"Usage-based."}})
	if code != 200 || !strings.Contains(body, "Acme: pricing") || !strings.Contains(body, "edited by antonio") {
		t.Fatalf("edit: %d %s", code, body)
	}
	if code, _, _ := admin.do(t, "POST", "/notes/"+id, url.Values{"title": {""}, "body": {"x"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty title: %d", code)
	}
	if _, body, _ := admin.do(t, "POST", "/notes/"+id+"/delete", url.Values{"confirm": {"on"}}); !strings.Contains(body, "Note deleted") {
		t.Fatalf("delete: %s", body)
	}
	if code, _, _ := admin.do(t, "GET", "/notes/"+id, nil); code != http.StatusNotFound {
		t.Fatalf("after delete: %d", code)
	}
}
