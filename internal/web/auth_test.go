package web

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/acheong08/ktx/internal/ai"
	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/feeds"
	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/mcpserver"
	"github.com/acheong08/ktx/internal/notes"
	"github.com/acheong08/ktx/internal/oauth"
	"github.com/acheong08/ktx/internal/secrets"
	"github.com/acheong08/ktx/internal/state"
	"github.com/acheong08/ktx/internal/store"
)

// fakeIDP is a minimal OIDC provider: discovery, JWKS, and a token endpoint
// that checks PKCE and issues a signed ID token for whoever "authorized".
type fakeIDP struct {
	*httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	codes  map[string]grant
	user   idpUser // who signs in next
	client string
}

type idpUser struct {
	sub, username string
	groups        []string
}

type grant struct {
	user                       idpUser
	nonce, challenge, redirect string
}

func newIDP(t *testing.T, clientID string) *fakeIDP {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &fakeIDP{key: key, codes: map[string]grant{}, client: clientID}
	mux := http.NewServeMux()
	idp.Server = httptest.NewServer(mux)
	t.Cleanup(idp.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.URL, "authorization_endpoint": idp.URL + "/authorize", "token_endpoint": idp.URL + "/token",
			"jwks_uri": idp.URL + "/jwks", "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	// The "browser" lands here; we approve at once and redirect back with a code.
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != clientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "bad authorize request", 400)
			return
		}
		code := random()
		idp.mu.Lock()
		idp.codes[code] = grant{user: idp.user, nonce: q.Get("nonce"), challenge: q.Get("code_challenge"), redirect: q.Get("redirect_uri")}
		idp.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		idp.mu.Lock()
		g, ok := idp.codes[r.PostFormValue("code")]
		delete(idp.codes, r.PostFormValue("code"))
		idp.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge || r.PostFormValue("redirect_uri") != g.redirect {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		user, pass, _ := r.BasicAuth()
		if user != clientID || pass != "client-secret" {
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		now := time.Now()
		claims, _ := json.Marshal(map[string]any{
			"iss": idp.URL, "aud": clientID, "sub": g.user.sub, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			"nonce": g.nonce, "preferred_username": g.user.username, "name": g.user.username, "email": g.user.username + "@melzi.dev",
			"groups": g.user.groups,
		})
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, (&jose.SignerOptions{}).WithType("JWT"))
		obj, _ := signer.Sign(claims)
		raw, _ := obj.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": raw})
	})
	return idp
}

// stub connector for the connections pages.
type stub struct{}

func (stub) Kind() string            { return "stub" }
func (stub) Title() string           { return "Stub" }
func (stub) Description() string     { return "A test service." }
func (stub) Interval() time.Duration { return time.Hour }
func (stub) Fields() []connectors.Field {
	return []connectors.Field{{Name: "api_key", Label: "API key", Secret: true, Required: true}, {Name: "region", Label: "Region"}}
}
func (stub) Check(_ context.Context, c connectors.Config) (string, error) {
	if c.Secrets["api_key"] == "bad" {
		return "", errors.New("invalid key")
	}
	return "someone@stub.test", nil
}
func (stub) Sync(_ context.Context, _ connectors.Config, cursor string, _ func(store.Item) error) (string, error) {
	return "synced", nil
}

type authEnv struct {
	ts      *httptest.Server
	idp     *fakeIDP
	db      *state.DB
	st      *store.Store
	cal     *calendar.Service
	notes   *notes.Service
	feedURL string // a Luma-like iCal feed
}

// testFeed is what the fake feed server serves.
const testFeed = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Luma//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:evt-web@events.lu.ma\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261124T020000Z\r\nDTEND:20261124T040000Z\r\nSUMMARY:Luma AI Night\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// llmDelay slows the fake model, to watch titles arrive in a browser.
var llmDelay time.Duration

func setupAuth(t *testing.T) *authEnv {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo, err := gitrepo.Open(context.Background(), gitrepo.Options{RemoteURL: remote, Branch: "main", Dir: filepath.Join(dir, "work"), AuthorName: "t", AuthorEmail: "t@e", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := store.New(repo)
	st.SetLinker(calendar.Linker{})
	cal := calendar.NewService(repo, st.Relink)
	box, _ := secrets.New(make([]byte, 32))
	mgr := connectors.NewManager(db, box, st, quiet, stub{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := mgr.Start(ctx); err != nil {
		t.Fatal(err)
	}

	idp := newIDP(t, "ktx")
	var handler http.Handler
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(ts.Close)
	auth := NewAuth(OIDCConfig{Issuer: idp.URL, ClientID: "ktx", ClientSecret: "client-secret", PublicURL: ts.URL, AdminGroup: "ktx-admins"}, db, quiet)
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(llmDelay)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","object":"chat.completion","created":0,"model":"nw-flash","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"Acme pricing follow-up"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(llm.Close)
	aiSettings := ai.NewSettings(db, box)
	aiSettings.SetBaseURL(llm.URL + "/v1/")
	nts := notes.New(repo, db, aiSettings, quiet)
	nts.SetTranscriptCheck(func(id string) bool { _, err := st.Get(id); return err == nil })
	nts.SetTiming(50*time.Millisecond, time.Millisecond)
	nts.Start(ctx)

	feedSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "t0k" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, testFeed)
	}))
	t.Cleanup(feedSrv.Close)
	subs := feeds.New(db, box, cal, filepath.Join(dir, "feeds"), quiet, feeds.WithHTTPClient(feedSrv.Client()))
	t.Cleanup(func() { calendar.SetFeedDir("") })

	mux := http.NewServeMux()
	prov := oauth.New(db, ts.URL, quiet)
	prov.Register(mux)
	mcpserver.New(st, cal, nts, prov, "test", quiet).Register(mux)
	mux.Handle("/", New(Options{Store: st, Log: quiet, DB: db, Auth: auth, Connectors: mgr, Calendar: cal, OAuth: prov, Notes: nts, AI: aiSettings, Feeds: subs}).Handler())
	handler = mux
	return &authEnv{ts: ts, idp: idp, db: db, st: st, cal: cal, notes: nts, feedURL: feedSrv.URL + "/ics?token=t0k"}
}

func (e *authEnv) browser() *env {
	jar, _ := cookiejar.New(nil)
	return &env{ts: e.ts, client: &http.Client{Jar: jar}}
}

// signIn runs the whole redirect dance as a browser would.
func (e *authEnv) signIn(t *testing.T, b *env, u idpUser, next string) (int, string) {
	t.Helper()
	e.idp.mu.Lock()
	e.idp.user = u
	e.idp.mu.Unlock()
	code, body, _ := b.do(t, "GET", next, nil)
	return code, body
}

func TestSignInFlow(t *testing.T) {
	e := setupAuth(t)
	b := e.browser()

	// Not signed in: pages send you to the identity provider and back.
	b.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	_, _, resp := b.do(t, "GET", "/settings/connections", nil)
	if loc := resp.Header.Get("Location"); loc != "/auth/login?next=%2Fsettings%2Fconnections" {
		t.Fatalf("redirect = %q", loc)
	}
	if code, _, resp := b.do(t, "GET", "/", nil, "HX-Request", "true"); code != http.StatusNoContent || !strings.HasPrefix(resp.Header.Get("HX-Redirect"), "/auth/login") {
		t.Errorf("htmx unauthenticated: %d", code)
	}
	b.client.CheckRedirect = nil

	code, body := e.signIn(t, b, idpUser{sub: "u1", username: "antonio", groups: []string{"melzi", "ktx-admins"}}, "/settings/connections")
	if code != 200 || !strings.Contains(body, "<h1>Connections</h1>") || !strings.Contains(body, "antonio") || !strings.Contains(body, `class="badge">admin`) {
		t.Fatalf("after sign-in: %d\n%s", code, body)
	}
	users, _ := e.db.Users()
	if len(users) != 1 || !users[0].IsAdmin || users[0].Email != "antonio@melzi.dev" {
		t.Errorf("users: %+v", users)
	}

	// Sign out ends the session.
	b.do(t, "POST", "/auth/logout", url.Values{})
	b.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if code, _, _ := b.do(t, "GET", "/", nil); code != http.StatusSeeOther {
		t.Errorf("after sign-out: %d", code)
	}

	// A member (not in the admin group) is not an admin.
	m := e.browser()
	if _, body := e.signIn(t, m, idpUser{sub: "u2", username: "maxim", groups: []string{"melzi"}}, "/"); strings.Contains(body, `class="badge">admin`) {
		t.Error("member shown as admin")
	}
}

func TestSignInRejectsForgedCallback(t *testing.T) {
	e := setupAuth(t)
	b := e.browser()
	// A callback this browser didn't start (no login cookie) is refused.
	code, body, _ := b.do(t, "GET", "/auth/callback?code=x&state=y", nil)
	if code != http.StatusUnauthorized || !strings.Contains(body, "another browser") {
		t.Errorf("forged callback: %d", code)
	}
	if code, _, _ := b.do(t, "GET", "/auth/callback?error=access_denied&error_description=no", nil); code != http.StatusUnauthorized {
		t.Errorf("provider error: %d", code)
	}
	if got := safeNext("//evil.example/x"); got != "/" {
		t.Errorf("open redirect: %q", got)
	}
}

func TestConnectionsAndTokensPages(t *testing.T) {
	e := setupAuth(t)
	admin, member := e.browser(), e.browser()
	e.signIn(t, admin, idpUser{sub: "u1", username: "antonio", groups: []string{"ktx-admins"}}, "/")
	e.signIn(t, member, idpUser{sub: "u2", username: "maxim"}, "/")

	// The form for a new connection, then a rejected and an accepted credential.
	_, body, _ := member.do(t, "GET", "/settings/connections/new?kind=stub", nil)
	if !strings.Contains(body, `value="Maxim&#39;s Stub"`) || !strings.Contains(body, `type="password"`) {
		t.Fatalf("new form:\n%s", body)
	}
	code, body, _ := member.do(t, "POST", "/settings/connections", url.Values{"kind": {"stub"}, "label": {"Maxim's Stub"}, "field_api_key": {"bad"}, "field_region": {"eu"}})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "rejected the credential") || strings.Contains(body, `value="bad"`) || !strings.Contains(body, `value="eu"`) {
		t.Fatalf("bad key: %d\n%s", code, body)
	}
	code, body, _ = member.do(t, "POST", "/settings/connections", url.Values{"kind": {"stub"}, "label": {"Maxim's Stub"}, "field_api_key": {"stub_key_0123456789"}})
	if code != 200 || !strings.Contains(body, "Connection added") || !strings.Contains(body, "someone@stub.test") {
		t.Fatalf("add: %d\n%s", code, body)
	}
	conns, _ := e.db.Connections()
	if len(conns) != 1 || conns[0].Owner != "maxim" {
		t.Fatalf("stored: %+v", conns)
	}
	id := fmt.Sprint(conns[0].ID)

	// The stored key never comes back; only its last characters.
	_, body, _ = member.do(t, "GET", "/settings/connections/"+id, nil)
	if strings.Contains(body, "stub_key_0123456789") || !strings.Contains(body, "stored (…6789)") {
		t.Errorf("detail leaks or lacks the hint:\n%s", body)
	}

	// Someone else's connection: visible, but only the owner or an admin may change it.
	other := e.browser()
	e.signIn(t, other, idpUser{sub: "u3", username: "guest"}, "/")
	if code, _, _ := other.do(t, "POST", "/settings/connections/"+id, url.Values{"label": {"mine now"}}); code != http.StatusForbidden {
		t.Errorf("non-owner edit: %d", code)
	}
	if code, _, _ := admin.do(t, "POST", "/settings/connections/"+id, url.Values{"label": {"Renamed by admin"}, "enabled": {"on"}}); code != 200 {
		t.Errorf("admin edit: %d", code)
	}
	if c, _ := e.db.Connection(conns[0].ID); c.Label != "Renamed by admin" {
		t.Errorf("label = %q", c.Label)
	}
	if code, body, _ := member.do(t, "POST", "/settings/connections/"+id+"/delete", url.Values{}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "Tick the box") {
		t.Errorf("delete without confirm: %d", code)
	}
	if code, body, _ := member.do(t, "POST", "/settings/connections/"+id+"/delete", url.Values{"confirm": {"on"}}); code != 200 || !strings.Contains(body, "Connection deleted") {
		t.Errorf("delete: %d", code)
	}

	// API tokens: shown once, then only listed; revoking removes them.
	code, body, _ = member.do(t, "POST", "/settings/tokens", url.Values{"name": {"import script"}})
	tok := regexp.MustCompile(`<pre id="new-token-api">(ktx_[^<]+)</pre>`).FindStringSubmatch(body)
	if code != 200 || tok == nil {
		t.Fatalf("create token: %d\n%s", code, body)
	}
	if u, err := e.db.APITokenUser(tok[1]); err != nil || u.Username != "maxim" {
		t.Errorf("token owner: %+v %v", u, err)
	}
	_, body, _ = member.do(t, "GET", "/settings/tokens", nil)
	if strings.Contains(body, tok[1]) || !strings.Contains(body, "import script") {
		t.Errorf("token list leaks the token or misses it")
	}
	list, _ := e.db.APITokens(1 + 1) // maxim is user 2
	member.do(t, "POST", fmt.Sprintf("/settings/tokens/%d/delete", list[0].ID), url.Values{})
	if _, err := e.db.APITokenUser(tok[1]); err == nil {
		t.Error("revoked token still works")
	}
}
