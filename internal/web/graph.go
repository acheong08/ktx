package web

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/notes"
	"github.com/acheong08/ktx/internal/store"
)

// The decision graph (docs/decision-graph.md): goals and decisions are
// notes with a kind; context is notes and transcripts; tasks say which
// goal or decision they're for.

// viewer is who's looking, for private notes (the same name notes are
// written under).
func viewer(r *http.Request) string { return actor(r) }

// refView is one "based on" link, resolved for display.
type refView struct {
	Ref, Quote string
	Kind       string // note, transcript
	NoteKind   string // goal or decision, for a note
	Title, URL string
	Missing    bool // deleted, or private to someone else
}

// Label is how a link is shown in forms.
func (r refView) Label() string {
	switch {
	case r.Missing:
		return r.Ref + " (not available)"
	case r.Kind == "transcript":
		return "Call: " + r.Title
	case r.NoteKind != "":
		return kindName(r.NoteKind) + ": " + r.Title
	}
	return "Note: " + r.Title
}

func kindName(k string) string {
	switch k {
	case notes.KindGoal:
		return "Goal"
	case notes.KindDecision:
		return "Decision"
	}
	return "Note"
}

// refResolver looks up notes and transcripts by id, once per request.
type refResolver struct {
	notes       map[string]notes.Note
	transcripts map[string]store.Entry
	viewer      string
}

func (u *UI) resolver(r *http.Request, all []notes.Note) (*refResolver, error) {
	rr := &refResolver{notes: map[string]notes.Note{}, transcripts: map[string]store.Entry{}, viewer: viewer(r)}
	for _, n := range all {
		rr.notes[n.ID] = n
	}
	entries, err := u.store.List()
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.ID != "" {
			rr.transcripts[e.ID] = e
		}
	}
	return rr, nil
}

func (rr *refResolver) resolve(refs []notes.Ref) []refView {
	var out []refView
	for _, ref := range refs {
		kind, id := ref.Target()
		v := refView{Ref: ref.Ref, Quote: ref.Quote, Kind: kind}
		switch kind {
		case "note":
			if n, ok := rr.notes[id]; ok && n.Visible(rr.viewer) {
				v.Title, v.URL, v.NoteKind = noteLabel(n), "/notes/"+id, n.Kind
			} else {
				v.Missing = true
			}
		case "transcript":
			if e, ok := rr.transcripts[id]; ok {
				v.Title, v.URL = e.Label(), "/transcripts/"+id
			} else {
				v.Missing = true
			}
		}
		out = append(out, v)
	}
	return out
}

// graphFieldsView is the goal/decision block of the note forms.
type graphFieldsView struct {
	Kind, Status, Strength string
	Private                bool
	Refs                   []refView
	Open                   bool // shown expanded (set, or prefilled)
}

func (u *UI) graphFieldsOf(rr *refResolver, f notes.Fields) graphFieldsView {
	return graphFieldsView{Kind: f.Kind, Status: f.Status, Strength: f.Strength, Private: f.Private, Refs: rr.resolve(f.BasedOn),
		Open: f.Kind != "" || f.Private || len(f.BasedOn) > 0}
}

// fieldsFromForm reads the goal/decision block. The form has a status
// select per kind; the one matching the kind counts.
func fieldsFromForm(r *http.Request) notes.Fields {
	f := notes.Fields{Kind: r.PostFormValue("kind"), Strength: r.PostFormValue("strength"), Private: r.PostFormValue("private") != ""}
	for _, s := range r.PostForm["status"] {
		if slices.Contains(notes.Statuses(f.Kind), s) {
			f.Status = s
		}
	}
	refs, quotes := r.PostForm["ref"], r.PostForm["quote"]
	for i, ref := range refs {
		q := ""
		if i < len(quotes) {
			q = quotes[i]
		}
		f.BasedOn = append(f.BasedOn, notes.Ref{Ref: ref, Quote: q})
	}
	return f
}

// refOption is a search result in the "based on" picker.
type refOption struct{ Ref, Label string }

// refSearch finds notes and transcripts to link to, by title (and text).
func (u *UI) refSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	var opts []refOption
	if q != "" {
		all, err := u.notes.List()
		if err != nil {
			u.fail(w, r, err)
			return
		}
		who := viewer(r)
		for _, n := range all {
			if len(opts) >= 6 {
				break
			}
			if n.Pending || !n.Visible(who) || !n.Matches(q) {
				continue
			}
			opts = append(opts, refOption{"note:" + n.ID, kindName(n.Kind) + ": " + n.Title})
		}
		entries, err := u.store.List()
		if err != nil {
			u.fail(w, r, err)
			return
		}
		for _, e := range entries {
			if len(opts) >= 10 {
				break
			}
			if e.ID != "" && e.Matches(q) {
				opts = append(opts, refOption{"transcript:" + e.ID, "Call: " + e.Label() + " (" + fmtDate(e) + ")"})
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	u.render(w, r, http.StatusOK, refResults(q, opts))
}

// --- the Graph page ------------------------------------------------------------

type graphTask struct {
	T   calendar.ToDo
	URL string
}

type graphNodeView struct {
	Node    *notes.GraphNode
	Context []refView // its links to plain notes and transcripts
	Others  []string  // titles of its other goal/decision parents ("also under")
	Tasks   []graphTask
	Kids    []graphNodeView
}

type graphView struct {
	Chrome   chrome
	Nodes    []graphNodeView
	Finished bool
	Count    int
}

// tasksFor maps "note:<id>" to the tasks for it, from every calendar.
func (u *UI) tasksFor() (map[string][]graphTask, error) {
	out := map[string][]graphTask{}
	if u.cal == nil {
		return out, nil
	}
	cals, err := u.cal.Calendars()
	if err != nil {
		return nil, err
	}
	tasks, _, err := u.cal.ToDos(cals)
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if t.For != "" {
			out[t.For] = append(out[t.For], graphTask{T: t, URL: taskURL(t.Calendar, t.Name)})
		}
	}
	return out, nil
}

func (u *UI) graphPage(w http.ResponseWriter, r *http.Request) {
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
	tasks, err := u.tasksFor()
	if err != nil {
		u.fail(w, r, err)
		return
	}
	finished := r.URL.Query().Get("finished") != ""
	v := graphView{Chrome: u.chrome(r, "graph"), Finished: finished}
	var build func(ns []*notes.GraphNode, parent string) []graphNodeView
	build = func(ns []*notes.GraphNode, parent string) []graphNodeView {
		var out []graphNodeView
		for _, n := range ns {
			gv := graphNodeView{Node: n}
			for _, p := range n.Parents {
				if p.ID != parent {
					gv.Others = append(gv.Others, p.Title)
				}
			}
			if !n.Again {
				v.Count++
				for _, ref := range rr.resolve(n.Note.BasedOn) {
					if ref.NoteKind == "" { // context: plain notes and transcripts
						gv.Context = append(gv.Context, ref)
					}
				}
				gv.Tasks = tasks["note:"+n.Note.ID]
				gv.Kids = build(n.Children, n.Note.ID)
			}
			out = append(out, gv)
		}
		return out
	}
	v.Nodes = build(notes.Outline(all, viewer(r), finished), "")
	u.render(w, r, http.StatusOK, graphPage(v))
}

// graphTargets are the goals and decisions a task can be for: the open
// ones the viewer can see, plus the current one.
func (u *UI) graphTargets(r *http.Request, current string) ([]refOption, error) {
	all, err := u.notes.List()
	if err != nil {
		return nil, err
	}
	who := viewer(r)
	var out []refOption
	for _, n := range all {
		if !n.IsNode() || n.Pending || !n.Visible(who) {
			continue
		}
		ref := "note:" + n.ID
		if notes.Finished(n.Status) && ref != current {
			continue
		}
		out = append(out, refOption{ref, kindName(n.Kind) + ": " + n.Title})
	}
	slices.SortFunc(out, func(a, b refOption) int { return strings.Compare(a.Label, b.Label) })
	return out, nil
}

// statusClass colours a status tag.
func statusClass(status string) string {
	switch {
	case notes.Finished(status):
		return "label status-done"
	case status == "considering" || status == "paused":
		return "label status-open"
	}
	return "label status-live"
}

func fmtTaskDue(t calendar.ToDo) string { return fmtDue(t, time.Now()) }
