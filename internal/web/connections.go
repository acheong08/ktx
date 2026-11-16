package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/state"
)

// --- connections -------------------------------------------------------------

type connRow struct {
	Conn    state.Connection
	Title   string
	Last    *state.Run
	Syncing bool
	CanEdit bool
}

type connectionsView struct {
	Chrome  chrome
	Rows    []connRow
	Kinds   []connectors.Connector
	NoKey   bool // no master key: credentials can't be stored
	Polling bool // something is syncing: refresh the table
	Flash   flash
}

func (u *UI) connectionsView(r *http.Request, fl flash) (connectionsView, error) {
	user, _ := currentUser(r)
	conns, err := u.db.Connections()
	if err != nil {
		return connectionsView{}, err
	}
	v := connectionsView{Chrome: u.chrome(r, "connections"), Kinds: u.conns.Kinds(), NoKey: !u.conns.CanStoreSecrets(), Flash: fl}
	for _, c := range conns {
		row := connRow{Conn: c, Title: c.Kind, Syncing: u.conns.Syncing(c.ID), CanEdit: canEdit(user, c)}
		if k, ok := u.conns.Kind(c.Kind); ok {
			row.Title = k.Title()
		}
		if runs, _ := u.db.Runs(c.ID, 1); len(runs) == 1 {
			row.Last = &runs[0]
			if runs[0].FinishedAt.IsZero() {
				row.Syncing = true
			}
		}
		v.Polling = v.Polling || row.Syncing
		v.Rows = append(v.Rows, row)
	}
	return v, nil
}

func (u *UI) connections(w http.ResponseWriter, r *http.Request) {
	var fl flash
	switch r.URL.Query().Get("done") {
	case "created":
		fl = flash{"ok", "Connection added. Its first sync has started."}
	case "deleted":
		fl = flash{"ok", "Connection deleted. What it already stored stays in git."}
	case "sync":
		fl = flash{"ok", "Sync started."}
	}
	v, err := u.connectionsView(r, fl)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if isPartial(r) {
		u.render(w, r, http.StatusOK, connectionsTable(v))
		return
	}
	u.render(w, r, http.StatusOK, connectionsPage(v))
}

type connFormView struct {
	Chrome   chrome
	Kind     connectors.Connector
	Conn     *state.Connection // nil when adding
	Label    string
	Values   map[string]string // non-secret values to show again
	Enabled  bool
	Problems []string
	Runs     []state.Run
	CanEdit  bool
	Syncing  bool
}

func (u *UI) newConnection(w http.ResponseWriter, r *http.Request) {
	kind, ok := u.conns.Kind(r.URL.Query().Get("kind"))
	if !ok {
		u.fail(w, r, errNotFound)
		return
	}
	user, _ := currentUser(r)
	u.render(w, r, http.StatusOK, connectionFormPage(connFormView{
		Chrome: u.chrome(r, "connections"), Kind: kind, Label: possessive(user.Display()) + " " + kind.Title(),
		Values: map[string]string{}, Enabled: true, CanEdit: true,
	}))
}

func (u *UI) createConnection(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	kind, ok := u.conns.Kind(r.PostFormValue("kind"))
	if !ok {
		u.fail(w, r, errNotFound)
		return
	}
	values := formValues(r, kind)
	label := r.PostFormValue("label")
	id, err := u.conns.Create(r.Context(), user, kind.Kind(), label, values)
	if err != nil {
		u.log.Warn("adding connection failed", "kind", kind.Kind(), "user", user.Display(), "err", err)
		u.render(w, r, http.StatusUnprocessableEntity, connectionFormPage(connFormView{
			Chrome: u.chrome(r, "connections"), Kind: kind, Label: label, Values: publicValues(kind, values),
			Enabled: true, Problems: problems(err), CanEdit: true,
		}))
		return
	}
	u.log.Info("connection added", "id", id, "kind", kind.Kind(), "user", user.Display())
	http.Redirect(w, r, "/connections?done=created", http.StatusSeeOther)
}

func (u *UI) loadConnection(w http.ResponseWriter, r *http.Request) (state.Connection, connectors.Connector, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		u.fail(w, r, errNotFound)
		return state.Connection{}, nil, false
	}
	c, err := u.db.Connection(id)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			err = errNotFound
		}
		u.fail(w, r, err)
		return state.Connection{}, nil, false
	}
	kind, ok := u.conns.Kind(c.Kind)
	if !ok {
		u.fail(w, r, errors.New("connector "+c.Kind+" is not available in this build"))
		return state.Connection{}, nil, false
	}
	return c, kind, true
}

func (u *UI) connection(w http.ResponseWriter, r *http.Request) {
	c, kind, ok := u.loadConnection(w, r)
	if !ok {
		return
	}
	u.renderConnection(w, r, http.StatusOK, c, kind, nil, c.Label, c.Settings, c.Enabled)
}

func (u *UI) renderConnection(w http.ResponseWriter, r *http.Request, status int, c state.Connection, kind connectors.Connector,
	probs []string, label string, values map[string]string, enabled bool) {
	user, _ := currentUser(r)
	runs, _ := u.db.Runs(c.ID, 20)
	u.render(w, r, status, connectionFormPage(connFormView{
		Chrome: u.chrome(r, "connections"), Kind: kind, Conn: &c, Label: label, Values: values,
		Enabled: enabled, Problems: probs, Runs: runs, CanEdit: canEdit(user, c), Syncing: u.conns.Syncing(c.ID),
	}))
}

func (u *UI) updateConnection(w http.ResponseWriter, r *http.Request) {
	c, kind, ok := u.loadConnection(w, r)
	if !ok {
		return
	}
	user, _ := currentUser(r)
	if !canEdit(user, c) {
		u.forbidden(w, r)
		return
	}
	values := formValues(r, kind)
	label, enabled := r.PostFormValue("label"), r.PostFormValue("enabled") == "on"
	if err := u.conns.Update(r.Context(), c.ID, label, enabled, values); err != nil {
		u.renderConnection(w, r, http.StatusUnprocessableEntity, c, kind, problems(err), label, publicValues(kind, values), enabled)
		return
	}
	u.log.Info("connection updated", "id", c.ID, "user", user.Display())
	http.Redirect(w, r, "/connections/"+strconv.FormatInt(c.ID, 10), http.StatusSeeOther)
}

func (u *UI) syncConnection(w http.ResponseWriter, r *http.Request) {
	c, _, ok := u.loadConnection(w, r)
	if !ok {
		return
	}
	if !u.conns.SyncNow(c.ID) {
		u.render(w, r, http.StatusConflict, errorPage(u.chrome(r, "connections"), "This connection isn't running (it's disabled, or no master key is configured)."))
		return
	}
	to := "/connections?done=sync"
	if r.PostFormValue("back") == "detail" {
		to = "/connections/" + strconv.FormatInt(c.ID, 10)
	}
	// Give the run a moment to register, so the next page shows it.
	time.Sleep(300 * time.Millisecond)
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (u *UI) deleteConnection(w http.ResponseWriter, r *http.Request) {
	c, kind, ok := u.loadConnection(w, r)
	if !ok {
		return
	}
	user, _ := currentUser(r)
	if !canEdit(user, c) {
		u.forbidden(w, r)
		return
	}
	if r.PostFormValue("confirm") != "on" {
		u.renderConnection(w, r, http.StatusUnprocessableEntity, c, kind, []string{"Tick the box to confirm deleting this connection."}, c.Label, c.Settings, c.Enabled)
		return
	}
	if err := u.conns.Delete(c.ID); err != nil {
		u.fail(w, r, err)
		return
	}
	u.log.Info("connection deleted", "id", c.ID, "kind", c.Kind, "user", user.Display())
	http.Redirect(w, r, "/connections?done=deleted", http.StatusSeeOther)
}

// canEdit: the owner and admins may change or delete a connection.
func canEdit(user state.User, c state.Connection) bool { return user.IsAdmin || user.ID == c.OwnerID }

func formValues(r *http.Request, kind connectors.Connector) map[string]string {
	values := map[string]string{}
	for _, f := range kind.Fields() {
		values[f.Name] = r.PostFormValue("field_" + f.Name)
	}
	return values
}

// publicValues drops secrets, so they are never echoed back into a page.
func publicValues(kind connectors.Connector, values map[string]string) map[string]string {
	out := map[string]string{}
	for _, f := range kind.Fields() {
		if !f.Secret {
			out[f.Name] = values[f.Name]
		}
	}
	return out
}

func problems(err error) []string {
	var fe *connectors.FormError
	if errors.As(err, &fe) {
		return fe.Problems
	}
	return []string{err.Error()}
}

func possessive(name string) string {
	if name == "" {
		return "My"
	}
	name = strings.ToUpper(name[:1]) + name[1:]
	if strings.HasSuffix(name, "s") {
		return name + "'"
	}
	return name + "'s"
}

// --- API tokens --------------------------------------------------------------

type tokensView struct {
	Chrome chrome
	Tokens []state.APIToken
	New    string // a token just created, shown once
	Name   string
	Error  string
}

func (u *UI) tokensView(r *http.Request) (tokensView, error) {
	user, _ := currentUser(r)
	toks, err := u.db.APITokens(user.ID)
	return tokensView{Chrome: u.chrome(r, "tokens"), Tokens: toks}, err
}

func (u *UI) tokens(w http.ResponseWriter, r *http.Request) {
	v, err := u.tokensView(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, r, http.StatusOK, tokensPage(v))
}

func (u *UI) createToken(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		v, _ := u.tokensView(r)
		v.Error = "Give the token a name, e.g. what will use it."
		u.render(w, r, http.StatusUnprocessableEntity, tokensPage(v))
		return
	}
	tok, err := u.db.CreateAPIToken(user.ID, name)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.log.Info("API token created", "user", user.Display(), "name", name)
	v, err := u.tokensView(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	v.New, v.Name = tok, name
	w.Header().Set("Cache-Control", "no-store")
	u.render(w, r, http.StatusOK, tokensPage(v))
}

func (u *UI) deleteToken(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err := u.db.DeleteAPIToken(user.ID, id); err != nil && !errors.Is(err, state.ErrNotFound) {
		u.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/tokens", http.StatusSeeOther)
}

func (u *UI) forbidden(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, http.StatusForbidden, errorPage(u.chrome(r, ""), "Only the connection's owner or an admin can change it."))
}

// --- view helpers --------------------------------------------------------------

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + " min ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + " h ago"
	default:
		return t.Local().Format("2 Jan 2006")
	}
}

func runSummary(r state.Run) string {
	if r.FinishedAt.IsZero() {
		return "running"
	}
	if r.Error != "" {
		return "failed"
	}
	if r.Created+r.Updated == 0 {
		return "nothing new"
	}
	return strconv.Itoa(r.Created) + " new, " + strconv.Itoa(r.Updated) + " updated"
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func formAction(v connFormView) templ.SafeURL {
	if v.Conn == nil {
		return templ.URL("/connections")
	}
	return templ.URL("/connections/" + strconv.FormatInt(v.Conn.ID, 10))
}

func secretPlaceholder(v connFormView, placeholder string) string {
	if v.Conn != nil && v.Conn.SecretHint != "" {
		return "stored (" + v.Conn.SecretHint + "); leave empty to keep it"
	}
	return placeholder
}
