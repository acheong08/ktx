// Package web serves the browser UI for reading and sorting transcripts.
// Pages are templ components; interactions use htmx 4.
//
// Sign-in is through the identity provider (Pocket ID, over OIDC) when one is
// configured; without one the UI is open to whoever can reach it, which is
// only acceptable on a private network. Cross-origin form posts are refused
// either way, so other websites can't drive the UI through a visitor's browser.
package web

//go:generate go tool templ generate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"

	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/state"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

//go:embed static
var staticFS embed.FS

// UI is the web interface.
type UI struct {
	store  *store.Store
	db     *state.DB           // nil: no sign-in, connections or API tokens
	auth   *Auth               // nil: no sign-in
	conns  *connectors.Manager // nil: no connections page
	types  []string            // suggested types, merged with the folders that exist
	log    *slog.Logger
	assets assets
	md     goldmark.Markdown
}

type assets struct{ CSS, JS, HTMX string }

// Options configure the UI. Only Store and Log are required.
type Options struct {
	Store      *store.Store
	Types      []string // type suggestions when sorting
	Log        *slog.Logger
	DB         *state.DB
	Auth       *Auth
	Connectors *connectors.Manager
}

// New builds the UI.
func New(o Options) *UI {
	return &UI{
		store: o.Store, db: o.DB, auth: o.Auth, conns: o.Connectors, types: o.Types, log: o.Log,
		assets: assets{
			CSS:  assetURL("style.css"),
			JS:   assetURL("app.js"),
			HTMX: assetURL("htmx.min.js"),
		},
		md: goldmark.New(
			goldmark.WithExtensions(extension.GFM),
			goldmark.WithRendererOptions(html.WithHardWraps()), // raw HTML stays escaped (no WithUnsafe)
		),
	}
}

// assetURL adds a content hash so assets can be cached forever.
func assetURL(name string) string {
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return "/static/" + name + "?v=" + hex.EncodeToString(sum[:4])
}

// Handler returns the UI routes, protected against cross-origin form posts.
func (u *UI) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(static)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	})))

	if u.auth != nil {
		mux.HandleFunc("GET /auth/login", u.auth.login)
		mux.HandleFunc("GET /auth/callback", u.callback)
		mux.HandleFunc("POST /auth/logout", u.logout)
		mux.HandleFunc("GET /auth/signed-out", u.signedOut)
	}

	mux.HandleFunc("GET /{$}", u.page(u.list))
	mux.HandleFunc("GET /transcripts/new", u.page(u.newTranscript))
	mux.HandleFunc("POST /transcripts", u.page(u.createTranscript))
	mux.HandleFunc("GET /transcripts/{id}", u.page(u.detail))
	mux.HandleFunc("GET /transcripts/{id}/edit", u.page(u.editTranscript))
	mux.HandleFunc("POST /transcripts/{id}", u.page(u.saveTranscript))
	mux.HandleFunc("POST /transcripts/{id}/type", u.page(u.setType))

	if u.auth != nil && u.db != nil {
		mux.HandleFunc("GET /tokens", u.page(u.tokens))
		mux.HandleFunc("POST /tokens", u.page(u.createToken))
		mux.HandleFunc("POST /tokens/{id}/delete", u.page(u.deleteToken))
	}
	if u.auth != nil && u.conns != nil {
		mux.HandleFunc("GET /connections", u.page(u.connections))
		mux.HandleFunc("GET /connections/new", u.page(u.newConnection))
		mux.HandleFunc("POST /connections", u.page(u.createConnection))
		mux.HandleFunc("GET /connections/{id}", u.page(u.connection))
		mux.HandleFunc("POST /connections/{id}", u.page(u.updateConnection))
		mux.HandleFunc("POST /connections/{id}/sync", u.page(u.syncConnection))
		mux.HandleFunc("POST /connections/{id}/delete", u.page(u.deleteConnection))
	}

	return securityHeaders(http.NewCrossOriginProtection().Handler(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

type ctxUser struct{}

// page requires a signed-in user when sign-in is configured, and puts the
// user in the request context.
func (u *UI) page(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if u.auth == nil {
			next(w, r)
			return
		}
		user, ok := u.auth.user(r)
		if !ok {
			to := "/auth/login?next=" + url.QueryEscape(r.URL.RequestURI())
			if r.Method != http.MethodGet {
				to = "/auth/login"
			}
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", to)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Redirect(w, r, to, http.StatusSeeOther)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxUser{}, user)))
	}
}

// currentUser is the signed-in user (ok is false when sign-in is off).
func currentUser(r *http.Request) (state.User, bool) {
	user, ok := r.Context().Value(ctxUser{}).(state.User)
	return user, ok
}

// chrome is what every page's header needs.
type chrome struct {
	Assets      assets
	User        string
	IsAdmin     bool
	SignIn      bool
	Connections bool
	Tokens      bool
	Nav         string // active section
}

func (u *UI) chrome(r *http.Request, nav string) chrome {
	c := chrome{Assets: u.assets, SignIn: u.auth != nil, Nav: nav}
	if user, ok := currentUser(r); ok {
		c.User, c.IsAdmin = user.Display(), user.IsAdmin
		c.Connections = u.conns != nil
		c.Tokens = u.db != nil
	}
	return c
}

func (u *UI) callback(w http.ResponseWriter, r *http.Request) {
	user, next, err := u.auth.callback(w, r)
	if err != nil {
		u.log.Warn("sign-in failed", "err", err, "remote", r.RemoteAddr)
		u.render(w, r, http.StatusUnauthorized, signInFailed(u.chrome(r, ""), err.Error()))
		return
	}
	u.log.Info("signed in", "user", user.Display(), "admin", user.IsAdmin)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	u.auth.logout(w, r)
	http.Redirect(w, r, "/auth/signed-out", http.StatusSeeOther)
}

func (u *UI) signedOut(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, http.StatusOK, signedOutPage(u.chrome(r, "")))
}

// actor describes who made a change, for commit trailers: the signed-in
// user, or without sign-in the client's address (Caddy passes the tailnet
// IP in X-Forwarded-For).
func actor(r *http.Request) string {
	if user, ok := currentUser(r); ok {
		return user.Display()
	}
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(ip); err == nil {
		ip = host
	}
	if a, err := netip.ParseAddr(ip); err == nil && a.IsLoopback() {
		if fwd, _, _ := strings.Cut(r.Header.Get("X-Forwarded-For"), ","); strings.TrimSpace(fwd) != "" {
			ip = strings.TrimSpace(fwd)
		}
	}
	return "web UI (" + ip + ")"
}

// --- transcripts list ------------------------------------------------------

type flash struct{ Kind, Text string } // Kind: ok, warn, error

type tab struct {
	Label  string
	URL    string
	Count  int
	Active bool
}

type listView struct {
	Chrome  chrome
	Q, Sort string
	Folder  string // "" = all
	Tabs    []tab
	Entries []store.Entry
	Types   []string
	Flash   flash
}

type option struct{ Value, Label string }

var sortOptions = []option{{"newest", "Newest first"}, {"oldest", "Oldest first"}, {"title", "Title A–Z"}}

func (u *UI) list(w http.ResponseWriter, r *http.Request) {
	v, err := u.listView(r, r.URL.Query().Get("q"), r.URL.Query().Get("folder"), r.URL.Query().Get("sort"))
	if err != nil {
		u.fail(w, r, err)
		return
	}
	w.Header().Set("Vary", "HX-Request, HX-Request-Type")
	if isPartial(r) {
		u.render(w, r, http.StatusOK, results(v))
		return
	}
	u.render(w, r, http.StatusOK, listPage(v))
}

func (u *UI) listView(r *http.Request, q, folder, sortBy string) (listView, error) {
	entries, err := u.store.List()
	if err != nil {
		return listView{}, err
	}
	if !slices.ContainsFunc(sortOptions, func(o option) bool { return o.Value == sortBy }) {
		sortBy = "newest"
	}
	v := listView{Chrome: u.chrome(r, "transcripts"), Q: q, Sort: sortBy, Folder: folder, Types: u.typeOptions(entries)}

	counts := map[string]int{}
	var matching []store.Entry
	for _, e := range entries {
		if !e.Matches(q) {
			continue
		}
		counts[e.Folder]++
		if folder == "" || e.Folder == folder {
			matching = append(matching, e)
		}
	}

	switch sortBy {
	case "oldest":
		sort.SliceStable(matching, func(i, j int) bool { return matching[i].Start.Before(matching[j].Start) })
	case "title":
		sort.SliceStable(matching, func(i, j int) bool {
			return strings.ToLower(matching[i].Label()) < strings.ToLower(matching[j].Label())
		})
	}
	v.Entries = matching

	folders := []string{transcript.Unsorted}
	for f := range counts {
		if f != transcript.Unsorted {
			folders = append(folders, f)
		}
	}
	sort.Strings(folders[1:])
	total := 0
	for _, f := range folders {
		total += counts[f]
	}
	v.Tabs = append(v.Tabs, tab{Label: "All", URL: listURL(q, "", sortBy), Count: total, Active: folder == ""})
	for _, f := range folders {
		v.Tabs = append(v.Tabs, tab{Label: f, URL: listURL(q, f, sortBy), Count: counts[f], Active: folder == f})
	}
	return v, nil
}

// typeOptions is the configured suggestions plus every existing folder.
func (u *UI) typeOptions(entries []store.Entry) []string {
	types := slices.Clone(u.types)
	for _, e := range entries {
		if e.Folder != transcript.Unsorted && !slices.Contains(types, e.Folder) {
			types = append(types, e.Folder)
		}
	}
	sort.Strings(types)
	return append([]string{transcript.Unsorted}, types...)
}

func listURL(q, folder, sortBy string) string {
	vals := url.Values{}
	if q != "" {
		vals.Set("q", q)
	}
	if folder != "" {
		vals.Set("folder", folder)
	}
	if sortBy != "" && sortBy != "newest" {
		vals.Set("sort", sortBy)
	}
	if len(vals) == 0 {
		return "/"
	}
	return "/?" + vals.Encode()
}

// isPartial is true for htmx requests that swap a fragment. History restores
// and body-level swaps get the full page.
func isPartial(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" &&
		r.Header.Get("HX-Request-Type") == "partial" &&
		r.Header.Get("HX-History-Restore-Request") == ""
}

// --- detail ----------------------------------------------------------------

type detailView struct {
	Chrome    chrome
	Entry     store.Entry
	BodyHTML  string
	Types     []string
	BackURL   string
	BackLabel string
	Flash     flash
}

func (u *UI) detail(w http.ResponseWriter, r *http.Request) {
	var fl flash
	switch r.URL.Query().Get("saved") {
	case "":
	case "created":
		fl = flash{"ok", "Added, committed and pushed."}
	case "unchanged":
		fl = flash{"ok", "Nothing had changed."}
	default:
		fl = flash{"ok", "Saved, committed and pushed."}
	}
	u.renderDetail(w, r, http.StatusOK, r.PathValue("id"), fl)
}

func (u *UI) renderDetail(w http.ResponseWriter, r *http.Request, status int, id string, fl flash) {
	e, err := u.store.Get(id)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	entries, err := u.store.List()
	if err != nil {
		u.fail(w, r, err)
		return
	}
	var body bytes.Buffer
	if err := u.md.Convert([]byte(e.Body), &body); err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, r, status, detailPage(detailView{
		Chrome: u.chrome(r, "transcripts"), Entry: e, BodyHTML: body.String(), Types: u.typeOptions(entries),
		BackURL: listURL("", e.Folder, ""), BackLabel: e.Folder, Flash: fl,
	}))
}

// --- sorting ---------------------------------------------------------------

func (u *UI) setType(w http.ResponseWriter, r *http.Request) {
	id, user := r.PathValue("id"), actor(r)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	typ := r.PostFormValue("type")

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	res, err := u.store.SetType(ctx, id, typ, user)

	var fl flash
	switch {
	case errors.Is(err, store.ErrNotFound):
		u.fail(w, r, err)
		return
	case err != nil:
		u.log.Warn("set type failed", "id", id, "type", typ, "user", user, "err", err)
		fl = flash{"error", "Not saved: " + err.Error()}
	case res.Result == store.ResultUnchanged:
		fl = flash{"ok", "Already there; nothing changed."}
	case res.Git.PushErr != nil:
		fl = flash{"warn", "Moved and committed, but not pushed to GitHub yet. It will be pushed on the next sync."}
	default:
		dest := strings.TrimPrefix(res.Path, store.Root+"/")
		fl = flash{"ok", "Moved to " + dest[:strings.Index(dest, "/")] + "."}
		u.log.Info("transcript moved", "id", id, "path", res.Path, "previous_path", res.PreviousPath, "user", user, "commit", res.Git.Commit)
	}

	if isPartial(r) { // from the list: re-render it with the current filters
		v, lerr := u.listView(r, r.PostFormValue("q"), r.PostFormValue("folder"), r.PostFormValue("sort"))
		if lerr != nil {
			u.fail(w, r, lerr)
			return
		}
		v.Flash = fl
		u.render(w, r, http.StatusOK, results(v))
		return
	}

	if fl.Kind == "error" {
		u.renderDetail(w, r, http.StatusUnprocessableEntity, id, fl)
		return
	}
	http.Redirect(w, r, "/transcripts/"+id+"?saved=1", http.StatusSeeOther)
}

// --- helpers ---------------------------------------------------------------

func (u *UI) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		u.log.Error("render", "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// errNotFound is a generic "no such thing" for pages.
var errNotFound = errors.New("not found")

func (u *UI) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := http.StatusInternalServerError, "Something went wrong. The details are in the server log."
	if errors.Is(err, store.ErrNotFound) {
		status, msg = http.StatusNotFound, "No transcript with that id. It may have been removed."
	} else if errors.Is(err, errNotFound) {
		status, msg = http.StatusNotFound, "Not found."
	} else {
		u.log.Error("ui", "path", r.URL.Path, "err", err)
	}
	if isPartial(r) {
		u.render(w, r, status, flashBox(flash{"error", msg}))
		return
	}
	u.render(w, r, status, errorPage(u.chrome(r, ""), msg))
}

// --- view helpers used by the templates --------------------------------------

func fmtDate(e store.Entry) string {
	if e.Start.IsZero() {
		return "—"
	}
	return e.LocalStart().Format("Mon 2 Jan 2006, 15:04 MST")
}

func participantSummary(e store.Entry) string {
	var names []string
	for _, p := range e.Meta.Participants {
		if p.Name != "" {
			names = append(names, p.Name)
		} else {
			names = append(names, p.Email)
		}
	}
	if len(names) > 3 {
		return strings.Join(names[:3], ", ") + " +" + strconv.Itoa(len(names)-3)
	}
	return strings.Join(names, ", ")
}

// typeValue is what the type field shows: the folder, blank for unsorted.
func typeValue(e store.Entry) string {
	if e.Folder == transcript.Unsorted {
		return ""
	}
	return e.Folder
}

func participantLine(p transcript.Participant) string {
	var extra []string
	for _, s := range []string{p.Role, p.Company} {
		if s != "" {
			extra = append(extra, s)
		}
	}
	s := p.Name
	if s == "" {
		s = p.Email
	}
	if len(extra) > 0 {
		s += " (" + strings.Join(extra, ", ") + ")"
	}
	return s
}
