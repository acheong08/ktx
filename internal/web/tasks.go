package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/acheong08/ktx/internal/calendar"
)

// Tasks (VTODOs in the calendars), sorted by due date. Only a calendar's
// owner can change its tasks, as over CalDAV.

type taskRow struct {
	T       calendar.ToDo
	Mine    bool
	Due     string // "" when there's none
	Overdue bool
	URL     string // /tasks/<owner>/<calendar>/<file>
}

type tasksView struct {
	Chrome   chrome
	All      bool // everyone's tasks, not just mine
	ShowDone bool
	Rows     []taskRow
	DoneN    int // done tasks (shown or not)
	Mine     []calendar.Calendar
	Problems []string
	Flash    flash
	// The add form, kept when it fails.
	Title, DueDate, DueTime, Into string
}

func (u *UI) tasksView(r *http.Request, all, showDone bool) (tasksView, error) {
	user, _ := currentUser(r)
	owner := calendarOwner(user)
	v := tasksView{Chrome: u.chrome(r, "tasks"), All: all, ShowDone: showDone}
	if err := u.cal.EnsureCalendar(r.Context(), owner, DefaultCalendarID, "Personal"); err != nil {
		return v, err
	}
	cals, err := u.cal.Calendars()
	if err != nil {
		return v, err
	}
	var from []calendar.Calendar
	for _, c := range cals {
		if c.Owner == owner {
			v.Mine = append(v.Mine, c)
		}
		if all || c.Owner == owner {
			from = append(from, c)
		}
	}
	tasks, problems, err := u.cal.ToDos(from)
	if err != nil {
		return v, err
	}
	v.Problems = problems
	now := time.Now()
	for _, t := range tasks {
		if t.Done {
			v.DoneN++
			if !showDone {
				continue
			}
		}
		v.Rows = append(v.Rows, taskRow{T: t, Mine: t.Calendar.Owner == owner, Due: fmtDue(t, now),
			Overdue: t.Overdue(now), URL: taskURL(t.Calendar, t.Name)})
	}
	return v, nil
}

func taskURL(c calendar.Calendar, name string) string {
	return "/tasks/" + url.PathEscape(c.Owner) + "/" + url.PathEscape(c.ID) + "/" + url.PathEscape(name)
}

func tasksURL(all, showDone bool) string {
	v := url.Values{}
	if all {
		v.Set("who", "all")
	}
	if showDone {
		v.Set("done", "1")
	}
	if len(v) == 0 {
		return "/tasks"
	}
	return "/tasks?" + v.Encode()
}

// fmtDue says when a task is due, relative to today where that's clearer.
func fmtDue(t calendar.ToDo, now time.Time) string {
	if t.Due.IsZero() {
		return ""
	}
	loc := calendar.DefaultZone()
	due := t.Due.In(loc)
	if t.DueAllDay {
		due = time.Date(t.Due.Year(), t.Due.Month(), t.Due.Day(), 0, 0, 0, 0, loc)
	}
	today := time.Date(now.In(loc).Year(), now.In(loc).Month(), now.In(loc).Day(), 0, 0, 0, 0, loc)
	day := time.Date(due.Year(), due.Month(), due.Day(), 0, 0, 0, 0, loc)
	var s string
	switch days := int(day.Sub(today).Hours() / 24); {
	case days == 0:
		s = "Today"
	case days == 1:
		s = "Tomorrow"
	case days == -1:
		s = "Yesterday"
	case day.Year() == today.Year():
		s = due.Format("Mon 2 Jan")
	default:
		s = due.Format("Mon 2 Jan 2006")
	}
	if !t.DueAllDay {
		s += ", " + due.Format("15:04")
	}
	return s
}

func (u *UI) tasksPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	v, err := u.tasksView(r, q.Get("who") == "all", q.Get("done") == "1")
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, r, http.StatusOK, tasksPage(v))
}

// parseDue reads the date and optional time fields, in the workspace's
// time zone. No date means no due date.
func parseDue(date, clock string) (time.Time, bool, error) {
	date, clock = strings.TrimSpace(date), strings.TrimSpace(clock)
	if date == "" {
		if clock != "" {
			return time.Time{}, false, errors.New("a due time needs a due date")
		}
		return time.Time{}, false, nil
	}
	loc := calendar.DefaultZone()
	if clock == "" {
		d, err := time.ParseInLocation("2006-01-02", date, loc)
		if err != nil {
			return time.Time{}, false, errors.New("the due date should look like 2026-11-20")
		}
		return d, true, nil
	}
	d, err := time.ParseInLocation("2006-01-02 15:04", date+" "+clock, loc)
	if err != nil {
		return time.Time{}, false, errors.New("the due time should look like 17:00")
	}
	return d, false, nil
}

func (u *UI) createTask(w http.ResponseWriter, r *http.Request) {
	user, _ := currentUser(r)
	owner := calendarOwner(user)
	title, date, clock, into := r.PostFormValue("title"), r.PostFormValue("due_date"), r.PostFormValue("due_time"), r.PostFormValue("calendar")
	if into == "" {
		into = DefaultCalendarID
	}
	fail := func(msg string) {
		v, err := u.tasksView(r, false, false)
		if err != nil {
			u.fail(w, r, err)
			return
		}
		v.Flash = flash{"error", msg}
		v.Title, v.DueDate, v.DueTime, v.Into = title, date, clock, into
		u.render(w, r, http.StatusUnprocessableEntity, tasksPage(v))
	}
	c, err := u.cal.Calendar(owner, into)
	if err != nil {
		fail("You have no calendar called " + into + ".")
		return
	}
	due, allDay, err := parseDue(date, clock)
	if err != nil {
		fail(err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	t, err := u.cal.SaveToDo(ctx, calendar.ToDo{Calendar: c, Summary: title, Due: due, DueAllDay: allDay}, time.Now(), actor(r))
	if errors.Is(err, calendar.ErrInvalid) {
		fail("Give the task a title.")
		return
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.log.Info("task added", "calendar", c.Key(), "task", t.Name, "user", actor(r))
	http.Redirect(w, r, "/tasks", http.StatusSeeOther)
}

// task resolves /tasks/{owner}/{cal}/{name}; mine says whether the user may
// change it.
func (u *UI) task(w http.ResponseWriter, r *http.Request) (calendar.ToDo, bool, bool) {
	user, _ := currentUser(r)
	c, err := u.cal.Calendar(r.PathValue("owner"), r.PathValue("cal"))
	var t calendar.ToDo
	if err == nil {
		t, err = u.cal.ToDo(c, r.PathValue("name"))
	}
	if errors.Is(err, calendar.ErrNotFound) {
		u.render(w, r, http.StatusNotFound, errorPage(u.chrome(r, "tasks"), "No such task. It may have been deleted, e.g. from a phone."))
		return t, false, false
	}
	if err != nil {
		u.fail(w, r, err)
		return t, false, false
	}
	return t, t.Calendar.Owner == calendarOwner(user), true
}

// toggleTask ticks a task done or open. From htmx it returns the list.
func (u *UI) toggleTask(w http.ResponseWriter, r *http.Request) {
	t, mine, ok := u.task(w, r)
	if !ok {
		return
	}
	all, showDone := r.PostFormValue("who") == "all", r.PostFormValue("show_done") == "1"
	var msg flash
	if !mine {
		msg = flash{"error", "That task is " + t.Calendar.Owner + "'s; only they can change it."}
	} else {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
		defer cancel()
		_, err := u.cal.SetToDoDone(ctx, t.Calendar, t.Name, r.PostFormValue("etag"), r.PostFormValue("done") == "1", time.Now(), actor(r))
		switch {
		case errors.Is(err, calendar.ErrPrecondition):
			msg = flash{"warn", "“" + t.Summary + "” changed elsewhere (a phone?) since this page loaded. Here's the current list; try again."}
		case err != nil:
			u.fail(w, r, err)
			return
		}
	}
	if r.Header.Get("HX-Request") != "true" {
		http.Redirect(w, r, tasksURL(all, showDone), http.StatusSeeOther)
		return
	}
	v, err := u.tasksView(r, all, showDone)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	v.Flash = msg
	u.render(w, r, http.StatusOK, taskList(v))
}

type taskFormView struct {
	Chrome                         chrome
	T                              calendar.ToDo
	Mine                           bool
	URL                            string
	Title, DueDate, DueTime, Notes string
	Tags                           string
	Done                           bool
	Flash                          flash
}

func taskForm(c chrome, t calendar.ToDo, mine bool) taskFormView {
	v := taskFormView{Chrome: c, T: t, Mine: mine, URL: taskURL(t.Calendar, t.Name),
		Title: t.Summary, Notes: t.Description, Tags: strings.Join(t.Categories, ", "), Done: t.Done}
	if !t.Due.IsZero() {
		if t.DueAllDay {
			v.DueDate = t.Due.Format("2006-01-02")
		} else {
			d := t.Due.In(calendar.DefaultZone())
			v.DueDate, v.DueTime = d.Format("2006-01-02"), d.Format("15:04")
		}
	}
	return v
}

func (u *UI) taskPage(w http.ResponseWriter, r *http.Request) {
	t, mine, ok := u.task(w, r)
	if !ok {
		return
	}
	u.render(w, r, http.StatusOK, taskPage(taskForm(u.chrome(r, "tasks"), t, mine)))
}

func (u *UI) saveTask(w http.ResponseWriter, r *http.Request) {
	t, mine, ok := u.task(w, r)
	if !ok {
		return
	}
	if !mine {
		http.Error(w, "Only "+t.Calendar.Owner+" can change this task.", http.StatusForbidden)
		return
	}
	v := taskForm(u.chrome(r, "tasks"), t, mine)
	v.Title, v.DueDate, v.DueTime = r.PostFormValue("title"), r.PostFormValue("due_date"), r.PostFormValue("due_time")
	v.Notes, v.Tags, v.Done = r.PostFormValue("notes"), r.PostFormValue("tags"), r.PostFormValue("done") == "1"
	due, allDay, err := parseDue(v.DueDate, v.DueTime)
	if err != nil {
		v.Flash = flash{"error", err.Error()}
		u.render(w, r, http.StatusUnprocessableEntity, taskPage(v))
		return
	}
	t.ETag = r.PostFormValue("etag")
	t.Summary, t.Due, t.DueAllDay, t.Done = v.Title, due, allDay, v.Done
	t.Description = strings.ReplaceAll(v.Notes, "\r\n", "\n")
	t.Categories = nil
	for _, tag := range strings.Split(v.Tags, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			t.Categories = append(t.Categories, tag)
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	_, err = u.cal.SaveToDo(ctx, t, time.Now(), actor(r))
	switch {
	case errors.Is(err, calendar.ErrPrecondition):
		v.Flash = flash{"error", "This task changed elsewhere (a phone?) since you opened it. Reload to see the current version; your edits are still in the form."}
		u.render(w, r, http.StatusConflict, taskPage(v))
	case errors.Is(err, calendar.ErrInvalid):
		v.Flash = flash{"error", "Give the task a title."}
		u.render(w, r, http.StatusUnprocessableEntity, taskPage(v))
	case err != nil:
		u.fail(w, r, err)
	default:
		http.Redirect(w, r, "/tasks", http.StatusSeeOther)
	}
}

func (u *UI) deleteTask(w http.ResponseWriter, r *http.Request) {
	t, mine, ok := u.task(w, r)
	if !ok {
		return
	}
	if !mine {
		http.Error(w, "Only "+t.Calendar.Owner+" can delete this task.", http.StatusForbidden)
		return
	}
	if r.PostFormValue("confirm") == "" {
		http.Redirect(w, r, taskURL(t.Calendar, t.Name), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	err := u.cal.Delete(ctx, t.Calendar, t.Name, r.PostFormValue("etag"), actor(r))
	if errors.Is(err, calendar.ErrPrecondition) {
		v := taskForm(u.chrome(r, "tasks"), t, mine)
		v.Flash = flash{"error", "This task changed elsewhere since you opened it; it wasn't deleted. Check it and try again."}
		u.render(w, r, http.StatusConflict, taskPage(v))
		return
	}
	if err != nil && !errors.Is(err, calendar.ErrNotFound) {
		u.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/tasks", http.StatusSeeOther)
}

func doneValue(done bool) string {
	if done {
		return "1"
	}
	return "0"
}
