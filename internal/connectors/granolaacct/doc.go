package granolaacct

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/transcript"
)

// The internal API's shapes (only the fields used).

type document struct {
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeletedAt     *time.Time `json:"deleted_at"`
	WasTrashed    bool       `json:"was_trashed"`
	NotesMarkdown string     `json:"notes_markdown"`
	Notes         rich       `json:"notes"`
	Panel         *struct {
		Content         rich   `json:"content"`
		OriginalContent string `json:"original_content"`
	} `json:"last_viewed_panel"`
	Event *struct {
		ID       string `json:"id"`
		ICalUID  string `json:"iCalUID"`
		Summary  string `json:"summary"`
		Start    gtime  `json:"start"`
		End      gtime  `json:"end"`
		Attendee []struct {
			Email       string `json:"email"`
			DisplayName string `json:"displayName"`
			Status      string `json:"responseStatus"`
			Resource    bool   `json:"resource"`
		} `json:"attendees"`
	} `json:"google_calendar_event"`
	People *struct {
		Creator   *personRef  `json:"creator"`
		Attendees []personRef `json:"attendees"`
	} `json:"people"`
}

type gtime struct {
	DateTime time.Time `json:"dateTime"`
	TimeZone string    `json:"timeZone"`
}

type personRef struct {
	Name    string `json:"name"`
	Email   string `json:"email"`
	Details *struct {
		Person *struct {
			Name *struct {
				FullName string `json:"fullName"`
			} `json:"name"`
		} `json:"person"`
	} `json:"details"`
}

func (p personRef) name() string {
	if p.Name != "" {
		return p.Name
	}
	if p.Details != nil && p.Details.Person != nil && p.Details.Person.Name != nil {
		return p.Details.Person.Name.FullName
	}
	return ""
}

type segment struct {
	Text        string    `json:"text"`
	Source      string    `json:"source"` // microphone (me), system (others), …
	Start       time.Time `json:"start_timestamp"`
	SpeakerName *string   `json:"detected_speaker_name"`
	IsFinal     *bool     `json:"is_final"`
}

// pmNode is a ProseMirror document node (Granola's rich text).
type pmNode struct {
	Type    string         `json:"type"`
	Text    string         `json:"text"`
	Attrs   map[string]any `json:"attrs"`
	Marks   []pmMark       `json:"marks"`
	Content []pmNode       `json:"content"`
}

type pmMark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs"`
}

// rich is Granola rich text: a ProseMirror tree, or sometimes a string
// (HTML or plain text).
type rich struct {
	tree *pmNode
	str  string
}

func (r *rich) UnmarshalJSON(b []byte) error {
	switch {
	case len(b) == 0 || string(b) == "null":
		return nil
	case b[0] == '"':
		return json.Unmarshal(b, &r.str)
	case b[0] == '{':
		r.tree = &pmNode{}
		return json.Unmarshal(b, r.tree)
	}
	return nil // something else: ignore
}

// Markdown renders it.
func (r rich) Markdown() string {
	if r.tree != nil {
		return strings.TrimSpace(markdown(*r.tree))
	}
	if strings.Contains(r.str, "<") {
		return htmlText(r.str)
	}
	return strings.TrimSpace(r.str)
}

// summary is the note's generated summary as Markdown ("" if none yet).
func (d document) summary() string {
	if d.Panel == nil {
		return ""
	}
	if md := d.Panel.Content.Markdown(); md != "" {
		return md
	}
	return strings.TrimSpace(htmlText(d.Panel.OriginalContent))
}

// myNotes is what the owner typed during the meeting.
func (d document) myNotes() string {
	if md := strings.TrimSpace(d.NotesMarkdown); md != "" && !onlyEmptyBullets(md) {
		return md
	}
	return d.Notes.Markdown()
}

var emptyBullet = regexp.MustCompile(`(?m)^\s*[-*]\s*$`)

func onlyEmptyBullets(s string) bool {
	return strings.TrimSpace(emptyBullet.ReplaceAllString(s, "")) == ""
}

// --- ProseMirror → Markdown -------------------------------------------------------

func markdown(n pmNode) string {
	var b strings.Builder
	blocks(&b, n.Content, "")
	return strings.TrimSpace(b.String())
}

func blocks(b *strings.Builder, ns []pmNode, indent string) {
	for _, n := range ns {
		switch n.Type {
		case "heading":
			level := 2
			if l, ok := n.Attrs["level"].(float64); ok && l >= 1 {
				level = int(l) + 1 // the note's own h2 sections sit under ours
			}
			fmt.Fprintf(b, "%s%s %s\n\n", indent, strings.Repeat("#", min(level, 6)), inline(n.Content))
		case "paragraph":
			if t := inline(n.Content); strings.TrimSpace(t) != "" {
				fmt.Fprintf(b, "%s%s\n\n", indent, t)
			}
		case "bulletList", "orderedList":
			for i, item := range n.Content {
				marker := "- "
				if n.Type == "orderedList" {
					marker = fmt.Sprintf("%d. ", i+1)
				}
				var ib strings.Builder
				blocks(&ib, item.Content, "")
				lines := strings.Split(strings.TrimSpace(ib.String()), "\n")
				for j, line := range lines {
					switch {
					case j == 0:
						fmt.Fprintf(b, "%s%s%s\n", indent, marker, line)
					case strings.TrimSpace(line) == "":
						continue
					default:
						fmt.Fprintf(b, "%s%s%s\n", indent, strings.Repeat(" ", len(marker)), line)
					}
				}
			}
			b.WriteString("\n")
		case "horizontalRule":
			b.WriteString(indent + "---\n\n")
		case "blockquote":
			var ib strings.Builder
			blocks(&ib, n.Content, "")
			for _, line := range strings.Split(strings.TrimSpace(ib.String()), "\n") {
				fmt.Fprintf(b, "%s> %s\n", indent, line)
			}
			b.WriteString("\n")
		default:
			if len(n.Content) > 0 {
				blocks(b, n.Content, indent)
			} else if t := inline([]pmNode{n}); t != "" {
				fmt.Fprintf(b, "%s%s\n\n", indent, t)
			}
		}
	}
}

func inline(ns []pmNode) string {
	var b strings.Builder
	for _, n := range ns {
		switch n.Type {
		case "text":
			t := n.Text
			for _, m := range n.Marks {
				switch m.Type {
				case "bold", "strong":
					t = "**" + t + "**"
				case "italic", "em":
					t = "*" + t + "*"
				case "code":
					t = "`" + t + "`"
				case "link":
					if href, _ := m.Attrs["href"].(string); href != "" {
						t = "[" + t + "](" + href + ")"
					}
				}
			}
			b.WriteString(t)
		case "hardBreak":
			b.WriteString("  \n")
		case "emoji":
			if e, _ := n.Attrs["emoji"].(string); e != "" {
				b.WriteString(e)
			} else if name, _ := n.Attrs["name"].(string); name != "" {
				b.WriteString(":" + name + ":")
			}
		default:
			b.WriteString(inline(n.Content))
		}
	}
	return b.String()
}

var tagRe = regexp.MustCompile(`<[^>]+>`)

// htmlText is a crude fallback: HTML with tags dropped.
func htmlText(h string) string {
	h = strings.NewReplacer("<li>", "- ", "</p>", "\n\n", "</li>", "\n", "<br>", "\n").Replace(h)
	return strings.TrimSpace(tagRe.ReplaceAllString(h, ""))
}

// --- the stored document ----------------------------------------------------------

type frontMatter struct {
	Title           string        `yaml:"title"`
	Date            string        `yaml:"date"`
	Timezone        string        `yaml:"timezone,omitempty"`
	DurationMinutes int           `yaml:"duration_minutes,omitempty"`
	Participants    []participant `yaml:"participants"`
	Source          string        `yaml:"source"`
	SourceID        string        `yaml:"source_id"`
	GranolaURL      string        `yaml:"granola_url,omitempty"`
	CalendarEventID string        `yaml:"calendar_event_id,omitempty"`
}

type participant struct {
	Name  string `yaml:"name,omitempty"`
	Email string `yaml:"email,omitempty"`
}

// toDoc turns a note into a stored document. ok is false for a note with
// nothing in it (no summary, notes or transcript).
func toDoc(d document, segs []segment, summary, ownerName string, owner connectors.Person, loc *time.Location) (*transcript.Doc, bool, error) {
	notes := d.myNotes()
	text := renderTranscript(segs, ownerName)
	if summary == "" && notes == "" && text == "" {
		return nil, false, nil
	}
	fm := frontMatter{Source: "granola", SourceID: d.ID, GranolaURL: "https://notes.granola.ai/d/" + d.ID}
	start := d.CreatedAt
	title := strings.TrimSpace(d.Title)
	if ev := d.Event; ev != nil {
		if !ev.Start.DateTime.IsZero() {
			start = ev.Start.DateTime
			if ev.End.DateTime.After(start) {
				fm.DurationMinutes = int(ev.End.DateTime.Sub(start).Round(time.Minute) / time.Minute)
			}
			if z, err := time.LoadLocation(ev.Start.TimeZone); err == nil && ev.Start.TimeZone != "" {
				loc = z
			}
		}
		fm.CalendarEventID = ev.ICalUID
		if fm.CalendarEventID == "" {
			fm.CalendarEventID = ev.ID
		}
		if title == "" {
			title = strings.TrimSpace(ev.Summary)
		}
	}
	if title == "" {
		title = "Untitled meeting"
	}
	fm.Title = title
	fm.Date = start.In(loc).Format(time.RFC3339)
	if loc != time.UTC {
		fm.Timezone = loc.String()
	}

	// People: names from Granola's enrichment, emails from the event.
	names := map[string]string{}
	if d.People != nil {
		all := d.People.Attendees
		if d.People.Creator != nil {
			all = append([]personRef{*d.People.Creator}, all...)
		}
		for _, p := range all {
			if p.Email != "" {
				names[strings.ToLower(p.Email)] = p.name()
			}
		}
	}
	seen := map[string]bool{}
	add := func(name, email string) {
		key := strings.ToLower(email)
		if key == "" {
			key = strings.ToLower(name)
		}
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		fm.Participants = append(fm.Participants, participant{Name: name, Email: strings.ToLower(email)})
	}
	if d.Event != nil {
		for _, a := range d.Event.Attendee {
			if a.Resource || strings.EqualFold(a.Status, "declined") {
				continue
			}
			name := names[strings.ToLower(a.Email)]
			if name == "" {
				name = a.DisplayName
			}
			add(name, a.Email)
		}
	}
	if len(fm.Participants) == 0 && d.People != nil {
		for email, name := range names {
			add(name, email)
		}
	}
	if len(fm.Participants) == 0 { // a solo voice note
		add(owner.Name, owner.Email)
	}
	if len(fm.Participants) == 0 {
		add(ownerName, "")
	}
	if len(fm.Participants) == 0 {
		add("Me", "")
	}

	var b bytes.Buffer
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(fm); err != nil {
		return nil, false, err
	}
	enc.Close()
	b.WriteString("---\n\n")
	section(&b, "Summary", summary)
	section(&b, "My notes", notes)
	section(&b, "Transcript", text)
	doc, err := transcript.Parse(b.Bytes())
	return doc, err == nil, err
}

func section(b *bytes.Buffer, title, body string) {
	if body = strings.TrimSpace(body); body != "" {
		fmt.Fprintf(b, "## %s\n\n%s\n\n", title, body)
	}
}

// renderTranscript writes one paragraph per speaker turn. The microphone
// is the owner; the system audio is everyone else.
func renderTranscript(segs []segment, owner string) string {
	var b strings.Builder
	prev := ""
	for _, s := range segs {
		if s.IsFinal != nil && !*s.IsFinal {
			continue
		}
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		who := speaker(s, owner)
		if who == prev {
			b.WriteString(" " + text)
			continue
		}
		if prev != "" {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "**%s:** %s", who, text)
		prev = who
	}
	return b.String()
}

func speaker(s segment, owner string) string {
	switch {
	case s.SpeakerName != nil && strings.TrimSpace(*s.SpeakerName) != "":
		return strings.TrimSpace(*s.SpeakerName)
	case s.Source == "microphone":
		if owner != "" {
			return owner
		}
		return "Me"
	case s.Source == "system":
		return "Them"
	default:
		return "Speaker"
	}
}
