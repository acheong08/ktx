package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/acheong08/ktx/internal/oauth"
	"github.com/acheong08/ktx/internal/state"
)

// The consent page of the OAuth authorization server (package oauth): an
// app (an MCP client) asks to read ktx as the signed-in person.

type authorizeView struct {
	Chrome      chrome
	App         string
	Destination string // where the browser goes afterwards: host, or the app's scheme
	Request     string // the authorization request, url-encoded, to post back
}

func (u *UI) authorize(w http.ResponseWriter, r *http.Request) {
	req, err := u.oauth.ParseAuthorize(r.URL.Query())
	var rerr *oauth.RedirectError
	if errors.As(err, &rerr) {
		http.Redirect(w, r, u.oauth.Deny(req, rerr), http.StatusSeeOther)
		return
	}
	if err != nil {
		u.render(w, r, http.StatusBadRequest, errorPage(u.chrome(r, ""), err.Error()))
		return
	}
	dest, _ := url.Parse(req.RedirectURI)
	v := authorizeView{Chrome: u.chrome(r, ""), App: req.Client.Name, Destination: dest.Host, Request: r.URL.Query().Encode()}
	if v.Destination == "" {
		v.Destination = dest.Scheme + ":"
	}
	// Browsers apply form-action to the redirect that follows the form
	// post, so the app's address has to be allowed on this page.
	h := w.Header()
	h.Set("Content-Security-Policy", strings.Replace(h.Get("Content-Security-Policy"),
		"form-action 'self'", "form-action 'self' "+oauth.RedirectOrigin(req.RedirectURI), 1))
	h.Set("Cache-Control", "no-store")
	u.render(w, r, http.StatusOK, authorizePage(v))
}

func (u *UI) authorizeDecision(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	q, err := url.ParseQuery(r.PostFormValue("request"))
	if err != nil {
		u.render(w, r, http.StatusBadRequest, errorPage(u.chrome(r, ""), "The request was garbled; start again from the app."))
		return
	}
	req, err := u.oauth.ParseAuthorize(q)
	var rerr *oauth.RedirectError
	switch {
	case errors.As(err, &rerr):
		http.Redirect(w, r, u.oauth.Deny(req, rerr), http.StatusSeeOther)
	case err != nil:
		u.render(w, r, http.StatusBadRequest, errorPage(u.chrome(r, ""), err.Error()))
	case r.PostFormValue("decision") == "allow":
		http.Redirect(w, r, u.oauth.Approve(req, user), http.StatusSeeOther)
	default:
		http.Redirect(w, r, u.oauth.Deny(req, nil), http.StatusSeeOther)
	}
}

func (u *UI) revokeApp(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := u.db.DeleteOAuthGrant(user.ID, id); err != nil && !errors.Is(err, state.ErrNotFound) {
		u.fail(w, r, err)
		return
	}
	u.log.Info("oauth app revoked", "user", user.Display(), "grant", id)
	http.Redirect(w, r, "/settings/tokens#apps", http.StatusSeeOther)
}
