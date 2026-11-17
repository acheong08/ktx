package web

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/state"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

const maxICSBytes = 50 << 20

// DefaultCalendarID is the calendar made for someone who has none yet.
const DefaultCalendarID = "personal"

// calendarOwner is the calendar owner id of a signed-in user.
func calendarOwner(u state.User) string { return strings.ToLower(u.Username) }

type calRow struct {
	Cal    calendar.Calendar
	Mine   bool
	Events int
}

type occRow struct {
	Occ         calendar.Occurrence
	Day, Time   string
	Guests      []calendar.Attendee
	Transcripts []store.Entry
}

type calendarView struct {
	Chrome    chrome
	Owner     string
	Calendars []calRow
	Mine      []calendar.Calendar
	Events    []occRow
	Problems  []string
	DAVURL    string
	Report    *calendar.ImportReport
	Into      string
	Flash     flash
	NewID     string
	NewName   string
}

func davURL(r *http.Request) string { return "https://" + r.Host + "/dav/" }

func (u *UI) calendarView(r *http.Request) (calendarView, error) {
	user, _ := currentUser(r)
	owner := calendarOwner(user)
	v := calendarView{Chrome: u.chrome(r, "calendar"), Owner: owner, DAVURL: davURL(r)}
	if err := u.cal.EnsureCalendar(r.Context(), owner, DefaultCalendarID, "Personal"); err != nil {
		return v, err
	}
	cals, err := u.cal.Calendars()
	if err != nil {
		return v, err
	}
	for _, c := range cals {
		objs, err := u.cal.Objects(c)
		if err != nil {
			return v, err
		}
		v.Calendars = append(v.Calendars, calRow{Cal: c, Mine: c.Owner == owner, Events: len(objs)})
		if c.Owner == owner {
			v.Mine = append(v.Mine, c)
		}
	}
	sort.SliceStable(v.Calendars, func(i, j int) bool { return v.Calendars[i].Mine && !v.Calendars[j].Mine })

	now := time.Now()
	from, to := now.AddDate(0, 0, -7), now.AddDate(0, 0, 14)
	occs, problems, err := u.cal.Occurrences(cals, from, to)
	if err != nil {
		return v, err
	}
	v.Problems = problems
	linked := map[string][]store.Entry{}
	entries, err := u.store.List()
	if err != nil {
		return v, err
	}
	for _, e := range entries {
		if l := e.Meta.CalendarEvent; l != nil && l.UID != "" {
			linked[linkKey(l.UID, l.RecurrenceID)] = append(linked[linkKey(l.UID, l.RecurrenceID)], e)
		}
	}
	for _, o := range occs {
		local := o.Start.In(calendar.DefaultZone)
		row := occRow{Occ: o, Day: local.Format("Mon 2 Jan"), Guests: o.Guests()}
		if o.AllDay {
			row.Time = "all day"
		} else {
			row.Time = local.Format("15:04") + "–" + o.End.In(calendar.DefaultZone).Format("15:04")
		}
		rid := ""
		if o.Recurring() {
			rid = o.RecurrenceID.Format(time.RFC3339)
		}
		row.Transcripts = linked[linkKey(o.UID, rid)]
		v.Events = append(v.Events, row)
	}
	return v, nil
}

// linkKey identifies an occurrence: its UID and, for a repeating event, the
// instant it was originally scheduled for.
func linkKey(uid, rid string) string {
	if rid != "" {
		if t, err := time.Parse(time.RFC3339, rid); err == nil {
			rid = t.UTC().Format(time.RFC3339)
		}
	}
	return uid + "|" + rid
}

func (u *UI) calendarPage(w http.ResponseWriter, r *http.Request) {
	v, err := u.calendarView(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, r, http.StatusOK, calendarPage(v))
}

func (u *UI) createCalendar(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	id := strings.ToLower(strings.TrimSpace(r.PostFormValue("id")))
	name := strings.TrimSpace(r.PostFormValue("name"))
	err := u.cal.CreateCalendar(r.Context(), calendar.Calendar{Owner: calendarOwner(user), ID: id, Name: name}, actor(r))
	if err == nil {
		http.Redirect(w, r, "/settings/calendar", http.StatusSeeOther)
		return
	}
	v, verr := u.calendarView(r)
	if verr != nil {
		u.fail(w, r, verr)
		return
	}
	v.NewID, v.NewName = id, name
	switch {
	case errors.Is(err, calendar.ErrExists):
		v.Flash = flash{"error", "You already have a calendar called " + id + "."}
	case errors.Is(err, calendar.ErrInvalid):
		v.Flash = flash{"error", "Calendar ids are lowercase letters, digits, '-' and '_', e.g. work."}
	default:
		u.fail(w, r, err)
		return
	}
	u.render(w, r, http.StatusUnprocessableEntity, calendarPage(v))
}

// renameCalendar changes the display name of one of your calendars. Its id,
// and so its CalDAV path, stay the same.
func (u *UI) renameCalendar(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	c, err := u.cal.Calendar(calendarOwner(user), r.PathValue("id"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	old := c.Title()
	c.Name = strings.TrimSpace(r.PostFormValue("name"))
	err = u.cal.UpdateCalendar(r.Context(), c, actor(r))
	v, verr := u.calendarView(r)
	if verr != nil {
		u.fail(w, r, verr)
		return
	}
	status := http.StatusOK
	switch {
	case errors.Is(err, calendar.ErrInvalid):
		v.Flash, status = flash{"error", err.Error()}, http.StatusUnprocessableEntity
	case err != nil:
		u.fail(w, r, err)
		return
	case c.Name == "" || c.Name == old:
		v.Flash = flash{"ok", "Nothing changed."}
	default:
		v.Flash = flash{"ok", "Renamed “" + old + "” to “" + c.Name + "”. The path stays " + davPath(v.Owner, c.Owner, c.ID) + "; apps show the new name on their next refresh."}
	}
	u.render(w, r, status, calendarPage(v))
}

func (u *UI) importCalendar(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	r.Body = http.MaxBytesReader(w, r.Body, maxICSBytes+1<<20)
	fail := func(status int, msg string) {
		v, err := u.calendarView(r)
		if err != nil {
			u.fail(w, r, err)
			return
		}
		v.Flash = flash{"error", msg}
		u.render(w, r, status, calendarPage(v))
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		fail(http.StatusRequestEntityTooLarge, "Couldn't read the upload. Files can be up to 50 MB.")
		return
	}
	c, err := u.cal.Calendar(calendarOwner(user), r.FormValue("calendar"))
	if err != nil {
		fail(http.StatusUnprocessableEntity, "Choose one of your own calendars to import into.")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		fail(http.StatusUnprocessableEntity, "Choose an .ics file to import.")
		return
	}
	defer f.Close()
	rep, git, err := u.cal.Import(r.Context(), c, f, actor(r))
	if errors.Is(err, calendar.ErrInvalid) {
		fail(http.StatusUnprocessableEntity, err.Error())
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.log.Info("calendar import", "calendar", c.Key(), "by", actor(r), "added", rep.Added, "updated", rep.Updated, "unchanged", rep.Unchanged, "skipped", len(rep.Skipped))
	v, err := u.calendarView(r)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	v.Report, v.Into = &rep, c.Title()
	switch {
	case rep.Added+rep.Updated == 0:
		v.Flash = flash{"ok", "Nothing new: every event was already there."}
	case git.Pushed:
		v.Flash = flash{"ok", "Imported, committed and pushed."}
	default:
		v.Flash = flash{"warn", "Imported and committed; pushing to GitHub failed and will be retried."}
	}
	u.render(w, r, http.StatusOK, calendarPage(v))
}

// --- linking a transcript to its event ------------------------------------------

type eventChoice struct {
	Value, Label string
	Selected     bool
}

// eventChoices lists what a transcript can be linked to: automatic, no
// event, and the events within 12 hours of it.
func (u *UI) eventChoices(e store.Entry) []eventChoice {
	l := e.Meta.CalendarEvent
	manual := l != nil && (l.Match == calendar.MatchManual || l.Match == "")
	out := []eventChoice{
		{Value: "auto", Label: "Match automatically (by time)", Selected: l == nil || l.Match == calendar.MatchTime || l.Match == calendar.MatchExact},
		{Value: "none", Label: "No calendar event", Selected: l != nil && l.Match == calendar.MatchNone},
	}
	if e.Start.IsZero() {
		return out
	}
	cands, err := u.cal.Candidates(e.Start, e.UploadedBy)
	if err != nil {
		u.log.Warn("event candidates", "err", err)
		return out
	}
	for _, o := range cands {
		if o.AllDay {
			continue
		}
		label := o.Start.In(calendar.DefaultZone).Format("Mon 2 Jan 15:04") + " · " + o.Summary + " (" + o.Calendar + ")"
		rid := ""
		if o.Recurring() {
			rid = o.RecurrenceID.Format(time.RFC3339)
		}
		out = append(out, eventChoice{
			Value: o.Calendar + "\n" + o.UID + "\n" + rid, Label: label,
			Selected: manual && l.UID == o.UID && linkKey(l.UID, l.RecurrenceID) == linkKey(o.UID, rid),
		})
	}
	return out
}

func (u *UI) setEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	id, choice := r.PathValue("id"), r.PostFormValue("event")
	subject := "Link transcript to calendar event"
	_, err := u.store.EditLinks(r.Context(), id, actor(r), subject, func(root string, doc *transcript.Doc) error {
		switch choice {
		case "auto":
			calendar.ClearManual(doc)
		case "none":
			calendar.SetManual(doc, nil)
		default:
			parts := strings.Split(choice, "\n")
			if len(parts) != 3 {
				return errBadChoice
			}
			cals, err := calendar.ListCalendars(root)
			if err != nil {
				return err
			}
			occs, _, err := calendar.Occurrences(root, cals, doc.Start.Add(-12*time.Hour), doc.Start.Add(12*time.Hour))
			if err != nil {
				return err
			}
			want := calendar.Link{UID: parts[1], RecurrenceID: parts[2]}
			var found *calendar.Occurrence
			for i := range occs {
				if occs[i].Calendar == parts[0] && want.Is(occs[i]) {
					found = &occs[i]
					break
				}
			}
			if found == nil {
				return errBadChoice
			}
			calendar.SetManual(doc, found)
		}
		_, err := calendar.Linker{}.Link(root, doc, nil)
		return err
	})
	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)
	case errors.Is(err, errBadChoice):
		u.renderDetail(w, r, http.StatusUnprocessableEntity, id, flash{"error", "That event isn't there any more; pick again."})
	case err != nil:
		u.fail(w, r, err)
	default:
		http.Redirect(w, r, "/transcripts/"+id+"?saved=event", http.StatusSeeOther)
	}
}

var errBadChoice = errors.New("unknown event choice")

func matchLabel(m string) string {
	switch m {
	case calendar.MatchExact:
		return "matched by the recording's event id"
	case calendar.MatchTime:
		return "matched by time"
	case calendar.MatchNone:
		return "no event, set by hand"
	}
	return "chosen by hand"
}

func eventWhen(l *transcript.EventLink) string {
	t, err := time.Parse(time.RFC3339, l.Start)
	if err != nil {
		return l.Start
	}
	return t.In(calendar.DefaultZone).Format("Mon 2 Jan 2006, 15:04 MST")
}
