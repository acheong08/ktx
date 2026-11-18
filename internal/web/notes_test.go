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

func TestNoteLabelsUI(t *testing.T) {
	e := setupAuth(t)
	b := e.browser()
	e.signIn(t, b, idpUser{sub: "u2", username: "maxim", groups: []string{"melzi"}}, "/")

	// No labels yet: the scratchpad points to Settings.
	if _, body, _ := b.do(t, "GET", "/notes", nil); !strings.Contains(body, `href="/settings/labels">Define labels</a>`) {
		t.Fatalf("no labels hint missing")
	}
	// Anyone signed in can define labels.
	code, body, _ := b.do(t, "POST", "/settings/labels", url.Values{"name": {"slop"}, "description": {"Pasted AI output, not reviewed"}})
	if code != 200 || !strings.Contains(body, "Saved “slop”") {
		t.Fatalf("add label: %d %s", code, body)
	}
	if code, _, _ := b.do(t, "POST", "/settings/labels", url.Values{"name": {"Bad Name"}, "description": {"x"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("bad name: %d", code)
	}

	// Tick it on the scratchpad.
	_, body, _ = b.do(t, "GET", "/notes", nil)
	if !strings.Contains(body, `name="labels" value="slop"`) || !strings.Contains(body, "Pasted AI output, not reviewed") {
		t.Fatalf("picker: %s", body)
	}
	_, body, _ = b.do(t, "POST", "/notes", url.Values{"body": {"Some pasted output"}, "labels": {"slop"}})
	id := regexp.MustCompile(`href="/notes/([0-9a-f]{12})"`).FindStringSubmatch(body)[1]
	for i := 0; i < 200; i++ {
		if n, _ := e.notes.Get(id); !n.Pending {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, body, _ = b.do(t, "GET", "/notes", nil)
	if !strings.Contains(body, `<a class="label" href="/notes?label=slop" title="Pasted AI output, not reviewed">slop</a>`) {
		t.Fatalf("chip: %s", body)
	}
	// Filter tabs.
	b.do(t, "POST", "/notes", url.Values{"body": {"Unlabelled note"}})
	time.Sleep(300 * time.Millisecond)
	_, body, _ = b.do(t, "GET", "/notes?label=slop", nil)
	if !strings.Contains(body, "Some pasted output") && !strings.Contains(body, "/notes/"+id) || strings.Contains(body, "Unlabelled note") {
		t.Fatalf("filter: %s", body)
	}

	// In use: no Delete button, and deleting is refused.
	_, body, _ = b.do(t, "GET", "/settings/labels", nil)
	if strings.Contains(body, "/settings/labels/slop/delete") {
		t.Fatal("delete offered for a label in use")
	}
	if code, body, _ := b.do(t, "POST", "/settings/labels/slop/delete", url.Values{}); code != http.StatusConflict || !strings.Contains(body, "take it off them first") {
		t.Fatalf("delete in use: %d", code)
	}
	// Untick it on the edit form, then delete.
	_, body, _ = b.do(t, "GET", "/notes/"+id+"/edit", nil)
	if !strings.Contains(body, `value="slop" checked`) {
		t.Fatalf("edit form: %s", body)
	}
	b.do(t, "POST", "/notes/"+id, url.Values{"title": {"Pasted"}, "body": {"Some pasted output"}})
	if code, body, _ := b.do(t, "POST", "/settings/labels/slop/delete", url.Values{}); code != 200 || !strings.Contains(body, "Deleted “slop”") {
		t.Fatalf("delete: %d %s", code, body)
	}
}
