package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

// formView is the transcript form, for a new transcript (ID "") or an edit.
type formView struct {
	Chrome       chrome
	ID           string
	Source       string // e.g. granola: edits may be replaced by the next sync
	Title        string
	Date         string // datetime-local value
	Timezone     string
	Duration     string
	Type         string
	Participants string // one per line
	Tags         string // comma-separated
	Body         string
	NewForm      bool // the browser fills in its time zone
	Types        []string
	Problems     []string
}

const maxFormBytes = 16 << 20

var commonZones = []string{"America/Los_Angeles", "America/New_York", "Europe/London", "Europe/Berlin", "Asia/Singapore", "UTC"}

func (u *UI) newTranscript(w http.ResponseWriter, r *http.Request) {
	v, err := u.formView(r, formView{NewForm: true, Duration: ""})
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, r, http.StatusOK, transcriptForm(v))
}

func (u *UI) editTranscript(w http.ResponseWriter, r *http.Request) {
	e, err := u.store.Get(r.PathValue("id"))
	if err != nil {
		u.fail(w, r, err)
		return
	}
	f := formView{ID: e.ID, Source: e.Meta.Source, Title: e.Meta.Title, Timezone: e.Meta.Timezone, Type: typeValue(e), Body: e.Body}
	if !e.Start.IsZero() {
		start := e.LocalStart()
		if f.Timezone == "" { // no zone recorded: edit in UTC rather than guess
			f.Timezone, start = "UTC", e.Start.UTC()
		}
		f.Date = start.Format("2006-01-02T15:04")
	}
	if e.Meta.DurationMinutes > 0 {
		f.Duration = strconv.Itoa(e.Meta.DurationMinutes)
	}
	var lines []string
	for _, p := range e.Meta.Participants {
		lines = append(lines, transcript.ParticipantLine(p))
	}
	f.Participants = strings.Join(lines, "\n")
	f.Tags = strings.Join(e.Meta.Tags, ", ")
	v, err := u.formView(r, f)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.render(w, r, http.StatusOK, transcriptForm(v))
}

func (u *UI) formView(r *http.Request, f formView) (formView, error) {
	entries, err := u.store.List()
	if err != nil {
		return f, err
	}
	f.Chrome, f.Types = u.chrome(r, "transcripts"), u.typeOptions(entries)
	return f, nil
}

// readForm reads the posted form into a formView and returns the changes
// to make to a document, or the problems with what was entered.
func readForm(r *http.Request) (formView, func(*transcript.Doc) error, []string) {
	f := formView{
		Title: strings.TrimSpace(r.PostFormValue("title")), Date: strings.TrimSpace(r.PostFormValue("date")),
		Timezone: strings.TrimSpace(r.PostFormValue("timezone")), Duration: strings.TrimSpace(r.PostFormValue("duration")),
		Type: strings.TrimSpace(r.PostFormValue("type")), Participants: strings.ReplaceAll(r.PostFormValue("participants"), "\r\n", "\n"),
		Tags: strings.TrimSpace(r.PostFormValue("tags")), Body: strings.ReplaceAll(r.PostFormValue("body"), "\r\n", "\n"),
	}
	var problems []string
	if f.Title == "" {
		problems = append(problems, "Title is required.")
	}
	loc, err := time.LoadLocation(f.Timezone)
	if f.Timezone == "" || err != nil {
		problems = append(problems, "Time zone must be an IANA name, e.g. America/Los_Angeles or UTC.")
		loc = nil
	}
	var start time.Time
	if loc != nil {
		if start, err = time.ParseInLocation("2006-01-02T15:04", f.Date, loc); err != nil {
			problems = append(problems, "Date and time are required.")
		}
	}
	duration := 0
	if f.Duration != "" {
		if duration, err = strconv.Atoi(f.Duration); err != nil || duration < 0 {
			problems = append(problems, "Duration must be a whole number of minutes.")
		}
	}
	typ, err := transcript.NormalizeType(f.Type)
	if err != nil {
		problems = append(problems, "Type: "+err.Error())
	}
	participants, pp := transcript.ParseParticipantLines(f.Participants)
	problems = append(problems, pp...)
	if len(participants) == 0 && len(pp) == 0 {
		problems = append(problems, "List at least one participant.")
	}
	var tags []string
	for _, t := range strings.Split(f.Tags, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	body := strings.TrimSpace(f.Body)
	if body == "" {
		problems = append(problems, "The transcript or notes can't be empty.")
	}
	if len(problems) > 0 {
		return f, nil, problems
	}

	apply := func(d *transcript.Doc) error {
		d.Set("title", f.Title)
		d.Set("date", start.Format(time.RFC3339))
		d.Set("timezone", loc.String())
		if duration > 0 {
			if err := d.SetValue("duration_minutes", duration); err != nil {
				return err
			}
		} else {
			d.Unset("duration_minutes")
		}
		if typ != "" {
			d.Set("type", typ)
		} else {
			d.Unset("type")
		}
		if err := d.SetValue("participants", participants); err != nil {
			return err
		}
		if len(tags) > 0 {
			if err := d.SetValue("tags", tags); err != nil {
				return err
			}
		} else {
			d.Unset("tags")
		}
		d.Body = body + "\n"
		return nil
	}
	return f, apply, nil
}

func (u *UI) createTranscript(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	f, apply, problems := readForm(r)
	f.NewForm = true
	var doc *transcript.Doc
	if len(problems) == 0 {
		d := transcript.New()
		err := apply(d)
		if err == nil {
			// A stable id, so later edits and re-uploads find this transcript.
			d.Set("source", "manual")
			d.Set("source_id", randomID())
			var raw []byte
			if raw, err = d.Render(); err == nil {
				doc, err = transcript.Parse(raw)
			}
		}
		problems = append(problems, problemsOf(err)...)
	}
	if len(problems) > 0 {
		u.renderForm(w, r, f, problems)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	res, err := u.store.Put(ctx, doc, actor(r))
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.log.Info("transcript added in the UI", "id", doc.ID, "path", res.Path, "user", actor(r), "commit", res.Git.Commit)
	http.Redirect(w, r, "/transcripts/"+doc.ID+"?saved=created", http.StatusSeeOther)
}

func (u *UI) saveTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	f, apply, problems := readForm(r)
	f.ID = id
	if e, err := u.store.Get(id); err == nil {
		f.Source = e.Meta.Source
	}
	if len(problems) > 0 {
		u.renderForm(w, r, f, problems)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	res, err := u.store.Edit(ctx, id, actor(r), apply)
	if p := problemsOf(err); len(p) > 0 && !errors.Is(err, store.ErrNotFound) {
		var verr *transcript.ValidationError
		if errors.As(err, &verr) {
			u.renderForm(w, r, f, p)
			return
		}
	}
	if err != nil {
		u.fail(w, r, err)
		return
	}
	if res.Result != store.ResultUnchanged {
		u.log.Info("transcript edited", "id", id, "path", res.Path, "previous_path", res.PreviousPath, "user", actor(r), "commit", res.Git.Commit)
	}
	http.Redirect(w, r, "/transcripts/"+id+"?saved="+res.Result, http.StatusSeeOther)
}

func (u *UI) renderForm(w http.ResponseWriter, r *http.Request, f formView, problems []string) {
	v, err := u.formView(r, f)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	v.Problems = problems
	u.render(w, r, http.StatusUnprocessableEntity, transcriptForm(v))
}

func problemsOf(err error) []string {
	if err == nil {
		return nil
	}
	var verr *transcript.ValidationError
	if errors.As(err, &verr) {
		return verr.Problems
	}
	return []string{err.Error()}
}

func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func formTitle(v formView) string {
	if v.ID == "" {
		return "New transcript"
	}
	return "Edit transcript"
}

func formURL(v formView) templ.SafeURL {
	if v.ID == "" {
		return templ.URL("/transcripts")
	}
	return templ.URL("/transcripts/" + v.ID)
}

func (u *UI) deleteTranscript(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if r.PostFormValue("confirm") != "on" {
		u.renderDetail(w, r, http.StatusUnprocessableEntity, id, flash{"error", "Tick the box to confirm deleting this transcript."})
		return
	}
	e, err := u.store.Get(id)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	res, err := u.store.Delete(ctx, id, actor(r))
	if err != nil {
		u.fail(w, r, err)
		return
	}
	u.log.Info("transcript deleted", "id", id, "path", res.Path, "user", actor(r), "commit", res.Git.Commit)
	http.Redirect(w, r, listURL("", e.Folder, "")+sep(listURL("", e.Folder, ""))+"deleted="+url.QueryEscape(e.Label()), http.StatusSeeOther)
}

func sep(u string) string {
	if strings.Contains(u, "?") {
		return "&"
	}
	return "?"
}
