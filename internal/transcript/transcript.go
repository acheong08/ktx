// Package transcript parses, validates and renders transcript files: Markdown
// with a YAML metadata block (front matter) delimited by "---" lines.
//
//	---
//	title: Intro call with Acme
//	date: 2026-11-14T10:00:00-08:00
//	timezone: America/Los_Angeles
//	participants:
//	  - name: Jane Doe
//	    email: jane@acme.com
//	---
//
//	Transcript body...
package transcript

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // IANA zones even on minimal container images
	"unicode"

	"go.yaml.in/yaml/v3"
	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// Delimiter opens and closes the metadata block in rendered files. "---" is
// what GitHub, Obsidian and most Markdown tools read as YAML front matter.
const Delimiter = "---"

// Participant is one person in the conversation. In the metadata it can be a
// mapping or a single string such as "Jane Doe <jane@acme.com>".
type Participant struct {
	Name    string `yaml:"name,omitempty"`
	Email   string `yaml:"email,omitempty"`
	Company string `yaml:"company,omitempty"`
	Role    string `yaml:"role,omitempty"`
}

// UnmarshalYAML accepts both the mapping and the string form.
func (p *Participant) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s := strings.TrimSpace(n.Value)
		if addr, err := mail.ParseAddress(s); err == nil {
			p.Name, p.Email = addr.Name, addr.Address
		} else {
			p.Name = s
		}
		return nil
	}
	type plain Participant
	return n.Decode((*plain)(p))
}

// Meta holds the metadata fields the server understands. Other keys are kept
// in the rendered file unchanged.
type Meta struct {
	Title           string        `yaml:"title"`
	Date            string        `yaml:"date"`
	Timezone        string        `yaml:"timezone"`
	DurationMinutes int           `yaml:"duration_minutes"`
	Type            string        `yaml:"type"`
	Participants    []Participant `yaml:"participants"`
	Source          string        `yaml:"source"`
	SourceID        string        `yaml:"source_id"`
	Tags            []string      `yaml:"tags"`
}

// Doc is a parsed, validated transcript.
type Doc struct {
	Meta  Meta
	Start time.Time // Date resolved to an instant, in its own offset or Timezone
	Body  string
	ID    string // stable id: hash of source+source_id, else of the uploaded bytes

	node *yaml.Node // metadata mapping, kept to preserve order, comments and unknown keys
}

// ValidationError lists every problem found in an upload.
type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid transcript: " + strings.Join(e.Problems, "; ")
}

var typeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Parse reads raw upload bytes and enforces the upload rules. The metadata
// block may use "---" or "+++" delimiters; rendered files always use Delimiter.
func Parse(raw []byte) (*Doc, error) {
	d, text, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if errs := d.validate(); len(errs) > 0 {
		return nil, &ValidationError{Problems: errs}
	}

	h := sha256.New()
	if d.Meta.SourceID != "" {
		fmt.Fprintf(h, "source:%s\n%s", d.Meta.Source, d.Meta.SourceID)
	} else {
		h.Write([]byte(text))
	}
	d.ID = hex.EncodeToString(h.Sum(nil))[:12]
	return d, nil
}

// ParseStored reads a file that is already in the repository. Files may have
// been edited by hand, so the upload rules are reported in problems instead of
// failing; err is only set when the metadata block can't be read at all. The
// ID comes from the file's id key.
func ParseStored(raw []byte) (d *Doc, problems []string, err error) {
	d, _, err = decode(raw)
	if err != nil {
		return nil, nil, err
	}
	for i := 0; i+1 < len(d.node.Content); i += 2 {
		if d.node.Content[i].Value == "id" {
			d.ID = d.node.Content[i+1].Value
		}
	}
	return d, d.validate(), nil
}

func decode(raw []byte) (*Doc, string, error) {
	text := strings.ReplaceAll(string(bytes.TrimPrefix(raw, []byte("\ufeff"))), "\r\n", "\n")

	head, body, err := split(text)
	if err != nil {
		return nil, "", &ValidationError{Problems: []string{err.Error()}}
	}

	var root yaml.Node
	if err := yaml.Unmarshal([]byte(head), &root); err != nil {
		return nil, "", &ValidationError{Problems: []string{"metadata is not valid YAML: " + err.Error()}}
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return nil, "", &ValidationError{Problems: []string{"metadata must be a YAML mapping"}}
	}
	mapping := root.Content[0]

	var m Meta
	if err := mapping.Decode(&m); err != nil {
		return nil, "", &ValidationError{Problems: []string{"metadata: " + err.Error()}}
	}
	m.Type = strings.ToLower(strings.TrimSpace(m.Type))
	if m.Type == Unsorted {
		m.Type = ""
	}
	return &Doc{Meta: m, Body: strings.Trim(body, "\n") + "\n", node: mapping}, text, nil
}

// Unsorted is the folder for transcripts without a type. As a type value it
// means "no type".
const Unsorted = "unsorted"

// NormalizeType lowercases and checks a type name. "" and "unsorted" both
// mean no type and return "".
func NormalizeType(t string) (string, error) {
	t = strings.ToLower(strings.TrimSpace(t))
	if t == Unsorted {
		return "", nil
	}
	if t != "" && !typeRe.MatchString(t) {
		return "", fmt.Errorf("type %q must be lowercase letters, digits, '-' or '_'", t)
	}
	return t, nil
}

func split(text string) (head, body string, err error) {
	first, rest, ok := strings.Cut(text, "\n")
	delim := strings.TrimRight(first, " \t")
	if delim != "+++" && delim != "---" {
		return "", "", errors.New(`file must start with a "---" line opening the metadata block`)
	}
	if !ok {
		return "", "", fmt.Errorf("metadata block is not closed with a %q line", delim)
	}
	for off := 0; off <= len(rest); {
		line, _, _ := strings.Cut(rest[off:], "\n")
		if strings.TrimRight(line, " \t") == delim {
			end := off + len(line)
			if end < len(rest) {
				end++ // skip the newline after the closing delimiter
			}
			return rest[:off], rest[end:], nil
		}
		off += len(line) + 1
	}
	return "", "", fmt.Errorf("metadata block is not closed with a %q line", delim)
}

var naiveLayouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
}

func (d *Doc) validate() []string {
	var errs []string
	m := &d.Meta

	var loc *time.Location
	if m.Timezone != "" {
		l, err := time.LoadLocation(m.Timezone)
		if err != nil {
			errs = append(errs, fmt.Sprintf("timezone %q is not an IANA zone name (e.g. America/Los_Angeles)", m.Timezone))
		} else {
			loc = l
		}
	}

	switch date := strings.TrimSpace(m.Date); {
	case date == "":
		errs = append(errs, "date is required (RFC 3339, e.g. 2026-11-14T10:00:00-08:00)")
	default:
		if t, err := time.Parse(time.RFC3339, date); err == nil {
			d.Start = t
			break
		}
		if t, err := time.Parse("2006-01-02 15:04:05Z07:00", date); err == nil {
			d.Start = t
			break
		}
		if loc == nil {
			if m.Timezone == "" {
				errs = append(errs, fmt.Sprintf("date %q has no UTC offset: add one (e.g. -08:00) or set timezone", date))
			}
			break
		}
		parsed := false
		for _, layout := range naiveLayouts {
			if t, err := time.ParseInLocation(layout, date, loc); err == nil {
				d.Start, parsed = t, true
				break
			}
		}
		if !parsed {
			errs = append(errs, fmt.Sprintf("date %q is not RFC 3339 (e.g. 2026-11-14T10:00:00-08:00)", date))
		}
	}

	if len(m.Participants) == 0 {
		errs = append(errs, "participants must list at least one person")
	}
	for i, p := range m.Participants {
		if strings.TrimSpace(p.Name) == "" && strings.TrimSpace(p.Email) == "" {
			errs = append(errs, fmt.Sprintf("participants[%d] needs a name or an email", i))
		}
		if p.Email != "" {
			if _, err := mail.ParseAddress(p.Email); err != nil {
				errs = append(errs, fmt.Sprintf("participants[%d].email %q is not a valid address", i, p.Email))
			}
		}
	}

	if _, err := NormalizeType(m.Type); err != nil {
		errs = append(errs, err.Error())
	}
	if m.SourceID != "" && m.Source == "" {
		errs = append(errs, "source_id requires source (e.g. granola, pocket, manual)")
	}
	if m.DurationMinutes < 0 {
		errs = append(errs, "duration_minutes cannot be negative")
	}
	if strings.TrimSpace(d.Body) == "" {
		errs = append(errs, "transcript body after the metadata block is empty")
	}
	return errs
}

// Set sets a top-level metadata key to a string value, keeping its position
// if it already exists.
func (d *Doc) Set(key, value string) {
	d.setNode(key, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value})
}

// SetValue sets a top-level metadata key to any YAML-encodable value (a
// list of participants, tags, a number), keeping its position.
func (d *Doc) SetValue(key string, v any) error {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return err
	}
	d.setNode(key, &n)
	return nil
}

func (d *Doc) setNode(key string, value *yaml.Node) {
	m := d.node
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, value)
}

// New returns an empty document with a metadata block, to be filled with
// Set/SetValue and a Body.
func New() *Doc {
	return &Doc{node: &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}}
}

// ParticipantLine formats a participant for editing as one line:
//
//	Jane Doe <jane@acme.com> | Acme | CTO
func ParticipantLine(p Participant) string {
	s := p.Name
	switch {
	case p.Email != "" && p.Name != "":
		s = (&mail.Address{Name: p.Name, Address: p.Email}).String()
		if !strings.ContainsAny(p.Name, `"<>()[]:;@\,.`) {
			s = p.Name + " <" + p.Email + ">" // without the quoting mail.Address adds for non-ASCII
		}
	case p.Email != "":
		s = p.Email
	}
	if p.Company != "" || p.Role != "" {
		s += " | " + p.Company
	}
	if p.Role != "" {
		s += " | " + p.Role
	}
	return s
}

// ParseParticipantLines reads one participant per line, in the format of
// ParticipantLine. Company and role are optional; so is the email.
func ParseParticipantLines(text string) ([]Participant, []string) {
	var out []Participant
	var problems []string
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		who := strings.TrimSpace(parts[0])
		var p Participant
		if strings.Contains(who, "@") {
			a, err := mail.ParseAddress(who)
			if err != nil {
				problems = append(problems, fmt.Sprintf("participant line %d (%q): write it as Name <email>", i+1, line))
				continue
			}
			p.Name, p.Email = a.Name, strings.ToLower(a.Address)
		} else {
			p.Name = who
		}
		if len(parts) > 1 {
			p.Company = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			p.Role = strings.TrimSpace(strings.Join(parts[2:], "|"))
		}
		if p.Name == "" && p.Email == "" {
			problems = append(problems, fmt.Sprintf("participant line %d needs a name or an email", i+1))
			continue
		}
		out = append(out, p)
	}
	return out, problems
}

// Unset removes a top-level metadata key.
func (d *Doc) Unset(key string) {
	m := d.node
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// Render returns the file contents: the metadata (order, comments and
// unknown keys preserved, plus any Set/Unset changes), then the body.
func (d *Doc) Render() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(Delimiter + "\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(d.node); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	buf.WriteString(Delimiter + "\n\n")
	buf.WriteString(d.Body)
	return buf.Bytes(), nil
}

// ReadField returns one top-level metadata value of a stored file as a
// string, or "" if the file has no metadata block or no such key.
func ReadField(raw []byte, key string) string {
	head, _, err := split(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	if err != nil {
		return ""
	}
	var m map[string]any
	if yaml.Unmarshal([]byte(head), &m) != nil {
		return ""
	}
	switch v := m[key].(type) {
	case nil:
		return ""
	case time.Time:
		return v.UTC().Format(time.RFC3339)
	default:
		return fmt.Sprint(v)
	}
}

// Label is a short human description: the title, or the participants' names.
func (d *Doc) Label() string {
	if t := strings.TrimSpace(d.Meta.Title); t != "" {
		return t
	}
	var names []string
	for _, p := range d.Meta.Participants {
		if p.Name != "" {
			names = append(names, p.Name)
		} else {
			names = append(names, p.Email)
		}
	}
	return strings.Join(names, ", ")
}

// FileName is "<date>-<hhmm>-<slug>-<id>.md", using the meeting's local time.
// The id suffix makes names unique and lets the server find a transcript
// again after it has been moved to another folder.
func (d *Doc) FileName() string {
	slug := Slugify(d.Label(), 60)
	if slug == "" {
		slug = "transcript"
	}
	return fmt.Sprintf("%s-%s-%s.md", d.Start.Format("2006-01-02-1504"), slug, d.ID)
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slugify folds accents ("José" -> "jose"), lowercases, and keeps ASCII
// letters and digits joined by '-'.
func Slugify(s string, max int) string {
	if folded, _, err := transform.String(transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC), s); err == nil {
		s = folded
	}
	s = nonSlug.ReplaceAllString(strings.ToLower(s), "-")
	s = strings.Trim(s, "-")
	if len(s) > max {
		s = strings.TrimRight(s[:max], "-")
	}
	return s
}
