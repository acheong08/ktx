package dav

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/state"
	"github.com/acheong08/ktx/internal/store"
)

type env struct {
	ts                 *httptest.Server
	svc                *calendar.Service
	remote             string
	antonioTok, maxTok string
	apiTok             string
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	if out, err := exec.Command("git", "init", "--quiet", "--bare", "-b", "main", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo, err := gitrepo.Open(context.Background(), gitrepo.Options{RemoteURL: remote, Branch: "main", Dir: filepath.Join(dir, "work"),
		AuthorName: "t", AuthorEmail: "t@e", Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	st := store.New(repo)
	st.SetLinker(calendar.Linker{})
	svc := calendar.NewService(repo, st.Relink)
	db, err := state.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a, _ := db.Login("sub-a", "antonio", "Antonio", "", true)
	m, _ := db.Login("sub-m", "maxim", "Maxim", "", false)
	e := &env{svc: svc, remote: remote}
	e.antonioTok, _ = db.CreateToken(a.ID, "laptop", state.ScopeCalDAV)
	e.maxTok, _ = db.CreateToken(m.ID, "phone", state.ScopeCalDAV)
	e.apiTok, _ = db.CreateAPIToken(a.ID, "script")
	ctx := context.Background()
	for _, c := range []calendar.Calendar{{Owner: "antonio", ID: "work", Name: "Work"}, {Owner: "maxim", ID: "work", Name: "Work"}} {
		if err := svc.CreateCalendar(ctx, c, c.Owner); err != nil {
			t.Fatal(err)
		}
	}
	mux := http.NewServeMux()
	New(svc, db, log).Register(mux)
	e.ts = httptest.NewServer(mux)
	t.Cleanup(e.ts.Close)
	return e
}

func (e *env) client(t *testing.T, user, tok string) *caldav.Client {
	t.Helper()
	c, err := caldav.NewClient(webdav.HTTPClientWithBasicAuth(e.ts.Client(), user, tok), e.ts.URL+Prefix+"/")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func event(uid, summary string, start time.Time) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//test//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	ev := ical.NewEvent()
	ev.Props.SetText(ical.PropUID, uid)
	ev.Props.SetDateTime(ical.PropDateTimeStamp, start.UTC())
	ev.Props.SetDateTime(ical.PropDateTimeStart, start.UTC())
	ev.Props.SetDateTime(ical.PropDateTimeEnd, start.Add(30*time.Minute).UTC())
	ev.Props.SetText(ical.PropSummary, summary)
	cal.Children = append(cal.Children, ev.Component)
	return cal
}

func TestCalDAV(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(t, "antonio", e.antonioTok)

	// Discovery, as clients do it.
	principal, err := c.FindCurrentUserPrincipal(ctx)
	if err != nil || principal != "/dav/antonio/" {
		t.Fatalf("principal %q %v", principal, err)
	}
	home, err := c.FindCalendarHomeSet(ctx, principal)
	if err != nil || home != "/dav/antonio/calendars/" {
		t.Fatalf("home %q %v", home, err)
	}
	cals, err := c.FindCalendars(ctx, home)
	if err != nil || len(cals) != 2 {
		t.Fatalf("calendars %+v %v", cals, err)
	}
	if cals[0].Path != "/dav/antonio/calendars/work/" || cals[0].ReadOnly {
		t.Errorf("own calendar: %+v", cals[0])
	}
	if cals[1].Path != "/dav/antonio/calendars/maxim~work/" || !cals[1].ReadOnly || cals[1].Name != "Work (maxim)" {
		t.Errorf("shared calendar: %+v", cals[1])
	}

	// Write to your own calendar: one commit.
	at := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	obj, err := c.PutCalendarObject(ctx, "/dav/antonio/calendars/work/e1.ics", event("e1@test", "Acme intro", at), nil)
	if err != nil || obj.ETag == "" {
		t.Fatalf("put: %+v %v", obj, err)
	}
	out, _ := exec.Command("git", "--git-dir", e.remote, "log", "-1", "--format=%s", "main").Output()
	if got := strings.TrimSpace(string(out)); got != `Calendar: add "Acme intro" (antonio/work)` {
		t.Errorf("commit %q", got)
	}

	// Stale If-Match is refused.
	if _, err := c.PutCalendarObject(ctx, "/dav/antonio/calendars/work/e1.ics", event("e1@test", "Edited", at),
		&caldav.PutCalendarObjectOptions{IfMatch: webdav.ConditionalMatch(`"stale"`)}); err == nil || !strings.Contains(err.Error(), "412") {
		t.Errorf("stale If-Match: %v", err)
	}
	// The same UID under another name is refused.
	if _, err := c.PutCalendarObject(ctx, "/dav/antonio/calendars/work/other.ics", event("e1@test", "Dup", at), nil); err == nil {
		t.Error("duplicate UID accepted")
	}

	// Time-range query.
	objs, err := c.QueryCalendar(ctx, "/dav/antonio/calendars/work/", &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{Name: "VCALENDAR", AllProps: true, AllComps: true},
		CompFilter:  caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{Name: "VEVENT", Start: at.Add(-time.Hour), End: at.Add(time.Hour)}}},
	})
	if err != nil || len(objs) != 1 {
		t.Fatalf("query: %d %v", len(objs), err)
	}
	objs, _ = c.QueryCalendar(ctx, "/dav/antonio/calendars/work/", &caldav.CalendarQuery{
		CompRequest: caldav.CalendarCompRequest{Name: "VCALENDAR"},
		CompFilter:  caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{Name: "VEVENT", Start: at.Add(48 * time.Hour), End: at.Add(72 * time.Hour)}}},
	})
	if len(objs) != 0 {
		t.Errorf("out-of-range query returned %d", len(objs))
	}

	// Maxim reads Antonio's calendar but can't change it.
	mc := e.client(t, "maxim", e.maxTok)
	shared := "/dav/maxim/calendars/antonio~work/"
	got, err := mc.GetCalendarObject(ctx, shared+"e1.ics")
	if err != nil || got.Data == nil {
		t.Fatalf("shared read: %v", err)
	}
	if _, err := mc.PutCalendarObject(ctx, shared+"e2.ics", event("e2@test", "Sneaky", at), nil); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("write to shared calendar: %v", err)
	}
	req, _ := http.NewRequest(http.MethodDelete, e.ts.URL+shared+"e1.ics", nil)
	req.SetBasicAuth("maxim", e.maxTok)
	if resp, err := e.ts.Client().Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("delete on shared calendar: %v %v", resp.StatusCode, err)
	}
	// ...and can't reach Antonio's home by path.
	if _, err := mc.GetCalendarObject(ctx, "/dav/antonio/calendars/work/e1.ics"); err == nil {
		t.Error("read through someone else's home")
	}

	// Delete own event.
	req, _ = http.NewRequest(http.MethodDelete, e.ts.URL+"/dav/antonio/calendars/work/e1.ics", nil)
	req.SetBasicAuth("antonio", e.antonioTok)
	if resp, err := e.ts.Client().Do(req); err != nil || resp.StatusCode/100 != 2 {
		t.Errorf("delete: %v %v", resp.StatusCode, err)
	}

	// Create a calendar with MKCALENDAR-style MKCOL.
	req, _ = http.NewRequest("MKCOL", e.ts.URL+"/dav/antonio/calendars/personal/", strings.NewReader(`<?xml version="1.0"?>
<d:mkcol xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop>
<d:resourcetype><d:collection/><c:calendar/></d:resourcetype><d:displayname>Personal</d:displayname>
</d:prop></d:set></d:mkcol>`))
	req.Header.Set("Content-Type", "application/xml")
	req.SetBasicAuth("antonio", e.antonioTok)
	if resp, err := e.ts.Client().Do(req); err != nil || resp.StatusCode/100 != 2 {
		t.Errorf("mkcol: %v %v", resp.StatusCode, err)
	}
	if cal, err := e.svc.Calendar("antonio", "personal"); err != nil || cal.Name != "Personal" {
		t.Errorf("created calendar: %+v %v", cal, err)
	}
	// MKCALENDAR, as Apple Calendar sends it; not in someone else's home.
	for _, tc := range []struct {
		url  string
		want int
	}{{"/dav/antonio/calendars/trips/", 201}, {"/dav/antonio/calendars/trips/", 405}, {"/dav/maxim/calendars/x/", 403}} {
		req, _ = http.NewRequest("MKCALENDAR", e.ts.URL+tc.url, strings.NewReader(`<?xml version="1.0"?>
<c:mkcalendar xmlns:d="DAV:" xmlns:c="urn:ietf:params:xml:ns:caldav"><d:set><d:prop><d:displayname>Trips</d:displayname></d:prop></d:set></c:mkcalendar>`))
		req.SetBasicAuth("antonio", e.antonioTok)
		if resp, err := e.ts.Client().Do(req); err != nil || resp.StatusCode != tc.want {
			t.Errorf("mkcalendar %s: %v %v, want %d", tc.url, resp.StatusCode, err, tc.want)
		}
	}
	if cal, err := e.svc.Calendar("antonio", "trips"); err != nil || cal.Name != "Trips" {
		t.Errorf("mkcalendar result: %+v %v", cal, err)
	}
}

func TestAuth(t *testing.T) {
	e := setup(t)
	try := func(user, pass string) int {
		req, _ := http.NewRequest("PROPFIND", e.ts.URL+"/dav/", nil)
		if user != "" {
			req.SetBasicAuth(user, pass)
		}
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		name, user, pass string
	}{
		{"none", "", ""},
		{"API token", "antonio", e.apiTok},
		{"someone else's token", "antonio", e.maxTok},
		{"wrong", "antonio", "ktxcal_nope"},
	} {
		if got := try(c.user, c.pass); got != http.StatusUnauthorized {
			t.Errorf("%s: %d", c.name, got)
		}
	}
	if got := try("Antonio", e.antonioTok); got != http.StatusMultiStatus {
		t.Errorf("valid token: %d", got)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(e.ts.URL + "/.well-known/caldav")
	if err != nil || resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/dav/" {
		t.Errorf("well-known: %v %v", resp.StatusCode, err)
	}
}

func TestCTag(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(t, "antonio", e.antonioTok)
	ask := func(path, depth string) string {
		req, _ := http.NewRequest("PROPFIND", e.ts.URL+path, strings.NewReader(`<?xml version="1.0"?>
<d:propfind xmlns:d="DAV:" xmlns:cs="http://calendarserver.org/ns/"><d:prop><d:displayname/><cs:getctag/><d:sync-token/><d:supported-report-set/></d:prop></d:propfind>`))
		req.Header.Set("Content-Type", "application/xml")
		req.Header.Set("Depth", depth)
		req.SetBasicAuth("antonio", e.antonioTok)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusMultiStatus {
			t.Fatalf("PROPFIND %s: %d %s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	ctagRe := regexp.MustCompile(`<getctag xmlns="http://calendarserver.org/ns/">([0-9a-f]+)</getctag>`)
	body := ask("/dav/antonio/calendars/work/", "0")
	m := ctagRe.FindStringSubmatch(body)
	if m == nil || !strings.Contains(body, "calendar-multiget") || !strings.Contains(body, "<sync-token") {
		t.Fatalf("calendar props:\n%s", body)
	}
	// sync-token stays unanswered (404) so clients don't use sync-collection.
	if !regexp.MustCompile(`<sync-token xmlns="DAV:"></sync-token></prop><status>HTTP/1.1 404`).MatchString(body) {
		t.Errorf("sync-token answered:\n%s", body)
	}
	if _, err := c.PutCalendarObject(ctx, "/dav/antonio/calendars/work/x.ics", event("x@t", "X", time.Now()), nil); err != nil {
		t.Fatal(err)
	}
	if m2 := ctagRe.FindStringSubmatch(ask("/dav/antonio/calendars/work/", "0")); m2 == nil || m2[1] == m[1] {
		t.Error("ctag didn't change after a write")
	}
	// Depth 1 on the home: every calendar gets one; the home itself doesn't.
	body = ask("/dav/antonio/calendars/", "1")
	if n := len(ctagRe.FindAllString(body, -1)); n != 2 {
		t.Errorf("home listing has %d ctags:\n%s", n, body)
	}
	// The go-webdav client still parses the result.
	if cals, err := c.FindCalendars(ctx, "/dav/antonio/calendars/"); err != nil || len(cals) != 2 {
		t.Errorf("find calendars: %v %v", cals, err)
	}
}

func TestPropPatch(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	send := func(user, tok, path, body string) (int, string) {
		req, _ := http.NewRequest("PROPPATCH", e.ts.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/xml")
		req.SetBasicAuth(user, tok)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	// As Apple Calendar sends it: name, colour and its own order property.
	body := `<?xml version="1.0" encoding="UTF-8"?>
<A:propertyupdate xmlns:A="DAV:"><A:set><A:prop><A:displayname>Melzi work</A:displayname>
<B:calendar-color xmlns:B="http://apple.com/ns/ical/">#FF2968FF</B:calendar-color>
<B:calendar-order xmlns:B="http://apple.com/ns/ical/">3</B:calendar-order></A:prop></A:set></A:propertyupdate>`
	code, out := send("antonio", e.antonioTok, "/dav/antonio/calendars/work/", body)
	if code != http.StatusMultiStatus || !strings.Contains(out, "HTTP/1.1 200 OK") || !strings.Contains(out, `<displayname xmlns="DAV:"/>`) {
		t.Fatalf("proppatch: %d %s", code, out)
	}
	c, _ := e.svc.Calendar("antonio", "work")
	if c.Name != "Melzi work" || c.Color != "#FF2968FF" {
		t.Errorf("stored: %+v", c)
	}
	msg, _ := exec.Command("git", "--git-dir", e.remote, "log", "-1", "--format=%s", "main").Output()
	if got := strings.TrimSpace(string(msg)); got != `Calendar: rename antonio/work to "Melzi work"` {
		t.Errorf("commit %q", got)
	}
	// Same path; the new name is what clients see.
	cals, err := e.client(t, "antonio", e.antonioTok).FindCalendars(ctx, "/dav/antonio/calendars/")
	if err != nil || cals[0].Path != "/dav/antonio/calendars/work/" || cals[0].Name != "Melzi work" || cals[0].Color != "#FF2968FF" {
		t.Errorf("after rename: %+v %v", cals, err)
	}
	// Someone else's calendar, a bad colour.
	if code, _ := send("maxim", e.maxTok, "/dav/maxim/calendars/antonio~work/", body); code != http.StatusForbidden {
		t.Errorf("rename of shared calendar: %d", code)
	}
	bad := strings.Replace(body, "#FF2968FF", "red", 1)
	if _, out := send("antonio", e.antonioTok, "/dav/antonio/calendars/work/", bad); !strings.Contains(out, "409") {
		t.Errorf("bad colour: %s", out)
	}
}

func todo(uid, summary string, props map[string]string) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropProductID, "-//test//EN")
	cal.Props.SetText(ical.PropVersion, "2.0")
	c := ical.NewComponent(ical.CompToDo)
	c.Props.SetText(ical.PropUID, uid)
	c.Props.SetText(ical.PropSummary, summary)
	c.Props.SetDateTime(ical.PropDateTimeStamp, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	for k, v := range props {
		c.Props.Set(&ical.Prop{Name: k, Value: v, Params: ical.Params{}})
	}
	cal.Children = append(cal.Children, c)
	return cal
}

func TestToDos(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	c := e.client(t, "antonio", e.antonioTok)
	home := Prefix + "/antonio/calendars/work/"

	cals, err := c.FindCalendars(ctx, Prefix+"/antonio/calendars/")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cals[0].SupportedComponentSet, ","); got != "VEVENT,VTODO" {
		t.Fatalf("component set = %q", got)
	}

	for uid, props := range map[string]map[string]string{
		"due-dec":   {ical.PropDue: "20261210T170000Z"},
		"done-nov":  {ical.PropStatus: "COMPLETED", ical.PropCompleted: "20261105T120000Z", ical.PropCreated: "20261101T120000Z"},
		"undated":   {},
		"start-jan": {ical.PropDateTimeStart: "20270105T090000Z"},
		"weekly":    {ical.PropDateTimeStart: "20261102T170000Z", ical.PropDue: "20261102T170000Z", ical.PropRecurrenceRule: "FREQ=WEEKLY"},
	} {
		if _, err := c.PutCalendarObject(ctx, home+uid+".ics", todo(uid, "Task "+uid, props), nil); err != nil {
			t.Fatalf("put %s: %v", uid, err)
		}
	}
	// One file can't mix events and to-dos.
	mixed := todo("mixed", "x", nil)
	mixed.Children = append(mixed.Children, event("mixed", "y", time.Now()).Children...)
	if _, err := c.PutCalendarObject(ctx, home+"mixed.ics", mixed, nil); err == nil {
		t.Fatal("mixed file accepted")
	}

	query := func(comp string, start, end time.Time) []string {
		t.Helper()
		objs, err := c.QueryCalendar(ctx, home, &caldav.CalendarQuery{
			CompRequest: caldav.CalendarCompRequest{Name: "VCALENDAR", AllProps: true, AllComps: true},
			CompFilter:  caldav.CompFilter{Name: "VCALENDAR", Comps: []caldav.CompFilter{{Name: comp, Start: start, End: end}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		var uids []string
		for _, o := range objs {
			for _, ch := range o.Data.Children {
				if u, _ := ch.Props.Text(ical.PropUID); u != "" {
					uids = append(uids, u)
					break
				}
			}
		}
		sort.Strings(uids)
		return uids
	}
	if got := strings.Join(query("VTODO", time.Time{}, time.Time{}), ","); got != "done-nov,due-dec,start-jan,undated,weekly" {
		t.Fatalf("all to-dos: %s", got)
	}
	if got := strings.Join(query("VEVENT", time.Time{}, time.Time{}), ","); got != "" {
		t.Fatalf("events: %s", got)
	}
	// December: the one due then, the undated one (always in range), and
	// the weekly one that started in November.
	dec := query("VTODO", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if got := strings.Join(dec, ","); got != "due-dec,undated,weekly" {
		t.Fatalf("december: %s", got)
	}
	nov := query("VTODO", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if got := strings.Join(nov, ","); got != "done-nov,undated,weekly" {
		t.Fatalf("november: %s", got)
	}
}
