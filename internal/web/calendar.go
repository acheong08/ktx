package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/feeds"
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
	Feed   *feeds.Status // set for a subscription (nil without a feed manager)
	Events int
	Tasks  int // to-dos (VTODO), shown in calendar apps' task lists
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
	DAVURL    string
	Report    *calendar.ImportReport
	Into      string
	Flash     flash
	NewID     string
	NewName   string
	Feeds     bool   // subscriptions can be added
	CanSeal   bool   // there's a master key for feed URLs
	SubName   string // the subscribe form, kept on error
	SubURL    string
	SubInto   string
	Subs      []subRow // my subscriptions, merged or not
}

type subRow struct {
	Cal  calendar.Calendar
	Feed feeds.Status
	Into string // title of the calendar it's merged into, "" when it's its own
}

// mergedNames are the titles of my subscriptions merged into calendar id.
func (v calendarView) mergedNames(id string) []string {
	var out []string
	for _, s := range v.Subs {
		if s.Cal.MergedInto() == id && s.Into != "" {
			out = append(out, s.Cal.Title())
		}
	}
	return out
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
		row := calRow{Cal: c, Mine: c.Owner == owner}
		if c.Subscribed() && u.feeds != nil {
			st := u.feeds.Status(c)
			row.Feed = &st
		}
		for _, o := range objs {
			if o.IsToDo() {
				row.Tasks++
			} else {
				row.Events++
			}
		}
		v.Calendars = append(v.Calendars, row)
		if c.Owner == owner && !c.Subscribed() {
			v.Mine = append(v.Mine, c)
		}
	}
	v.Feeds = u.feeds != nil
	v.CanSeal = v.Feeds && u.feeds.CanStoreURLs()
	v.SubInto = DefaultCalendarID
	if v.Feeds {
		subs, err := u.cal.Subscriptions()
		if err != nil {
			return v, err
		}
		for _, c := range subs {
			if c.Owner != owner {
				continue
			}
			row := subRow{Cal: c, Feed: u.feeds.Status(c)}
			for _, m := range v.Mine {
				if m.ID == c.MergedInto() {
					row.Into = m.Title()
				}
			}
			v.Subs = append(v.Subs, row)
		}
	}
	sort.SliceStable(v.Calendars, func(i, j int) bool { return v.Calendars[i].Mine && !v.Calendars[j].Mine })

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
		label := o.Start.In(calendar.DefaultZone()).Format("Mon 2 Jan 15:04") + " · " + o.Summary + " (" + o.Calendar + ")"
		rid := ""
		if o.Recurring() {
			rid = o.RecurrenceID.Format(time.RFC3339)
		}
		out = append(out, eventChoice{
			Value: url.Values{"cal": {o.Calendar}, "uid": {o.UID}, "rid": {rid}}.Encode(), Label: label,
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
			// An encoded cal/uid/rid triple: no line breaks, which browsers
			// would turn into CR LF on submit.
			q, err := url.ParseQuery(choice)
			if err != nil || q.Get("cal") == "" || q.Get("uid") == "" {
				return errBadChoice
			}
			parts := []string{q.Get("cal"), q.Get("uid"), q.Get("rid")}
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
	return t.In(calendar.DefaultZone()).Format("Mon 2 Jan 2006, 15:04 MST")
}

// --- subscriptions -------------------------------------------------------------

// subscribe adds an iCal subscription, or (with id) gives one a new link.
func (u *UI) subscribe(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	owner := calendarOwner(user)
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	name, link, id := strings.TrimSpace(r.PostFormValue("name")), strings.TrimSpace(r.PostFormValue("url")), r.PostFormValue("id")
	into := r.PostFormValue("into")
	if id == "" {
		id = u.freeCalendarID(owner, name)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()
	err := u.feeds.Subscribe(ctx, owner, id, name, link, into, actor(r))
	v, verr := u.calendarView(r)
	if verr != nil {
		u.fail(w, r, verr)
		return
	}
	if err != nil {
		u.log.Warn("subscribing failed", "calendar", owner+"/"+id, "err", err)
		v.SubName, v.SubURL, v.SubInto = name, link, into
		v.Flash = flash{"error", "Couldn't subscribe: " + err.Error()}
		u.render(w, r, http.StatusUnprocessableEntity, calendarPage(v))
		return
	}
	msg := "Subscribed. Its events show in the agenda and your calendar apps (read-only), and are refreshed every 30 minutes."
	for _, s := range v.Subs {
		if s.Cal.ID == id && s.Into != "" {
			msg = "Subscribed. Its events show as part of " + s.Into + " (read-only there), refreshed every 30 minutes."
		}
	}
	v.Flash = flash{"ok", msg}
	u.render(w, r, http.StatusOK, calendarPage(v))
}

// freeCalendarID makes a calendar id from a name that owner doesn't use yet.
func (u *UI) freeCalendarID(owner, name string) string {
	base := transcript.Slugify(name, 30)
	if base == "" || !calendar.ValidID(base) {
		base = "subscription"
	}
	id := base
	for i := 2; ; i++ {
		if _, err := u.cal.Calendar(owner, id); err != nil {
			return id
		}
		id = base + "-" + strconv.Itoa(i)
	}
}

// mergeSubscription shows a subscription as part of one of my calendars, or
// as a calendar of its own.
func (u *UI) mergeSubscription(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()
	err := u.feeds.SetMerge(ctx, calendarOwner(user), r.PathValue("id"), r.PostFormValue("into"), actor(r))
	v, verr := u.calendarView(r)
	if verr != nil {
		u.fail(w, r, verr)
		return
	}
	if err != nil {
		v.Flash = flash{"error", "Couldn't change it: " + err.Error()}
		u.render(w, r, http.StatusUnprocessableEntity, calendarPage(v))
		return
	}
	v.Flash = flash{"ok", "Saved. Calendar apps pick it up on their next refresh."}
	u.render(w, r, http.StatusOK, calendarPage(v))
}

func (u *UI) refreshSubscription(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()
	changed, err := u.feeds.Refresh(ctx, calendarOwner(user), r.PathValue("id"))
	v, verr := u.calendarView(r)
	if verr != nil {
		u.fail(w, r, verr)
		return
	}
	switch {
	case err != nil:
		v.Flash = flash{"error", "Refreshing failed: " + err.Error() + ". The last copy is kept."}
	case changed:
		v.Flash = flash{"ok", "Refreshed; the feed had changes."}
	default:
		v.Flash = flash{"ok", "Refreshed; nothing changed."}
	}
	u.render(w, r, http.StatusOK, calendarPage(v))
}

func (u *UI) unsubscribe(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	if r.PostFormValue("confirm") == "" {
		http.Redirect(w, r, "/settings/calendar", http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
	defer cancel()
	err := u.feeds.Unsubscribe(ctx, calendarOwner(user), r.PathValue("id"), actor(r))
	v, verr := u.calendarView(r)
	if verr != nil {
		u.fail(w, r, verr)
		return
	}
	if err != nil {
		v.Flash = flash{"error", "Couldn't remove it: " + err.Error()}
		u.render(w, r, http.StatusUnprocessableEntity, calendarPage(v))
		return
	}
	v.Flash = flash{"ok", "Subscription removed."}
	u.render(w, r, http.StatusOK, calendarPage(v))
}

func itemCount(n int) string {
	if n == 1 {
		return "1 item"
	}
	return strconv.Itoa(n) + " items"
}
