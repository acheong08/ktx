package calendar

import (
	"context"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

var la = mustLoad("America/Los_Angeles")

// An export like Google's: a weekly series with one moved and one cancelled
// occurrence, a one-off meeting with guests, an all-day event, an Outlook
// time zone name, an event without DTSTAMP, and a task.
const export = `BEGIN:VCALENDAR
VERSION:2.0
PRODID:-//Google Inc//Google Calendar 70.9054//EN
BEGIN:VTIMEZONE
TZID:America/Los_Angeles
BEGIN:STANDARD
DTSTART:19701101T020000
TZOFFSETFROM:-0700
TZOFFSETTO:-0800
RRULE:FREQ=YEARLY;BYMONTH=11;BYDAY=1SU
END:STANDARD
END:VTIMEZONE
BEGIN:VEVENT
UID:weekly@google.com
DTSTAMP:20261101T000000Z
DTSTART;TZID=America/Los_Angeles:20261102T100000
DTEND;TZID=America/Los_Angeles:20261102T103000
RRULE:FREQ=WEEKLY;COUNT=6
EXDATE;TZID=America/Los_Angeles:20261123T100000
SUMMARY:Weekly sync
ORGANIZER;CN=Antonio:mailto:ac@melzi.dev
ATTENDEE;CN=Maxim;PARTSTAT=ACCEPTED:mailto:maxim@melzi.dev
END:VEVENT
BEGIN:VEVENT
UID:weekly@google.com
DTSTAMP:20261101T000000Z
RECURRENCE-ID;TZID=America/Los_Angeles:20261116T100000
DTSTART;TZID=America/Los_Angeles:20261116T140000
DTEND;TZID=America/Los_Angeles:20261116T143000
SUMMARY:Weekly sync (moved)
END:VEVENT
BEGIN:VEVENT
UID:acme-intro@google.com
DTSTAMP:20261101T000000Z
DTSTART:20261114T180000Z
DTEND:20261114T183000Z
SUMMARY:Acme <> Melzi
ORGANIZER;CN=Maxim:mailto:maxim@melzi.dev
ATTENDEE;CN=Jane Doe;PARTSTAT=ACCEPTED:mailto:Jane@Acme.com
ATTENDEE;CN=Bob;PARTSTAT=DECLINED:mailto:bob@acme.com
ATTENDEE;CUTYPE=ROOM;CN=Room 1:mailto:room1@resource.calendar.google.com
END:VEVENT
BEGIN:VEVENT
UID:offsite
DTSTAMP:20261101T000000Z
DTSTART;VALUE=DATE:20261114
DTEND;VALUE=DATE:20261115
SUMMARY:Offsite
END:VEVENT
BEGIN:VEVENT
UID:outlook-1
DTSTART;TZID="Eastern Standard Time":20261117T090000
DTEND;TZID="Eastern Standard Time":20261117T100000
SUMMARY:Investor call
ATTENDEE;CN=Pat:mailto:pat@fund.vc
END:VEVENT
BEGIN:VTODO
UID:task-1
DTSTAMP:20261101T000000Z
SUMMARY:A task
END:VTODO
END:VCALENDAR
`

type env struct {
	repo   *gitrepo.Repo
	st     *store.Store
	svc    *Service
	remote string
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	repo, err := gitrepo.Open(context.Background(), gitrepo.Options{RemoteURL: remote, Branch: "main", Dir: filepath.Join(dir, "work"),
		AuthorName: "t", AuthorEmail: "t@e", Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(repo)
	st.SetLinker(Linker{})
	return &env{repo: repo, st: st, svc: NewService(repo, st.Relink), remote: remote}
}

func gitOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestImportAndOccurrences(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	cal := Calendar{Owner: "antonio", ID: "work", Name: "Work"}
	if err := e.svc.CreateCalendar(ctx, cal, "antonio"); err != nil {
		t.Fatal(err)
	}
	rep, git, err := e.svc.Import(ctx, cal, strings.NewReader(export), "antonio")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Added != 4 || len(rep.Skipped) != 1 || !strings.Contains(rep.Skipped[0], "VTODO") || !git.Pushed {
		t.Fatalf("report: %+v %+v", rep, git)
	}
	msg := gitOut(t, "--git-dir", e.remote, "log", "-1", "--format=%s", "main")
	if msg != "Calendar: import into antonio/work: 4 added, 0 updated" {
		t.Errorf("subject = %q", msg)
	}
	// The series and its moved occurrence share one file named after the UID.
	file := gitOut(t, "--git-dir", e.remote, "show", "main:calendar/antonio/work/weekly@google.com.ics")
	if strings.Count(file, "BEGIN:VEVENT") != 2 || !strings.Contains(file, "BEGIN:VTIMEZONE") {
		t.Errorf("series file:\n%s", file)
	}
	// DTSTAMP was filled in for the Outlook event, from its start.
	if f := gitOut(t, "--git-dir", e.remote, "show", "main:calendar/antonio/work/outlook-1.ics"); !strings.Contains(f, "DTSTAMP:20261117T140000Z") {
		t.Errorf("outlook file:\n%s", f)
	}

	// Importing again changes nothing and commits nothing.
	n := gitOut(t, "--git-dir", e.remote, "rev-list", "--count", "main")
	rep, _, err = e.svc.Import(ctx, cal, strings.NewReader(export), "antonio")
	if err != nil || rep.Unchanged != 4 || rep.Added+rep.Updated != 0 {
		t.Fatalf("re-import: %+v %v", rep, err)
	}
	if gitOut(t, "--git-dir", e.remote, "rev-list", "--count", "main") != n {
		t.Error("unchanged import committed")
	}

	occs, problems, err := e.svc.Occurrences([]Calendar{cal}, time.Date(2026, 11, 1, 0, 0, 0, 0, la), time.Date(2026, 12, 31, 0, 0, 0, 0, la))
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	var got []string
	for _, o := range occs {
		got = append(got, o.Start.In(la).Format("Jan 2 15:04")+" "+o.Summary)
	}
	want := []string{
		"Nov 2 10:00 Weekly sync", "Nov 9 10:00 Weekly sync", "Nov 14 00:00 Offsite", "Nov 14 10:00 Acme <> Melzi",
		"Nov 16 14:00 Weekly sync (moved)", "Nov 17 06:00 Investor call", // 9:00 Eastern
		"Nov 30 10:00 Weekly sync", "Dec 7 10:00 Weekly sync", // Nov 23 is an exception date
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("occurrences:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, o := range occs {
		if o.UID == "acme-intro@google.com" {
			p := o.People()
			if len(p) != 2 || p[0].Email != "maxim@melzi.dev" || p[1].Email != "jane@acme.com" {
				t.Errorf("people (declined and rooms left out, emails lowercased): %+v", p)
			}
		}
	}
}

func occ(cal, uid, summary string, start time.Time, minutes int, guests ...string) Occurrence {
	o := Occurrence{Calendar: cal, Owner: strings.Split(cal, "/")[0], UID: uid, Summary: summary, Start: start, End: start.Add(time.Duration(minutes) * time.Minute)}
	for _, g := range guests {
		o.Attendees = append(o.Attendees, Attendee{Email: g})
	}
	return o
}

func TestMatch(t *testing.T) {
	at := time.Date(2026, 11, 14, 10, 0, 0, 0, la)
	acme := occ("maxim/work", "acme", "Acme <> Melzi", at, 30, "jane@acme.com")
	focus := occ("maxim/work", "focus", "Focus time", at, 120)

	cases := []struct {
		name     string
		occs     []Occurrence
		start    time.Time
		eventID  string
		title    string
		wantUID  string
		wantKind string
		wantTied int
	}{
		{"starts late, within window", []Occurrence{acme}, at.Add(12 * time.Minute), "", "", "acme", MatchTime, 0},
		{"too far before", []Occurrence{acme}, at.Add(-20 * time.Minute), "", "", "", "", 0},
		{"during a long event", []Occurrence{occ("a/x", "workshop", "Workshop", at, 180)}, at.Add(90 * time.Minute), "", "", "workshop", MatchTime, 0},
		{"guests beat a solo block", []Occurrence{focus, acme}, at, "", "", "acme", MatchTime, 0},
		{"guests beat a nearer solo block", []Occurrence{occ("a/x", "solo", "Standup", at.Add(2*time.Minute), 10), occ("a/x", "acme", "Acme", at.Add(-3*time.Minute), 30, "j@acme.com")}, at, "", "", "acme", MatchTime, 0},
		{"nearest start wins", []Occurrence{acme, occ("a/x", "b", "Other", at.Add(10*time.Minute), 30, "x@y.z")}, at.Add(2 * time.Minute), "", "", "acme", MatchTime, 0},
		{"title breaks a tie", []Occurrence{occ("a/x", "a", "Lunch", at, 30, "x@y.z"), occ("a/x", "b", "Acme intro", at, 30, "q@acme.com")}, at, "", "Acme intro call", "b", MatchTime, 0},
		{"true tie is no match", []Occurrence{occ("a/x", "a", "Call", at, 30, "x@y.z"), occ("a/x", "b", "Call", at, 30, "q@w.e")}, at, "", "", "", "", 2},
		{"all-day events never match", []Occurrence{{Calendar: "a/x", UID: "d", Start: at, End: at.Add(24 * time.Hour), AllDay: true}}, at, "", "", "", "", 0},
		{"granola event id wins", []Occurrence{acme, occ("a/x", "evt123@google.com", "Moved", at.Add(3*time.Hour), 30)}, at, "evt123", "", "evt123@google.com", MatchExact, 0},
		{"google instance id", []Occurrence{occ("a/x", "evt9@google.com", "Weekly", at.Add(-time.Hour), 30)}, at, "evt9_20261114T170000Z", "", "evt9@google.com", MatchExact, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, kind, tied := Match(c.occs, c.start, c.eventID, c.title, "")
			if o.UID != c.wantUID || kind != c.wantKind || len(tied) != c.wantTied {
				t.Errorf("got %q %q tied=%d", o.UID, kind, len(tied))
			}
		})
	}

	// The same event in two calendars counts once, from the recorder's calendar.
	a, m := acme, acme
	a.Calendar, a.Owner = "antonio/work", "antonio"
	if o, kind, _ := Match([]Occurrence{m, a}, at, "", "", "Antonio"); kind != MatchTime || o.Calendar != "antonio/work" {
		t.Errorf("shared event: %q %q", o.Calendar, kind)
	}
}

const call = `---
title: Acme intro
date: 2026-11-14T10:04:00-08:00
participants:
  - name: Maxim Iwanskyj
    email: maxim@melzi.dev
  - name: Jane Doe
source: granola
source_id: not_1
recorded_by: Maxim
---
Hello.
`

func TestLinkingThroughStore(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	cal := Calendar{Owner: "maxim", ID: "work"}
	if err := e.svc.CreateCalendar(ctx, cal, "maxim"); err != nil {
		t.Fatal(err)
	}

	// A call recorded before its calendar exists has no link...
	doc, err := transcript.Parse([]byte(call))
	if err != nil {
		t.Fatal(err)
	}
	res, err := e.st.Put(ctx, doc, "maxim")
	if err != nil {
		t.Fatal(err)
	}
	if f := gitOut(t, "--git-dir", e.remote, "show", "main:"+res.Path); strings.Contains(f, "calendar_event") {
		t.Fatalf("linked without a calendar:\n%s", f)
	}

	// ...and is linked in the same commit as the import that adds the event.
	if _, _, err := e.svc.Import(ctx, cal, strings.NewReader(export), "maxim"); err != nil {
		t.Fatal(err)
	}
	file := gitOut(t, "--git-dir", e.remote, "show", "main:"+res.Path)
	for _, want := range []string{
		"calendar_event:\n  uid: acme-intro@google.com\n  calendar: maxim/work\n  title: Acme <> Melzi\n  start: \"2026-11-14T18:00:00Z\"\n  match: time",
		"  - name: Jane Doe\n    email: jane@acme.com\n    from: calendar", // added from the invite
	} {
		if !strings.Contains(file, want) {
			t.Errorf("transcript lacks %q:\n%s", want, file)
		}
	}
	if strings.Count(file, "maxim@melzi.dev") != 1 {
		t.Errorf("listed participant duplicated:\n%s", file)
	}
	if msg := gitOut(t, "--git-dir", e.remote, "log", "-1", "--format=%B", "main"); !strings.Contains(msg, "Relinked-Transcripts: 1") {
		t.Errorf("commit:\n%s", msg)
	}

	// A re-sync from the source keeps the link and changes nothing.
	again, _ := transcript.Parse([]byte(call))
	if r, err := e.st.Put(ctx, again, "maxim"); err != nil || r.Result != store.ResultUnchanged {
		t.Fatalf("re-sync: %+v %v", r, err)
	}

	// A person says "no event": that sticks through re-syncs and calendar changes.
	if _, err := e.st.EditLinks(ctx, doc.ID, "antonio", "Unlink event", func(root string, d *transcript.Doc) error {
		SetManual(d, nil)
		_, err := Linker{}.Link(root, d, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	again, _ = transcript.Parse([]byte(call))
	if _, err := e.st.Put(ctx, again, "maxim"); err != nil {
		t.Fatal(err)
	}
	moved := strings.Replace(export, "SUMMARY:Acme <> Melzi", "SUMMARY:Acme <> Melzi (renamed)", 1)
	if _, _, err := e.svc.Import(ctx, cal, strings.NewReader(moved), "maxim"); err != nil {
		t.Fatal(err)
	}
	file = gitOut(t, "--git-dir", e.remote, "show", "main:"+res.Path)
	if !strings.Contains(file, "calendar_event:\n  match: none") || strings.Contains(file, "from: calendar") {
		t.Errorf("manual 'no event' not kept:\n%s", file)
	}
}
