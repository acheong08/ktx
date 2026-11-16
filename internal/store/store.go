// Package store reads and writes documents in the git working copy.
//
//	transcripts/<type>/...-<id>.md   uploaded transcripts; the folder is the type, or "unsorted"
//	meetings/...-<id>.md             meeting notes from connectors such as Granola
//
// File names end in the document id, so a document is found again wherever
// it has been moved within its collection.
package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/acheong08/ktx/internal/gitrepo"
	"github.com/acheong08/ktx/internal/transcript"
)

// Collections are the top-level folders the store manages.
const (
	// Transcripts are sorted into transcripts/<type>/ (or unsorted).
	Transcripts = "transcripts"
	// Meetings are notes from meeting tools such as Granola, in meetings/.
	Meetings = "meetings"
	// Root is the transcripts collection (kept for existing callers).
	Root = Transcripts
	// UnsortedDir is where transcripts without a type live.
	UnsortedDir = Transcripts + "/" + transcript.Unsorted
)

// Collections lists every collection, in display order.
var Collections = []string{Transcripts, Meetings}

// ErrNotFound means no document has the requested id.
var ErrNotFound = errors.New("transcript not found")

// ErrNotSortable means the document isn't a transcript, so it has no type folder.
var ErrNotSortable = errors.New("only transcripts are sorted by type; meetings stay in meetings/")

// Repo is the part of gitrepo.Repo the store needs.
type Repo interface {
	Update(ctx context.Context, fn func(root string) (gitrepo.Change, error)) (gitrepo.Result, error)
	View(fn func(root string) error) error
}

// Store reads and writes transcripts.
type Store struct {
	repo Repo
	now  func() time.Time
}

// New returns a Store on repo.
func New(repo Repo) *Store { return &Store{repo: repo, now: time.Now} }

// SetClock replaces time.Now, for tests.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Entry is one stored transcript, as read from its file.
type Entry struct {
	ID         string // from the file name suffix; "" for hand-added files without one
	Path       string // relative to the repo root, slash-separated
	Collection string // Transcripts or Meetings
	Folder     string // a transcript's type folder (first directory under transcripts/), else the collection
	Meta       transcript.Meta
	Start      time.Time // zero if the date can't be read
	// IngestedAt and UploadedBy are the server-managed keys, as stored.
	IngestedAt, UploadedBy string
	Problems               []string // why the file doesn't meet the upload rules, if it doesn't
	Body                   string

	search string // lowercased text for Matches
}

// Label is the title, or the participants' names.
func (e Entry) Label() string {
	if t := strings.TrimSpace(e.Meta.Title); t != "" {
		return t
	}
	var names []string
	for _, p := range e.Meta.Participants {
		if p.Name != "" {
			names = append(names, p.Name)
		} else if p.Email != "" {
			names = append(names, p.Email)
		}
	}
	if len(names) == 0 {
		return path.Base(e.Path)
	}
	return strings.Join(names, ", ")
}

// LocalStart is Start in the transcript's timezone when it has one.
func (e Entry) LocalStart() time.Time {
	if e.Meta.Timezone != "" {
		if loc, err := time.LoadLocation(e.Meta.Timezone); err == nil {
			return e.Start.In(loc)
		}
	}
	return e.Start
}

// Matches reports whether every word of q appears in the title,
// participants, tags, source or body.
func (e Entry) Matches(q string) bool {
	for _, w := range strings.Fields(strings.ToLower(q)) {
		if !strings.Contains(e.search, w) {
			return false
		}
	}
	return true
}

// PutResult describes what Put did.
type PutResult struct {
	Result       string // ResultCreated, ResultUpdated or ResultUnchanged
	Path         string
	PreviousPath string // set when an update moved the file
	Git          gitrepo.Result
}

// Results reported by Put and SetType.
const (
	ResultCreated   = "created"
	ResultUpdated   = "updated"
	ResultUnchanged = "unchanged"
)

// Item is one document to store and the collection it belongs in.
type Item struct {
	Doc        *transcript.Doc
	Collection string // Transcripts or Meetings
}

// Put stores an uploaded transcript, overwriting one with the same id.
//
// The folder is the transcript's type, or unsorted. An update without a type
// keeps the folder the file is already in (so files sorted by hand stay
// sorted) and sets the type key to match it.
func (s *Store) Put(ctx context.Context, doc *transcript.Doc, user string) (PutResult, error) {
	res, git, err := s.PutBatch(ctx, []Item{{Doc: doc, Collection: Transcripts}}, user, "")
	var out PutResult
	if len(res) == 1 {
		out = res[0]
	}
	out.Git = git
	return out, err
}

// PutBatch stores several documents in one commit, overwriting any with the
// same id. summary heads the commit message when there is more than one
// change (e.g. "Sync granola"); extra trailers are added to it.
func (s *Store) PutBatch(ctx context.Context, items []Item, user, summary string, trailers ...string) ([]PutResult, gitrepo.Result, error) {
	for _, it := range items {
		if !slices.Contains(Collections, it.Collection) {
			return nil, gitrepo.Result{}, fmt.Errorf("unknown collection %q", it.Collection)
		}
	}
	results := make([]PutResult, len(items))
	git, err := s.repo.Update(ctx, func(root string) (gitrepo.Change, error) {
		var ch gitrepo.Change
		var changed []int
		for i, it := range items {
			res, paths, err := s.putOne(root, it, user)
			ch.Paths = append(ch.Paths, paths...)
			if err != nil {
				return ch, fmt.Errorf("%s: %w", it.Doc.Label(), err)
			}
			results[i] = res
			if res.Result != ResultUnchanged {
				changed = append(changed, i)
			}
		}
		if len(changed) == 0 {
			return gitrepo.Change{}, nil
		}

		tr := append([]string{"Uploaded-By: " + user}, trailers...)
		if len(changed) == 1 {
			it, res := items[changed[0]], results[changed[0]]
			verb := map[string]string{ResultCreated: "Add", ResultUpdated: "Update"}[res.Result]
			noun := map[string]string{Transcripts: "transcript", Meetings: "meeting"}[it.Collection]
			if it.Doc.Meta.Source != "" {
				tr = append(tr, "Source: "+it.Doc.Meta.Source)
			}
			ch.Message = commitMessage(verb+" "+noun, it.Doc.Label(), it.Doc.Start, it.Doc.ID, tr...)
			return ch, nil
		}

		counts := map[string]int{}
		var lines []string
		for _, i := range changed {
			counts[results[i].Result]++
			lines = append(lines, fmt.Sprintf("- %s %s", results[i].Result, results[i].Path))
		}
		if summary == "" {
			summary = "Store documents"
		}
		subject := fmt.Sprintf("%s: %d added, %d updated", summary, counts[ResultCreated], counts[ResultUpdated])
		ch.Message = subject + "\n\n" + strings.Join(lines, "\n") + "\n\n" + strings.Join(tr, "\n")
		return ch, nil
	})
	for i := range results {
		results[i].Git = git
	}
	return results, git, err
}

// putOne writes one document into the working copy and returns the paths to
// stage (the new file, and the old one if it moved).
func (s *Store) putOne(root string, it Item, user string) (PutResult, []string, error) {
	doc := it.Doc
	var out PutResult
	existing, err := findByID(root, doc.ID)
	if err != nil {
		return out, nil, err
	}

	ingestedAt := s.now().UTC().Format(time.RFC3339)
	var old []byte
	dir := Meetings
	if it.Collection == Transcripts {
		dir = folderDir(doc.Meta.Type)
	}
	if existing != "" {
		if old, err = os.ReadFile(filepath.Join(root, filepath.FromSlash(existing))); err != nil {
			return out, nil, err
		}
		if v := transcript.ReadField(old, "ingested_at"); v != "" {
			ingestedAt = v // first ingestion time; git history has the rest
		}
		if it.Collection == Transcripts && doc.Meta.Type == "" && collectionOf(existing) == Transcripts {
			dir = path.Dir(existing)
			if f := folderOf(existing); f != transcript.Unsorted {
				doc.Meta.Type = f
			}
		}
	}

	if it.Collection == Transcripts {
		if doc.Meta.Type != "" {
			doc.Set("type", doc.Meta.Type)
		} else {
			doc.Unset("type")
		}
	}
	doc.Set("id", doc.ID)
	doc.Set("ingested_at", ingestedAt)
	doc.Set("uploaded_by", user)
	content, err := doc.Render()
	if err != nil {
		return out, nil, err
	}

	rel := path.Join(dir, doc.FileName())
	out.Path = rel
	switch {
	case existing == "":
		out.Result = ResultCreated
	case existing == rel && bytes.Equal(old, content):
		out.Result = ResultUnchanged
		return out, nil, nil
	default:
		out.Result = ResultUpdated
	}

	paths := []string{rel}
	if err := writeFile(root, rel, content); err != nil {
		return out, paths, err
	}
	if existing != "" && existing != rel {
		paths = append(paths, existing)
		out.PreviousPath = existing
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(existing))); err != nil {
			return out, paths, err
		}
	}
	return out, paths, nil
}

// SetType moves a transcript to the folder for typ ("" or "unsorted" for
// none) and sets its type key to match. Nothing is committed if it is
// already there.
func (s *Store) SetType(ctx context.Context, id, typ, user string) (PutResult, error) {
	typ, err := transcript.NormalizeType(typ)
	if err != nil {
		return PutResult{}, err
	}
	var out PutResult
	res, err := s.repo.Update(ctx, func(root string) (gitrepo.Change, error) {
		existing, err := findByID(root, id)
		if err != nil {
			return gitrepo.Change{}, err
		}
		if existing == "" {
			return gitrepo.Change{}, ErrNotFound
		}
		if collectionOf(existing) != Transcripts {
			return gitrepo.Change{}, ErrNotSortable
		}
		old, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(existing)))
		if err != nil {
			return gitrepo.Change{}, err
		}
		doc, _, err := transcript.ParseStored(old)
		if err != nil {
			return gitrepo.Change{}, fmt.Errorf("%s: %w", existing, err)
		}

		if typ != "" {
			doc.Set("type", typ)
		} else {
			doc.Unset("type")
		}
		content, err := doc.Render()
		if err != nil {
			return gitrepo.Change{}, err
		}
		rel := path.Join(folderDir(typ), path.Base(existing))
		out.Path = rel
		if rel == existing && bytes.Equal(old, content) {
			out.Result = ResultUnchanged
			return gitrepo.Change{}, nil
		}
		out.Result = ResultUpdated

		subject := "Move transcript to " + typ
		if typ == "" {
			subject = "Mark transcript unsorted"
		}
		label := Entry{Meta: doc.Meta, Path: existing}.Label()
		ch := gitrepo.Change{Paths: []string{rel}, Message: commitMessage(subject, label, doc.Start, id, "Changed-By: "+user)}
		if err := writeFile(root, rel, content); err != nil {
			return ch, err
		}
		if rel != existing {
			ch.Paths = append(ch.Paths, existing)
			out.PreviousPath = existing
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(existing))); err != nil {
				return ch, err
			}
		}
		return ch, nil
	})
	out.Git = res
	return out, err
}

// List reads every document in every collection, newest first.
func (s *Store) List() ([]Entry, error) {
	var entries []Entry
	err := s.repo.View(func(root string) error {
		return walk(root, func(rel string) error {
			e, err := readEntry(root, rel)
			if err != nil {
				return err
			}
			entries = append(entries, e)
			return nil
		})
	})
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Start.After(entries[j].Start) })
	return entries, err
}

// Get reads one transcript by id.
func (s *Store) Get(id string) (Entry, error) {
	var e Entry
	err := s.repo.View(func(root string) error {
		rel, err := findByID(root, id)
		if err != nil {
			return err
		}
		if rel == "" {
			return ErrNotFound
		}
		e, err = readEntry(root, rel)
		return err
	})
	return e, err
}

var idSuffix = regexp.MustCompile(`-([0-9a-f]{12})\.md$`)

func readEntry(root, rel string) (Entry, error) {
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return Entry{}, err
	}
	e := Entry{Path: rel, Collection: collectionOf(rel), Folder: folderOf(rel)}
	if m := idSuffix.FindStringSubmatch(rel); m != nil {
		e.ID = m[1]
	}

	doc, problems, err := transcript.ParseStored(raw)
	if err != nil {
		e.Problems = []string{err.Error()}
		e.Body = string(raw)
	} else {
		e.Meta, e.Start, e.Body, e.Problems = doc.Meta, doc.Start, doc.Body, problems
		e.IngestedAt = transcript.ReadField(raw, "ingested_at")
		e.UploadedBy = transcript.ReadField(raw, "uploaded_by")
	}
	if e.ID == "" {
		e.Problems = append(e.Problems, "file name has no -<id>.md suffix, so it can't be updated or moved from here")
	}

	var b strings.Builder
	b.WriteString(e.Meta.Title + "\n" + e.Meta.Source + "\n" + strings.Join(e.Meta.Tags, " ") + "\n")
	for _, p := range e.Meta.Participants {
		b.WriteString(p.Name + " " + p.Email + " " + p.Company + " " + p.Role + "\n")
	}
	b.WriteString(e.Body)
	e.search = strings.ToLower(b.String())
	return e, nil
}

// errStop ends a walk early.
var errStop = errors.New("stop")

// walk calls fn for every .md file in every collection. fn may return
// errStop to end the walk.
func walk(root string, fn func(rel string) error) error {
	for _, c := range Collections {
		err := filepath.WalkDir(filepath.Join(root, c), func(p string, d fs.DirEntry, err error) error {
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
		if errors.Is(err, errStop) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// findByID returns the path of the document whose file name ends in
// -<id>.md, in any collection, or "".
func findByID(root, id string) (string, error) {
	suffix := "-" + id + ".md"
	var found string
	err := walk(root, func(rel string) error {
		if strings.HasSuffix(rel, suffix) {
			found = rel
			return errStop
		}
		return nil
	})
	return found, err
}

func folderDir(typ string) string {
	if typ == "" {
		return UnsortedDir
	}
	return path.Join(Root, typ)
}

// collectionOf is the top-level folder of rel.
func collectionOf(rel string) string {
	c, _, _ := strings.Cut(rel, "/")
	return c
}

// folderOf is the folder shown for rel: a transcript's type folder (the
// first directory under transcripts/), or the collection name otherwise.
func folderOf(rel string) string {
	if collectionOf(rel) != Transcripts {
		return collectionOf(rel)
	}
	rest := strings.TrimPrefix(rel, Transcripts+"/")
	if dir, _, ok := strings.Cut(rest, "/"); ok {
		return dir
	}
	return transcript.Unsorted
}

func writeFile(root, rel string, content []byte) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, content, 0o644)
}

func commitMessage(action, label string, start time.Time, id string, trailers ...string) string {
	subject := action + ": " + label
	if !start.IsZero() {
		subject += " (" + start.Format("2006-01-02") + ")"
	}
	if r := []rune(subject); len(r) > 100 {
		subject = string(r[:97]) + "..."
	}
	return subject + "\n\n" + strings.Join(append([]string{"Transcript-Id: " + id}, trailers...), "\n")
}
