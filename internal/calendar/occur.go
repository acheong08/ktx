package calendar

import (
	"fmt"
	"net/mail"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-ical"
	"github.com/teambition/rrule-go"
)

// Attendee is someone invited to an event.
type Attendee struct {
	Name   string
	Email  string // lowercase; "" when the address isn't an email
	Status string // PARTSTAT, e.g. ACCEPTED, DECLINED
	Type   string // CUTYPE, e.g. INDIVIDUAL, ROOM
}

// Occurrence is one instance of an event in time.
type Occurrence struct {
	Calendar     string // "<owner>/<id>"
	Owner        string
	UID          string
	RecurrenceID time.Time // set for an occurrence of a repeating event
	Start, End   time.Time
	AllDay       bool
	Summary      string
	Location     string
	Description  string
	Organizer    Attendee
	Attendees    []Attendee
}

// Recurring reports whether this is one occurrence of a repeating event.
func (o Occurrence) Recurring() bool { return !o.RecurrenceID.IsZero() }

// Guests are the attendees other than the organizer who haven't declined
// and aren't rooms or resources.
func (o Occurrence) Guests() []Attendee {
	var out []Attendee
	for _, a := range o.Attendees {
		if strings.EqualFold(a.Status, "DECLINED") || a.Type == "ROOM" || a.Type == "RESOURCE" {
			continue
		}
		if a.Email != "" && a.Email == o.Organizer.Email {
			continue
		}
		out = append(out, a)
	}
	return out
}

// People is the organizer plus the guests: everyone in the meeting.
func (o Occurrence) People() []Attendee {
	var out []Attendee
	if o.Organizer.Email != "" || o.Organizer.Name != "" {
		out = append(out, o.Organizer)
	}
	return append(out, o.Guests()...)
}

// event is one VEVENT, parsed.
type event struct {
	UID          string
	RecurrenceID time.Time
	Start, End   time.Time
	AllDay       bool
	Cancelled    bool
	Summary      string
	Location     string
	Description  string
	Organizer    Attendee
	Attendees    []Attendee
	rset         *rrule.Set
}

var (
	parseMu    sync.Mutex
	parseCache = map[string][]event{} // by ETag (content hash)
)

var defaultZone atomic.Pointer[time.Location]

func init() { defaultZone.Store(mustLoad("America/Los_Angeles")) }

// DefaultZone is the workspace's time zone (Settings → General): times are
// shown in it, and "floating" calendar times, which carry no zone, are read
// in it. America/Los_Angeles until set.
func DefaultZone() *time.Location { return defaultZone.Load() }

// SetDefaultZone changes the workspace's time zone. Parsed events are
// forgotten, since floating times depend on it.
func SetDefaultZone(loc *time.Location) {
	defaultZone.Store(loc)
	parseMu.Lock()
	parseCache = map[string][]event{}
	parseMu.Unlock()
}

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return l
}

// parsed returns the events of an object, cached by content.
func parsed(o Object) ([]event, error) {
	parseMu.Lock()
	ev, ok := parseCache[o.ETag]
	parseMu.Unlock()
	if ok {
		return ev, nil
	}
	cal, err := o.Decode()
	if err != nil {
		return nil, err
	}
	ev, err = parseEvents(cal)
	if err != nil {
		return nil, err
	}
	parseMu.Lock()
	parseCache[o.ETag] = ev
	parseMu.Unlock()
	return ev, nil
}

func parseEvents(cal *ical.Calendar) ([]event, error) {
	var out []event
	for _, c := range cal.Children {
		if c.Name != ical.CompEvent {
			continue
		}
		e, err := parseEvent(c)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func parseEvent(c *ical.Component) (event, error) {
	var e event
	e.UID, _ = c.Props.Text(ical.PropUID)
	e.Summary, _ = c.Props.Text(ical.PropSummary)
	e.Location, _ = c.Props.Text(ical.PropLocation)
	e.Description, _ = c.Props.Text(ical.PropDescription)
	if st := c.Props.Get(ical.PropStatus); st != nil && strings.EqualFold(st.Value, "CANCELLED") {
		e.Cancelled = true
	}
	start := c.Props.Get(ical.PropDateTimeStart)
	if start == nil {
		return e, fmt.Errorf("event %q has no start", e.UID)
	}
	var err error
	if e.Start, e.AllDay, err = propTime(start); err != nil {
		return e, err
	}
	switch {
	case c.Props.Get(ical.PropDateTimeEnd) != nil:
		if e.End, _, err = propTime(c.Props.Get(ical.PropDateTimeEnd)); err != nil {
			return e, err
		}
	case c.Props.Get(ical.PropDuration) != nil:
		d, err := c.Props.Get(ical.PropDuration).Duration()
		if err != nil {
			return e, err
		}
		e.End = e.Start.Add(d)
	case e.AllDay:
		e.End = e.Start.AddDate(0, 0, 1)
	default:
		e.End = e.Start
	}
	if rid := c.Props.Get(ical.PropRecurrenceID); rid != nil {
		if e.RecurrenceID, _, err = propTime(rid); err != nil {
			return e, err
		}
	}
	if org := c.Props.Get(ical.PropOrganizer); org != nil {
		e.Organizer = attendee(org)
	}
	for _, a := range c.Props.Values(ical.PropAttendee) {
		e.Attendees = append(e.Attendees, attendee(&a))
	}

	if rr := c.Props.Get(ical.PropRecurrenceRule); rr != nil && e.RecurrenceID.IsZero() {
		opt, err := rrule.StrToROption(rr.Value)
		if err != nil {
			return e, fmt.Errorf("event %q: repeat rule: %w", e.UID, err)
		}
		opt.Dtstart = e.Start
		rule, err := rrule.NewRRule(*opt)
		if err != nil {
			return e, fmt.Errorf("event %q: repeat rule: %w", e.UID, err)
		}
		set := &rrule.Set{}
		set.RRule(rule)
		for _, p := range c.Props.Values(ical.PropExceptionDates) {
			for _, t := range propTimes(&p) {
				set.ExDate(t)
			}
		}
		for _, p := range c.Props.Values(ical.PropRecurrenceDates) {
			for _, t := range propTimes(&p) {
				set.RDate(t)
			}
		}
		e.rset = set
	}
	return e, nil
}

func attendee(p *ical.Prop) Attendee {
	a := Attendee{
		Name:   strings.TrimSpace(p.Params.Get(ical.ParamCommonName)),
		Status: strings.ToUpper(p.Params.Get(ical.ParamParticipationStatus)),
		Type:   strings.ToUpper(p.Params.Get(ical.ParamCalendarUserType)),
	}
	v := strings.TrimSpace(p.Value)
	if lower := strings.ToLower(v); strings.HasPrefix(lower, "mailto:") {
		v = v[len("mailto:"):]
	}
	if addr, err := mail.ParseAddress(v); err == nil {
		a.Email = strings.ToLower(addr.Address)
	}
	return a
}

// propTime parses a DATE or DATE-TIME property, resolving its TZID.
func propTime(p *ical.Prop) (time.Time, bool, error) {
	v := p.Value
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return parseValue(v, p.Params.Get(ical.ParamTimezoneID), p.ValueType() == ical.ValueDate)
}

// propTimes parses every value of a (possibly comma-separated) EXDATE/RDATE.
func propTimes(p *ical.Prop) []time.Time {
	var out []time.Time
	for _, v := range strings.Split(p.Value, ",") {
		if t, _, err := parseValue(strings.TrimSpace(v), p.Params.Get(ical.ParamTimezoneID), p.ValueType() == ical.ValueDate); err == nil {
			out = append(out, t)
		}
	}
	return out
}

func parseValue(v, tzid string, date bool) (time.Time, bool, error) {
	loc := DefaultZone()
	if tzid != "" {
		loc = Zone(tzid)
	}
	if date || len(v) == len("20060102") {
		t, err := time.ParseInLocation("20060102", v, loc)
		return t, true, err
	}
	if strings.HasSuffix(v, "Z") {
		t, err := time.Parse("20060102T150405Z", v)
		return t, false, err
	}
	t, err := time.ParseInLocation("20060102T150405", v, loc)
	return t, false, err
}

// Zone resolves an iCalendar TZID: IANA names, Windows names (Outlook), and
// prefixed names such as "/mozilla.org/20050126_1/America/New_York".
// Unknown zones fall back to DefaultZone().
func Zone(tzid string) *time.Location {
	tzid = strings.Trim(tzid, `" `)
	if l, err := time.LoadLocation(tzid); err == nil && tzid != "" && tzid != "Local" {
		return l
	}
	if name, ok := windowsZones[tzid]; ok {
		return mustLoad(name)
	}
	parts := strings.Split(tzid, "/")
	for i := 1; i < len(parts); i++ {
		if l, err := time.LoadLocation(strings.Join(parts[i:], "/")); err == nil {
			return l
		}
	}
	return DefaultZone()
}

// windowsZones maps the Windows zone names Outlook writes to IANA names.
var windowsZones = map[string]string{
	"Pacific Standard Time":          "America/Los_Angeles",
	"Mountain Standard Time":         "America/Denver",
	"US Mountain Standard Time":      "America/Phoenix",
	"Central Standard Time":          "America/Chicago",
	"Eastern Standard Time":          "America/New_York",
	"Atlantic Standard Time":         "America/Halifax",
	"Alaskan Standard Time":          "America/Anchorage",
	"Hawaiian Standard Time":         "Pacific/Honolulu",
	"GMT Standard Time":              "Europe/London",
	"Greenwich Standard Time":        "Atlantic/Reykjavik",
	"W. Europe Standard Time":        "Europe/Berlin",
	"Romance Standard Time":          "Europe/Paris",
	"Central Europe Standard Time":   "Europe/Budapest",
	"Central European Standard Time": "Europe/Warsaw",
	"E. Europe Standard Time":        "Europe/Chisinau",
	"FLE Standard Time":              "Europe/Kiev",
	"GTB Standard Time":              "Europe/Bucharest",
	"Russian Standard Time":          "Europe/Moscow",
	"Turkey Standard Time":           "Europe/Istanbul",
	"Israel Standard Time":           "Asia/Jerusalem",
	"Arabian Standard Time":          "Asia/Dubai",
	"India Standard Time":            "Asia/Kolkata",
	"China Standard Time":            "Asia/Shanghai",
	"Singapore Standard Time":        "Asia/Singapore",
	"Tokyo Standard Time":            "Asia/Tokyo",
	"Korea Standard Time":            "Asia/Seoul",
	"AUS Eastern Standard Time":      "Australia/Sydney",
	"New Zealand Standard Time":      "Pacific/Auckland",
	"E. South America Standard Time": "America/Sao_Paulo",
	"UTC":                            "UTC",
}

// expand returns the occurrences of one file's events that overlap
// [from, to). A file is one series: the main event plus overridden
// occurrences (RECURRENCE-ID).
func expand(cal Calendar, events []event, from, to time.Time) []Occurrence {
	var master *event
	overrides := map[int64]event{}
	for i := range events {
		if events[i].RecurrenceID.IsZero() {
			master = &events[i]
		} else {
			overrides[events[i].RecurrenceID.Unix()] = events[i]
		}
	}
	occ := func(e event, start time.Time, rid time.Time) Occurrence {
		return Occurrence{
			Calendar: cal.Key(), Owner: cal.Owner, UID: e.UID, RecurrenceID: rid,
			Start: start, End: start.Add(e.End.Sub(e.Start)), AllDay: e.AllDay,
			Summary: e.Summary, Location: e.Location, Description: e.Description,
			Organizer: e.Organizer, Attendees: e.Attendees,
		}
	}
	overlaps := func(o Occurrence) bool {
		end := o.End
		if !end.After(o.Start) {
			end = o.Start.Add(time.Second)
		}
		return o.Start.Before(to) && end.After(from)
	}
	var out []Occurrence
	if master != nil && !master.Cancelled {
		if master.rset != nil {
			dur := master.End.Sub(master.Start)
			for _, t := range master.rset.Between(from.Add(-dur), to, true) {
				if _, over := overrides[t.Unix()]; over {
					continue
				}
				if o := occ(*master, t, t); overlaps(o) {
					out = append(out, o)
				}
			}
		} else if o := occ(*master, master.Start, time.Time{}); overlaps(o) {
			out = append(out, o)
		}
	}
	for _, e := range overrides {
		if e.Cancelled {
			continue
		}
		if o := occ(e, e.Start, e.RecurrenceID); overlaps(o) {
			out = append(out, o)
		}
	}
	return out
}

// Occurrences returns every occurrence overlapping [from, to) in the given
// calendars, sorted by start. Files that can't be parsed are skipped and
// reported in problems.
func Occurrences(root string, cals []Calendar, from, to time.Time) (out []Occurrence, problems []string, err error) {
	for _, c := range cals {
		objs, err := ListObjects(root, c)
		if err != nil {
			return nil, nil, err
		}
		for _, o := range objs {
			ev, err := parsed(o)
			if err != nil {
				problems = append(problems, o.Path+": "+err.Error())
				continue
			}
			out = append(out, expand(c, ev, from, to)...)
		}
	}
	subscribed := map[string]bool{}
	for _, c := range cals {
		if c.Subscribed() {
			subscribed[c.Key()] = true
		}
	}
	if len(subscribed) > 0 {
		out = dropSubscribedCopies(out, subscribed)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, problems, nil
}
