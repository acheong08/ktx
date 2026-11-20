package feeds

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/secrets"
	"github.com/acheong08/ktx/internal/state"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

const feedV1 = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Luma//EN\r\n" +
	"BEGIN:VEVENT\r\nUID:evt-aaa@events.lu.ma\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261124T020000Z\r\nDTEND:20261124T040000Z\r\nSUMMARY:AI Night\r\nURL:https://luma.com/aaa\r\nEND:VEVENT\r\n" +
	"BEGIN:VEVENT\r\nUID:evt-bbb@events.lu.ma\r\nDTSTAMP:20261101T000000Z\r\nDTSTART:20261125T020000Z\r\nDTEND:20261125T030000Z\r\nSUMMARY:Founders dinner\r\nEND:VEVENT\r\n" +
	"END:VCALENDAR\r\n"

// fakeFeed serves a feed with an ETag and counts requests.
type fakeFeed struct {
	*httptest.Server
	mu    sync.Mutex
	body  string
	gets  int
	n304  int
	token string
}

func newFeed(t *testing.T, body string) *fakeFeed {
	f := &fakeFeed{body: body, token: "sekret-token-123"}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.gets++
		if r.URL.Query().Get("token") != f.token {
			http.Error(w, "nope", http.StatusForbidden)
			return
		}
		etag := fmt.Sprintf(`"%x"`, sha256.Sum256([]byte(f.body)))
		if r.Header.Get("If-None-Match") == etag {
			f.n304++
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "text/calendar")
		io.WriteString(w, f.body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeFeed) url() string { return f.URL + "/ics/get?token=" + f.token }

type env struct {
	m      *Manager
	cal    *calendar.Service
	st     *store.Store
	db     *state.DB
	remote string
	dir    string
}

func setup(t *testing.T, f *fakeFeed) *env {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo, err := gitrepo.Open(context.Background(), gitrepo.Options{RemoteURL: remote, Branch: "main", Dir: filepath.Join(dir, "work"), AuthorName: "t", AuthorEmail: "t@e", Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st := store.New(repo)
	st.SetLinker(calendar.Linker{})
	cal := calendar.NewService(repo, st.Relink)
	box, _ := secrets.New(make([]byte, 32))
	var opts []Option
	if f != nil {
		opts = append(opts, WithHTTPClient(f.Client()))
	}
	m := New(db, box, cal, filepath.Join(dir, "feeds"), quiet, opts...)
	t.Cleanup(func() { calendar.SetFeedDir("") })
	return &env{m: m, cal: cal, st: st, db: db, remote: remote, dir: dir}
}

func gitOut(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestSubscribeRefreshUnsubscribe(t *testing.T) {
	f := newFeed(t, feedV1)
	e := setup(t, f)
	ctx := context.Background()

	// Bad links are refused and nothing is created.
	if err := e.m.Subscribe(ctx, "antonio", "luma", "Luma", "ftp://x/y", "", "antonio"); !errors.Is(err, calendar.ErrInvalid) {
		t.Fatalf("ftp: %v", err)
	}
	err := e.m.Subscribe(ctx, "antonio", "luma", "Luma", strings.Replace(f.url(), "sekret-token-123", "wrong", 1), "", "antonio")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("forbidden: %v", err)
	}
	if _, err := e.cal.Calendar("antonio", "luma"); !errors.Is(err, calendar.ErrNotFound) {
		t.Fatal("calendar created for a failed subscription")
	}

	// webcal:// is read as https.
	if err := e.m.Subscribe(ctx, "antonio", "luma", "Luma", strings.Replace(f.url(), "https://", "webcal://", 1), "", "antonio"); err != nil {
		t.Fatal(err)
	}
	c, err := e.cal.Calendar("antonio", "luma")
	if err != nil || !c.Subscribed() || c.Name != "Luma" {
		t.Fatalf("calendar: %+v %v", c, err)
	}
	// Git has the calendar, not the link or the events.
	files := gitOut(t, "--git-dir", e.remote, "ls-tree", "-r", "--name-only", "main", "calendar/antonio/luma")
	if strings.TrimSpace(files) != "calendar/antonio/luma/calendar.json" {
		t.Fatalf("git files: %s", files)
	}
	if log := gitOut(t, "--git-dir", e.remote, "log", "-p", "main"); strings.Contains(log, "sekret") {
		t.Fatal("the feed's secret is in git")
	}
	sub, _ := e.db.Subscription("antonio", "luma")
	if strings.Contains(string(sub.URL), "sekret") || sub.URLHint != "127.0.0.1" || sub.Items != 2 {
		t.Fatalf("stored: hint %q items %d sealed=%v", sub.URLHint, sub.Items, !strings.Contains(string(sub.URL), "sekret"))
	}

	// Events are there, read-only.
	objs, err := e.cal.Objects(c)
	if err != nil || len(objs) != 2 {
		t.Fatalf("objects: %d %v", len(objs), err)
	}
	occs, _, err := e.cal.Occurrences([]calendar.Calendar{c}, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(occs) != 2 || occs[0].Summary != "AI Night" {
		t.Fatalf("occurrences: %+v %v", occs, err)
	}
	ev := ical.NewCalendar()
	ev.Props.SetText(ical.PropVersion, "2.0")
	ev.Props.SetText(ical.PropProductID, "x")
	comp := ical.NewComponent(ical.CompEvent)
	comp.Props.SetText(ical.PropUID, "mine")
	comp.Props.SetDateTime(ical.PropDateTimeStart, time.Now())
	comp.Props.SetDateTime(ical.PropDateTimeStamp, time.Now())
	ev.Children = append(ev.Children, comp)
	if _, _, err := e.cal.Put(ctx, c, "mine.ics", ev, calendar.PutOptions{}, "antonio"); !errors.Is(err, calendar.ErrReadOnly) {
		t.Fatalf("put: %v", err)
	}
	if err := e.cal.Delete(ctx, c, objs[0].Name, "", "antonio"); !errors.Is(err, calendar.ErrReadOnly) {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := e.cal.Import(ctx, c, strings.NewReader(feedV1), "antonio"); !errors.Is(err, calendar.ErrReadOnly) {
		t.Fatalf("import: %v", err)
	}
	// Renaming is fine and keeps it a subscription.
	c.Name = "Luma events"
	if err := e.cal.UpdateCalendar(ctx, c, "antonio"); err != nil {
		t.Fatal(err)
	}
	if c, _ = e.cal.Calendar("antonio", "luma"); !c.Subscribed() || c.Name != "Luma events" {
		t.Fatalf("after rename: %+v", c)
	}

	// Unchanged feed: a conditional request, nothing changes.
	if changed, err := e.m.Refresh(ctx, "antonio", "luma"); err != nil || changed || f.n304 != 1 {
		t.Fatalf("refresh unchanged: %v %v 304s=%d", changed, err, f.n304)
	}
	// An event dropped from the feed disappears.
	f.mu.Lock()
	f.body = strings.Replace(feedV1, "BEGIN:VEVENT\r\nUID:evt-bbb", "BEGIN:VXXX\r\nUID:evt-bbb", 1)
	f.body = strings.Replace(f.body, "SUMMARY:Founders dinner\r\nEND:VEVENT", "SUMMARY:Founders dinner\r\nEND:VXXX", 1)
	f.mu.Unlock()
	if changed, err := e.m.Refresh(ctx, "antonio", "luma"); err != nil || !changed {
		t.Fatalf("refresh changed: %v %v", changed, err)
	}
	if objs, _ := e.cal.Objects(c); len(objs) != 1 {
		t.Fatalf("after change: %d objects", len(objs))
	}
	// A broken feed keeps the last copy and records the error.
	f.mu.Lock()
	f.body = "<html>maintenance</html>"
	f.mu.Unlock()
	if _, err := e.m.Refresh(ctx, "antonio", "luma"); err == nil {
		t.Fatal("broken feed accepted")
	}
	if objs, _ := e.cal.Objects(c); len(objs) != 1 {
		t.Fatal("last good copy lost")
	}
	if sub, _ = e.db.Subscription("antonio", "luma"); sub.Error == "" {
		t.Fatal("error not recorded")
	}

	// Unsubscribe removes the calendar, the link and the cache.
	if err := e.m.Unsubscribe(ctx, "antonio", "luma", "antonio"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.cal.Calendar("antonio", "luma"); !errors.Is(err, calendar.ErrNotFound) {
		t.Fatal("calendar still there")
	}
	if _, err := e.db.Subscription("antonio", "luma"); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("link still stored")
	}
	if _, err := os.Stat(filepath.Join(e.dir, "feeds", "antonio", "luma.ics")); !os.IsNotExist(err) {
		t.Fatal("cache still there")
	}
	if files := gitOut(t, "--git-dir", e.remote, "ls-tree", "-r", "--name-only", "main", "calendar/antonio"); strings.Contains(files, "luma") {
		t.Fatalf("git still has it: %s", files)
	}
}

func TestFeedDuplicatesAndLinking(t *testing.T) {
	f := newFeed(t, feedV1)
	e := setup(t, f)
	ctx := context.Background()
	own := calendar.Calendar{Owner: "antonio", ID: "personal", Name: "Work"}
	if err := e.cal.CreateCalendar(ctx, own, "antonio"); err != nil {
		t.Fatal(err)
	}
	// The same Luma event, accepted from its email invite, is in personal too.
	invite := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:evt-aaa@events.lu.ma\r\nDTSTAMP:20261101T000000Z\r\n" +
		"DTSTART:20261124T020000Z\r\nDTEND:20261124T040000Z\r\nSUMMARY:AI Night (invite)\r\nATTENDEE;CN=Jane:mailto:jane@acme.com\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, _, err := e.cal.Import(ctx, own, strings.NewReader(invite), "antonio"); err != nil {
		t.Fatal(err)
	}
	// A call during the dinner, which is only in the feed.
	doc, err := transcript.Parse([]byte("---\ntitle: Dinner chat\ndate: 2026-11-25T02:05:00Z\nparticipants: [Antonio <antonio@melzi.dev>]\n---\nHi.\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.Put(ctx, doc, "antonio"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Subscribe(ctx, "antonio", "luma", "Luma", f.url(), "", "antonio"); err != nil {
		t.Fatal(err)
	}
	cals, _ := e.cal.Calendars()
	occs, _, _ := e.cal.Occurrences(cals, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	var got []string
	for _, o := range occs {
		got = append(got, o.Summary+"@"+o.Calendar)
	}
	if strings.Join(got, ",") != "AI Night (invite)@antonio/personal,Founders dinner@antonio/luma" {
		t.Fatalf("occurrences: %v", got)
	}
	// Subscribing relinked the transcript to the feed's event.
	entry, err := e.st.Get(doc.ID)
	if err != nil || entry.Meta.CalendarEvent == nil || entry.Meta.CalendarEvent.Calendar != "antonio/luma" || entry.Meta.CalendarEvent.UID != "evt-bbb@events.lu.ma" {
		t.Fatalf("link: %+v %v", entry.Meta.CalendarEvent, err)
	}
}

func TestPrivateAddressesRefused(t *testing.T) {
	f := newFeed(t, feedV1)
	e := setup(t, nil) // the real client
	err := e.m.Subscribe(context.Background(), "antonio", "luma", "Luma", f.url(), "", "antonio")
	if err == nil || !strings.Contains(err.Error(), "private address") || strings.Contains(err.Error(), "sekret") {
		t.Fatalf("err = %v", err)
	}
	for addr, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "100.64.0.14": false, "100.127.255.1": false,
		"169.254.169.254": false, "::1": false, "fd7a:115c:a1e0::1": false, "::ffff:127.0.0.1": false, "0.0.0.0": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Public(%s) = %v", addr, got)
		}
	}
}

func TestMergedSubscription(t *testing.T) {
	f := newFeed(t, feedV1)
	e := setup(t, f)
	ctx := context.Background()
	own := calendar.Calendar{Owner: "antonio", ID: "personal", Name: "Work"}
	if err := e.cal.CreateCalendar(ctx, own, "antonio"); err != nil {
		t.Fatal(err)
	}
	// AI Night was also accepted from its email invite: that copy is in git.
	invite := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:x\r\nBEGIN:VEVENT\r\nUID:evt-aaa@events.lu.ma\r\nDTSTAMP:20261101T000000Z\r\n" +
		"DTSTART:20261124T020000Z\r\nDTEND:20261124T040000Z\r\nSUMMARY:AI Night (invite)\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	if _, _, err := e.cal.Import(ctx, own, strings.NewReader(invite), "antonio"); err != nil {
		t.Fatal(err)
	}
	doc, _ := transcript.Parse([]byte("---\ntitle: Dinner chat\ndate: 2026-11-25T02:05:00Z\nparticipants: [Antonio <antonio@melzi.dev>]\n---\nHi.\n"))
	if _, err := e.st.Put(ctx, doc, "antonio"); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Subscribe(ctx, "antonio", "luma", "Luma", f.url(), "nope", "antonio"); !errors.Is(err, calendar.ErrInvalid) {
		t.Fatalf("merge into a missing calendar: %v", err)
	}
	if err := e.m.Subscribe(ctx, "antonio", "luma", "Luma", f.url(), "personal", "antonio"); err != nil {
		t.Fatal(err)
	}

	// One calendar: Work, with the feed's other event in it, read-only.
	cals, _ := e.cal.Calendars()
	if len(cals) != 1 || cals[0].ID != "personal" {
		t.Fatalf("calendars: %+v", cals)
	}
	if subs, _ := e.cal.Subscriptions(); len(subs) != 1 || subs[0].MergedInto() != "personal" {
		t.Fatalf("subscriptions: %+v", subs)
	}
	work := cals[0]
	objs, _ := e.cal.Objects(work)
	var feedObj calendar.Object
	var got []string
	for _, o := range objs {
		cal, _ := o.Decode()
		s, _ := cal.Children[0].Props.Text(ical.PropSummary)
		got = append(got, s)
		if o.Feed != "" {
			feedObj = o
		}
	}
	if strings.Join(got, ",") != "AI Night (invite),Founders dinner" || feedObj.Feed != "antonio/luma" {
		t.Fatalf("work objects: %v %+v", got, feedObj.Feed)
	}
	if o, err := e.cal.Object(work, feedObj.Name); err != nil || o.Feed == "" {
		t.Fatalf("get feed object: %v", err)
	}
	cal, _ := feedObj.Decode()
	if _, _, err := e.cal.Put(ctx, work, feedObj.Name, cal, calendar.PutOptions{}, "antonio"); !errors.Is(err, calendar.ErrReadOnly) {
		t.Fatalf("put over feed event: %v", err)
	}
	if err := e.cal.Delete(ctx, work, feedObj.Name, "", "antonio"); !errors.Is(err, calendar.ErrReadOnly) {
		t.Fatalf("delete feed event: %v", err)
	}
	// The transcript links to the event in Work.
	entry, _ := e.st.Get(doc.ID)
	if l := entry.Meta.CalendarEvent; l == nil || l.Calendar != "antonio/personal" || l.UID != "evt-bbb@events.lu.ma" {
		t.Fatalf("link: %+v", entry.Meta.CalendarEvent)
	}
	// Not in git.
	if files := gitOut(t, "--git-dir", e.remote, "ls-tree", "-r", "--name-only", "main", "calendar/antonio"); strings.Contains(files, "evt-bbb") {
		t.Fatalf("feed event in git: %s", files)
	}

	// Back to a calendar of its own: listed again, links follow.
	if err := e.m.SetMerge(ctx, "antonio", "luma", "", "antonio"); err != nil {
		t.Fatal(err)
	}
	if cals, _ := e.cal.Calendars(); len(cals) != 2 {
		t.Fatalf("calendars after unmerge: %+v", cals)
	}
	work, _ = e.cal.Calendar("antonio", "personal")
	if objs, _ := e.cal.Objects(work); len(objs) != 1 {
		t.Fatalf("work after unmerge: %d", len(objs))
	}
	if entry, _ = e.st.Get(doc.ID); entry.Meta.CalendarEvent == nil || entry.Meta.CalendarEvent.Calendar != "antonio/luma" {
		t.Fatalf("link after unmerge: %+v", entry.Meta.CalendarEvent)
	}
}
