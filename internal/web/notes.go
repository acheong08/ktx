package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/acheong08/ktx/internal/ai"
	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/notes"
)

// --- notes -----------------------------------------------------------------

type notesView struct {
	Chrome chrome
	Q      string
	Label  string // filter: only notes with this label
	Kind   string // filter: "" all, "plain", "goal", "decision"
	Kinds  []tab
	Graph  graphFieldsView // the scratchpad's goal/decision block
	Notes  []notes.Note
	Labels []notes.Label // defined labels, with counts
	Tabs   []tab
	Draft  string // the scratchpad, kept when saving failed
	Title  string
	Picked []string // labels ticked on the scratchpad
	Flash  flash
	AIOff  bool // titles come from the first line
}

func (u *UI) notesView(r *http.Request, q, label, kind string) (notesView, error) {
	all, err := u.notes.List()
	if err != nil {
		return notesView{}, err
	}
	defs, err := u.notes.Labels()
	if err != nil {
		return notesView{}, err
	}
	v := notesView{Chrome: u.chrome(r, "notes"), Q: q, Label: label, Kind: kind, Labels: defs}
	counts := map[string]int{}
	kinds := map[string]int{}
	total := 0
	who := viewer(r)
	for _, n := range all {
		if !n.Visible(who) || !n.Matches(q) {
			continue
		}
		k := n.Kind
		if k == "" {
			k = "plain"
		}
		kinds[k]++
		if kind != "" && kind != k {
			continue
		}
		total++
		for _, l := range n.Labels {
			counts[l]++
		}
		if label == "" || slices.Contains(n.Labels, label) {
			v.Notes = append(v.Notes, n)
		}
	}
	v.Tabs = append(v.Tabs, tab{Label: "All", URL: notesKindURL(q, "", kind), Count: total, Active: label == ""})
	for _, d := range defs {
		v.Tabs = append(v.Tabs, tab{Label: d.Name, URL: notesKindURL(q, d.Name, kind), Count: counts[d.Name], Active: label == d.Name})
	}
	v.Kinds = append(v.Kinds, tab{Label: "Everything", URL: notesKindURL(q, label, ""), Count: kinds["plain"] + kinds["goal"] + kinds["decision"], Active: kind == ""})
	for _, k := range []struct{ id, name string }{{"plain", "Notes"}, {"goal", "Goals"}, {"decision", "Decisions"}} {
		v.Kinds = append(v.Kinds, tab{Label: k.name, URL: notesKindURL(q, label, k.id), Count: kinds[k.id], Active: kind == k.id})
	}
	if u.ai != nil {
		st, err := u.ai.Status()
		v.AIOff = err != nil || !st.Configured
	} else {
		v.AIOff = true
	}
	return v, nil
}

func (u *UI) notesPage(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	v, err := u.notesView(r, query.Get("q"), query.Get("label"), query.Get("kind"))
	if err != nil {
		u.fail(w, r, err)
		return
	}
	// "Add a goal or decision based on this" links prefill the scratchpad.
	if ref := query.Get("based_on"); ref != "" || query.Get("new_kind") != "" {
		all, err := u.notes.List()
		if err != nil {
			u.fail(w, r, err)
			return
		}
		rr, err := u.resolver(r, all)
		if err != nil {
			u.fail(w, r, err)
			return
		}
		f := notes.Fields{Kind: query.Get("new_kind")}
		if ref != "" {
			f.BasedOn = []notes.Ref{{Ref: ref}}
		}
		v.Graph = u.graphFieldsOf(rr, f)
		v.Graph.Open = true
	}
	if r.URL.Query().Get("deleted") != "" {
		v.Flash = flash{"ok", "Note deleted. It stays in git history."}
	}
	w.Header().Set("Vary", "HX-Request, HX-Request-Type")
	if isPartial(r) {
		u.render(w, r, http.StatusOK, noteResults(v))
		return
	}
	u.render(w, r, http.StatusOK, notesListPage(v))
}

func (u *UI) createNote(w http.ResponseWriter, r *http.Request) {
	body := r.PostFormValue("body")
	picked := r.PostForm["labels"]
	fields := fieldsFromForm(r)
	if _, err := u.notes.SubmitNote(notes.Submission{Body: body, Author: actor(r), Labels: picked,
		Title: r.PostFormValue("title"), Fields: fields}); err != nil {
		v, verr := u.notesView(r, "", "", "")
		if verr != nil {
			u.fail(w, r, verr)
			return
		}
		all, _ := u.notes.List()
		if rr, rerr := u.resolver(r, all); rerr == nil {
			v.Graph = u.graphFieldsOf(rr, fields)
		}
		v.Draft, v.Picked, v.Flash = body, picked, flash{"error", err.Error()}
		v.Title = r.PostFormValue("title")
		u.render(w, r, http.StatusUnprocessableEntity, notesListPage(v))
		return
	}
	http.Redirect(w, r, "/notes", http.StatusSeeOther)
}

// note returns the note named in the path, or writes a 404.
func (u *UI) note(w http.ResponseWriter, r *http.Request) (notes.Note, bool) {
	n, err := u.notes.Get(r.PathValue("id"))
	if err == nil && !n.Visible(viewer(r)) {
		err = notes.ErrNotFound // private to someone else
	}
	if errors.Is(err, notes.ErrNotFound) {
		u.render(w, r, http.StatusNotFound, errorPage(u.chrome(r, "notes"), "No note with that id. It may have been deleted."))
		return n, false
	}
	if err != nil {
		u.fail(w, r, err)
		return n, false
	}
	return n, true
}

type noteView struct {
	Chrome   chrome
	Note     notes.Note
	Labels   []notes.Label
	BasedOn  []refView    // what it's based on
	Leads    []notes.Note // what's based on it
	Tasks    []graphTask  // tasks for it
	BodyHTML string
	Flash    flash
}

func (u *UI) noteDetail(w http.ResponseWriter, r *http.Request) {
	n, ok := u.note(w, r)
	if !ok {
		return
	}
	var body bytes.Buffer
	if err := u.md.Convert([]byte(n.Body), &body); err != nil {
		u.fail(w, r, err)
		return
	}
	defs, err := u.notes.Labels()
	if err != nil {
		u.fail(w, r, err)
		return
	}
	v := noteView{Chrome: u.chrome(r, "notes"), Note: n, Labels: defs, BodyHTML: body.String()}
	all, err := u.notes.List()
	if err != nil {
		u.fail(w, r, err)
		return
	}
	rr, err := u.resolver(r, all)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	v.BasedOn = rr.resolve(n.BasedOn)
	v.Leads = notes.BasedOnThis(all, "note:"+n.ID, viewer(r))
	if n.IsNode() {
		tasks, err := u.tasksFor()
		if err != nil {
			u.fail(w, r, err)
			return
		}
		v.Tasks = tasks["note:"+n.ID]
	}
	if n.Pending && n.Error != "" {
		v.Flash = flash{"warn", "Saving this note failed and will be retried: " + n.Error}
	}
	u.render(w, r, http.StatusOK, noteDetailPage(v))
}

// noteTitle is the title fragment; pending notes poll it until their title
// is ready. On the note's own page, the page reloads then (for its buttons).
func (u *UI) noteTitle(w http.ResponseWriter, r *http.Request) {
	n, ok := u.note(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if !n.Pending && r.URL.Query().Get("reload") != "" {
		w.Header().Set("HX-Redirect", "/notes/"+n.ID)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	u.render(w, r, http.StatusOK, noteTitle(n, r.URL.Query().Get("reload") != ""))
}

type noteFormView struct {
	Chrome      chrome
	ID          string
	Title, Body string
	Labels      []notes.Label // defined, plus any the note has that aren't
	Picked      []string
	Graph       graphFieldsView
	Error       string
}

// formLabels is the defined labels plus any the note has without a
// definition, so they can be kept or taken off.
func (u *UI) formLabels(have []string) ([]notes.Label, error) {
	defs, err := u.notes.Labels()
	if err != nil {
		return nil, err
	}
	for _, l := range have {
		if !slices.ContainsFunc(defs, func(d notes.Label) bool { return d.Name == l }) {
			defs = append(defs, notes.Label{Name: l, Description: "(no longer defined)"})
		}
	}
	return defs, nil
}

func (u *UI) editNote(w http.ResponseWriter, r *http.Request) {
	n, ok := u.note(w, r)
	if !ok {
		return
	}
	if n.Pending {
		http.Redirect(w, r, "/notes/"+n.ID, http.StatusSeeOther)
		return
	}
	labels, err := u.formLabels(n.Labels)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	all, err := u.notes.List()
	if err != nil {
		u.fail(w, r, err)
		return
	}
	rr, err := u.resolver(r, all)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	g := u.graphFieldsOf(rr, n.Fields)
	g.Open = true
	u.render(w, r, http.StatusOK, noteForm(noteFormView{Chrome: u.chrome(r, "notes"), ID: n.ID, Title: n.Title, Body: n.Body, Labels: labels, Picked: n.Labels, Graph: g}))
}

func (u *UI) saveNote(w http.ResponseWriter, r *http.Request) {
	n, ok := u.note(w, r)
	if !ok {
		return
	}
	id := n.ID
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	fields := fieldsFromForm(r)
	_, err := u.notes.Edit(ctx, id, notes.Changes{Title: r.PostFormValue("title"), Body: r.PostFormValue("body"),
		Labels: r.PostForm["labels"], Fields: fields}, actor(r))
	switch {
	case errors.Is(err, notes.ErrNotFound):
		u.render(w, r, http.StatusNotFound, errorPage(u.chrome(r, "notes"), "No note with that id. It may have been deleted."))
	case err != nil:
		if !errors.Is(err, notes.ErrPending) {
			u.log.Warn("editing note", "id", id, "err", err)
		}
		labels, _ := u.formLabels(r.PostForm["labels"])
		var g graphFieldsView
		if all, lerr := u.notes.List(); lerr == nil {
			if rr, rerr := u.resolver(r, all); rerr == nil {
				g = u.graphFieldsOf(rr, fields)
			}
		}
		g.Open = true
		u.render(w, r, http.StatusUnprocessableEntity, noteForm(noteFormView{
			Chrome: u.chrome(r, "notes"), ID: id, Title: r.PostFormValue("title"), Body: r.PostFormValue("body"),
			Labels: labels, Picked: r.PostForm["labels"], Graph: g, Error: err.Error(),
		}))
	default:
		http.Redirect(w, r, "/notes/"+id, http.StatusSeeOther)
	}
}

func (u *UI) deleteNote(w http.ResponseWriter, r *http.Request) {
	n, ok := u.note(w, r)
	if !ok {
		return
	}
	id := n.ID
	if r.PostFormValue("confirm") == "" {
		http.Redirect(w, r, "/notes/"+id, http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	if _, err := u.notes.Delete(ctx, id, actor(r)); err != nil {
		if errors.Is(err, notes.ErrPending) || errors.Is(err, notes.ErrNotFound) {
			http.Redirect(w, r, "/notes/"+id, http.StatusSeeOther)
			return
		}
		u.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/notes?deleted=1", http.StatusSeeOther)
}

func fmtNoteDate(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(calendar.DefaultZone()).Format("Mon 2 Jan 2006, 15:04")
}

// --- Settings → AI -----------------------------------------------------------

type aiView struct {
	Chrome   chrome
	Status   ai.Status
	CanStore bool
	CanEdit  bool // admins only
	Model    string
	BaseURL  string
	Flash    flash
}

func (u *UI) aiView(r *http.Request) (aiView, error) {
	st, err := u.ai.Status()
	user, _ := currentUser(r)
	return aiView{Chrome: u.chrome(r, "ai"), Status: st, CanStore: u.ai.CanStoreKey(), CanEdit: user.IsAdmin,
		Model: st.Model, BaseURL: ai.DefaultBaseURL}, err
}

func (u *UI) aiPage(w http.ResponseWriter, r *http.Request) {
	v, err := u.aiView(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, r, http.StatusOK, aiSettingsPage(v))
}

func (u *UI) saveAI(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	if !user.IsAdmin {
		http.Error(w, "Only admins can change the AI settings.", http.StatusForbidden)
		return
	}
	v, err := u.aiView(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if r.PostFormValue("remove") != "" {
		if err := u.ai.Remove(); err != nil {
			u.fail(w, r, err)
			return
		}
		u.log.Info("ai settings removed", "user", user.Display())
		v, _ = u.aiView(r)
		v.Flash = flash{"ok", "Removed. New notes get their first line as title."}
		u.render(w, r, http.StatusOK, aiSettingsPage(v))
		return
	}
	model := r.PostFormValue("model")
	if err := u.ai.Save(r.Context(), r.PostFormValue("key"), model, user.Display()); err != nil {
		v.Model, v.Flash = model, flash{"error", err.Error()}
		u.render(w, r, http.StatusUnprocessableEntity, aiSettingsPage(v))
		return
	}
	u.log.Info("ai settings saved", "user", user.Display(), "model", model)
	v, _ = u.aiView(r)
	v.Flash = flash{"ok", "Saved, and checked with a test title."}
	u.render(w, r, http.StatusOK, aiSettingsPage(v))
}

func noteTitleURL(id string, reload bool) string {
	if reload {
		return "/notes/" + id + "/title?reload=1"
	}
	return "/notes/" + id + "/title"
}

func noteLabel(n notes.Note) string {
	if n.Pending {
		return "New note"
	}
	return n.Title
}

func keyPlaceholder(v aiView) string {
	if v.Status.Configured {
		return "Leave empty to keep the current key"
	}
	return ""
}

func notesURL(q, label string) string { return notesKindURL(q, label, "") }

func notesKindURL(q, label, kind string) string {
	v := url.Values{}
	if q != "" {
		v.Set("q", q)
	}
	if label != "" {
		v.Set("label", label)
	}
	if kind != "" {
		v.Set("kind", kind)
	}
	if len(v) == 0 {
		return "/notes"
	}
	return "/notes?" + v.Encode()
}

// labelMeaning is a label's description, for tooltips.
func labelMeaning(defs []notes.Label, name string) string {
	for _, d := range defs {
		if d.Name == name {
			return d.Description
		}
	}
	return "(no longer defined)"
}

// --- Settings → Labels -------------------------------------------------------

type labelsView struct {
	Chrome            chrome
	Labels            []notes.Label
	Flash             flash
	Name, Description string // the add form, kept on error
}

func (u *UI) labelsView(r *http.Request) (labelsView, error) {
	defs, err := u.notes.LabelsInUse()
	return labelsView{Chrome: u.chrome(r, "labels"), Labels: defs}, err
}

func (u *UI) labelsPage(w http.ResponseWriter, r *http.Request) {
	v, err := u.labelsView(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, r, http.StatusOK, labelsSettingsPage(v))
}

// putLabel adds a label or changes its meaning.
func (u *UI) putLabel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	name, desc := r.PostFormValue("name"), r.PostFormValue("description")
	l, err := u.notes.PutLabel(ctx, name, desc, actor(r))
	v, verr := u.labelsView(r)
	if verr != nil {
		u.fail(w, r, verr)
		return
	}
	if err != nil {
		v.Flash, v.Name, v.Description = flash{"error", err.Error()}, name, desc
		u.render(w, r, http.StatusUnprocessableEntity, labelsSettingsPage(v))
		return
	}
	u.log.Info("note label saved", "label", l.Name, "user", actor(r))
	v.Flash = flash{"ok", "Saved “" + l.Name + "”."}
	u.render(w, r, http.StatusOK, labelsSettingsPage(v))
}

func (u *UI) deleteLabel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	name := r.PathValue("name")
	err := u.notes.DeleteLabel(ctx, name, actor(r))
	if err != nil && !errors.Is(err, notes.ErrLabelInUse) {
		u.fail(w, r, err)
		return
	}
	v, verr := u.labelsView(r)
	if verr != nil {
		u.fail(w, r, verr)
		return
	}
	if err != nil {
		v.Flash = flash{"error", err.Error()}
		u.render(w, r, http.StatusConflict, labelsSettingsPage(v))
		return
	}
	u.log.Info("note label deleted", "label", name, "user", actor(r))
	v.Flash = flash{"ok", "Deleted “" + name + "”."}
	u.render(w, r, http.StatusOK, labelsSettingsPage(v))
}
