// Package notes keeps free-form notes in the brain repository, apart from
// transcripts:
//
//	notes/<YYYY-MM-DD>-<HHMM>-<slug>-<id>.md   Markdown with YAML front matter
//	notes/labels.yaml                         the labels notes can have, and what each means
//
// A new note is saved at once to a queue in the state database; a worker
// then names it (package ai) and commits and pushes it, so whoever wrote it
// doesn't wait. Until then it's listed as "Generating title…". The model
// also adds labels whose meaning fits, to those the author picked. Edits,
// deletes and label definitions are committed straight away.
package notes

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/acheong08/ktx/internal/ai"
	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/state"
	"github.com/acheong08/ktx/internal/transcript"
)

// Root is the folder notes live in.
const Root = "notes"

// ErrNotFound is returned for an unknown note id.
var ErrNotFound = errors.New("note not found")

// ErrPending is returned when changing a note that isn't in git yet.
var ErrPending = errors.New("this note is still being saved; try again in a moment")

// Repo is the git working copy (gitrepo.Repo).
type Repo interface {
	Update(ctx context.Context, fn func(root string) (gitrepo.Change, error)) (gitrepo.Result, error)
	View(fn func(root string) error) error
}

// Note is one note.
type Note struct {
	ID       string
	Path     string // relative to the repo root; "" while pending
	Title    string // "" while pending
	Created  time.Time
	Author   string
	EditedAt string
	EditedBy string
	Body     string
	Labels   []string
	Via      string // what wrote it, e.g. "MCP: <app>"; "" for a person in the web UI
	Pending  bool   // not in git yet: being named or committed
	Error    string // why the last attempt to save a pending note failed

	search string
}

// Matches reports whether every word of q appears in the title, author,
// labels or text.
func (n Note) Matches(q string) bool {
	for _, w := range strings.Fields(strings.ToLower(q)) {
		if !strings.Contains(n.search, w) {
			return false
		}
	}
	return true
}

type meta struct {
	Title    string   `yaml:"title"`
	Created  string   `yaml:"created"`
	Author   string   `yaml:"author"`
	EditedAt string   `yaml:"edited_at,omitempty"`
	EditedBy string   `yaml:"edited_by,omitempty"`
	Labels   []string `yaml:"labels,omitempty,flow"`
	Via      string   `yaml:"via,omitempty"`
}

// Service stores notes and runs the worker that saves new ones.
type Service struct {
	repo      Repo
	db        *state.DB
	ai        *ai.Settings // may be nil: titles come from the first line
	log       *slog.Logger
	now       func() time.Time
	wake      chan struct{}
	retry     time.Duration
	aiTries   int
	aiBackoff time.Duration
}

// New makes a Service. Call Start to run the worker.
func New(repo Repo, db *state.DB, settings *ai.Settings, log *slog.Logger) *Service {
	return &Service{repo: repo, db: db, ai: settings, log: log, now: time.Now,
		wake: make(chan struct{}, 1), retry: time.Minute, aiTries: 2, aiBackoff: 5 * time.Second}
}

// SetTiming shortens the worker's waits, for tests.
func (s *Service) SetTiming(retry, aiBackofff time.Duration) {
	s.retry, s.aiBackoff = retry, aiBackofff
}

// Start runs the worker until ctx ends. Notes queued before a restart are
// picked up again.
func (s *Service) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(s.retry)
		defer t.Stop()
		for {
			s.drain(ctx)
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-t.C:
			}
		}
	}()
}

// Submit queues a new note from a person in the web UI and returns its id.
// The note is named and committed in the background.
func (s *Service) Submit(body, author string, labels []string) (string, error) {
	return s.SubmitNote(Submission{Body: body, Author: author, Labels: labels})
}

// Submission is a new note.
type Submission struct {
	Body   string
	Author string
	Labels []string
	Title  string // optional; without one the model writes it
	Via    string // what wrote it, e.g. "MCP: <app>"
}

// MaxBody bounds a note's text.
const MaxBody = 1 << 20

// SubmitNote queues a new note and returns its id. It's named (unless it
// has a title) and labelled by the model, and committed, in the background.
func (s *Service) SubmitNote(sub Submission) (string, error) {
	body := normalize(sub.Body)
	if strings.TrimSpace(body) == "" {
		return "", errors.New("the note is empty")
	}
	if len(body) > MaxBody {
		return "", fmt.Errorf("the note is longer than %d bytes", MaxBody)
	}
	labels, err := s.checkLabels(sub.Labels, nil)
	if err != nil {
		return "", err
	}
	title := strings.Join(strings.Fields(sub.Title), " ")
	if r := []rune(title); len(r) > 100 {
		title = string(r[:100])
	}
	id := newID()
	if err := s.db.AddPendingNote(state.PendingNote{ID: id, Author: sub.Author, Body: body, Labels: labels,
		Title: title, Via: sub.Via, CreatedAt: s.now()}); err != nil {
		return "", err
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return id, nil
}

func (s *Service) drain(ctx context.Context) {
	pending, err := s.db.PendingNotes()
	if err != nil {
		s.log.Error("listing pending notes", "err", err)
		return
	}
	for _, p := range pending {
		if ctx.Err() != nil {
			return
		}
		if err := s.save(ctx, p); err != nil {
			s.log.Warn("saving note failed; will retry", "id", p.ID, "attempts", p.Attempts+1, "err", err)
			_ = s.db.FailPendingNote(p.ID, err.Error())
		}
	}
}

// save names a pending note and commits it.
func (s *Service) save(ctx context.Context, p state.PendingNote) error {
	defs, err := s.Labels()
	if err != nil {
		return err
	}
	title, suggested, how := s.describe(ctx, p.Body, defs)
	if p.Title != "" {
		title, how = p.Title, "given"
	}
	n := Note{ID: p.ID, Title: title, Created: p.CreatedAt.In(calendar.DefaultZone()), Author: p.Author, Body: p.Body,
		Labels: ordered(append(slices.Clone(p.Labels), suggested...), defs), Via: p.Via}
	gctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res, err := s.repo.Update(gctx, func(root string) (gitrepo.Change, error) {
		if existing, err := findByID(root, n.ID); err != nil || existing != "" {
			return gitrepo.Change{}, err // already committed (e.g. a crash after the commit)
		}
		n.Path = path.Join(Root, fileName(n))
		msg := commitMessage("Add note", n.Title, n.ID, n.Author)
		if n.Via != "" {
			msg += "\nVia: " + n.Via
		}
		ch := gitrepo.Change{Paths: []string{n.Path}, Message: msg}
		return ch, writeFile(root, n.Path, render(n))
	})
	if err != nil {
		return err
	}
	if err := s.db.DeletePendingNote(p.ID); err != nil {
		return err
	}
	s.log.Info("note saved", "id", n.ID, "path", n.Path, "title", n.Title, "labels", n.Labels, "suggested", suggested, "titled_by", how,
		"author", n.Author, "commit", res.Commit, "pushed", res.Pushed)
	return nil
}

// describe asks the model for a title and labels, retrying once; without
// it the title is the first line and there are no suggested labels.
func (s *Service) describe(ctx context.Context, body string, defs []Label) (title string, labels []string, how string) {
	if s.ai == nil {
		return ai.FallbackTitle(body), nil, "first line"
	}
	aiDefs := make([]ai.LabelDef, len(defs))
	for i, d := range defs {
		aiDefs[i] = ai.LabelDef{Name: d.Name, Description: d.Description}
	}
	cfg, err := s.ai.Config()
	if err != nil {
		if !errors.Is(err, ai.ErrNotConfigured) {
			s.log.Error("ai settings", "err", err)
		}
		return ai.FallbackTitle(body), nil, "first line"
	}
	for try := 1; try <= s.aiTries; try++ {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		t, labels, err := ai.Describe(cctx, cfg, body, aiDefs)
		cancel()
		if err == nil {
			return t, labels, cfg.Model
		}
		s.log.Warn("generating a note title failed", "model", cfg.Model, "try", try, "err", err)
		if try < s.aiTries {
			select {
			case <-ctx.Done():
				return ai.FallbackTitle(body), nil, "first line"
			case <-time.After(s.aiBackoff):
			}
		}
	}
	return ai.FallbackTitle(body), nil, "first line"
}

// List returns every note, pending ones included, newest first.
func (s *Service) List() ([]Note, error) {
	var out []Note
	err := s.repo.View(func(root string) error {
		return walk(root, func(rel string) error {
			n, err := readNote(root, rel)
			if err != nil {
				return err
			}
			out = append(out, n)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	pending, err := s.db.PendingNotes()
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, n := range out {
		have[n.ID] = true
	}
	for _, p := range pending {
		if !have[p.ID] {
			out = append(out, pendingNote(p))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Get returns one note, pending or not.
func (s *Service) Get(id string) (Note, error) {
	var n Note
	err := s.repo.View(func(root string) error {
		rel, err := findByID(root, id)
		if err != nil || rel == "" {
			return err
		}
		n, err = readNote(root, rel)
		return err
	})
	if err != nil || n.ID != "" {
		return n, err
	}
	p, err := s.db.PendingNote(id)
	if errors.Is(err, state.ErrNotFound) {
		return Note{}, ErrNotFound
	}
	if err != nil {
		return Note{}, err
	}
	return pendingNote(p), nil
}

// Edit changes a note's title and text in one commit, renaming the file if
// the title changed.
func (s *Service) Edit(ctx context.Context, id, title, body string, labels []string, user string) (Note, error) {
	title = strings.Join(strings.Fields(title), " ")
	body = normalize(body)
	if title == "" {
		return Note{}, errors.New("give the note a title")
	}
	if strings.TrimSpace(body) == "" {
		return Note{}, errors.New("the note is empty")
	}
	var n Note
	_, err := s.repo.Update(ctx, func(root string) (gitrepo.Change, error) {
		rel, err := findByID(root, id)
		if err != nil {
			return gitrepo.Change{}, err
		}
		if rel == "" {
			return gitrepo.Change{}, s.missing(id)
		}
		if n, err = readNote(root, rel); err != nil {
			return gitrepo.Change{}, err
		}
		defs, err := readLabels(root)
		if err != nil {
			return gitrepo.Change{}, err
		}
		// A label kept from before stays even if its definition is gone.
		if labels, err = checkLabelsIn(labels, defs, n.Labels); err != nil {
			return gitrepo.Change{}, err
		}
		if n.Title == title && n.Body == body && slices.Equal(n.Labels, labels) {
			return gitrepo.Change{}, nil
		}
		n.Title, n.Body, n.Labels = title, body, labels
		n.EditedAt, n.EditedBy = s.now().In(calendar.DefaultZone()).Format(time.RFC3339), user
		n.Path = path.Join(Root, fileName(n))
		ch := gitrepo.Change{Paths: []string{n.Path}, Message: commitMessage("Edit note", n.Title, n.ID, user)}
		if rel != n.Path {
			ch.Paths = append(ch.Paths, rel)
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
				return ch, err
			}
		}
		return ch, writeFile(root, n.Path, render(n))
	})
	return n, err
}

// Delete removes a note in one commit. It stays in git history.
func (s *Service) Delete(ctx context.Context, id, user string) (Note, error) {
	var n Note
	_, err := s.repo.Update(ctx, func(root string) (gitrepo.Change, error) {
		rel, err := findByID(root, id)
		if err != nil {
			return gitrepo.Change{}, err
		}
		if rel == "" {
			return gitrepo.Change{}, s.missing(id)
		}
		if n, err = readNote(root, rel); err != nil {
			return gitrepo.Change{}, err
		}
		ch := gitrepo.Change{Paths: []string{rel}, Message: commitMessage("Delete note", n.Title, n.ID, user)}
		return ch, os.Remove(filepath.Join(root, filepath.FromSlash(rel)))
	})
	return n, err
}

// missing is ErrPending for a queued note, else ErrNotFound.
func (s *Service) missing(id string) error {
	if _, err := s.db.PendingNote(id); err == nil {
		return ErrPending
	}
	return ErrNotFound
}

func pendingNote(p state.PendingNote) Note {
	n := Note{ID: p.ID, Created: p.CreatedAt.In(calendar.DefaultZone()), Author: p.Author, Body: p.Body, Labels: p.Labels, Via: p.Via, Pending: true, Error: p.Error}
	n.search = strings.ToLower(n.Author + "\n" + strings.Join(n.Labels, " ") + "\n" + n.Body)
	return n
}

// --- files -----------------------------------------------------------------

var (
	idSuffix = regexp.MustCompile(`-([0-9a-f]{12})\.md$`)
	idRe     = regexp.MustCompile(`^[0-9a-f]{12}$`)
)

// ValidID reports whether s looks like a note id.
func ValidID(s string) bool { return idRe.MatchString(s) }

func newID() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func fileName(n Note) string {
	slug := transcript.Slugify(n.Title, 60)
	if slug == "" {
		slug = "note"
	}
	return fmt.Sprintf("%s-%s-%s.md", n.Created.Format("2006-01-02-1504"), slug, n.ID)
}

func render(n Note) []byte {
	var b bytes.Buffer
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	_ = enc.Encode(meta{Title: n.Title, Created: n.Created.Format(time.RFC3339), Author: n.Author, EditedAt: n.EditedAt, EditedBy: n.EditedBy, Labels: n.Labels, Via: n.Via})
	b.WriteString("---\n")
	b.WriteString(n.Body)
	if !strings.HasSuffix(n.Body, "\n") {
		b.WriteString("\n")
	}
	return b.Bytes()
}

func readNote(root, rel string) (Note, error) {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return Note{}, err
	}
	n := Note{Path: rel}
	if m := idSuffix.FindStringSubmatch(rel); m != nil {
		n.ID = m[1]
	}
	body := string(raw)
	if rest, ok := strings.CutPrefix(body, "---\n"); ok {
		if fm, b, ok := strings.Cut(rest, "\n---\n"); ok {
			var m meta
			if err := yaml.Unmarshal([]byte(fm), &m); err == nil {
				n.Title, n.Author, n.EditedAt, n.EditedBy, n.Labels, n.Via = m.Title, m.Author, m.EditedAt, m.EditedBy, m.Labels, m.Via
				if t, err := time.Parse(time.RFC3339, m.Created); err == nil {
					n.Created = t
				}
				body = b
			}
		}
	}
	n.Body = body
	if n.Title == "" {
		n.Title = ai.FallbackTitle(body)
	}
	n.search = strings.ToLower(n.Title + "\n" + n.Author + "\n" + strings.Join(n.Labels, " ") + "\n" + n.Body)
	return n, nil
}

func walk(root string, fn func(rel string) error) error {
	return filepath.WalkDir(filepath.Join(root, Root), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		return fn(filepath.ToSlash(rel))
	})
}

func findByID(root, id string) (string, error) {
	if !ValidID(id) {
		return "", nil
	}
	var found string
	err := walk(root, func(rel string) error {
		if strings.HasSuffix(rel, "-"+id+".md") {
			found = rel
			return fs.SkipAll
		}
		return nil
	})
	return found, err
}

func writeFile(root, rel string, content []byte) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, content, 0o644)
}

func commitMessage(action, title, id, user string) string {
	subject := action + ": " + title
	if r := []rune(subject); len(r) > 100 {
		subject = string(r[:97]) + "..."
	}
	return subject + "\n\nNote-Id: " + id + "\nChanged-By: " + user
}

// normalize turns CR LF (as browsers submit) into LF.
func normalize(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}
