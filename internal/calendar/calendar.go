// Package calendar keeps calendars in the brain repository and links
// transcripts to the events they were recorded in.
//
//	calendar/<owner>/<calendar>/calendar.json   name, colour, description
//	calendar/<owner>/<calendar>/<name>.ics      one event (all occurrences of one UID)
//
// The CalDAV server (package dav) and the web UI read and write through a
// Service, which takes the repository lock and commits each change. The
// functions here that take a root work on a checked-out working copy and
// must be called with that lock held.
package calendar

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-ical"
)

// Root is the top-level folder of calendars in the repository.
const Root = "calendar"

const metaFile = "calendar.json"

// Errors callers map to HTTP statuses.
var (
	ErrNotFound     = errors.New("not found")
	ErrExists       = errors.New("already exists")
	ErrConflict     = errors.New("another event in this calendar has the same UID")
	ErrPrecondition = errors.New("the event changed since it was read")
	ErrInvalid      = errors.New("invalid")
)

var (
	idRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+=-]{0,200}\.ics$`)
)

// ValidID reports whether s can be an owner or calendar id.
func ValidID(s string) bool { return idRe.MatchString(s) }

// ValidObjectName reports whether s can be an event file name.
func ValidObjectName(s string) bool { return nameRe.MatchString(s) && !strings.Contains(s, "..") }

// Calendar is one calendar of one owner.
type Calendar struct {
	Owner       string `json:"-"`
	ID          string `json:"-"`
	Name        string `json:"name"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
}

// Key is "<owner>/<id>".
func (c Calendar) Key() string { return c.Owner + "/" + c.ID }

// Dir is the calendar's folder, relative to the repository root.
func (c Calendar) Dir() string { return path.Join(Root, c.Owner, c.ID) }

// Title is the name to show, falling back to the id.
func (c Calendar) Title() string {
	if c.Name != "" {
		return c.Name
	}
	return c.ID
}

// Object is one stored event file.
type Object struct {
	Name    string // file name, e.g. 0F3A….ics
	Path    string // relative to the repository root
	ETag    string
	ModTime time.Time
	Raw     []byte
}

// Decode parses the object's iCalendar data.
func (o Object) Decode() (*ical.Calendar, error) {
	return ical.NewDecoder(bytes.NewReader(o.Raw)).Decode()
}

// ListCalendars returns every calendar of every owner, sorted.
func ListCalendars(root string) ([]Calendar, error) {
	owners, err := os.ReadDir(filepath.Join(root, Root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Calendar
	for _, o := range owners {
		if !o.IsDir() || !ValidID(o.Name()) {
			continue
		}
		cals, err := os.ReadDir(filepath.Join(root, Root, o.Name()))
		if err != nil {
			return nil, err
		}
		for _, c := range cals {
			if !c.IsDir() || !ValidID(c.Name()) {
				continue
			}
			cal, err := GetCalendar(root, o.Name(), c.Name())
			if err != nil {
				return nil, err
			}
			out = append(out, cal)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// GetCalendar reads one calendar's metadata.
func GetCalendar(root, owner, id string) (Calendar, error) {
	if !ValidID(owner) || !ValidID(id) {
		return Calendar{}, ErrNotFound
	}
	c := Calendar{Owner: owner, ID: id}
	dir := filepath.Join(root, filepath.FromSlash(c.Dir()))
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return Calendar{}, ErrNotFound
	}
	b, err := os.ReadFile(filepath.Join(dir, metaFile))
	if err == nil {
		_ = json.Unmarshal(b, &c)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Calendar{}, err
	}
	return c, nil
}

// CreateCalendar writes a new calendar and returns the paths to commit.
func CreateCalendar(root string, c Calendar) ([]string, error) {
	if !ValidID(c.Owner) || !ValidID(c.ID) {
		return nil, fmt.Errorf("%w: calendar ids are lowercase letters, digits, '-' and '_'", ErrInvalid)
	}
	if _, err := GetCalendar(root, c.Owner, c.ID); err == nil {
		return nil, ErrExists
	}
	if strings.TrimSpace(c.Name) == "" {
		c.Name = c.ID
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	rel := path.Join(c.Dir(), metaFile)
	return []string{rel}, writeFile(root, rel, append(b, '\n'))
}

// colorRe accepts #rrggbb and #rrggbbaa, as Apple Calendar writes them.
var colorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}([0-9A-Fa-f]{2})?$`)

// UpdateCalendar rewrites an existing calendar's metadata (name, colour,
// description); its id and path stay. It returns the paths to commit, none
// if nothing changed.
func UpdateCalendar(root string, c Calendar) ([]string, error) {
	old, err := GetCalendar(root, c.Owner, c.ID)
	if err != nil {
		return nil, err
	}
	c.Name = strings.TrimSpace(c.Name)
	if c.Name == "" {
		c.Name = c.ID
	}
	if c.Color != "" && !colorRe.MatchString(c.Color) {
		return nil, fmt.Errorf("%w: colours are #rrggbb", ErrInvalid)
	}
	if len(c.Name) > 200 || len(c.Description) > 2000 {
		return nil, fmt.Errorf("%w: name or description too long", ErrInvalid)
	}
	if old.Name == c.Name && old.Color == c.Color && old.Description == c.Description {
		return nil, nil
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	rel := path.Join(c.Dir(), metaFile)
	return []string{rel}, writeFile(root, rel, append(b, '\n'))
}

// ListObjects reads every event file of a calendar, by name.
func ListObjects(root string, c Calendar) ([]Object, error) {
	dir := filepath.Join(root, filepath.FromSlash(c.Dir()))
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var out []Object
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".ics") {
			continue
		}
		o, err := readObject(root, path.Join(c.Dir(), e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// GetObject reads one event file.
func GetObject(root string, c Calendar, name string) (Object, error) {
	if !ValidObjectName(name) {
		return Object{}, ErrNotFound
	}
	return readObject(root, path.Join(c.Dir(), name))
}

// PutOptions are the conditions of a write (HTTP If-Match / If-None-Match).
type PutOptions struct {
	IfMatch     string // write only if the current ETag is this
	IfNoneMatch bool   // write only if the object doesn't exist
}

// PutObject stores one event file, re-encoded canonically. It returns the
// stored object, whether it was new, and the paths to commit; unchanged
// content returns no paths.
func PutObject(root string, c Calendar, name string, data *ical.Calendar, opt PutOptions) (Object, bool, []string, error) {
	if !ValidObjectName(name) {
		return Object{}, false, nil, fmt.Errorf("%w: event file names are letters, digits and ._@+=- ending in .ics", ErrInvalid)
	}
	if _, err := GetCalendar(root, c.Owner, c.ID); err != nil {
		return Object{}, false, nil, err
	}
	uid, err := checkObject(data)
	if err != nil {
		return Object{}, false, nil, err
	}
	normalize(data)
	rel := path.Join(c.Dir(), name)
	old, err := readObject(root, rel)
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return Object{}, false, nil, err
	}
	if opt.IfNoneMatch && exists {
		return Object{}, false, nil, ErrPrecondition
	}
	if opt.IfMatch != "" && (!exists || old.ETag != opt.IfMatch) {
		return Object{}, false, nil, ErrPrecondition
	}
	if other, _ := findUID(root, c, uid); other != "" && other != name {
		return Object{}, false, nil, ErrConflict
	}

	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(data); err != nil {
		return Object{}, false, nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if exists && bytes.Equal(old.Raw, buf.Bytes()) {
		return old, false, nil, nil
	}
	if err := writeFile(root, rel, buf.Bytes()); err != nil {
		return Object{}, false, nil, err
	}
	o, err := readObject(root, rel)
	return o, !exists, []string{rel}, err
}

// DeleteObject removes one event file and returns the paths to commit.
func DeleteObject(root string, c Calendar, name, ifMatch string) ([]string, error) {
	o, err := GetObject(root, c, name)
	if err != nil {
		return nil, err
	}
	if ifMatch != "" && o.ETag != ifMatch {
		return nil, ErrPrecondition
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(o.Path))); err != nil {
		return nil, err
	}
	return []string{o.Path}, nil
}

// checkObject accepts one VCALENDAR whose components are events sharing one
// UID (a series and its overridden occurrences), plus time zones.
func checkObject(data *ical.Calendar) (string, error) {
	if data == nil || data.Component == nil {
		return "", fmt.Errorf("%w: empty calendar", ErrInvalid)
	}
	uid := ""
	events := 0
	for _, child := range data.Children {
		switch child.Name {
		case ical.CompTimezone:
		case ical.CompEvent:
			events++
			u, _ := child.Props.Text(ical.PropUID)
			if u == "" {
				return "", fmt.Errorf("%w: event without a UID", ErrInvalid)
			}
			if uid != "" && u != uid {
				return "", fmt.Errorf("%w: one file may hold only one event UID", ErrInvalid)
			}
			uid = u
		default:
			return "", fmt.Errorf("%w: only events are stored here, not %s", ErrInvalid, child.Name)
		}
	}
	if events == 0 {
		return "", fmt.Errorf("%w: no event in the data", ErrInvalid)
	}
	return uid, nil
}

// findUID returns the file name in c holding uid, or "".
func findUID(root string, c Calendar, uid string) (string, error) {
	objs, err := ListObjects(root, c)
	if err != nil {
		return "", err
	}
	for _, o := range objs {
		if fileUID(o) == uid {
			return o.Name, nil
		}
	}
	return "", nil
}

// fileUID returns the UID of an event file (cached by content).
func fileUID(o Object) string {
	if ev, err := parsed(o); err == nil && len(ev) > 0 {
		return ev[0].UID
	}
	return ""
}

// --- file access with a small cache --------------------------------------------

type cached struct {
	size  int64
	mtime time.Time
	raw   []byte
	etag  string
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cached{}
)

func readObject(root, rel string) (Object, error) {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	st, err := os.Stat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return Object{}, ErrNotFound
	}
	if err != nil {
		return Object{}, err
	}
	cacheMu.Lock()
	c, ok := cache[abs]
	cacheMu.Unlock()
	if !ok || c.size != st.Size() || !c.mtime.Equal(st.ModTime()) {
		raw, err := os.ReadFile(abs)
		if err != nil {
			return Object{}, err
		}
		sum := sha256.Sum256(raw)
		c = cached{size: st.Size(), mtime: st.ModTime(), raw: raw, etag: hex.EncodeToString(sum[:12])}
		cacheMu.Lock()
		cache[abs] = c
		cacheMu.Unlock()
	}
	return Object{Name: path.Base(rel), Path: rel, ETag: c.etag, ModTime: st.ModTime(), Raw: c.raw}, nil
}

func writeFile(root, rel string, content []byte) error {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	return os.WriteFile(abs, content, 0o644)
}
