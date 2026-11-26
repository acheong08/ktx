package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestConnectionSignIn(t *testing.T) {
	e := setupAuth(t)
	ant, max := e.browser(), e.browser()
	e.signIn(t, ant, idpUser{sub: "u1", username: "antonio", groups: []string{"melzi"}}, "/")
	e.signIn(t, max, idpUser{sub: "u2", username: "maxim", groups: []string{"melzi"}}, "/")
	deviceStub.mu.Lock()
	deviceStub.approved, deviceStub.syncs = false, nil
	deviceStub.mu.Unlock()

	_, body, _ := ant.do(t, "GET", "/settings/connections/new?kind=device", nil)
	if !strings.Contains(body, "Sign in to Device") {
		t.Fatalf("form:\n%s", body)
	}
	// Bad settings are refused before signing in.
	if code, body, _ := ant.do(t, "POST", "/settings/connections", url.Values{"kind": {"device"}, "label": {"Mine"}, "field_type": {"bad"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "bad type") {
		t.Fatalf("bad settings: %d", code)
	}
	ant.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, _, resp := ant.do(t, "POST", "/settings/connections", url.Values{"kind": {"device"}, "label": {"Antonio's Granola"}, "field_type": {"internal"}})
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/settings/connections/signin/") {
		t.Fatalf("redirect: %q", loc)
	}
	ant.client.CheckRedirect = nil
	_, body, _ = ant.do(t, "GET", loc, nil)
	if !strings.Contains(body, `href="https://auth.example/device?code=WXYZ"`) || !strings.Contains(body, ">WXYZ</code>") || !strings.Contains(body, `hx-get="`+loc+`/status"`) {
		t.Fatalf("sign-in page:\n%s", body)
	}
	// Someone else can't see or finish it.
	if code, _, _ := max.do(t, "GET", loc, nil); code != http.StatusNotFound {
		t.Fatalf("maxim sees the sign-in: %d", code)
	}
	// Not approved yet: still waiting.
	if _, body, _ = ant.do(t, "GET", loc+"/status", nil, "HX-Request", "true"); !strings.Contains(body, "Waiting for approval") {
		t.Fatalf("waiting: %s", body)
	}
	deviceStub.approve()
	time.Sleep(5 * time.Millisecond)
	_, _, resp = ant.do(t, "GET", loc+"/status", nil, "HX-Request", "true")
	to := resp.Header.Get("HX-Redirect")
	if !regexp.MustCompile(`^/settings/connections/\d+\?done=signedin$`).MatchString(to) {
		t.Fatalf("done: %q", to)
	}
	_, body, _ = ant.do(t, "GET", to, nil)
	if !strings.Contains(body, "Signed in.") || !strings.Contains(body, "antonio@melzi.dev") || !strings.Contains(body, "Sign in again") {
		t.Fatalf("connection page:\n%s", body)
	}
	id := strings.TrimSuffix(strings.TrimPrefix(to, "/settings/connections/"), "?done=signedin")

	// A sync rotates the secret; saving the form doesn't put the old one back.
	waitSyncs := func(n int) {
		t.Helper()
		for i := 0; i < 300; i++ {
			deviceStub.mu.Lock()
			got := len(deviceStub.syncs)
			deviceStub.mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("no sync %d", n)
	}
	waitSyncs(1)
	time.Sleep(50 * time.Millisecond)
	if code, _, _ := ant.do(t, "POST", "/settings/connections/"+id, url.Values{"label": {"Renamed"}, "enabled": {"on"}, "field_type": {"internal"}}); code != 200 {
		t.Fatalf("save: %d", code)
	}
	ant.do(t, "POST", "/settings/connections/"+id+"/sync", url.Values{})
	waitSyncs(2)
	deviceStub.mu.Lock()
	first, second := deviceStub.syncs[0]["refresh"], deviceStub.syncs[1]["refresh"]
	deviceStub.mu.Unlock()
	if first != "r0" || second != "r0+" {
		t.Fatalf("secrets seen by syncs: %q then %q", first, second)
	}
	// Only the owner can sign it in again (it's their account).
	if code, _, _ := max.do(t, "POST", "/settings/connections/"+id+"/signin", url.Values{}); code != http.StatusForbidden {
		t.Fatalf("maxim re-signs: %d", code)
	}
}
