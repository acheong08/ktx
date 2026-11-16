package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

type keypair struct{ priv, pub string }

func newKey(t *testing.T, comment string) keypair {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not installed")
	}
	p := filepath.Join(t.TempDir(), "id")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", comment, "-f", p).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	pub, _ := os.ReadFile(p + ".pub")
	return keypair{p, string(pub)}
}

func signWith(t *testing.T, k keypair, namespace, msg string) string {
	t.Helper()
	cmd := exec.Command("ssh-keygen", "-Y", "sign", "-n", namespace, "-f", k.priv)
	cmd.Stdin = strings.NewReader(msg)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestAuth(t *testing.T) {
	alice, mallory := newKey(t, "alice"), newKey(t, "mallory")
	keys, err := ParseAuthorizedKeys("# comment\n" + alice.pub)
	if err != nil {
		t.Fatal(err)
	}
	a := NewAuth(keys, time.Hour)

	c := a.NewChallenge()
	if _, _, err := a.Login(c, signWith(t, mallory, Namespace, c)); err == nil || !strings.Contains(err.Error(), "not authorized") {
		t.Errorf("unauthorized key: %v", err)
	}
	if _, _, err := a.Login(c, signWith(t, alice, "git", c)); err == nil {
		t.Error("wrong namespace accepted")
	}
	if _, _, err := a.Login(c, signWith(t, alice, Namespace, "other")); err == nil {
		t.Error("signature over other text accepted")
	}

	token, user, err := a.Login(c, signWith(t, alice, Namespace, c+"\n")) // as if signed with echo
	if err != nil || user != "alice" {
		t.Fatalf("login: %q %v", user, err)
	}
	if u, ok := a.User(token); !ok || u != "alice" {
		t.Error("session not found")
	}
	if _, _, err := a.Login(c, signWith(t, alice, Namespace, c)); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Errorf("replay: %v", err)
	}

	forged := strings.Replace(c, c[len(c)-4:], "AAAA", 1)
	if _, _, err := a.Login(forged, signWith(t, alice, Namespace, forged)); err == nil {
		t.Error("forged challenge accepted")
	}

	a.now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	c2 := NewAuth(keys, time.Hour) // challenge from a different server instance (restart)
	cc := c2.NewChallenge()
	if _, _, err := a.Login(cc, signWith(t, alice, Namespace, cc)); err == nil {
		t.Error("challenge from another instance accepted")
	}
	old := a.NewChallenge()
	a.now = func() time.Time { return time.Now().Add(20 * time.Minute) }
	if _, _, err := a.Login(old, signWith(t, alice, Namespace, old)); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired: %v", err)
	}
	a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, ok := a.User(token); ok {
		t.Error("session should have expired")
	}

	a.Logout(token)
	if _, err := ParseAuthorizedKeys("not a key"); err == nil {
		t.Error("bad key accepted")
	}
}

type env struct {
	ts     *httptest.Server
	client *http.Client
	st     *store.Store
	key    keypair
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	repo, err := gitrepo.Open(context.Background(), gitrepo.Options{RemoteURL: remote, Branch: "main", Dir: filepath.Join(dir, "work"), AuthorName: "t", AuthorEmail: "t@e", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(repo)
	k := newKey(t, "antonio")
	keys, _ := ParseAuthorizedKeys(k.pub)
	ui := New(st, NewAuth(keys, time.Hour), []string{"advisor"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ts := httptest.NewServer(ui.Handler())
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &env{ts: ts, st: st, key: k, client: &http.Client{Jar: jar}}
}

func (e *env) do(t *testing.T, method, path string, form url.Values, headers ...string) (int, string, *http.Response) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp
}

var challengeRe = regexp.MustCompile(`name="challenge" value="([^"]+)"`)

func (e *env) login(t *testing.T) {
	t.Helper()
	_, page, _ := e.do(t, "GET", "/login", nil)
	m := challengeRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no challenge in login page:\n%s", page)
	}
	c := m[1]
	if !strings.Contains(page, "printf &#39;%s&#39; &#39;"+c+"&#39; | ssh-keygen -Y sign -n brain-server") {
		t.Errorf("command missing from login page")
	}
	code, body, _ := e.do(t, "POST", "/login", url.Values{"challenge": {c}, "signature": {signWith(t, e.key, Namespace, c)}})
	if code != 200 || !strings.Contains(body, "antonio") {
		t.Fatalf("login: %d\n%s", code, body)
	}
}

func put(t *testing.T, st *store.Store, src string) string {
	t.Helper()
	doc, err := transcript.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(context.Background(), doc, "maxim"); err != nil {
		t.Fatal(err)
	}
	return doc.ID
}

func TestLoginCommandFollowsOS(t *testing.T) {
	e := setup(t)
	for ua, want := range map[string]string{
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 14_5) AppleWebKit/605.1.15":      "| pbcopy</pre>",
		"Mozilla/5.0 (X11; Linux x86_64; rv:140.0) Gecko/20100101 Firefox/140.0": "| wl-copy</pre>",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64)":                              "-f ~/.ssh/id_ed25519</pre>",
	} {
		_, page, _ := e.do(t, "GET", "/login", nil, "User-Agent", ua)
		if !strings.Contains(page, want) {
			t.Errorf("%s: command should end with %q", ua, want)
		}
		if !strings.Contains(page, `data-template="printf &#39;%s&#39; &#39;brain-login:`) || !strings.Contains(page, "xclip -selection clipboard") {
			t.Errorf("%s: variants missing", ua)
		}
	}
}

func TestUIFlow(t *testing.T) {
	e := setup(t)
	id := put(t, e.st, "---\ntitle: Acme <intro>\ndate: 2026-11-14T10:00:00-08:00\ntimezone: America/Los_Angeles\nparticipants: [Jane Doe <jane@acme.com>]\nsource: granola\nsource_id: a\n---\n**Jane:** <script>alert(1)</script> tenant isolation\n")
	put(t, e.st, "---\ntitle: Board prep\ndate: 2026-11-01T10:00:00Z\nparticipants: [Bob]\nsource: granola\nsource_id: b\n---\nNumbers.\n")

	// Not logged in.
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if code, _, resp := e.do(t, "GET", "/", nil); code != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("unauthenticated: %d", code)
	}
	if code, _, resp := e.do(t, "GET", "/", nil, "HX-Request", "true"); code != http.StatusNoContent || resp.Header.Get("HX-Redirect") != "/login" {
		t.Fatalf("unauthenticated htmx: %d", code)
	}
	code, body, _ := e.do(t, "POST", "/login", url.Values{"challenge": {"brain-login:1:x:y"}, "signature": {"nope"}})
	if code != http.StatusUnauthorized || !strings.Contains(body, "unknown challenge") {
		t.Errorf("bad login: %d", code)
	}
	e.client.CheckRedirect = nil

	e.login(t)

	// Full list page.
	code, body, resp := e.do(t, "GET", "/", nil)
	if code != 200 || !strings.Contains(body, "Acme &lt;intro&gt;") || !strings.Contains(body, "Board prep") || !strings.Contains(body, `id="filters"`) {
		t.Fatalf("list: %d\n%s", code, body)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("CSP = %q", csp)
	}
	if !strings.Contains(body, "Sat 14 Nov 2026, 10:00 PST") {
		t.Error("date not shown in the meeting's timezone")
	}

	// Search as a partial.
	_, body, _ = e.do(t, "GET", "/?q=tenant", nil, "HX-Request", "true", "HX-Request-Type", "partial")
	if strings.Contains(body, "<html") || !strings.Contains(body, "Acme") || strings.Contains(body, "Board prep") {
		t.Errorf("search partial:\n%s", body)
	}

	// Sort from the list (htmx): row moves, list re-renders with the filters.
	_, body, _ = e.do(t, "POST", "/transcripts/"+id+"/type", url.Values{"type": {"advisor"}, "folder": {"unsorted"}},
		"HX-Request", "true", "HX-Request-Type", "partial")
	if !strings.Contains(body, "Moved to advisor.") || strings.Contains(body, "Acme") || !strings.Contains(body, "Board prep") {
		t.Errorf("set type partial:\n%s", body)
	}

	// Detail page: rendered markdown with HTML escaped.
	code, body, _ = e.do(t, "GET", "/transcripts/"+id, nil)
	if code != 200 || !strings.Contains(body, "<strong>Jane:</strong>") || strings.Contains(body, "<script>alert") || !strings.Contains(body, `value="advisor"`) {
		t.Errorf("detail: %d\n%s", code, body)
	}

	// Plain form: invalid type shows an error, valid "save and next" goes to the next unsorted.
	code, body, _ = e.do(t, "POST", "/transcripts/"+id+"/type", url.Values{"type": {"bad type"}, "then": {"stay"}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "Not saved") {
		t.Errorf("invalid type: %d", code)
	}
	e.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, _, resp = e.do(t, "POST", "/transcripts/"+id+"/type", url.Values{"type": {"investor"}, "then": {"next"}})
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/transcripts/") || strings.Contains(loc, id) {
		t.Errorf("next unsorted redirect = %q", loc)
	}
	if code, _, _ := e.do(t, "GET", "/transcripts/000000000000", nil); code != http.StatusNotFound {
		t.Errorf("missing transcript: %d", code)
	}

	// Cross-origin form posts are refused.
	if code, _, _ := e.do(t, "POST", "/transcripts/"+id+"/type", url.Values{"type": {"advisor"}}, "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-origin post: %d", code)
	}

	// Logout ends the session.
	e.do(t, "POST", "/logout", url.Values{})
	if code, _, _ := e.do(t, "GET", "/", nil); code != http.StatusSeeOther {
		t.Errorf("after logout: %d", code)
	}
}
