package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSubscriptionsUI(t *testing.T) {
	e := setupAuth(t)
	ant, max := e.browser(), e.browser()
	e.signIn(t, ant, idpUser{sub: "u1", username: "antonio", groups: []string{"melzi"}}, "/")
	e.signIn(t, max, idpUser{sub: "u2", username: "maxim", groups: []string{"melzi"}}, "/")

	_, body, _ := ant.do(t, "GET", "/settings/calendar", nil)
	if !strings.Contains(body, "Subscribe to a calendar") {
		t.Fatalf("no subscribe form:\n%s", body)
	}
	// A wrong link is refused, and the form keeps what was typed.
	bad := strings.Replace(e.feedURL, "t0k", "nope", 1)
	code, body, _ := ant.do(t, "POST", "/settings/calendar/subscribe", url.Values{"name": {"Luma"}, "url": {bad}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "Couldn&#39;t subscribe") || !strings.Contains(body, `value="Luma"`) {
		t.Fatalf("bad link: %d", code)
	}
	code, body, _ = ant.do(t, "POST", "/settings/calendar/subscribe", url.Values{"name": {"Luma"}, "url": {e.feedURL}})
	if code != 200 || !strings.Contains(body, "Subscribed.") || !strings.Contains(body, ">subscription</span>") || !strings.Contains(body, "1 item") {
		t.Fatalf("subscribe: %d\n%s", code, body)
	}
	if strings.Contains(body, "t0k") {
		t.Fatal("the link's secret is shown")
	}
	// Its events show in the agenda, for others too.
	if _, body, _ = max.do(t, "GET", "/agenda?who=all&week=2026-11-23", nil); !strings.Contains(body, "Luma AI Night") {
		t.Fatalf("agenda:\n%s", body)
	}
	// It isn't offered for imports or new tasks.
	_, body, _ = ant.do(t, "GET", "/settings/calendar", nil)
	if strings.Contains(body, `<option value="luma">`) {
		t.Fatal("subscription offered as an import target")
	}
	// Refresh and remove; Maxim can't.
	if code, _, _ := max.do(t, "POST", "/settings/calendar/luma/refresh", url.Values{}); code == 200 {
		if _, b, _ := max.do(t, "GET", "/settings/calendar", nil); strings.Contains(b, "Refreshed") {
			t.Fatal("maxim refreshed antonio's subscription")
		}
	}
	if _, body, _ = ant.do(t, "POST", "/settings/calendar/luma/refresh", url.Values{}); !strings.Contains(body, "Refreshed; nothing changed.") {
		t.Fatalf("refresh:\n%s", body)
	}
	if _, body, _ = ant.do(t, "POST", "/settings/calendar/luma/unsubscribe", url.Values{"confirm": {"on"}}); !strings.Contains(body, "Subscription removed.") || strings.Contains(body, ">subscription</span>") {
		t.Fatalf("remove:\n%s", body)
	}
}

func TestMergedSubscriptionUI(t *testing.T) {
	e := setupAuth(t)
	ant := e.browser()
	e.signIn(t, ant, idpUser{sub: "u1", username: "antonio", groups: []string{"melzi"}}, "/")
	_, body, _ := ant.do(t, "GET", "/settings/calendar", nil)
	if !strings.Contains(body, `<option value="personal" selected>In Personal</option>`) {
		t.Fatalf("default isn't merge into personal:\n%s", body)
	}
	code, body, _ := ant.do(t, "POST", "/settings/calendar/subscribe", url.Values{"name": {"Luma"}, "url": {e.feedURL}, "into": {"personal"}})
	if code != 200 || !strings.Contains(body, "show as part of Personal") || !strings.Contains(body, ">+ Luma</span>") || strings.Contains(body, ">subscription</span>") {
		t.Fatalf("merged subscribe: %d\n%s", code, body)
	}
	if !strings.Contains(body, "shown in Personal") {
		t.Fatalf("status:\n%s", body)
	}
	// The event shows in the agenda, under Personal.
	if _, body, _ = ant.do(t, "GET", "/agenda?week=2026-11-23", nil); !strings.Contains(body, "Luma AI Night") {
		t.Fatalf("agenda:\n%s", body)
	}
	// Split it out again.
	if _, body, _ = ant.do(t, "POST", "/settings/calendar/luma/merge", url.Values{"into": {""}}); !strings.Contains(body, ">subscription</span>") || strings.Contains(body, ">+ Luma</span>") {
		t.Fatalf("unmerge:\n%s", body)
	}
}
