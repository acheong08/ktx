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
// the rules of RFC 4791 section 9.9 (calendar.ToDoInRange), with repeating
// tasks expanded.

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
		if c.Name == ical.CompToDo && calendar.ToDoInRange(c, r.start, r.end) {
			return true
		}
	}
	return false
}
