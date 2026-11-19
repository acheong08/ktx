package web

import (
	"net/http"
	"net/url"
	"time"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/store"
)

// The Calendar page: a week of events as an agenda, day by day, with the
// transcripts recorded in each.

type agendaDay struct {
	Label  string // "Mon 17 Nov"
	Today  bool
	Events []occRow
}

type agendaView struct {
	Chrome            chrome
	All               bool // everyone's calendars, not just mine
	Title             string
	Days              []agendaDay
	Empty             bool
	Prev, Next, Today string
	Problems          []string
	Names             map[string]string // calendar key -> what to call it
}

func weekURL(week time.Time, all bool) string {
	v := url.Values{}
	v.Set("week", week.Format("2006-01-02"))
	if all {
		v.Set("who", "all")
	}
	return "/agenda?" + v.Encode()
}

// monday is the start of the week containing t, in t's location.
func monday(t time.Time) time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

func (u *UI) agendaPage(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	owner := calendarOwner(user)
	all := r.URL.Query().Get("who") == "all"
	loc := calendar.DefaultZone()
	now := time.Now().In(loc)
	start := monday(now)
	if w := r.URL.Query().Get("week"); w != "" {
		if d, err := time.ParseInLocation("2006-01-02", w, loc); err == nil {
			start = monday(d)
		}
	}
	end := start.AddDate(0, 0, 7)
	v := agendaView{Chrome: u.chrome(r, "agenda"), All: all, Names: map[string]string{},
		Prev: weekURL(start.AddDate(0, 0, -7), all), Next: weekURL(end, all), Today: weekURL(now, all)}
	last := end.AddDate(0, 0, -1)
	switch {
	case start.Year() != last.Year():
		v.Title = start.Format("2 Jan 2006") + " – " + last.Format("2 Jan 2006")
	case start.Month() != last.Month():
		v.Title = start.Format("2 Jan") + " – " + last.Format("2 Jan 2006")
	default:
		v.Title = start.Format("2") + "–" + last.Format("2 Jan 2006")
	}

	if err := u.cal.EnsureCalendar(r.Context(), owner, DefaultCalendarID, "Personal"); err != nil {
		u.fail(w, r, err)
		return
	}
	cals, err := u.cal.Calendars()
	if err != nil {
		u.fail(w, r, err)
		return
	}
	var from []calendar.Calendar
	for _, c := range cals {
		if all || c.Owner == owner {
			from = append(from, c)
			v.Names[c.Key()] = c.Title()
			if c.Owner != owner {
				v.Names[c.Key()] += " (" + c.Owner + ")"
			}
		}
	}
	occs, problems, err := u.cal.Occurrences(from, start, end)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	v.Problems = problems
	linked := map[string][]store.Entry{}
	entries, err := u.store.List()
	if err != nil {
		u.fail(w, r, err)
		return
	}
	for _, e := range entries {
		if l := e.Meta.CalendarEvent; l != nil && l.UID != "" {
			linked[linkKey(l.UID, l.RecurrenceID)] = append(linked[linkKey(l.UID, l.RecurrenceID)], e)
		}
	}
	for d := 0; d < 7; d++ {
		day := start.AddDate(0, 0, d)
		v.Days = append(v.Days, agendaDay{Label: day.Format("Mon 2 Jan"), Today: day.Equal(time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc))})
	}
	for _, o := range occs {
		s := o.Start.In(loc)
		row := occRow{Occ: o, Guests: o.Guests()}
		if o.AllDay {
			row.Time = "All day"
			s = time.Date(o.Start.Year(), o.Start.Month(), o.Start.Day(), 0, 0, 0, 0, loc)
		} else {
			row.Time = s.Format("15:04") + "–" + o.End.In(loc).Format("15:04")
		}
		rid := ""
		if o.Recurring() {
			rid = o.RecurrenceID.Format(time.RFC3339)
		}
		row.Transcripts = linked[linkKey(o.UID, rid)]
		// By calendar date (not hours, which DST changes); an event that
		// started before this week (e.g. a multi-day one) shows on Monday.
		i := 0
		for d := 0; d < 7; d++ {
			if day := start.AddDate(0, 0, d); !s.Before(day) {
				i = d
			}
		}
		v.Days[i].Events = append(v.Days[i].Events, row)
	}
	v.Empty = len(occs) == 0
	u.render(w, r, http.StatusOK, agendaPage(v))
}

// weekURLFrom switches whose calendars a week URL shows.
func weekURLFrom(u string, all bool) string {
	parsed, err := url.Parse(u)
	if err != nil {
		return "/agenda"
	}
	q := parsed.Query()
	q.Del("who")
	if all {
		q.Set("who", "all")
	}
	return "/agenda?" + q.Encode()
}
