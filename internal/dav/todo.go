package dav

import (
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav/caldav"

	"github.com/acheong08/ktx/internal/calendar"
)

// go-webdav's Match only applies time ranges to events, and says no to any
// to-do queried with one, which hides every task from clients that ask for
// "to-dos in this range" (Thunderbird, Apple). So time ranges on VTODO
// filters are taken out before calling Match and checked here instead, by
// the rules of RFC 4791 section 9.9.

type span struct{ start, end time.Time }

// matches is caldav.Match with to-do time ranges handled.
func matches(f caldav.CompFilter, co *caldav.CalendarObject) (bool, error) {
	stripped, ranges := stripToDoRanges(f)
	ok, err := caldav.Match(stripped, co)
	if !ok || err != nil || len(ranges) == 0 {
		return ok, err
	}
	for _, r := range ranges {
		if !anyToDoIn(co.Data, r) {
			return false, nil
		}
	}
	return true, nil
}

// stripToDoRanges copies the filter without the time ranges of its VTODO
// filters, and returns those ranges.
func stripToDoRanges(f caldav.CompFilter) (caldav.CompFilter, []span) {
	var ranges []span
	comps := make([]caldav.CompFilter, len(f.Comps))
	for i, c := range f.Comps {
		if c.Name == ical.CompToDo && (!c.Start.IsZero() || !c.End.IsZero()) {
			ranges = append(ranges, span{c.Start, c.End})
			c.Start, c.End = time.Time{}, time.Time{}
		}
		comps[i] = c
	}
	f.Comps = comps
	return f, ranges
}

func anyToDoIn(cal *ical.Calendar, r span) bool {
	for _, c := range cal.Children {
		if c.Name == ical.CompToDo && toDoIn(c, r.start, r.end) {
			return true
		}
	}
	return false
}

// toDoIn applies the VTODO rows of RFC 4791 section 9.9. A zero end means
// open-ended.
func toDoIn(c *ical.Component, start, end time.Time) bool {
	if end.IsZero() {
		end = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	get := func(name string) (time.Time, bool) {
		p := c.Props.Get(name)
		if p == nil {
			return time.Time{}, false
		}
		t, err := p.DateTime(calendar.DefaultZone())
		return t, err == nil
	}
	dtstart, hasStart := get(ical.PropDateTimeStart)
	due, hasDue := get(ical.PropDue)
	completed, hasCompleted := get(ical.PropCompleted)
	created, hasCreated := get(ical.PropCreated)
	var dur time.Duration
	hasDur := false
	if p := c.Props.Get(ical.PropDuration); p != nil {
		if d, err := p.Duration(); err == nil {
			dur, hasDur = d, true
		}
	}
	le := func(a, b time.Time) bool { return !a.After(b) } // a <= b
	switch {
	case hasStart && hasDur:
		e := dtstart.Add(dur)
		return le(start, e) && (end.After(dtstart) || !end.Before(e))
	case hasStart && hasDue:
		return (start.Before(due) || le(start, dtstart)) && (end.After(dtstart) || !end.Before(due))
	case hasStart:
		return le(start, dtstart) && end.After(dtstart)
	case hasDue:
		return start.Before(due) && !end.Before(due)
	case hasCompleted && hasCreated:
		return (le(start, created) || le(start, completed)) && (!end.Before(created) || !end.Before(completed))
	case hasCompleted:
		return le(start, completed) && !end.Before(completed)
	case hasCreated:
		return end.After(created)
	default:
		return true
	}
}
