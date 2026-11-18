package web

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/acheong08/ktx/internal/ai"
	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/notes"
)

// --- notes -----------------------------------------------------------------

type notesView struct {
	Chrome chrome
	Q      string
	Notes  []notes.Note
	Draft  string // the scratchpad, kept when saving failed
	Flash  flash
	AIOff  bool // titles come from the first line
}

func (u *UI) notesView(r *http.Request, q string) (notesView, error) {
	all, err := u.notes.List()
	if err != nil {
		return notesView{}, err
	}
	v := notesView{Chrome: u.chrome(r, "notes"), Q: q}
	for _, n := range all {
		if n.Matches(q) {
			v.Notes = append(v.Notes, n)
		}
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
	q := r.URL.Query().Get("q")
	v, err := u.notesView(r, q)
	if err != nil {
		u.fail(w, r, err)
		return
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
	if _, err := u.notes.Submit(body, actor(r)); err != nil {
		v, verr := u.notesView(r, "")
		if verr != nil {
			u.fail(w, r, verr)
			return
		}
		v.Draft, v.Flash = body, flash{"error", err.Error()}
		u.render(w, r, http.StatusUnprocessableEntity, notesListPage(v))
		return
	}
	http.Redirect(w, r, "/notes", http.StatusSeeOther)
}

// note returns the note named in the path, or writes a 404.
func (u *UI) note(w http.ResponseWriter, r *http.Request) (notes.Note, bool) {
	n, err := u.notes.Get(r.PathValue("id"))
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
	v := noteView{Chrome: u.chrome(r, "notes"), Note: n, BodyHTML: body.String()}
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
	Error       string
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
	u.render(w, r, http.StatusOK, noteForm(noteFormView{Chrome: u.chrome(r, "notes"), ID: n.ID, Title: n.Title, Body: n.Body}))
}

func (u *UI) saveNote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	_, err := u.notes.Edit(ctx, id, r.PostFormValue("title"), r.PostFormValue("body"), actor(r))
	switch {
	case errors.Is(err, notes.ErrNotFound):
		u.render(w, r, http.StatusNotFound, errorPage(u.chrome(r, "notes"), "No note with that id. It may have been deleted."))
	case err != nil:
		if !errors.Is(err, notes.ErrPending) {
			u.log.Warn("editing note", "id", id, "err", err)
		}
		u.render(w, r, http.StatusUnprocessableEntity, noteForm(noteFormView{
			Chrome: u.chrome(r, "notes"), ID: id, Title: r.PostFormValue("title"), Body: r.PostFormValue("body"), Error: err.Error(),
		}))
	default:
		http.Redirect(w, r, "/notes/"+id, http.StatusSeeOther)
	}
}

func (u *UI) deleteNote(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	return t.In(calendar.DefaultZone).Format("Mon 2 Jan 2006, 15:04")
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
