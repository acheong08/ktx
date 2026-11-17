// Package granola is the connector for Granola's public API (Business and
// Enterprise plans): https://docs.granola.ai/introduction
//
// Each run lists notes updated since the bookmark, fetches each one with its
// transcript, and stores it in transcripts/meetings/ (type "meetings") as
// Markdown: front matter, then the
// summary, the owner's own notes, and the transcript. Only notes with a
// generated summary exist in the API, so unfinished notes never land in git;
// a note edited later overwrites the same file (git keeps the history).
package granola

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

// DefaultType is where new Granola notes are filed: transcripts/meetings/.
const DefaultType = "meetings"

// DefaultBaseURL is Granola's API.
const DefaultBaseURL = "https://public-api.granola.ai/v1"

// overlap re-lists a little before the bookmark, in case notes are updated
// out of order around a run. Re-fetching is harmless.
const overlap = 10 * time.Minute

// Connector reads notes from Granola.
type Connector struct {
	BaseURL string // DefaultBaseURL when empty
	HTTP    *http.Client
	// MinInterval spaces out requests: the API allows 5/s sustained.
	MinInterval time.Duration

	mu   sync.Mutex
	last time.Time
}

// New returns a connector for the real API.
func New() *Connector {
	return &Connector{HTTP: &http.Client{Timeout: 60 * time.Second}, MinInterval: 250 * time.Millisecond}
}

func (c *Connector) Kind() string  { return "granola" }
func (c *Connector) Title() string { return "Granola" }
func (c *Connector) Description() string {
	return "Meeting notes, summaries and transcripts from Granola, stored in transcripts/meetings/ by default. Needs a Business plan API key."
}
func (c *Connector) Interval() time.Duration { return 10 * time.Minute }

func (c *Connector) Fields() []connectors.Field {
	return []connectors.Field{
		{Name: "api_key", Label: "API key", Secret: true, Required: true, Placeholder: "grn_…",
			Help: "In the Granola desktop app: Settings → Connectors → API keys → Create new key, with the Personal notes scope."},
		{Name: "type", Label: "Type for new notes", Placeholder: DefaultType,
			Help: "The transcripts/<type>/ folder new notes go to, e.g. customer_discovery. Notes already stored stay where they are. Optional; default meetings."},
		{Name: "timezone", Label: "Time zone", Placeholder: "America/Los_Angeles",
			Help: "Granola reports times in UTC; this sets the meeting's local time zone. Optional."},
	}
}

// newType is the type new notes are filed under.
func newType(cfg connectors.Config) (string, error) {
	t := cfg.Settings["type"]
	if t == "" {
		return DefaultType, nil
	}
	return transcript.NormalizeType(t)
}

// Check makes one small request and reports the owner of the newest note.
func (c *Connector) Check(ctx context.Context, cfg connectors.Config) (string, error) {
	if _, err := newType(cfg); err != nil {
		return "", err
	}
	if tz := cfg.Settings["timezone"]; tz != "" {
		if _, err := time.LoadLocation(tz); err != nil {
			return "", fmt.Errorf("time zone %q is not an IANA name like America/Los_Angeles", tz)
		}
	}
	var page listPage
	if err := c.get(ctx, cfg, "/notes?page_size=1", &page); err != nil {
		return "", err
	}
	if len(page.Notes) > 0 && page.Notes[0].Owner.Email != "" {
		return page.Notes[0].Owner.Email, nil
	}
	return "key works (no notes yet)", nil
}

// Sync emits every note updated since cursor (an RFC 3339 time) and returns
// the newest update time seen.
func (c *Connector) Sync(ctx context.Context, cfg connectors.Config, cursor string, emit func(store.Item) error) (string, error) {
	loc := time.UTC
	if tz := cfg.Settings["timezone"]; tz != "" {
		l, err := time.LoadLocation(tz)
		if err != nil {
			return cursor, err
		}
		loc = l
	}
	typ, err := newType(cfg)
	if err != nil {
		return cursor, err
	}

	q := url.Values{"page_size": {"30"}}
	newest, _ := time.Parse(time.RFC3339, cursor)
	if !newest.IsZero() {
		q.Set("updated_after", newest.Add(-overlap).UTC().Format(time.RFC3339))
	}

	var ids []string
	for {
		var page listPage
		if err := c.get(ctx, cfg, "/notes?"+q.Encode(), &page); err != nil {
			return cursor, fmt.Errorf("listing notes: %w", err)
		}
		for _, n := range page.Notes {
			if n.DeletedAt != nil {
				continue
			}
			ids = append(ids, n.ID)
			if n.UpdatedAt.After(newest) {
				newest = n.UpdatedAt
			}
		}
		if !page.HasMore || page.Cursor == "" {
			break
		}
		q.Set("cursor", page.Cursor)
	}

	for _, id := range ids {
		n, err := c.note(ctx, cfg, id)
		if err != nil {
			return cursor, fmt.Errorf("note %s: %w", id, err)
		}
		doc, err := toDoc(n, loc)
		if err != nil {
			return cursor, fmt.Errorf("note %s: %w", id, err)
		}
		// New notes go to the configured type; ones already stored stay put.
		if err := emit(store.Item{Doc: doc, DefaultType: typ}); err != nil {
			return cursor, err
		}
	}
	if newest.IsZero() {
		return cursor, nil
	}
	return newest.UTC().Format(time.RFC3339), nil
}

// API shapes (only the fields used).

type person struct {
	Name  *string `json:"name"`
	Email string  `json:"email"`
}

func (p person) name() string {
	if p.Name != nil {
		return strings.TrimSpace(*p.Name)
	}
	return ""
}

type listPage struct {
	Notes []struct {
		ID        string     `json:"id"`
		Owner     person     `json:"owner"`
		UpdatedAt time.Time  `json:"updated_at"`
		DeletedAt *time.Time `json:"deleted_at"`
	} `json:"notes"`
	HasMore bool   `json:"hasMore"`
	Cursor  string `json:"cursor"`
}

type note struct {
	ID            string    `json:"id"`
	Title         *string   `json:"title"`
	Owner         person    `json:"owner"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	WebURL        string    `json:"web_url"`
	CalendarEvent *struct {
		EventTitle         *string    `json:"event_title"`
		CalendarEventID    *string    `json:"calendar_event_id"`
		ScheduledStartTime *time.Time `json:"scheduled_start_time"`
		ScheduledEndTime   *time.Time `json:"scheduled_end_time"`
	} `json:"calendar_event"`
	Attendees        []person `json:"attendees"`
	FolderMembership []struct {
		Name string `json:"name"`
	} `json:"folder_membership"`
	SummaryText          string      `json:"summary_text"`
	SummaryMarkdown      *string     `json:"summary_markdown"`
	PrivateNotesMarkdown *string     `json:"private_notes_markdown"`
	Transcript           []utterance `json:"transcript"`
}

type utterance struct {
	Speaker struct {
		Source           string  `json:"source"`
		Attribution      string  `json:"attribution"`
		DiarizationLabel *string `json:"diarization_label"`
		Name             *string `json:"name"`
	} `json:"speaker"`
	Text string `json:"text"`
}

// note fetches one note with its transcript, paging the transcript
// separately when it is too large to come inline.
func (c *Connector) note(ctx context.Context, cfg connectors.Config, id string) (note, error) {
	var n note
	err := c.get(ctx, cfg, "/notes/"+url.PathEscape(id)+"?include=transcript", &n)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Code == "TRANSCRIPT_TOO_LARGE" {
		if err = c.get(ctx, cfg, "/notes/"+url.PathEscape(id), &n); err != nil {
			return n, err
		}
		n.Transcript, err = c.transcript(ctx, cfg, id)
	}
	return n, err
}

func (c *Connector) transcript(ctx context.Context, cfg connectors.Config, id string) ([]utterance, error) {
	var all []utterance
	q := url.Values{"page_size": {"100"}}
	for {
		var page struct {
			Transcript []utterance `json:"transcript"`
			HasMore    bool        `json:"hasMore"`
			Cursor     string      `json:"cursor"`
		}
		if err := c.get(ctx, cfg, "/notes/"+url.PathEscape(id)+"/transcript?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		all = append(all, page.Transcript...)
		if !page.HasMore || page.Cursor == "" {
			return all, nil
		}
		q.Set("cursor", page.Cursor)
	}
}

// APIError is an error response from Granola.
type APIError struct {
	Status  int
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d", e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.Status, e.Message)
}

func (c *Connector) get(ctx context.Context, cfg connectors.Config, path string, out any) error {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	for attempt := 0; ; attempt++ {
		c.pace()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+cfg.Secrets["api_key"])
		req.Header.Set("Accept", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 5 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			e := &APIError{Status: resp.StatusCode}
			_ = json.Unmarshal(body, e)
			return e
		}
		return json.Unmarshal(body, out)
	}
}

// pace keeps requests at least MinInterval apart.
func (c *Connector) pace() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.MinInterval - time.Since(c.last); wait > 0 {
		time.Sleep(wait)
	}
	c.last = time.Now()
}

// frontMatter is written in this order.
type frontMatter struct {
	Title           string        `yaml:"title"`
	Date            string        `yaml:"date"`
	Timezone        string        `yaml:"timezone,omitempty"`
	DurationMinutes int           `yaml:"duration_minutes,omitempty"`
	Participants    []participant `yaml:"participants"`
	Source          string        `yaml:"source"`
	SourceID        string        `yaml:"source_id"`
	GranolaURL      string        `yaml:"granola_url,omitempty"`
	GranolaOwner    string        `yaml:"granola_owner,omitempty"`
	CalendarEventID string        `yaml:"calendar_event_id,omitempty"`
	GranolaFolders  []string      `yaml:"granola_folders,omitempty"`
}

type participant struct {
	Name  string `yaml:"name,omitempty"`
	Email string `yaml:"email,omitempty"`
}

// toDoc turns a note into a stored document.
func toDoc(n note, loc *time.Location) (*transcript.Doc, error) {
	fm := frontMatter{Source: "granola", SourceID: n.ID, GranolaURL: n.WebURL, GranolaOwner: n.Owner.Email}
	start := n.CreatedAt
	if ev := n.CalendarEvent; ev != nil {
		if ev.ScheduledStartTime != nil {
			start = *ev.ScheduledStartTime
			if ev.ScheduledEndTime != nil && ev.ScheduledEndTime.After(start) {
				fm.DurationMinutes = int(ev.ScheduledEndTime.Sub(start).Round(time.Minute) / time.Minute)
			}
		}
		if ev.CalendarEventID != nil {
			fm.CalendarEventID = *ev.CalendarEventID
		}
		if n.Title == nil && ev.EventTitle != nil {
			n.Title = ev.EventTitle
		}
	}
	fm.Date = start.In(loc).Format(time.RFC3339)
	if loc != time.UTC {
		fm.Timezone = loc.String()
	}
	fm.Title = "Untitled meeting"
	if n.Title != nil && strings.TrimSpace(*n.Title) != "" {
		fm.Title = strings.TrimSpace(*n.Title)
	}
	for _, a := range n.Attendees {
		if a.Email != "" || a.name() != "" {
			fm.Participants = append(fm.Participants, participant{Name: a.name(), Email: a.Email})
		}
	}
	if len(fm.Participants) == 0 { // e.g. a solo voice note
		fm.Participants = []participant{{Name: n.Owner.name(), Email: n.Owner.Email}}
	}
	for _, f := range n.FolderMembership {
		fm.GranolaFolders = append(fm.GranolaFolders, f.Name)
	}

	var b bytes.Buffer
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(fm); err != nil {
		return nil, err
	}
	enc.Close()
	b.WriteString("---\n\n")

	summary := n.SummaryText
	if n.SummaryMarkdown != nil && strings.TrimSpace(*n.SummaryMarkdown) != "" {
		summary = *n.SummaryMarkdown
	}
	section(&b, "Summary", summary)
	if n.PrivateNotesMarkdown != nil {
		section(&b, "My notes", *n.PrivateNotesMarkdown)
	}
	section(&b, "Transcript", renderTranscript(n.Transcript, n.Owner.name()))

	return transcript.Parse(b.Bytes())
}

func section(b *bytes.Buffer, title, body string) {
	body = strings.TrimSpace(body)
	if body == "" {
		return
	}
	fmt.Fprintf(b, "## %s\n\n%s\n\n", title, body)
}

// renderTranscript writes one paragraph per speaker turn.
func renderTranscript(items []utterance, owner string) string {
	var b strings.Builder
	prev := ""
	for _, u := range items {
		text := strings.TrimSpace(u.Text)
		if text == "" {
			continue
		}
		who := speaker(u, owner)
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

func speaker(u utterance, owner string) string {
	s := u.Speaker
	switch {
	case s.Name != nil && strings.TrimSpace(*s.Name) != "":
		return strings.TrimSpace(*s.Name)
	case s.DiarizationLabel != nil && *s.DiarizationLabel != "":
		return *s.DiarizationLabel
	case s.Attribution == "me" || (s.Attribution == "" && s.Source == "microphone"):
		if owner != "" {
			return owner
		}
		return "Me"
	default:
		return "Them"
	}
}
