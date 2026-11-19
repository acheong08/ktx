package calendar

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// Repeating tasks: a VTODO with an RRULE. The rule repeats from DTSTART,
// or from DUE when the task has no start (common in task apps). Ticking a
// repeating task done moves it to its next date (as Apple Reminders and
// most task apps do) rather than completing the series; the series is
// completed when the rule has no date left.

// ValidateRule checks an RRULE value (e.g. "FREQ=WEEKLY;BYDAY=MO").
func ValidateRule(rule string) error {
	opt, err := rrule.StrToROption(rule)
	if err != nil {
		return fmt.Errorf("%w: repeat rule %q: %v", ErrInvalid, rule, err)
	}
	opt.Dtstart = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	if _, err := rrule.NewRRule(*opt); err != nil {
		return fmt.Errorf("%w: repeat rule %q: %v", ErrInvalid, rule, err)
	}
	return nil
}

// anchorOf is the property a task's repeat rule counts from.
func anchorOf(m *ical.Component) (name string, p *ical.Prop) {
	if p := m.Props.Get(ical.PropDateTimeStart); p != nil {
		return ical.PropDateTimeStart, p
	}
	if p := m.Props.Get(ical.PropDue); p != nil {
		return ical.PropDue, p
	}
	return "", nil
}

// toDoSet is the task's dates as a recurrence set (with EXDATE/RDATE), and
// the time it counts from. ok is false for a task that doesn't repeat (or
// whose rule can't be read).
func toDoSet(m *ical.Component) (set *rrule.Set, anchor time.Time, ok bool) {
	rr := m.Props.Get(ical.PropRecurrenceRule)
	_, ap := anchorOf(m)
	if rr == nil || ap == nil {
		return nil, time.Time{}, false
	}
	anchor, _, err := propTime(ap)
	if err != nil {
		return nil, time.Time{}, false
	}
	opt, err := rrule.StrToROption(rr.Value)
	if err != nil {
		return nil, time.Time{}, false
	}
	opt.Dtstart = anchor
	rule, err := rrule.NewRRule(*opt)
	if err != nil {
		return nil, time.Time{}, false
	}
	set = &rrule.Set{}
	set.RRule(rule)
	for _, p := range m.Props.Values(ical.PropExceptionDates) {
		for _, t := range propTimes(&p) {
			set.ExDate(t)
		}
	}
	for _, p := range m.Props.Values(ical.PropRecurrenceDates) {
		for _, t := range propTimes(&p) {
			set.RDate(t)
		}
	}
	return set, anchor, true
}

// advanceToDo moves a repeating task to its first date after both its
// current one and now, shifting DTSTART and DUE together and using up
// COUNT. It returns the new anchor time, or ok=false when the rule has no
// date left (the caller then completes the task).
func advanceToDo(m *ical.Component, now time.Time) (next time.Time, ok bool, err error) {
	set, anchor, repeats := toDoSet(m)
	if !repeats {
		return time.Time{}, false, nil
	}
	after := anchor
	if now.After(after) {
		after = now
	}
	used := 0 // dates of the series up to and including the one ticked off
	it := set.Iterator()
	for {
		t, more := it()
		if !more {
			return time.Time{}, false, nil
		}
		if t.After(after) {
			next = t
			break
		}
		used++
		if used > 100000 {
			return time.Time{}, false, fmt.Errorf("repeat rule runs too long")
		}
	}
	anchorName, _ := anchorOf(m)
	for _, name := range []string{ical.PropDateTimeStart, ical.PropDue} {
		p := m.Props.Get(name)
		if p == nil {
			continue
		}
		old, allDay, err := propTime(p)
		if err != nil {
			return time.Time{}, false, err
		}
		var t time.Time
		switch {
		case name == anchorName:
			t = next
		case allDay:
			t = next.AddDate(0, 0, daysBetween(anchor, old))
		default:
			t = next.Add(old.Sub(anchor))
		}
		setTimeLike(m, name, p, t)
	}
	// COUNT counts from the anchor, which has moved.
	if rr := m.Props.Get(ical.PropRecurrenceRule); rr != nil && used > 0 {
		parts := strings.Split(rr.Value, ";")
		for i, part := range parts {
			if k, v, found := strings.Cut(part, "="); found && strings.EqualFold(k, "COUNT") {
				if n, err := strconv.Atoi(v); err == nil {
					parts[i] = "COUNT=" + strconv.Itoa(max(n-used, 1))
				}
			}
		}
		rr.Value = strings.Join(parts, ";")
	}
	return next, true, nil
}

// daysBetween counts calendar days from a to b.
func daysBetween(a, b time.Time) int {
	da := time.Date(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, time.UTC)
	db := time.Date(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, time.UTC)
	return int(db.Sub(da).Hours() / 24)
}

// setTimeLike writes t into a property in the same form as like: a date,
// a time in like's TZID, UTC, or a floating time.
func setTimeLike(m *ical.Component, name string, like *ical.Prop, t time.Time) {
	switch {
	case like.ValueType() == ical.ValueDate:
		m.Props.SetDate(name, t)
	case like.Params.Get(ical.ParamTimezoneID) != "":
		tzid := like.Params.Get(ical.ParamTimezoneID)
		p := ical.NewProp(name)
		p.Value = t.In(Zone(tzid)).Format("20060102T150405")
		p.Params.Set(ical.ParamTimezoneID, tzid)
		m.Props.Set(p)
	case strings.HasSuffix(like.Value, "Z"):
		m.Props.SetDateTime(name, t.UTC())
	default:
		p := ical.NewProp(name)
		p.Value = t.In(DefaultZone()).Format("20060102T150405")
		m.Props.Set(p)
	}
}

// ToDoInRange reports whether a VTODO overlaps [start, end) by the rules
// of RFC 4791 section 9.9. A repeating task is checked date by date. A
// zero end means open-ended.
func ToDoInRange(c *ical.Component, start, end time.Time) bool {
	if end.IsZero() {
		end = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	get := func(name string) (time.Time, bool) {
		p := c.Props.Get(name)
		if p == nil {
			return time.Time{}, false
		}
		t, _, err := propTime(p)
		return t, err == nil
	}
	ts := toDoTimes{}
	ts.start, ts.hasStart = get(ical.PropDateTimeStart)
	ts.due, ts.hasDue = get(ical.PropDue)
	ts.completed, ts.hasCompleted = get(ical.PropCompleted)
	ts.created, ts.hasCreated = get(ical.PropCreated)
	if p := c.Props.Get(ical.PropDuration); p != nil {
		if d, err := p.Duration(); err == nil {
			ts.dur, ts.hasDur = d, true
		}
	}
	set, anchor, repeats := toDoSet(c)
	if !repeats {
		return ts.in(start, end)
	}
	// Each date of the series is the task with its start and due shifted
	// together. Look from far enough back to catch a date whose span
	// reaches into the range.
	span := 24 * time.Hour
	if ts.hasStart && ts.hasDue && ts.due.After(ts.start) {
		span += ts.due.Sub(ts.start)
	}
	if ts.hasDur {
		span += ts.dur
	}
	it := set.Iterator()
	for i := 0; i < 100000; i++ {
		t, more := it()
		if !more || !t.Before(end) {
			return false
		}
		if t.Before(start.Add(-span)) {
			continue
		}
		inst := ts
		if ts.hasStart {
			inst.start = t
			if ts.hasDue {
				inst.due = t.Add(ts.due.Sub(anchor))
			}
		} else {
			inst.due = t
		}
		if inst.in(start, end) {
			return true
		}
	}
	return false
}

type toDoTimes struct {
	start, due, completed, created             time.Time
	hasStart, hasDue, hasCompleted, hasCreated bool
	dur                                        time.Duration
	hasDur                                     bool
}

// in applies the VTODO rows of RFC 4791 section 9.9.
func (t toDoTimes) in(start, end time.Time) bool {
	le := func(a, b time.Time) bool { return !a.After(b) } // a <= b
	switch {
	case t.hasStart && t.hasDur:
		e := t.start.Add(t.dur)
		return le(start, e) && (end.After(t.start) || !end.Before(e))
	case t.hasStart && t.hasDue:
		return (start.Before(t.due) || le(start, t.start)) && (end.After(t.start) || !end.Before(t.due))
	case t.hasStart:
		return le(start, t.start) && end.After(t.start)
	case t.hasDue:
		return start.Before(t.due) && !end.Before(t.due)
	case t.hasCompleted && t.hasCreated:
		return (le(start, t.created) || le(start, t.completed)) && (!end.Before(t.created) || !end.Before(t.completed))
	case t.hasCompleted:
		return le(start, t.completed) && !end.Before(t.completed)
	case t.hasCreated:
		return end.After(t.created)
	default:
		return true
	}
}
