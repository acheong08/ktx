package calendar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/emersion/go-ical"
)

// Subscribed calendars: a calendar whose calendar.json has "subscription"
// shows the events of an iCal feed (Luma, Google, Outlook…) instead of
// files in git. Git keeps only the calendar itself (name, colour, refresh);
// the feed's URL holds a secret, so it lives sealed in the state database
// (package feeds), and the last fetched copy lives in a cache directory
// outside git. Subscribed calendars are read-only, for their owner too.

// Subscription marks a calendar as an iCal subscription.
type Subscription struct {
	Refresh string `json:"refresh,omitempty"` // how often to fetch, e.g. "30m"
}

// Subscribed reports whether c shows an iCal feed.
func (c Calendar) Subscribed() bool { return c.Subscription != nil }

// ErrReadOnly is returned for writes to a subscribed calendar.
var ErrReadOnly = errors.New("this calendar is an iCal subscription; it's read-only")

var (
	feedMu    sync.Mutex
	feedDir   string // where fetched feeds are kept; "" means none are
	feedCache = map[string]feedEntry{}
)

type feedEntry struct {
	size  int64
	mtime time.Time
	objs  []Object
}

// SetFeedDir sets where fetched feeds are kept (package feeds writes them).
func SetFeedDir(dir string) {
	feedMu.Lock()
	feedDir, feedCache = dir, map[string]feedEntry{}
	feedMu.Unlock()
}

// FeedPath is the cached copy of a subscribed calendar's feed, or "" when
// no feed directory is set.
func FeedPath(c Calendar) string {
	feedMu.Lock()
	defer feedMu.Unlock()
	if feedDir == "" || !ValidID(c.Owner) || !ValidID(c.ID) {
		return ""
	}
	return filepath.Join(feedDir, c.Owner, c.ID+".ics")
}

// feedObjects is a subscribed calendar's feed split into one object per
// UID, as git calendars store them. No cached feed means no objects.
func feedObjects(c Calendar) ([]Object, error) {
	p := FeedPath(c)
	if p == "" {
		return nil, nil
	}
	st, err := os.Stat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	feedMu.Lock()
	e, ok := feedCache[p]
	feedMu.Unlock()
	if ok && e.size == st.Size() && e.mtime.Equal(st.ModTime()) {
		return e.objs, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	parts, _, err := SplitFeed(f)
	if err != nil {
		return nil, err
	}
	objs := make([]Object, 0, len(parts))
	for _, part := range parts {
		sum := sha256.Sum256(part.Data)
		objs = append(objs, Object{Name: part.Name, Path: path.Join(c.Dir(), part.Name), ETag: hex.EncodeToString(sum[:12]),
			ModTime: st.ModTime(), Raw: part.Data})
	}
	feedMu.Lock()
	feedCache[p] = feedEntry{size: st.Size(), mtime: st.ModTime(), objs: objs}
	feedMu.Unlock()
	return objs, nil
}

// FeedPart is one UID of a feed, as a calendar object.
type FeedPart struct {
	UID, Name string
	Data      []byte
}

// SplitFeed reads an iCal feed and splits it into one VCALENDAR per UID
// (a series with its changed occurrences, and the time zones it uses), in
// feed order, as Import stores them. What can't be stored is reported in
// skipped. It's an error if the data isn't iCalendar at all.
func SplitFeed(r io.Reader) (parts []FeedPart, skipped []string, err error) {
	zones := map[string]*ical.Component{}
	byUID := map[string][]*ical.Component{}
	var order []string
	dec := ical.NewDecoder(r)
	for n := 0; ; n++ {
		cal, err := dec.Decode()
		if errors.Is(err, io.EOF) {
			if n == 0 {
				return nil, nil, fmt.Errorf("%w: no calendar data", ErrInvalid)
			}
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: not iCalendar data: %v", ErrInvalid, err)
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
					s, _ := child.Props.Text(ical.PropSummary)
					skipped = append(skipped, fmt.Sprintf("%q: no UID", s))
					continue
				}
				if _, seen := byUID[uid]; !seen {
					order = append(order, uid)
				}
				byUID[uid] = append(byUID[uid], child)
			}
		}
	}
	taken := map[string]bool{}
	for _, uid := range order {
		comps := byUID[uid]
		cal := ical.NewCalendar()
		cal.Props.SetText(ical.PropProductID, prodID)
		cal.Props.SetText(ical.PropVersion, "2.0")
		used := map[string]bool{}
		var tzs []*ical.Component
		for _, c := range comps {
			for _, props := range c.Props {
				for _, p := range props {
					if tz := p.Params.Get(ical.ParamTimezoneID); tz != "" && zones[tz] != nil && !used[tz] {
						used[tz] = true
						tzs = append(tzs, zones[tz])
					}
				}
			}
		}
		sort.SliceStable(tzs, func(i, j int) bool {
			a, _ := tzs[i].Props.Text(ical.PropTimezoneID)
			b, _ := tzs[j].Props.Text(ical.PropTimezoneID)
			return a < b
		})
		cal.Children = append(tzs, comps...)
		normalize(cal)
		if _, err := checkObject(cal); err != nil {
			skipped = append(skipped, uid+": "+err.Error())
			continue
		}
		var buf bytes.Buffer
		if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
			skipped = append(skipped, uid+": "+err.Error())
			continue
		}
		name := nameForUID(uid, taken)
		taken[name] = true
		parts = append(parts, FeedPart{UID: uid, Name: name, Data: buf.Bytes()})
	}
	return parts, skipped, nil
}

// dropSubscribedCopies drops occurrences from subscribed calendars that a git calendar
// also has (same UID and occurrence), e.g. a Luma event accepted from its
// email invite and also in the Luma feed. The git copy wins.
func dropSubscribedCopies(occs []Occurrence, subscribed map[string]bool) []Occurrence {
	own := map[string]bool{}
	for _, o := range occs {
		if !subscribed[o.Calendar] {
			own[o.UID+"|"+o.RecurrenceID.UTC().Format(time.RFC3339)] = true
		}
	}
	out := occs[:0]
	for _, o := range occs {
		if subscribed[o.Calendar] && own[o.UID+"|"+o.RecurrenceID.UTC().Format(time.RFC3339)] {
			continue
		}
		out = append(out, o)
	}
	return out
}

// DeleteCalendar removes a calendar's folder and returns the path to
// commit. Only subscribed calendars can be deleted; a git calendar holds
// people's events.
func DeleteCalendar(root string, c Calendar) ([]string, error) {
	cur, err := GetCalendar(root, c.Owner, c.ID)
	if err != nil {
		return nil, err
	}
	if !cur.Subscribed() {
		return nil, fmt.Errorf("%w: only subscriptions can be removed here", ErrInvalid)
	}
	if err := os.RemoveAll(filepath.Join(root, filepath.FromSlash(cur.Dir()))); err != nil {
		return nil, err
	}
	return []string{cur.Dir()}, nil
}
