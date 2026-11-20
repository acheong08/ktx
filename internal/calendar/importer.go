package calendar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/emersion/go-ical"
)

const prodID = "-//ktx//calendar//EN"

// normalize fills in what the encoder requires and some exports leave out,
// without making the result depend on the time of the write: PRODID and
// VERSION on the calendar, DTSTAMP on each event or to-do (from
// LAST-MODIFIED, CREATED, DTSTART or DUE).
func normalize(cal *ical.Calendar) {
	if cal.Props.Get(ical.PropProductID) == nil {
		cal.Props.SetText(ical.PropProductID, prodID)
	}
	if cal.Props.Get(ical.PropVersion) == nil {
		cal.Props.SetText(ical.PropVersion, "2.0")
	}
	for _, c := range cal.Children {
		if (c.Name != ical.CompEvent && c.Name != ical.CompToDo) || c.Props.Get(ical.PropDateTimeStamp) != nil {
			continue
		}
		for _, from := range []string{ical.PropLastModified, ical.PropCreated, ical.PropDateTimeStart, ical.PropDue} {
			if p := c.Props.Get(from); p != nil {
				if t, _, err := propTime(p); err == nil {
					c.Props.SetDateTime(ical.PropDateTimeStamp, t.UTC())
					break
				}
			}
		}
	}
}

// ImportReport says what an import did.
type ImportReport struct {
	Added, Updated, Unchanged int
	Skipped                   []string // what wasn't imported, and why
	Warnings                  []string // imported, but something to know
}

// Import upserts every event and to-do in an .ics stream into c: they are
// grouped by UID (a series and its changed occurrences together), each UID
// becomes one file, and files whose content is unchanged are left alone.
// Nothing is deleted. It returns the paths to commit.
func Import(root string, c Calendar, r io.Reader) (ImportReport, []string, error) {
	var rep ImportReport
	if cur, err := GetCalendar(root, c.Owner, c.ID); err != nil {
		return rep, nil, err
	} else if cur.Subscribed() {
		return rep, nil, ErrReadOnly
	}

	zones := map[string]*ical.Component{}
	byUID := map[string][]*ical.Component{}
	var order []string
	dec := ical.NewDecoder(r)
	for n := 0; ; n++ {
		cal, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			if n == 0 {
				return rep, nil, fmt.Errorf("%w: no calendar data in the file", ErrInvalid)
			}
			break
		}
		if err != nil {
			return rep, nil, fmt.Errorf("%w: not a valid .ics file: %v", ErrInvalid, err)
		}
		for _, child := range cal.Children {
			switch child.Name {
			case ical.CompTimezone:
				if id, _ := child.Props.Text(ical.PropTimezoneID); id != "" {
					zones[id] = child
				}
			case ical.CompEvent, ical.CompToDo:
				uid, _ := child.Props.Text(ical.PropUID)
				if uid == "" {
					summary, _ := child.Props.Text(ical.PropSummary)
					rep.Skipped = append(rep.Skipped, fmt.Sprintf("%q: no UID", summary))
					continue
				}
				if _, seen := byUID[uid]; !seen {
					order = append(order, uid)
				}
				byUID[uid] = append(byUID[uid], child)
			default:
				rep.Skipped = append(rep.Skipped, "a "+child.Name+" (only events and to-dos are imported)")
			}
		}
	}

	existing := map[string]string{} // uid -> file name
	taken := map[string]bool{}
	objs, err := ListObjects(root, c)
	if err != nil {
		return rep, nil, err
	}
	for _, o := range objs {
		taken[o.Name] = true
		if uid := fileUID(o); uid != "" {
			existing[uid] = o.Name
		}
	}

	changed := false
	for _, uid := range order {
		events := byUID[uid]
		cal := ical.NewCalendar()
		cal.Props.SetText(ical.PropProductID, prodID)
		cal.Props.SetText(ical.PropVersion, "2.0")
		used := map[string]bool{}
		for _, e := range events {
			for _, props := range e.Props {
				for _, p := range props {
					if tz := p.Params.Get(ical.ParamTimezoneID); tz != "" && zones[tz] != nil && !used[tz] {
						used[tz] = true
						cal.Children = append(cal.Children, zones[tz])
					}
				}
			}
		}
		sort.SliceStable(cal.Children, func(i, j int) bool { // zones first, in a stable order
			a, _ := cal.Children[i].Props.Text(ical.PropTimezoneID)
			b, _ := cal.Children[j].Props.Text(ical.PropTimezoneID)
			return a < b
		})
		cal.Children = append(cal.Children, events...)
		normalize(cal)

		label := uid
		if s, _ := events[0].Props.Text(ical.PropSummary); s != "" {
			label = fmt.Sprintf("%q", s)
		}
		var buf bytes.Buffer
		if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
			rep.Skipped = append(rep.Skipped, label+": "+err.Error())
			continue
		}
		if _, err := checkObject(cal); err != nil {
			rep.Skipped = append(rep.Skipped, label+": "+err.Error())
			continue
		}
		if _, err := parseEvents(cal); err != nil {
			rep.Warnings = append(rep.Warnings, label+": stored, but its times can't be read: "+err.Error())
		}

		name, ok := existing[uid]
		if !ok {
			name = nameForUID(uid, taken)
			taken[name] = true
		}
		old, err := GetObject(root, c, name)
		switch {
		case err == nil && bytes.Equal(old.Raw, buf.Bytes()):
			rep.Unchanged++
			continue
		case err == nil:
			rep.Updated++
		default:
			rep.Added++
		}
		if err := writeFile(root, c.Dir()+"/"+name, buf.Bytes()); err != nil {
			return rep, nil, err
		}
		changed = true
	}
	if !changed {
		return rep, nil, nil
	}
	return rep, []string{c.Dir()}, nil
}

// nameForUID makes a file name from a UID: the UID itself when it is a safe
// name, else a hash of it.
func nameForUID(uid string, taken map[string]bool) string {
	if name := uid + ".ics"; ValidObjectName(name) && !taken[name] {
		return name
	}
	sum := sha256.Sum256([]byte(uid))
	return strings.ToLower(hex.EncodeToString(sum[:10])) + ".ics"
}
