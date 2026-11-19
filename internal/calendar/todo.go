package calendar

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-ical"
)

// ToDo is a task (VTODO) in a calendar, as the web UI shows and edits it.
// Editing changes only these fields; everything else in the file (alarms,
// relations, properties written by other apps) is kept.
type ToDo struct {
	Calendar    Calendar
	Name        string // file name, e.g. vikunja-123.ics
	ETag        string
	UID         string
	Summary     string
	Description string
	Due         time.Time // zero: no due date
	DueAllDay   bool      // Due is a date, without a time
	Done        bool
	Completed   time.Time
	Priority    int // 1 highest .. 9 lowest; 0 none
	Categories  []string
	Created     time.Time
	Rule        string // repeat rule (RRULE value), "" if it doesn't repeat
	Repeats     bool   // Rule != ""; ticking it done moves it to its next date
}

// Overdue reports whether an open task's due date has passed.
func (t ToDo) Overdue(now time.Time) bool {
	if t.Done || t.Due.IsZero() {
		return false
	}
	if t.DueAllDay {
		return !now.In(t.Due.Location()).Before(t.Due.AddDate(0, 0, 1))
	}
	return now.After(t.Due)
}

// ToDos returns the tasks in cals: open ones by due date (undated last),
// then done ones, most recently completed first. Files that can't be read
// are reported in problems.
func (s *Service) ToDos(cals []Calendar) ([]ToDo, []string, error) {
	var out []ToDo
	var problems []string
	err := s.repo.View(func(root string) error {
		for _, c := range cals {
			objs, err := ListObjects(root, c)
			if err != nil {
				return err
			}
			for _, o := range objs {
				if !o.IsToDo() {
					continue
				}
				t, err := toDoOf(c, o)
				if err != nil {
					problems = append(problems, o.Path+": "+err.Error())
					continue
				}
				out = append(out, t)
			}
		}
		return nil
	})
	SortToDos(out)
	return out, problems, err
}

// SortToDos orders tasks: open before done; open by due date (undated
// last), then priority, then creation; done by completion, newest first.
func SortToDos(ts []ToDo) {
	sort.SliceStable(ts, func(i, j int) bool {
		a, b := ts[i], ts[j]
		if a.Done != b.Done {
			return !a.Done
		}
		if a.Done {
			return a.Completed.After(b.Completed)
		}
		if a.Due.IsZero() != b.Due.IsZero() {
			return !a.Due.IsZero()
		}
		if !a.Due.Equal(b.Due) {
			return a.Due.Before(b.Due)
		}
		pa, pb := a.Priority, b.Priority
		if pa == 0 {
			pa = 10
		}
		if pb == 0 {
			pb = 10
		}
		if pa != pb {
			return pa < pb
		}
		if !a.Created.Equal(b.Created) {
			return a.Created.Before(b.Created)
		}
		return a.Summary < b.Summary
	})
}

// ToDo returns one task.
func (s *Service) ToDo(c Calendar, name string) (ToDo, error) {
	o, err := s.Object(c, name)
	if err != nil {
		return ToDo{}, err
	}
	if !o.IsToDo() {
		return ToDo{}, ErrNotFound
	}
	return toDoOf(c, o)
}

// master is the VTODO without a RECURRENCE-ID in cal.
func master(cal *ical.Calendar) *ical.Component {
	var first *ical.Component
	for _, c := range cal.Children {
		if c.Name != ical.CompToDo {
			continue
		}
		if first == nil {
			first = c
		}
		if c.Props.Get(ical.PropRecurrenceID) == nil {
			return c
		}
	}
	return first
}

func toDoOf(c Calendar, o Object) (ToDo, error) {
	cal, err := o.Decode()
	if err != nil {
		return ToDo{}, err
	}
	m := master(cal)
	if m == nil {
		return ToDo{}, errors.New("no VTODO in the file")
	}
	t := ToDo{Calendar: c, Name: o.Name, ETag: o.ETag}
	t.UID, _ = m.Props.Text(ical.PropUID)
	t.Summary, _ = m.Props.Text(ical.PropSummary)
	t.Description, _ = m.Props.Text(ical.PropDescription)
	if p := m.Props.Get(ical.PropDue); p != nil {
		if t.Due, t.DueAllDay, err = propTime(p); err != nil {
			return ToDo{}, fmt.Errorf("due date: %w", err)
		}
	}
	status, _ := m.Props.Text(ical.PropStatus)
	t.Done = strings.EqualFold(status, "COMPLETED") || m.Props.Get(ical.PropCompleted) != nil
	if p := m.Props.Get(ical.PropCompleted); p != nil {
		t.Completed, _, _ = propTime(p)
	}
	if p := m.Props.Get(ical.PropPriority); p != nil {
		t.Priority, _ = strconv.Atoi(strings.TrimSpace(p.Value))
	}
	for _, p := range m.Props.Values(ical.PropCategories) {
		if vs, err := p.TextList(); err == nil {
			for _, v := range vs {
				if v = strings.TrimSpace(v); v != "" {
					t.Categories = append(t.Categories, v)
				}
			}
		}
	}
	if p := m.Props.Get(ical.PropCreated); p != nil {
		t.Created, _, _ = propTime(p)
	}
	if rr := m.Props.Get(ical.PropRecurrenceRule); rr != nil {
		t.Rule, t.Repeats = rr.Value, true
	}
	return t, nil
}

// SaveToDo creates a task (t.Name empty) or updates one, writing only the
// fields ToDo has. An update needs the ETag it was read with, so changes
// made meanwhile (e.g. from a phone) aren't overwritten (ErrPrecondition).
// Ticking a repeating task done moves it to its next date instead (see
// advanceToDo); it stays open unless its rule has no date left.
func (s *Service) SaveToDo(ctx context.Context, t ToDo, now time.Time, user string) (ToDo, error) {
	t.Summary = strings.Join(strings.Fields(t.Summary), " ")
	if t.Summary == "" {
		return ToDo{}, fmt.Errorf("%w: give the task a title", ErrInvalid)
	}
	t.Rule = strings.TrimPrefix(strings.TrimSpace(t.Rule), "RRULE:")
	if t.Rule != "" {
		if t.Due.IsZero() {
			return ToDo{}, ErrNeedsDue
		}
		if err := ValidateRule(t.Rule); err != nil {
			return ToDo{}, err
		}
	}
	var cal *ical.Calendar
	opt := PutOptions{}
	if t.Name == "" {
		t.UID = newUID()
		t.Name = t.UID + ".ics"
		t.Created = now
		cal = ical.NewCalendar()
		cal.Props.SetText(ical.PropProductID, prodID)
		cal.Props.SetText(ical.PropVersion, "2.0")
		m := ical.NewComponent(ical.CompToDo)
		m.Props.SetText(ical.PropUID, t.UID)
		m.Props.SetDateTime(ical.PropCreated, now.UTC())
		cal.Children = append(cal.Children, m)
		opt.IfNoneMatch = true
	} else {
		o, err := s.Object(t.Calendar, t.Name)
		if err != nil {
			return ToDo{}, err
		}
		if t.ETag != "" && o.ETag != t.ETag {
			return ToDo{}, ErrPrecondition
		}
		if cal, err = o.Decode(); err != nil {
			return ToDo{}, err
		}
		opt.IfMatch = o.ETag
	}
	m := master(cal)
	if m == nil {
		return ToDo{}, ErrNotFound
	}
	m.Props.SetText(ical.PropSummary, t.Summary)
	if d := strings.TrimSpace(t.Description); d != "" {
		m.Props.SetText(ical.PropDescription, d)
	} else {
		m.Props.Del(ical.PropDescription)
	}
	var oldDue time.Time
	oldDueAllDay := false
	oldDueProp := m.Props.Get(ical.PropDue)
	if oldDueProp != nil {
		oldDue, oldDueAllDay, _ = propTime(oldDueProp)
	}
	wasDone := isDone(m)
	switch {
	case t.Due.IsZero():
		m.Props.Del(ical.PropDue)
	case oldDueProp != nil && oldDue.Equal(t.Due) && oldDueAllDay == t.DueAllDay:
		// Unchanged: keep it as written (e.g. in its own TZID).
	case oldDueProp != nil && oldDueAllDay == t.DueAllDay:
		setTimeLike(m, ical.PropDue, oldDueProp, t.Due)
	case t.DueAllDay:
		m.Props.SetDate(ical.PropDue, t.Due)
	default:
		m.Props.SetDateTime(ical.PropDue, t.Due.UTC())
	}
	if !t.Due.IsZero() {
		// A repeating task counts from its start: move the start with the
		// due date, so the series moves too.
		if p := m.Props.Get(ical.PropDateTimeStart); p != nil && t.Rule != "" && !oldDue.IsZero() && !oldDue.Equal(t.Due) {
			if st, stAllDay, err := propTime(p); err == nil {
				var moved time.Time
				if stAllDay || oldDueAllDay || t.DueAllDay {
					moved = st.AddDate(0, 0, daysBetween(oldDue, t.Due))
				} else {
					moved = st.Add(t.Due.Sub(oldDue))
				}
				setTimeLike(m, ical.PropDateTimeStart, p, moved)
			}
		}
		// DUE must not be before DTSTART; drop a start that would break that.
		if p := m.Props.Get(ical.PropDateTimeStart); p != nil {
			if st, _, err := propTime(p); err == nil && st.After(t.Due) {
				m.Props.Del(ical.PropDateTimeStart)
			}
		}
	}
	m.Props.Del(ical.PropCategories)
	if len(t.Categories) > 0 {
		p := ical.NewProp(ical.PropCategories)
		p.SetTextList(t.Categories)
		m.Props.Set(p)
	}
	if t.Rule == "" {
		m.Props.Del(ical.PropRecurrenceRule)
	} else {
		if m.Props.Get(ical.PropDateTimeStart) == nil {
			// RFC 5545 counts repeats from DTSTART; task apps set it to the
			// due date (Apple Reminders does), so do the same.
			setTimeLike(m, ical.PropDateTimeStart, m.Props.Get(ical.PropDue), t.Due)
		}
		rr := ical.NewProp(ical.PropRecurrenceRule)
		rr.Value = t.Rule
		m.Props.Set(rr)
	}
	done := t.Done
	advanced := false
	if done && !wasDone && t.Rule != "" {
		_, ok, err := advanceToDo(m, now)
		if err != nil {
			return ToDo{}, err
		}
		if ok {
			done, advanced = false, true
		}
	}
	setDone(m, done, now)
	if advanced {
		// Keep when it was last done, for apps that show it.
		m.Props.SetDateTime("X-KTX-LAST-DONE", now.UTC())
	}
	m.Props.SetDateTime(ical.PropLastModified, now.UTC())
	m.Props.SetDateTime(ical.PropDateTimeStamp, now.UTC())

	var out Object
	_, err := s.update(ctx, user, func(root string) ([]string, string, error) {
		o, isNew, paths, err := PutObject(root, t.Calendar, t.Name, cal, opt)
		out = o
		verb := "update task"
		switch {
		case isNew:
			verb = "add task"
		case advanced:
			verb = "done, next due " + nextDueLabel(m)
		}
		return paths, fmt.Sprintf("Calendar: %s %q (%s)", verb, t.Summary, t.Calendar.Key()), err
	})
	if err != nil {
		return ToDo{}, err
	}
	return toDoOf(t.Calendar, out)
}

// ErrNeedsDue is returned for a repeating task without a due date.
var ErrNeedsDue = fmt.Errorf("%w: a repeating task needs a due date", ErrInvalid)

func isDone(m *ical.Component) bool {
	st, _ := m.Props.Text(ical.PropStatus)
	return strings.EqualFold(st, "COMPLETED") || m.Props.Get(ical.PropCompleted) != nil
}

func nextDueLabel(m *ical.Component) string {
	if p := m.Props.Get(ical.PropDue); p != nil {
		if t, allDay, err := propTime(p); err == nil {
			if allDay {
				return t.Format("2006-01-02")
			}
			return t.In(DefaultZone()).Format("2006-01-02 15:04")
		}
	}
	return "?"
}

// SetToDoDone ticks a task done or open again. A repeating task ticked done
// moves to its next date and stays open.
func (s *Service) SetToDoDone(ctx context.Context, c Calendar, name, etag string, done bool, now time.Time, user string) (ToDo, error) {
	t, err := s.ToDo(c, name)
	if err != nil {
		return ToDo{}, err
	}
	if etag != "" && etag != t.ETag {
		return ToDo{}, ErrPrecondition
	}
	if t.Done == done {
		return t, nil
	}
	t.Done = done
	return s.SaveToDo(ctx, t, now, user)
}

func setDone(m *ical.Component, done bool, now time.Time) {
	wasDone := m.Props.Get(ical.PropCompleted) != nil
	if st, _ := m.Props.Text(ical.PropStatus); strings.EqualFold(st, "COMPLETED") {
		wasDone = true
	}
	if done {
		m.Props.SetText(ical.PropStatus, "COMPLETED")
		m.Props.SetText(ical.PropPercentComplete, "100")
		if !wasDone || m.Props.Get(ical.PropCompleted) == nil {
			m.Props.SetDateTime(ical.PropCompleted, now.UTC())
		}
		return
	}
	if wasDone {
		m.Props.SetText(ical.PropStatus, "NEEDS-ACTION")
		m.Props.Del(ical.PropCompleted)
		m.Props.Del(ical.PropPercentComplete)
	} else if m.Props.Get(ical.PropStatus) == nil {
		m.Props.SetText(ical.PropStatus, "NEEDS-ACTION")
	}
}

func newUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:] + "@ktx"
}
