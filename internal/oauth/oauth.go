// Package oauth is ktx's OAuth 2.1 authorization server, for apps that use
// the MCP server (/mcp). It's what MCP clients expect:
//
//   - protected resource metadata (RFC 9728) pointing at ktx itself;
//   - authorization server metadata (RFC 8414);
//   - dynamic client registration (RFC 7591), public clients only;
//   - authorization code with PKCE (S256), and rotating refresh tokens.
//
// People approve an app on a consent page in the web UI (package web), after
// signing in through the identity provider as usual. Grants are kept in the
// state database and can be revoked under Settings → Tokens.
package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/acheong08/ktx/internal/state"
)

// Lifetimes.
const (
	AccessTTL  = time.Hour
	RefreshTTL = 60 * 24 * time.Hour // unused this long, an app must ask again
	codeTTL    = 5 * time.Minute

	// Scope is the only scope: read transcripts and calendars.
	Scope = "mcp"
)

// Provider is the authorization server.
type Provider struct {
	db   *state.DB
	base string // ktx's public URL, no trailing slash; also the issuer
	log  *slog.Logger
	now  func() time.Time

	mu    sync.Mutex
	codes map[string]code // by code; single use, kept in memory
}

type code struct {
	userID      int64
	clientID    string
	redirectURI string
	challenge   string
	expires     time.Time
}

// New makes a Provider. publicURL is ktx's own base URL (BRAIN_PUBLIC_URL).
func New(db *state.DB, publicURL string, log *slog.Logger) *Provider {
	return &Provider{db: db, base: strings.TrimSuffix(publicURL, "/"), log: log, now: time.Now, codes: map[string]code{}}
}

// SetClock replaces time.Now, for tests.
func (p *Provider) SetClock(now func() time.Time) { p.now = now }

// ResourceURL is the MCP endpoint, the resource tokens are for.
func (p *Provider) ResourceURL() string { return p.base + "/mcp" }

// ResourceMetadataURL is where clients learn how to get a token for /mcp.
func (p *Provider) ResourceMetadataURL() string {
	return p.base + "/.well-known/oauth-protected-resource/mcp"
}

// Register adds the endpoints that don't need a browser session. The
// consent page (/oauth/authorize) is served by the web UI.
func (p *Provider) Register(mux *http.ServeMux) {
	mux.HandleFunc("/.well-known/oauth-protected-resource", cors(p.resourceMetadata))
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", cors(p.resourceMetadata))
	mux.HandleFunc("/.well-known/oauth-authorization-server", cors(p.serverMetadata))
	mux.HandleFunc("/oauth/register", cors(p.register))
	mux.HandleFunc("/oauth/token", cors(p.token))
}

// cors lets browser-based MCP clients use the endpoints. None of them use
// cookies, so any origin is fine.
func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Protocol-Version")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

func (p *Provider) resourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 p.ResourceURL(),
		"resource_name":            "ktx",
		"authorization_servers":    []string{p.base},
		"scopes_supported":         []string{Scope},
		"bearer_methods_supported": []string{"header"},
	})
}

func (p *Provider) serverMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                         p.base,
		"authorization_endpoint":                         p.base + "/oauth/authorize",
		"token_endpoint":                                 p.base + "/oauth/token",
		"registration_endpoint":                          p.base + "/oauth/register",
		"scopes_supported":                               []string{Scope},
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"code_challenge_methods_supported":               []string{"S256"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// --- registration ----------------------------------------------------------

var (
	schemeRe = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)
	hostRe   = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]+)?$|^\[[0-9A-Fa-f:.]+\](:[0-9]+)?$`)
)

// CheckRedirectURI accepts the redirect URIs apps may register: https or
// http (ktx is only reachable on the tailnet, and the consent page names
// where the browser will be sent), or an app's own scheme. Browser-dangerous
// schemes are refused, and hosts are kept simple so the address can go in a
// Content-Security-Policy.
func CheckRedirectURI(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return fmt.Errorf("%q is not an absolute URL", raw)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Errorf("%q has a fragment", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "javascript", "data", "file", "vbscript", "blob", "about", "filesystem":
		return fmt.Errorf("%q: scheme %s isn't allowed", raw, scheme)
	}
	if !schemeRe.MatchString(scheme) {
		return fmt.Errorf("%q: invalid scheme", raw)
	}
	if (scheme == "http" || scheme == "https") && !hostRe.MatchString(u.Host) {
		return fmt.Errorf("%q: invalid host", raw)
	}
	if u.Host != "" && !hostRe.MatchString(u.Host) {
		return fmt.Errorf("%q: invalid host", raw)
	}
	return nil
}

// RedirectOrigin is the CSP source for a redirect URI: scheme://host for
// web addresses, scheme: otherwise.
func RedirectOrigin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Host != "" {
		return strings.ToLower(u.Scheme) + "://" + u.Host
	}
	return strings.ToLower(u.Scheme) + ":"
}

func (p *Provider) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be JSON client metadata")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "give between 1 and 10 redirect_uris")
		return
	}
	for _, u := range req.RedirectURIs {
		if err := CheckRedirectURI(u); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", err.Error())
			return
		}
	}
	name := strings.Join(strings.Fields(req.ClientName), " ")
	if name == "" {
		name = "Unnamed app"
	}
	if r := []rune(name); len(r) > 80 {
		name = string(r[:80])
	}
	c, err := p.db.CreateOAuthClient(name, req.RedirectURIs)
	if err != nil {
		p.log.Error("registering oauth client", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	p.log.Info("oauth client registered", "client", c.ID, "name", c.Name, "redirect_uris", c.RedirectURIs)
	// Whatever authentication method was asked for, the client is public.
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.CreatedAt.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"scope":                      Scope,
	})
}

// --- authorization ---------------------------------------------------------

// Request is a checked authorization request.
type Request struct {
	Client      state.OAuthClient
	RedirectURI string
	State       string
	Challenge   string
}

// RedirectError is a problem to report to the app, by redirecting to it.
type RedirectError struct {
	Code, Description string
}

func (e *RedirectError) Error() string { return e.Code + ": " + e.Description }

// ParseAuthorize checks an authorization request. If the app or its
// redirect URI can't be trusted, the error is a plain error, to show to the
// person; otherwise problems are a *RedirectError, to send back to the app
// with Deny.
func (p *Provider) ParseAuthorize(q url.Values) (Request, error) {
	var req Request
	c, err := p.db.OAuthClientByID(q.Get("client_id"))
	if errors.Is(err, state.ErrNotFound) {
		return req, errors.New("this app isn't registered with ktx (unknown client_id); start the connection again from the app")
	}
	if err != nil {
		return req, err
	}
	req.Client = c
	req.RedirectURI = q.Get("redirect_uri")
	if req.RedirectURI == "" && len(c.RedirectURIs) == 1 {
		req.RedirectURI = c.RedirectURIs[0]
	}
	registered := false
	for _, u := range c.RedirectURIs {
		registered = registered || u == req.RedirectURI
	}
	if !registered {
		return req, errors.New("the app asked to return to an address it didn't register (redirect_uri)")
	}
	req.State = q.Get("state")
	req.Challenge = q.Get("code_challenge")

	switch {
	case q.Get("response_type") != "code":
		return req, &RedirectError{"unsupported_response_type", "only response_type=code is supported"}
	case req.Challenge == "" || q.Get("code_challenge_method") != "S256":
		return req, &RedirectError{"invalid_request", "PKCE with code_challenge_method=S256 is required"}
	case len(req.Challenge) < 43 || len(req.Challenge) > 128:
		return req, &RedirectError{"invalid_request", "code_challenge has the wrong length"}
	}
	if res := q.Get("resource"); res != "" && !p.isResource(res) {
		return req, &RedirectError{"invalid_target", "this server only issues tokens for " + p.ResourceURL()}
	}
	for _, s := range strings.Fields(q.Get("scope")) {
		if s != Scope {
			return req, &RedirectError{"invalid_scope", "the only scope is " + Scope}
		}
	}
	return req, nil
}

func (p *Provider) isResource(res string) bool {
	res = strings.TrimSuffix(res, "/")
	return res == p.ResourceURL() || res == p.base
}

// Approve issues a code for a signed-in user and returns where to send
// the browser.
func (p *Provider) Approve(req Request, user state.User) string {
	c := random()
	p.mu.Lock()
	now := p.now()
	for k, v := range p.codes {
		if now.After(v.expires) {
			delete(p.codes, k)
		}
	}
	p.codes[c] = code{userID: user.ID, clientID: req.Client.ID, redirectURI: req.RedirectURI,
		challenge: req.Challenge, expires: now.Add(codeTTL)}
	p.mu.Unlock()
	p.log.Info("oauth app approved", "user", user.Display(), "client", req.Client.ID, "name", req.Client.Name)
	return p.redirect(req, url.Values{"code": {c}})
}

// Deny returns where to send the browser to report err (or a refusal, if
// err is nil) to the app.
func (p *Provider) Deny(req Request, err *RedirectError) string {
	if err == nil {
		err = &RedirectError{"access_denied", "the person declined"}
	}
	return p.redirect(req, url.Values{"error": {err.Code}, "error_description": {err.Description}})
}

func (p *Provider) redirect(req Request, v url.Values) string {
	if req.State != "" {
		v.Set("state", req.State)
	}
	v.Set("iss", p.base)
	u, _ := url.Parse(req.RedirectURI)
	q := u.Query()
	for k, vs := range v {
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// --- tokens ----------------------------------------------------------------

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	r.Body = io.NopCloser(io.LimitReader(r.Body, 64<<10))
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "the body must be form-encoded")
		return
	}
	clientID := r.PostForm.Get("client_id")
	if clientID == "" { // a client that sends Basic auth anyway
		clientID, _, _ = r.BasicAuth()
	}
	if res := r.PostForm.Get("resource"); res != "" && !p.isResource(res) {
		oauthError(w, http.StatusBadRequest, "invalid_target", "this server only issues tokens for "+p.ResourceURL())
		return
	}

	var toks state.OAuthTokens
	var err error
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		p.mu.Lock()
		c, ok := p.codes[r.PostForm.Get("code")]
		delete(p.codes, r.PostForm.Get("code")) // single use, even when the exchange fails
		p.mu.Unlock()
		switch {
		case !ok || p.now().After(c.expires):
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code is unknown, used or expired")
			return
		case c.clientID != clientID:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the code was issued to another client")
			return
		case r.PostForm.Get("redirect_uri") != "" && r.PostForm.Get("redirect_uri") != c.redirectURI:
			oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri doesn't match")
			return
		case !verifyPKCE(r.PostForm.Get("code_verifier"), c.challenge):
			oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier doesn't match the code_challenge")
			return
		}
		toks, err = p.db.CreateOAuthGrant(c.userID, c.clientID, AccessTTL, RefreshTTL)
	case "refresh_token":
		toks, err = p.db.RefreshOAuthGrant(r.PostForm.Get("refresh_token"), clientID, AccessTTL, RefreshTTL)
		if errors.Is(err, state.ErrNotFound) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "the refresh token is unknown, revoked or expired")
			return
		}
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
		return
	}
	if err != nil {
		p.log.Error("issuing oauth tokens", "client", clientID, "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  toks.Access,
		"token_type":    "Bearer",
		"expires_in":    int(toks.AccessExpires.Sub(p.now()).Seconds()),
		"refresh_token": toks.Refresh,
		"scope":         Scope,
	})
}

func verifyPKCE(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(sum[:])), []byte(challenge)) == 1
}

// Verify returns the user an access token acts for.
func (p *Provider) Verify(token string) (state.User, state.OAuthGrant, error) {
	return p.db.OAuthAccessUser(token)
}

func oauthError(w http.ResponseWriter, status int, code, desc string) {
	body := map[string]string{"error": code}
	if desc != "" {
		body["error_description"] = desc
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
