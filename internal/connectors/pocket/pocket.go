// Package pocket is the connector for Pocket (heypocketai.com) recordings,
// using Pocket's public API: https://docs.heypocketai.com/docs/api
//
// Each recording becomes a transcript: front matter, then Pocket's AI summary
// and the timestamped transcript. New recordings are filed under a default
// type (internal, unless the connection says otherwise); ones sorted by hand
// stay where they are on later syncs.
package pocket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/acheong08/ktx/internal/connectors"
	"github.com/acheong08/ktx/internal/store"
	"github.com/acheong08/ktx/internal/transcript"
)

// DefaultBaseURL is Pocket's API.
const DefaultBaseURL = "https://public.heypocketai.com/api/v1"

// DefaultType files new recordings in transcripts/internal/.
const DefaultType = "internal"

// recheck is how far back each run re-reads recordings. Editing a summary
// in Pocket doesn't change the recording's updated_at, so recent recordings
// are fetched again; unchanged ones are not committed.
const recheck = 7 * 24 * time.Hour

// Connector reads recordings from Pocket.
type Connector struct {
	BaseURL     string // DefaultBaseURL when empty
	HTTP        *http.Client
	MinInterval time.Duration // spacing between requests
	Now         func() time.Time

	mu   sync.Mutex
	last time.Time
}

// New returns a connector for the real API.
func New() *Connector {
	return &Connector{HTTP: &http.Client{Timeout: 60 * time.Second}, MinInterval: 250 * time.Millisecond}
}

func (c *Connector) Kind() string  { return "pocket" }
func (c *Connector) Title() string { return "Pocket" }
func (c *Connector) Description() string {
	return "Recordings from a Pocket device: summary and full transcript, stored in transcripts/internal/ by default."
}
func (c *Connector) Interval() time.Duration { return 15 * time.Minute }

func (c *Connector) Fields() []connectors.Field {
	return []connectors.Field{
		{Name: "api_key", Label: "API key", Secret: true, Required: true, Placeholder: "pk_…",
			Help: "In the Pocket app: Settings → Developer → API Keys → create a key (read access is enough)."},
		{Name: "type", Label: "Type for new recordings", Placeholder: DefaultType,
			Help: "The transcripts/<type>/ folder new recordings go to. Optional; default internal."},
		{Name: "participants", Label: "Usual participants", Placeholder: "Maxim Iwanskyj <maxim@melzi.dev>, Antonio Cheong <ac@melzi.dev>",
			Help: "Pocket rarely knows who spoke. These are listed on every recording, after any speakers Pocket names. Optional; default the connection's owner."},
		{Name: "timezone", Label: "Time zone", Placeholder: "America/Los_Angeles",
			Help: "Pocket reports times in UTC; this sets the recording's local time zone. Optional."},
	}
}

// Check validates the settings and makes one small request.
func (c *Connector) Check(ctx context.Context, cfg connectors.Config) (string, error) {
	if _, err := settings(cfg); err != nil {
		return "", err
	}
	var page listPage
	if err := c.get(ctx, cfg, "/public/recordings", url.Values{"page": {"1"}, "limit": {"1"}}, &page); err != nil {
		return "", err
	}
	if len(page.Data) > 0 && page.Data[0].RecordedBy.Email != "" {
		return page.Data[0].RecordedBy.Email, nil
	}
	return fmt.Sprintf("key works (%d recordings)", page.Pagination.Total), nil
}

type opts struct {
	typ          string
	loc          *time.Location
	participants []transcript.Participant
}

func settings(cfg connectors.Config) (opts, error) {
	o := opts{typ: DefaultType, loc: time.UTC}
	if t := cfg.Settings["type"]; t != "" {
		typ, err := transcript.NormalizeType(t)
		if err != nil {
			return o, err
		}
		o.typ = typ
	}
	if tz := cfg.Settings["timezone"]; tz != "" {
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return o, fmt.Errorf("time zone %q is not an IANA name like America/Los_Angeles", tz)
		}
		o.loc = loc
	}
	if p := strings.TrimSpace(cfg.Settings["participants"]); p != "" {
		list, err := mail.ParseAddressList(p)
		if err != nil {
			return o, fmt.Errorf(`usual participants must look like "Jane Doe <jane@example.com>, ...": %v`, err)
		}
		for _, a := range list {
			o.participants = append(o.participants, transcript.Participant{Name: a.Name, Email: strings.ToLower(a.Address)})
		}
	}
	return o, nil
}

// Sync emits every completed recording: all of them on the first run, then
// those recorded within the recheck window before the last run.
func (c *Connector) Sync(ctx context.Context, cfg connectors.Config, cursor string, emit func(store.Item) error) (string, error) {
	o, err := settings(cfg)
	if err != nil {
		return cursor, err
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	started := now().UTC()

	q := url.Values{"limit": {"100"}}
	if last, err := time.Parse(time.RFC3339, cursor); err == nil {
		q.Set("start_date", last.Add(-recheck).Format("2006-01-02"))
	}
	for page := 1; page <= 500; page++ {
		q.Set("page", fmt.Sprint(page))
		var list listPage
		if err := c.get(ctx, cfg, "/public/recordings", q, &list); err != nil {
			return cursor, fmt.Errorf("listing recordings: %w", err)
		}
		for _, r := range list.Data {
			if builtin(r.ID, r.Title) || r.State != "completed" {
				continue // the sample every account starts with; or still processing (picked up later)
			}
			var det struct {
				Data recording `json:"data"`
			}
			if err := c.get(ctx, cfg, "/public/recordings/"+url.PathEscape(r.ID),
				url.Values{"include_transcript": {"true"}, "include_summarizations": {"true"}}, &det); err != nil {
				return cursor, fmt.Errorf("recording %s: %w", r.ID, err)
			}
			doc, err := toDoc(det.Data, o, cfg.Owner)
			if err != nil {
				return cursor, fmt.Errorf("recording %s: %w", r.ID, err)
			}
			if err := emit(store.Item{Doc: doc, DefaultType: o.typ}); err != nil {
				return cursor, err
			}
		}
		if !list.Pagination.HasMore || len(list.Data) == 0 {
			break
		}
	}
	return started.Format(time.RFC3339), nil
}

// builtin is the "Getting Started with Pocket" sample every account has.
func builtin(id, title string) bool {
	return strings.HasSuffix(id, "_getting_started") || strings.EqualFold(strings.TrimSpace(title), "Getting Started with Pocket")
}

// API shapes (only the fields used).

type speaker struct {
	Name string `json:"name"`
}

type recordedBy struct {
	DisplayName string `json:"display_name"`
	Email       string `json:"email"`
}

type listPage struct {
	Data []struct {
		ID         string     `json:"id"`
		Title      string     `json:"title"`
		State      string     `json:"state"`
		RecordedBy recordedBy `json:"recorded_by"`
	} `json:"data"`
	Pagination struct {
		HasMore bool `json:"has_more"`
		Total   int  `json:"total"`
	} `json:"pagination"`
}

type recording struct {
	ID          string             `json:"id"`
	Title       string             `json:"title"`
	Duration    float64            `json:"duration"` // seconds
	RecordingAt string             `json:"recording_at"`
	CreatedAt   string             `json:"created_at"`
	RecordedBy  recordedBy         `json:"recorded_by"`
	Speakers    map[string]speaker `json:"speakers"`
	Tags        []struct {
		Name string `json:"name"`
	} `json:"tags"`
	Summarizations map[string]struct {
		CreatedAt string `json:"createdAt"`
		V2        struct {
			Summary struct {
				Markdown string `json:"markdown"`
			} `json:"summary"`
		} `json:"v2"`
	} `json:"summarizations"`
	Transcript *struct {
		Segments []struct {
			Start       float64 `json:"start"`
			Speaker     string  `json:"speaker"`
			SpeakerName string  `json:"speakerName"`
			Text        string  `json:"text"`
		} `json:"segments"`
	} `json:"transcript"`
}

// APIError is an error response from Pocket.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %.300s", e.Status, e.Body) }

func (c *Connector) get(ctx context.Context, cfg connectors.Config, path string, q url.Values, out any) error {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	for attempt := 0; ; attempt++ {
		c.pace()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
			return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(body))}
		}
		return json.Unmarshal(body, out)
	}
}

func (c *Connector) pace() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if wait := c.MinInterval - time.Since(c.last); wait > 0 {
		time.Sleep(wait)
	}
	c.last = time.Now()
}

type frontMatter struct {
	Title           string        `yaml:"title"`
	Date            string        `yaml:"date"`
	Timezone        string        `yaml:"timezone,omitempty"`
	DurationMinutes int           `yaml:"duration_minutes,omitempty"`
	Participants    []participant `yaml:"participants"`
	Source          string        `yaml:"source"`
	SourceID        string        `yaml:"source_id"`
	Tags            []string      `yaml:"tags,omitempty"`
}

type participant struct {
	Name  string `yaml:"name,omitempty"`
	Email string `yaml:"email,omitempty"`
}

// generic speaker labels such as SPEAKER_00 say nothing about who spoke.
var genericSpeaker = regexp.MustCompile(`(?i)^(speaker[ _]?\d+|unknown)$`)

func toDoc(r recording, o opts, owner connectors.Person) (*transcript.Doc, error) {
	if r.ID == "" {
		return nil, errors.New("recording has no id")
	}
	fm := frontMatter{Title: strings.TrimSpace(r.Title), Source: "pocket", SourceID: r.ID}
	if fm.Title == "" {
		fm.Title = "Untitled recording"
	}
	var start time.Time
	for _, s := range []string{r.RecordingAt, r.CreatedAt} {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			start = t
			break
		}
	}
	if start.IsZero() {
		return nil, errors.New("recording has no date")
	}
	fm.Date = start.In(o.loc).Format(time.RFC3339)
	if o.loc != time.UTC {
		fm.Timezone = o.loc.String()
	}
	if d := r.Duration; d > 0 {
		if d > 24*3600 {
			d /= 1000 // milliseconds
		}
		fm.DurationMinutes = int((time.Duration(d*float64(time.Second)) + 30*time.Second) / time.Minute)
	}

	seen := map[string]bool{}
	add := func(name, email string) {
		name, email = strings.TrimSpace(name), strings.ToLower(strings.TrimSpace(email))
		if (name == "" && email == "") || genericSpeaker.MatchString(name) {
			return
		}
		for _, k := range []string{email, strings.ToLower(name)} {
			if k != "" && seen[k] {
				return
			}
		}
		seen[email], seen[strings.ToLower(name)] = true, true
		fm.Participants = append(fm.Participants, participant{Name: name, Email: email})
	}
	add(r.RecordedBy.DisplayName, r.RecordedBy.Email)
	var names []string
	for _, s := range r.Speakers {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	for _, n := range names {
		add(n, "")
	}
	for _, p := range o.participants {
		add(p.Name, p.Email)
	}
	if len(fm.Participants) == 0 {
		add(owner.Name, owner.Email)
	}
	if len(fm.Participants) == 0 {
		fm.Participants = []participant{{Name: "Unknown"}}
	}
	for _, t := range r.Tags {
		if t.Name != "" {
			fm.Tags = append(fm.Tags, t.Name)
		}
	}

	var b strings.Builder
	b.WriteString("---\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(fm); err != nil {
		return nil, err
	}
	enc.Close()
	b.WriteString("---\n\n")
	if s := summary(r); s != "" {
		fmt.Fprintf(&b, "## Summary\n\n%s\n\n", s)
	}
	if t := renderTranscript(r); t != "" {
		fmt.Fprintf(&b, "## Transcript\n\n%s\n", t)
	}
	if b.Len() == 0 {
		return nil, errors.New("recording has neither summary nor transcript")
	}
	return transcript.Parse([]byte(b.String()))
}

// summary is the newest summary's Markdown (Pocket can hold several, one per
// template run). The mind map and other parts are left out.
func summary(r recording) string {
	best, bestAt := "", ""
	for _, s := range r.Summarizations {
		md := strings.TrimSpace(s.V2.Summary.Markdown)
		if md != "" && (best == "" || s.CreatedAt > bestAt) {
			best, bestAt = md, s.CreatedAt
		}
	}
	return best
}

func renderTranscript(r recording) string {
	if r.Transcript == nil {
		return ""
	}
	var b strings.Builder
	for _, s := range r.Transcript.Segments {
		text := strings.TrimSpace(s.Text)
		if text == "" {
			continue
		}
		who := s.SpeakerName
		if who == "" {
			who = s.Speaker
		}
		if who == "" {
			who = "Speaker"
		}
		fmt.Fprintf(&b, "[%02d:%02d] %s: %s\n", int(s.Start)/60, int(s.Start)%60, who, text)
	}
	return strings.TrimSpace(b.String())
}
