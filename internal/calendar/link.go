package calendar

import (
	"bytes"
	"math"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

// How a transcript got its event link.
const (
	MatchExact  = "exact"  // the recording names the event (Granola's calendar_event_id)
	MatchTime   = "time"   // the call started at or during the event
	MatchManual = "manual" // chosen by a person; never changed automatically
	MatchNone   = "none"   // a person said there is no event; never changed automatically
)

// Window is how far a call's start may be from an event's start.
const Window = 15 * time.Minute

// searchSpan is how far around a call events are considered at all.
const searchSpan = 12 * time.Hour

// LinkKey is the front matter key holding the link.
const LinkKey = "calendar_event"

// FromCalendar marks participants that came from the linked event.
const FromCalendar = "calendar"

// Link is a transcript's link to one event occurrence.
type Link struct {
	UID          string
	RecurrenceID string // RFC 3339; set for one occurrence of a repeating event
	Calendar     string // "<owner>/<id>"
	Title        string
	Start        string // RFC 3339
	Match        string
}

// Locked reports whether people set this link, so it is never recomputed.
func (l *Link) Locked() bool { return l != nil && (l.Match == MatchManual || l.Match == MatchNone) }

// ReadLink returns a document's event link, or nil. A link block without a
// match field was written by hand and counts as manual.
func ReadLink(doc *transcript.Doc) *Link {
	n := doc.Node(LinkKey)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	l := &Link{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		v := n.Content[i+1].Value
		switch n.Content[i].Value {
		case "uid":
			l.UID = v
		case "recurrence_id":
			l.RecurrenceID = v
		case "calendar":
			l.Calendar = v
		case "title":
			l.Title = v
		case "start":
			l.Start = v
		case "match":
			l.Match = v
		}
	}
	if l.Match == "" {
		l.Match = MatchManual
	}
	return l
}

// LinkFor describes an occurrence as a link.
func LinkFor(o Occurrence, match string) Link {
	l := Link{UID: o.UID, Calendar: o.Calendar, Title: o.Summary, Start: o.Start.Format(time.RFC3339), Match: match}
	if o.Recurring() {
		l.RecurrenceID = o.RecurrenceID.Format(time.RFC3339)
	}
	return l
}

func (l Link) node() *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode}
	add := func(k, v string) {
		if v != "" {
			m.Content = append(m.Content, scalar(k), scalar(v))
		}
	}
	add("uid", l.UID)
	add("recurrence_id", l.RecurrenceID)
	add("calendar", l.Calendar)
	add("title", l.Title)
	add("start", l.Start)
	add("match", l.Match)
	return m
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v} }

// Is reports whether an occurrence is the one a link points at.
func (l Link) Is(o Occurrence) bool {
	if o.UID != l.UID {
		return false
	}
	if l.RecurrenceID == "" {
		return !o.Recurring()
	}
	t, err := time.Parse(time.RFC3339, l.RecurrenceID)
	return err == nil && o.Recurring() && o.RecurrenceID.Equal(t)
}

// Match picks the event occurrence a call starting at start was recorded in.
//
//  1. eventID (Granola's calendar_event_id) naming an event: that event's
//     occurrence nearest the call.
//  2. Otherwise events (not all-day) the call started within Window of, or
//     during. One with guests beats one without (a call has someone else in
//     it); then the one with more of the call's own participants (by email
//     among its guests, or full name in its title); then the nearest start;
//     then the title closest to the call's. A tie on all of them is no match.
//
// The same event in several people's calendars counts once, preferring the
// calendar of preferOwner. It returns the match (kind "" for none) and, when
// there was a tie, the tied candidates.
func Match(occs []Occurrence, start time.Time, eventID, title, preferOwner string, people []transcript.Participant) (Occurrence, string, []Occurrence) {
	occs = dedupe(occs, strings.ToLower(preferOwner))

	if eventID != "" {
		base := eventID
		if i := strings.LastIndexByte(base, '_'); i > 0 { // Google instance ids: <event>_<start>
			base = base[:i]
		}
		var best *Occurrence
		for i := range occs {
			o := &occs[i]
			if o.UID != eventID && o.UID != base && !strings.HasPrefix(o.UID, eventID+"@") && !strings.HasPrefix(o.UID, base+"@") {
				continue
			}
			if best == nil || absDur(o.Start.Sub(start)) < absDur(best.Start.Sub(start)) {
				best = o
			}
		}
		if best != nil && absDur(best.Start.Sub(start)) <= searchSpan {
			return *best, MatchExact, nil
		}
	}

	type scored struct {
		o       Occurrence
		diff    time.Duration
		guests  bool
		overlap int
		sim     float64
	}
	var cands []scored
	for _, o := range occs {
		if o.AllDay {
			continue
		}
		diff := absDur(o.Start.Sub(start))
		during := !start.Before(o.Start) && start.Before(o.End)
		if diff > Window && !during {
			continue
		}
		if during && diff > Window {
			diff = Window // started during a long event: rank after near starts
		}
		cands = append(cands, scored{o, diff.Round(time.Minute), len(o.Guests()) > 0, overlap(people, o), similarity(title, o.Summary)})
	}
	if len(cands) == 0 {
		return Occurrence{}, "", nil
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.guests != b.guests { // a call has someone else in it
			return a.guests
		}
		if a.overlap != b.overlap {
			return a.overlap > b.overlap
		}
		if a.diff != b.diff {
			return a.diff < b.diff
		}
		return a.sim > b.sim
	})
	if len(cands) > 1 {
		a, b := cands[0], cands[1]
		same := func(x, y scored) bool {
			return x.diff == y.diff && x.guests == y.guests && x.overlap == y.overlap && math.Abs(x.sim-y.sim) < 1e-9
		}
		if same(a, b) {
			var tied []Occurrence
			for _, c := range cands {
				if same(c, a) {
					tied = append(tied, c.o)
				}
			}
			return Occurrence{}, "", tied
		}
	}
	return cands[0].o, MatchTime, nil
}

// overlap counts the call's participants who are in the event: their email
// among its organizer and guests, or their full name in its title.
// Participants that came from a calendar link don't count.
func overlap(people []transcript.Participant, o Occurrence) int {
	emails := map[string]bool{}
	for _, a := range o.People() {
		if a.Email != "" {
			emails[a.Email] = true
		}
	}
	title := words(o.Summary)
	n := 0
	for _, p := range people {
		if p.From == FromCalendar {
			continue
		}
		if e := strings.ToLower(strings.TrimSpace(p.Email)); e != "" && emails[e] {
			n++
			continue
		}
		name := words(p.Name)
		if len(name) < 2 {
			continue // a first name alone is too weak
		}
		all := true
		for w := range name {
			if !title[w] {
				all = false
				break
			}
		}
		if all {
			n++
		}
	}
	return n
}

// dedupe keeps one copy of each occurrence (same UID and occurrence),
// preferring the calendar of owner.
func dedupe(occs []Occurrence, owner string) []Occurrence {
	idx := map[string]int{}
	var out []Occurrence
	for _, o := range occs {
		k := o.UID + "|" + o.RecurrenceID.UTC().Format(time.RFC3339)
		if i, ok := idx[k]; ok {
			if owner != "" && o.Owner == owner && out[i].Owner != owner {
				out[i] = o
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, o)
	}
	return out
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// similarity is the share of words two titles have in common (0..1).
func similarity(a, b string) float64 {
	wa, wb := words(a), words(b)
	if len(wa) == 0 || len(wb) == 0 {
		return 0
	}
	common := 0
	for w := range wa {
		if wb[w] {
			common++
		}
	}
	return float64(common) / float64(len(wa)+len(wb)-common)
}

func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r > 127)
	}) {
		if len(w) >= 2 {
			out[w] = true
		}
	}
	return out
}

// Linker keeps transcripts' event links up to date. Its methods work on a
// checked-out working copy and must run under the repository lock.
type Linker struct {
	occs []Occurrence // prepared: every occurrence in a range, by start
	from time.Time
	to   time.Time
}

// Prepare returns a Linker that expands every event once over [from, to)
// (padded by the search span), for linking many transcripts at once.
func (Linker) Prepare(root string, from, to time.Time) (store.Linker, error) {
	from, to = from.Add(-searchSpan), to.Add(searchSpan)
	cals, err := ListCalendars(root)
	if err != nil {
		return nil, err
	}
	occs, _, err := Occurrences(root, cals, from, to)
	if err != nil {
		return nil, err
	}
	return &Linker{occs: occs, from: from, to: to}, nil
}

// near returns the occurrences overlapping [start-span, start+span).
func (l *Linker) near(root string, start time.Time) ([]Occurrence, error) {
	from, to := start.Add(-searchSpan), start.Add(searchSpan)
	if l.occs == nil || from.Before(l.from) || to.After(l.to) {
		cals, err := ListCalendars(root)
		if err != nil {
			return nil, err
		}
		occs, _, err := Occurrences(root, cals, from, to)
		return occs, err
	}
	// Occurrences start at most their duration before they overlap; events
	// longer than a day are rare, so look back a day further.
	lo := sort.Search(len(l.occs), func(i int) bool { return !l.occs[i].Start.Before(from.Add(-24 * time.Hour)) })
	var out []Occurrence
	for _, o := range l.occs[lo:] {
		if !o.Start.Before(to) {
			break
		}
		if o.End.After(from) || o.Start.After(from) {
			out = append(out, o)
		}
	}
	return out, nil
}

// Link sets doc's calendar_event and calendar participants. prev is the
// stored version of doc, if any: a link people set on it is carried over.
// It reports whether doc changed.
func (l Linker) Link(root string, doc, prev *transcript.Doc) (bool, error) {
	before, err := doc.Render()
	if err != nil {
		return false, err
	}
	cur := ReadLink(doc)
	if cur == nil && prev != nil {
		if p := ReadLink(prev); p.Locked() {
			doc.SetNode(LinkKey, Link(*p).node())
			cur = p
		}
	}
	if doc.Start.IsZero() {
		return false, nil
	}

	occs, err := l.near(root, doc.Start)
	if err != nil {
		return false, err
	}

	switch {
	case cur != nil && cur.Match == MatchNone:
		setPeople(doc, nil)
	case cur != nil && cur.Match == MatchManual:
		for _, o := range occs {
			if cur.Is(o) {
				setPeople(doc, o.People())
				break
			}
		}
	default:
		owner := doc.Field("recorded_by")
		if owner == "" {
			owner = doc.Field("uploaded_by")
		}
		o, kind, _ := Match(occs, doc.Start, doc.Field("calendar_event_id"), doc.Meta.Title, owner, doc.Meta.Participants)
		if kind == "" {
			doc.Unset(LinkKey)
			setPeople(doc, nil)
		} else {
			doc.SetNode(LinkKey, LinkFor(o, kind).node())
			setPeople(doc, o.People())
		}
	}

	after, err := doc.Render()
	if err != nil {
		return false, err
	}
	return !bytes.Equal(before, after), nil
}

// SetManual records a person's choice: an occurrence, or nil for "no event".
func SetManual(doc *transcript.Doc, o *Occurrence) {
	if o == nil {
		doc.SetNode(LinkKey, Link{Match: MatchNone}.node())
		return
	}
	doc.SetNode(LinkKey, LinkFor(*o, MatchManual).node())
}

// ClearManual hands the link back to automatic matching.
func ClearManual(doc *transcript.Doc) { doc.Unset(LinkKey) }

// setPeople replaces the participants that came from a calendar with
// people, skipping anyone whose email is already listed.
func setPeople(doc *transcript.Doc, people []Attendee) {
	seq := doc.Node("participants")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		if len(people) == 0 {
			return
		}
		seq = &yaml.Node{Kind: yaml.SequenceNode}
		doc.SetNode("participants", seq)
	}
	kept := seq.Content[:0]
	emails := map[string]bool{}
	for _, item := range seq.Content {
		if item.Kind == yaml.MappingNode && mapValue(item, "from") == FromCalendar {
			continue
		}
		kept = append(kept, item)
		switch item.Kind {
		case yaml.MappingNode:
			if e := strings.ToLower(mapValue(item, "email")); e != "" {
				emails[e] = true
			}
		case yaml.ScalarNode:
			if i, j := strings.LastIndexByte(item.Value, '<'), strings.LastIndexByte(item.Value, '>'); i >= 0 && j > i {
				emails[strings.ToLower(strings.TrimSpace(item.Value[i+1:j]))] = true
			}
		}
	}
	seq.Content = kept
	for _, p := range people {
		if p.Email == "" || emails[p.Email] {
			continue
		}
		emails[p.Email] = true
		m := &yaml.Node{Kind: yaml.MappingNode}
		if p.Name != "" {
			m.Content = append(m.Content, scalar("name"), scalar(p.Name))
		}
		m.Content = append(m.Content, scalar("email"), scalar(p.Email), scalar("from"), scalar(FromCalendar))
		seq.Content = append(seq.Content, m)
	}
}

func mapValue(m *yaml.Node, key string) string {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1].Value
		}
	}
	return ""
}
