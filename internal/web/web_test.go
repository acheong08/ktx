package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

type env struct {
	ts     *httptest.Server
	client *http.Client
	st     *store.Store
	remote string
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo, err := gitrepo.Open(context.Background(), gitrepo.Options{
		RemoteURL: remote, Branch: "main", Dir: filepath.Join(dir, "work"),
		AuthorName: "t", AuthorEmail: "t@e", Logger: quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(repo)
	ts := httptest.NewServer(New(st, []string{"advisor"}, quiet).Handler())
	t.Cleanup(ts.Close)
	return &env{ts: ts, st: st, remote: remote, client: &http.Client{}}
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

func TestUIFlow(t *testing.T) {
	e := setup(t)
	id := put(t, e.st, "---\ntitle: Acme <intro>\ndate: 2026-11-14T10:00:00-08:00\ntimezone: America/Los_Angeles\nparticipants: [Jane Doe <jane@acme.com>]\nsource: granola\nsource_id: a\n---\n**Jane:** <script>alert(1)</script> tenant isolation\n")
	put(t, e.st, "---\ntitle: Board prep\ndate: 2026-11-01T10:00:00Z\nparticipants: [Bob]\nsource: granola\nsource_id: b\n---\nNumbers.\n")

	// Full list page, no login needed.
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
	if code, _, _ := e.do(t, "GET", "/login", nil); code != http.StatusNotFound {
		t.Errorf("/login should be gone: %d", code)
	}

	// Search as a partial.
	_, body, _ = e.do(t, "GET", "/?q=tenant", nil, "HX-Request", "true", "HX-Request-Type", "partial")
	if strings.Contains(body, "<html") || !strings.Contains(body, "Acme") || strings.Contains(body, "Board prep") {
		t.Errorf("search partial:\n%s", body)
	}

	// Sort from the list (htmx): row moves, list re-renders with the filters.
	_, body, _ = e.do(t, "POST", "/transcripts/"+id+"/type", url.Values{"type": {"advisor"}, "folder": {"unsorted"}},
		"HX-Request", "true", "HX-Request-Type", "partial", "X-Forwarded-For", "100.64.0.3")
	if !strings.Contains(body, "Moved to advisor.") || strings.Contains(body, "Acme") || !strings.Contains(body, "Board prep") {
		t.Errorf("set type partial:\n%s", body)
	}
	// The commit names the client address Caddy forwarded.
	msg, err := exec.Command("git", "--git-dir", e.remote, "log", "-1", "--format=%B", "main").CombinedOutput()
	if err != nil || !strings.Contains(string(msg), "Changed-By: web UI (100.64.0.3)") {
		t.Errorf("commit message:\n%s", msg)
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

	// Cross-origin form posts are refused, so other sites can't use a
	// visitor's browser to change things.
	if code, _, _ := e.do(t, "POST", "/transcripts/"+id+"/type", url.Values{"type": {"advisor"}}, "Origin", "https://evil.example", "Sec-Fetch-Site", "cross-site"); code != http.StatusForbidden {
		t.Errorf("cross-origin post: %d", code)
	}
}

func TestActor(t *testing.T) {
	for _, c := range []struct{ remote, xff, want string }{
		{"127.0.0.1:5000", "100.64.0.3", "web UI (100.64.0.3)"},
		{"127.0.0.1:5000", "100.64.0.3, 10.0.0.1", "web UI (100.64.0.3)"},
		{"127.0.0.1:5000", "", "web UI (127.0.0.1)"},
		{"100.64.0.9:5000", "1.2.3.4", "web UI (100.64.0.9)"}, // not from the proxy: header ignored
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := actor(r); got != c.want {
			t.Errorf("actor(%s, %q) = %q, want %q", c.remote, c.xff, got, c.want)
		}
	}
}
