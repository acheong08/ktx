package calendar

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	"github.com/acheong08/ktx/internal/gitrepo"
)

// Repo is the part of gitrepo.Repo the service needs.
type Repo interface {
	Update(ctx context.Context, fn func(root string) (gitrepo.Change, error)) (gitrepo.Result, error)
	View(fn func(root string) error) error
}

// Service reads and writes calendars under the repository lock. Every write
// is one commit, which also carries the transcripts whose event links
// changed because of it.
type Service struct {
	repo   Repo
	relink func(root string) ([]string, error)
}

// NewService returns a Service. relink, if set, is run after every calendar
// change (inside the same commit) and returns the files it changed.
func NewService(repo Repo, relink func(root string) ([]string, error)) *Service {
	return &Service{repo: repo, relink: relink}
}

// Calendars lists every calendar.
func (s *Service) Calendars() ([]Calendar, error) {
	var out []Calendar
	err := s.repo.View(func(root string) (err error) { out, err = ListCalendars(root); return })
	return out, err
}

// Calendar returns one calendar.
func (s *Service) Calendar(owner, id string) (Calendar, error) {
	var out Calendar
	err := s.repo.View(func(root string) (err error) { out, err = GetCalendar(root, owner, id); return })
	return out, err
}

// Objects lists a calendar's event files.
func (s *Service) Objects(c Calendar) ([]Object, error) {
	var out []Object
	err := s.repo.View(func(root string) (err error) { out, err = ListObjects(root, c); return })
	return out, err
}

// Object returns one event file.
func (s *Service) Object(c Calendar, name string) (Object, error) {
	var out Object
	err := s.repo.View(func(root string) (err error) { out, err = GetObject(root, c, name); return })
	return out, err
}

// Occurrences returns occurrences overlapping [from, to) in cals.
func (s *Service) Occurrences(cals []Calendar, from, to time.Time) ([]Occurrence, []string, error) {
	var out []Occurrence
	var problems []string
	err := s.repo.View(func(root string) (err error) {
		out, problems, err = Occurrences(root, cals, from, to)
		return
	})
	return out, problems, err
}

// Candidates returns the occurrences in every calendar within 12 hours of
// start, nearest first, each event once (preferring owner's copy).
func (s *Service) Candidates(start time.Time, owner string) ([]Occurrence, error) {
	cals, err := s.Calendars()
	if err != nil {
		return nil, err
	}
	occs, _, err := s.Occurrences(cals, start.Add(-searchSpan), start.Add(searchSpan))
	if err != nil {
		return nil, err
	}
	occs = dedupe(occs, strings.ToLower(owner))
	sortByDistance(occs, start)
	return occs, nil
}

func sortByDistance(occs []Occurrence, t time.Time) {
	for i := 1; i < len(occs); i++ {
		for j := i; j > 0 && absDur(occs[j].Start.Sub(t)) < absDur(occs[j-1].Start.Sub(t)); j-- {
			occs[j], occs[j-1] = occs[j-1], occs[j]
		}
	}
}

// update runs one change (which returns the paths it touched and a commit
// subject), relinks transcripts, and commits.
func (s *Service) update(ctx context.Context, user string, fn func(root string) ([]string, string, error)) (gitrepo.Result, error) {
	return s.repo.Update(ctx, func(root string) (gitrepo.Change, error) {
		paths, subject, err := fn(root)
		ch := gitrepo.Change{Paths: paths}
		if err != nil || len(paths) == 0 {
			return ch, err
		}
		trailers := []string{"Changed-By: " + user}
		if s.relink != nil {
			more, err := s.relink(root)
			ch.Paths = append(ch.Paths, more...)
			if err != nil {
				return ch, fmt.Errorf("relinking transcripts: %w", err)
			}
			if len(more) > 0 {
				trailers = append(trailers, fmt.Sprintf("Relinked-Transcripts: %d", len(more)))
			}
		}
		if r := []rune(subject); len(r) > 100 {
			subject = string(r[:97]) + "..."
		}
		ch.Message = subject + "\n\n" + strings.Join(trailers, "\n")
		return ch, nil
	})
}

// CreateCalendar makes a new calendar.
func (s *Service) CreateCalendar(ctx context.Context, c Calendar, user string) error {
	_, err := s.update(ctx, user, func(root string) ([]string, string, error) {
		paths, err := CreateCalendar(root, c)
		return paths, "Calendar: create " + c.Key(), err
	})
	return err
}

// EnsureCalendar creates owner's calendar id if they have no calendar yet.
func (s *Service) EnsureCalendar(ctx context.Context, owner, id, name string) error {
	cals, err := s.Calendars()
	if err != nil {
		return err
	}
	for _, c := range cals {
		if c.Owner == owner {
			return nil
		}
	}
	return s.CreateCalendar(ctx, Calendar{Owner: owner, ID: id, Name: name}, owner)
}

// Put stores one event file.
func (s *Service) Put(ctx context.Context, c Calendar, name string, data *ical.Calendar, opt PutOptions, user string) (Object, bool, error) {
	var out Object
	var created bool
	_, err := s.update(ctx, user, func(root string) ([]string, string, error) {
		o, isNew, paths, err := PutObject(root, c, name, data, opt)
		out, created = o, isNew
		verb := "update"
		if isNew {
			verb = "add"
		}
		return paths, fmt.Sprintf("Calendar: %s %q (%s)", verb, summaryOf(data), c.Key()), err
	})
	return out, created, err
}

// Delete removes one event file.
func (s *Service) Delete(ctx context.Context, c Calendar, name, ifMatch, user string) error {
	_, err := s.update(ctx, user, func(root string) ([]string, string, error) {
		summary := name
		if o, err := GetObject(root, c, name); err == nil {
			if cal, err := o.Decode(); err == nil {
				summary = summaryOf(cal)
			}
		}
		paths, err := DeleteObject(root, c, name, ifMatch)
		return paths, fmt.Sprintf("Calendar: delete %q (%s)", summary, c.Key()), err
	})
	return err
}

// Import upserts an .ics file into c.
func (s *Service) Import(ctx context.Context, c Calendar, r io.Reader, user string) (ImportReport, gitrepo.Result, error) {
	var rep ImportReport
	res, err := s.update(ctx, user, func(root string) ([]string, string, error) {
		var paths []string
		var err error
		rep, paths, err = Import(root, c, r)
		return paths, fmt.Sprintf("Calendar: import into %s: %d added, %d updated", c.Key(), rep.Added, rep.Updated), err
	})
	return rep, res, err
}

func summaryOf(cal *ical.Calendar) string {
	for _, c := range cal.Children {
		if c.Name == ical.CompEvent {
			if s, _ := c.Props.Text(ical.PropSummary); s != "" {
				return s
			}
		}
	}
	return "untitled event"
}
