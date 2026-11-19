package web

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/acheong08/ktx/internal/calendar"
)

func TestTasksUI(t *testing.T) {
	e := setupAuth(t)
	ant, max := e.browser(), e.browser()
	e.signIn(t, ant, idpUser{sub: "u1", username: "antonio", groups: []string{"melzi"}}, "/")
	e.signIn(t, max, idpUser{sub: "u2", username: "maxim", groups: []string{"melzi"}}, "/")
	loc := calendar.DefaultZone()
	tomorrow := time.Now().In(loc).AddDate(0, 0, 1).Format("2006-01-02")
	later := time.Now().In(loc).AddDate(0, 0, 10)

	if code, body, _ := ant.do(t, "GET", "/tasks", nil); code != 200 || !strings.Contains(body, "No tasks.") {
		t.Fatalf("empty: %d %s", code, body)
	}
	add := func(v url.Values) (int, string) {
		t.Helper()
		code, body, _ := ant.do(t, "POST", "/tasks", v)
		return code, body
	}
	add(url.Values{"title": {"No date"}})
	add(url.Values{"title": {"Later"}, "due_date": {later.Format("2006-01-02")}})
	add(url.Values{"title": {"Tomorrow 9"}, "due_date": {tomorrow}, "due_time": {"09:00"}})
	if code, body := add(url.Values{"title": {"x"}, "due_time": {"09:00"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "needs a due date") {
		t.Fatalf("time without date: %d", code)
	}
	if code, _ := add(url.Values{"title": {" "}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty title: %d", code)
	}

	_, body, _ := ant.do(t, "GET", "/tasks", nil)
	order := regexp.MustCompile(`<a href="/tasks/antonio/personal/[^"]+">([^<]+)</a>`).FindAllStringSubmatch(body, -1)
	var got []string
	for _, m := range order {
		got = append(got, m[1])
	}
	if strings.Join(got, ",") != "Tomorrow 9,Later,No date" {
		t.Fatalf("order: %v\n%s", got, body)
	}
	if !strings.Contains(body, "Tomorrow, 09:00") || !strings.Contains(body, later.Format("Mon 2 Jan")) {
		t.Fatalf("due labels:\n%s", body)
	}

	// Tick "Later" done with htmx: the list comes back without it.
	m := regexp.MustCompile(`hx-post="(/tasks/antonio/personal/[^"]+/done)"[^>]*>\s*<input type="hidden" name="etag" value="([^"]+)"[^>]*>\s*<input type="hidden" name="done" value="1">\s*<input type="checkbox" aria-label="Done: Later"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no done form for Later:\n%s", body)
	}
	code, frag, _ := ant.do(t, "POST", m[1], url.Values{"etag": {m[2]}, "done": {"1"}}, "HX-Request", "true")
	if code != 200 || strings.Contains(frag, ">Later<") || strings.Contains(frag, "<html") || !strings.Contains(frag, `id="tasks"`) {
		t.Fatalf("toggle: %d %s", code, frag)
	}
	// A stale ETag (changed on a phone meanwhile) is refused with a warning.
	if _, frag, _ = ant.do(t, "POST", m[1], url.Values{"etag": {m[2]}, "done": {"0"}}, "HX-Request", "true"); !strings.Contains(frag, "changed elsewhere") {
		t.Fatalf("stale: %s", frag)
	}
	if _, body, _ = ant.do(t, "GET", "/tasks?done=1", nil); !strings.Contains(body, ">Later<") || !strings.Contains(body, "Hide done") {
		t.Fatalf("show done:\n%s", body)
	}

	// Maxim sees Antonio's tasks under Everyone, read-only.
	_, body, _ = max.do(t, "GET", "/tasks?who=all", nil)
	if !strings.Contains(body, ">Tomorrow 9<") || !strings.Contains(body, "disabled") || strings.Contains(body, "/done\"") {
		t.Fatalf("maxim's view:\n%s", body)
	}
	if _, body, _ = max.do(t, "GET", "/tasks", nil); strings.Contains(body, "Tomorrow 9") {
		t.Fatal("antonio's task among maxim's own")
	}
	max.do(t, "POST", m[1], url.Values{"done": {"0"}}) // refused: the task must stay done
	if _, body, _ = ant.do(t, "GET", "/tasks", nil); strings.Contains(body, ">Later<") {
		t.Fatal("maxim reopened antonio's task")
	}

	// Edit "No date": add a due date, tags and notes.
	link := regexp.MustCompile(`<a href="(/tasks/antonio/personal/[^"]+)">No date</a>`).FindStringSubmatch(body)[1]
	_, body, _ = ant.do(t, "GET", link, nil)
	etag := regexp.MustCompile(`name="etag" value="([^"]+)"`).FindStringSubmatch(body)[1]
	code, _, _ = ant.do(t, "POST", link, url.Values{"etag": {etag}, "title": {"Now dated"}, "due_date": {tomorrow}, "tags": {"Crush, home"}, "notes": {"one\r\ntwo"}})
	if code != 200 {
		t.Fatalf("save: %d", code)
	}
	_, body, _ = ant.do(t, "GET", "/tasks", nil)
	if !strings.Contains(body, ">Now dated<") || !strings.Contains(body, `<span class="label">Crush</span>`) {
		t.Fatalf("after edit:\n%s", body)
	}
	if code, _, _ := max.do(t, "POST", link, url.Values{"title": {"hijack"}}); code != http.StatusForbidden {
		t.Fatalf("maxim edit: %d", code)
	}
	// Delete it.
	_, body, _ = ant.do(t, "GET", link, nil)
	etag = regexp.MustCompile(`name="etag" value="([^"]+)"`).FindStringSubmatch(body)[1]
	ant.do(t, "POST", link+"/delete", url.Values{"etag": {etag}, "confirm": {"on"}})
	if code, _, _ := ant.do(t, "GET", link, nil); code != http.StatusNotFound {
		t.Fatalf("after delete: %d", code)
	}
}

func TestRepeatingTasksUI(t *testing.T) {
	e := setupAuth(t)
	ant := e.browser()
	e.signIn(t, ant, idpUser{sub: "u1", username: "antonio", groups: []string{"melzi"}}, "/")
	loc := calendar.DefaultZone()
	today := time.Now().In(loc)

	// The add form offers repeats; a repeat needs a due date.
	if _, body, _ := ant.do(t, "GET", "/tasks", nil); !strings.Contains(body, `<select name="repeat" aria-label="Repeat">`) || !strings.Contains(body, `<option value="FREQ=WEEKLY">Every week</option>`) {
		t.Fatalf("add form:\n%s", body)
	}
	if code, body, _ := ant.do(t, "POST", "/tasks", url.Values{"title": {"x"}, "repeat": {"FREQ=WEEKLY"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "A repeating task needs a due date") {
		t.Fatalf("repeat without due: %d", code)
	}
	ant.do(t, "POST", "/tasks", url.Values{"title": {"Water plants"}, "due_date": {today.Format("2006-01-02")}, "repeat": {"FREQ=WEEKLY"}})
	_, body, _ := ant.do(t, "GET", "/tasks", nil)
	if !strings.Contains(body, `<span class="badge" title="FREQ=WEEKLY">every week</span>`) {
		t.Fatalf("badge:\n%s", body)
	}

	// Ticking it done moves it a week on and keeps it open.
	m := regexp.MustCompile(`hx-post="(/tasks/antonio/personal/[^"]+/done)"[^>]*>\s*<input type="hidden" name="etag" value="([^"]+)"`).FindStringSubmatch(body)
	_, frag, _ := ant.do(t, "POST", m[1], url.Values{"etag": {m[2]}, "done": {"1"}}, "HX-Request", "true")
	next := today.AddDate(0, 0, 7).Format("Mon 2 Jan")
	if !strings.Contains(frag, "“Water plants” done; next due "+next) || !strings.Contains(frag, ">Water plants<") || !strings.Contains(frag, next) {
		t.Fatalf("after done:\n%s", frag)
	}

	// The edit form shows the rule; a rule set elsewhere is offered as Custom and kept.
	link := strings.TrimSuffix(m[1], "/done")
	_, body, _ = ant.do(t, "GET", link, nil)
	if !strings.Contains(body, `<select name="repeat" id="repeat">`) || !strings.Contains(body, `<option value="FREQ=WEEKLY" selected>Every week</option>`) {
		t.Fatalf("edit form:\n%s", body)
	}
	etag := regexp.MustCompile(`name="etag" value="([^"]+)"`).FindStringSubmatch(body)[1]
	due := regexp.MustCompile(`name="due_date" type="date" value="([^"]+)"`).FindStringSubmatch(body)[1]
	ant.do(t, "POST", link, url.Values{"etag": {etag}, "title": {"Water plants"}, "due_date": {due}, "repeat": {"FREQ=MONTHLY;BYMONTHDAY=1,15"}})
	_, body, _ = ant.do(t, "GET", link, nil)
	if !strings.Contains(body, `<option value="FREQ=MONTHLY;BYMONTHDAY=1,15" selected>Custom: FREQ=MONTHLY;BYMONTHDAY=1,15</option>`) {
		t.Fatalf("custom rule:\n%s", body)
	}
}
