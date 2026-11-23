package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/notes"
	"github.com/acheong08/ktx/internal/store"
)

// withIntent is every tool's input: each has an intent field (the schema
// generator doesn't flatten embedded structs, so it's declared in each).
type withIntent interface{ intentText() string }

func (i searchIn) intentText() string        { return i.Intent }
func (i getIn) intentText() string           { return i.Intent }
func (i listTypesIn) intentText() string     { return i.Intent }
func (i listCalendarsIn) intentText() string { return i.Intent }
func (i listEventsIn) intentText() string    { return i.Intent }
func (i searchNotesIn) intentText() string   { return i.Intent }
func (i getGraphIn) intentText() string      { return i.Intent }
func (i getNoteIn) intentText() string       { return i.Intent }
func (i listLabelsIn) intentText() string    { return i.Intent }
func (i createNoteIn) intentText() string    { return i.Intent }

func ptr[T any](v T) *T { return &v }

// addTool registers a tool that requires and logs an intent. Tools are
// read-only unless they set their own annotations.
func addTool[In withIntent, Out any](srv *mcp.Server, s *Server, c caller, t *mcp.Tool, h func(context.Context, In) (Out, error)) {
	if t.Annotations == nil {
		t.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptr(false)}
	}
	mcp.AddTool(srv, t, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		var zero Out
		intent := strings.Join(strings.Fields(in.intentText()), " ")
		if intent == "" {
			return nil, zero, errors.New("intent is required: say in one sentence what the user asked for and why this call helps")
		}
		s.log.Info("mcp call", "user", c.user.Display(), "app", c.grant.ClientName, "grant", c.grant.ID, "tool", t.Name, "intent", intent)
		out, err := h(ctx, in)
		if err != nil {
			s.log.Warn("mcp call failed", "user", c.user.Display(), "tool", t.Name, "err", err)
			return nil, zero, err
		}
		return nil, out, nil
	})
}

func (s *Server) addTools(srv *mcp.Server, c caller) {
	addTool(srv, s, c, &mcp.Tool{
		Name:  "search_transcripts",
		Title: "Search transcripts",
		Description: "Find call and meeting transcripts, newest first. All filters are optional and combine. " +
			"Returns metadata (title, time, type, participants, linked calendar event) and, with a query, a snippet; " +
			"read the text with get_transcript.",
	}, s.searchTranscripts)
	addTool(srv, s, c, &mcp.Tool{
		Name:        "get_transcript",
		Title:       "Read a transcript",
		Description: "Read one transcript: its metadata and text. Long texts come in pages: pass next_offset back as offset.",
	}, s.getTranscript)
	addTool(srv, s, c, &mcp.Tool{
		Name:        "list_transcript_types",
		Title:       "List transcript types",
		Description: "The transcript types (e.g. customer_discovery, internal) with how many transcripts each has.",
	}, s.listTypes)
	if s.notes != nil {
		addTool(srv, s, c, &mcp.Tool{
			Name:  "list_note_labels",
			Title: "List note labels",
			Description: "The labels notes can have, what each means, and how many notes have it. " +
				"Check it before trusting a note: a label's meaning may say how reliable its notes are.",
		}, s.listNoteLabels)
		addTool(srv, s, c, &mcp.Tool{
			Name:  "search_notes",
			Title: "Search notes",
			Description: "Find notes people wrote in ktx (separate from transcripts), newest first. All filters are optional. " +
				"Returns titles, authors, labels, and for goals and decisions their status, strength and what they're based on; " +
				"with a query, a snippet. Read one with get_note. See list_note_labels for what labels mean, and get_graph for the goals and decisions as a tree.",
		}, func(ctx context.Context, in searchNotesIn) (searchNotesOut, error) { return s.searchNotes(ctx, in, c) })
		addTool(srv, s, c, &mcp.Tool{
			Name:  "get_graph",
			Title: "Goals and decisions",
			Description: "The team's goals and decisions as an outline: each with its status and strength, what it's based on " +
				"(other goals and decisions, notes, and calls, with quotes of what was said) and the tasks for it. " +
				"Read this to understand what the user is trying to achieve and why, before suggesting work.",
		}, func(ctx context.Context, in getGraphIn) (getGraphOut, error) { return s.getGraph(ctx, in, c) })
		addTool(srv, s, c, &mcp.Tool{
			Name:  "create_note",
			Title: "Write a note",
			Description: "Save a note in ktx as the user. It is always labelled " + SlopLabel + " (see list_note_labels), " +
				"so people know an agent wrote it; you may add other labels. It's saved at once and committed within seconds; " +
				"without a title, one is written for it. Only write what the user asked you to keep.",
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: ptr(false), IdempotentHint: false, OpenWorldHint: ptr(false)},
		}, func(ctx context.Context, in createNoteIn) (createNoteOut, error) {
			return s.createNote(ctx, in, c)
		})
		addTool(srv, s, c, &mcp.Tool{
			Name:  "get_note",
			Title: "Read a note",
			Description: "Read one note: its title, author, dates and text (Markdown); for a goal or decision also its status, " +
				"strength, what it's based on and what's based on it.",
		}, func(ctx context.Context, in getNoteIn) (getNoteOut, error) { return s.getNote(ctx, in, c) })
	}
	if s.cal == nil {
		return
	}
	addTool(srv, s, c, &mcp.Tool{
		Name:        "list_calendars",
		Title:       "List calendars",
		Description: "Every calendar in ktx, with its owner. `yours` marks the user's own.",
	}, func(ctx context.Context, in listCalendarsIn) (listCalendarsOut, error) {
		return s.listCalendars(ctx, in, c)
	})
	addTool(srv, s, c, &mcp.Tool{
		Name:  "list_events",
		Title: "List calendar events",
		Description: "Calendar events in a time range (repeating events expanded into occurrences), with organizer, guests, " +
			"location, description and the transcripts recorded in each. Defaults to the next 7 days from today, in all calendars.",
	}, s.listEvents)
}

// --- times -----------------------------------------------------------------

// parseWhen reads a date (YYYY-MM-DD), a local time (YYYY-MM-DDTHH:MM) or
// an RFC 3339 time. Dates and local times are in calendar.DefaultZone().
// With endOfDay, a date means the end of that day.
func parseWhen(v string, endOfDay bool) (time.Time, error) {
	v = strings.TrimSpace(v)
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, v, calendar.DefaultZone()); err == nil {
			return t, nil
		}
	}
	if t, err := time.ParseInLocation("2006-01-02", v, calendar.DefaultZone()); err == nil {
		if endOfDay {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("can't read %q as a date (YYYY-MM-DD) or time (RFC 3339)", v)
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// --- transcripts -----------------------------------------------------------

type participant struct {
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	Company string `json:"company,omitempty"`
	Role    string `json:"role,omitempty"`
	// From is "calendar" for guests added from the linked event.
	From string `json:"from,omitempty"`
}

type eventRef struct {
	Calendar     string `json:"calendar" jsonschema:"owner/calendar id"`
	UID          string `json:"uid"`
	RecurrenceID string `json:"recurrence_id,omitempty" jsonschema:"set for one occurrence of a repeating event"`
	Title        string `json:"title,omitempty"`
	Start        string `json:"start,omitempty"`
	Match        string `json:"match,omitempty" jsonschema:"how it was linked: exact (the recorder's own event id), time, or manual (set by a person)"`
}

type transcriptSummary struct {
	ID              string        `json:"id"`
	Title           string        `json:"title"`
	Start           string        `json:"start,omitempty" jsonschema:"when the call started (RFC 3339, in the call's time zone, else the workspace's)"`
	DurationMinutes int           `json:"duration_minutes,omitempty"`
	Type            string        `json:"type"`
	Participants    []participant `json:"participants,omitempty"`
	Tags            []string      `json:"tags,omitempty"`
	Source          string        `json:"source,omitempty"`
	CalendarEvent   *eventRef     `json:"calendar_event,omitempty"`
	TextBytes       int           `json:"text_bytes" jsonschema:"length of the text"`
	Snippet         string        `json:"snippet,omitempty" jsonschema:"text around the first match of the query"`
}

func summarize(e store.Entry) transcriptSummary {
	t := transcriptSummary{
		ID: e.ID, Title: e.Label(), Start: formatTime(startOf(e)), DurationMinutes: e.Meta.DurationMinutes,
		Type: e.Folder, Tags: e.Meta.Tags, Source: e.Meta.Source, TextBytes: len(e.Body),
	}
	for _, p := range e.Meta.Participants {
		t.Participants = append(t.Participants, participant{Name: p.Name, Email: p.Email, Company: p.Company, Role: p.Role, From: p.From})
	}
	if l := e.Meta.CalendarEvent; l != nil && l.UID != "" {
		t.CalendarEvent = &eventRef{Calendar: l.Calendar, UID: l.UID, RecurrenceID: l.RecurrenceID, Title: l.Title, Start: l.Start, Match: l.Match}
	}
	return t
}

type searchIn struct {
	Intent      string `json:"intent" jsonschema:"One sentence: what the user asked for and why this call helps. Logged for the people whose data this is."`
	Query       string `json:"query,omitempty" jsonschema:"words that must all appear (case-insensitive) in the title, participants, tags, source or text"`
	Type        string `json:"type,omitempty" jsonschema:"only this type (see list_transcript_types); unsorted for none"`
	Participant string `json:"participant,omitempty" jsonschema:"only calls with a participant whose name, email or company contains this"`
	From        string `json:"from,omitempty" jsonschema:"only calls starting on or after this date (YYYY-MM-DD) or time (RFC 3339)"`
	To          string `json:"to,omitempty" jsonschema:"only calls starting up to and including this date, or before this time"`
	Limit       int    `json:"limit,omitempty" jsonschema:"how many to return (default 20, at most 100)"`
	Offset      int    `json:"offset,omitempty" jsonschema:"skip this many, for the next page"`
}

type searchOut struct {
	Total       int                 `json:"total" jsonschema:"how many transcripts match"`
	Transcripts []transcriptSummary `json:"transcripts"`
	NextOffset  int                 `json:"next_offset,omitempty" jsonschema:"pass as offset for more results"`
}

func (s *Server) searchTranscripts(_ context.Context, in searchIn) (searchOut, error) {
	var from, to time.Time
	var err error
	if in.From != "" {
		if from, err = parseWhen(in.From, false); err != nil {
			return searchOut{}, fmt.Errorf("from: %w", err)
		}
	}
	if in.To != "" {
		if to, err = parseWhen(in.To, true); err != nil {
			return searchOut{}, fmt.Errorf("to: %w", err)
		}
	}
	typ := strings.ToLower(strings.TrimSpace(in.Type))
	who := strings.ToLower(strings.TrimSpace(in.Participant))
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	offset := max(in.Offset, 0)

	entries, err := s.store.List()
	if err != nil {
		return searchOut{}, err
	}
	out := searchOut{Transcripts: []transcriptSummary{}}
	for _, e := range entries {
		switch {
		case typ != "" && e.Folder != typ,
			!from.IsZero() && e.Start.Before(from),
			!to.IsZero() && !e.Start.Before(to),
			who != "" && !hasParticipant(e, who),
			!e.Matches(in.Query):
			continue
		}
		out.Total++
		if out.Total <= offset || len(out.Transcripts) >= limit {
			continue
		}
		t := summarize(e)
		t.Snippet = snippet(e.Body, in.Query)
		out.Transcripts = append(out.Transcripts, t)
	}
	if offset+len(out.Transcripts) < out.Total {
		out.NextOffset = offset + len(out.Transcripts)
	}
	return out, nil
}

func hasParticipant(e store.Entry, who string) bool {
	for _, p := range e.Meta.Participants {
		if strings.Contains(strings.ToLower(p.Name+"\x00"+p.Email+"\x00"+p.Company), who) {
			return true
		}
	}
	return false
}

// snippet is about 300 bytes of body around the first word of q found.
func snippet(body, q string) string {
	lower := strings.ToLower(body)
	for _, w := range strings.Fields(strings.ToLower(q)) {
		i := strings.Index(lower, w)
		if i < 0 {
			continue
		}
		start, end := runeStart(body, max(i-150, 0)), runeStart(body, min(i+len(w)+150, len(body)))
		s := strings.Join(strings.Fields(body[start:end]), " ")
		if start > 0 {
			s = "…" + s
		}
		if end < len(body) {
			s += "…"
		}
		return s
	}
	return ""
}

// runeStart moves i back to the start of a UTF-8 character.
func runeStart(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

var idRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

type getIn struct {
	Intent   string `json:"intent" jsonschema:"One sentence: what the user asked for and why this call helps. Logged for the people whose data this is."`
	ID       string `json:"id" jsonschema:"the transcript id (12 hex characters), from search_transcripts"`
	Offset   int    `json:"offset,omitempty" jsonschema:"byte offset in the text to start at (next_offset of the previous page)"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"at most this many bytes of text (default 60000)"`
}

type getOut struct {
	transcriptSummary
	Path       string   `json:"path" jsonschema:"file in the company-brain repository"`
	UploadedBy string   `json:"uploaded_by,omitempty"`
	IngestedAt string   `json:"ingested_at,omitempty"`
	EditedBy   string   `json:"edited_by,omitempty"`
	EditedAt   string   `json:"edited_at,omitempty"`
	Problems   []string `json:"problems,omitempty" jsonschema:"why the file doesn't meet the format rules, if it doesn't"`
	Text       string   `json:"text" jsonschema:"the transcript text (Markdown), or this page of it"`
	Offset     int      `json:"offset"`
	NextOffset int      `json:"next_offset,omitempty" jsonschema:"set when there is more text: pass it as offset"`
}

func (s *Server) getTranscript(_ context.Context, in getIn) (getOut, error) {
	id := strings.ToLower(strings.TrimSpace(in.ID))
	if !idRe.MatchString(id) {
		return getOut{}, fmt.Errorf("%q is not a transcript id (12 hex characters)", in.ID)
	}
	e, err := s.store.Get(id)
	if errors.Is(err, store.ErrNotFound) {
		return getOut{}, fmt.Errorf("no transcript with id %s", id)
	}
	if err != nil {
		return getOut{}, err
	}
	out := getOut{
		transcriptSummary: summarize(e), Path: e.Path,
		UploadedBy: e.UploadedBy, IngestedAt: e.IngestedAt, EditedBy: e.EditedBy, EditedAt: e.EditedAt, Problems: e.Problems,
	}
	maxBytes := in.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 60000
	}
	start := runeStart(e.Body, min(max(in.Offset, 0), len(e.Body)))
	end := len(e.Body)
	if end-start > maxBytes {
		// End at a line break when there's one in the last quarter of the page.
		end = runeStart(e.Body, start+maxBytes)
		if nl := strings.LastIndexByte(e.Body[start:end], '\n'); nl > maxBytes*3/4 {
			end = start + nl + 1
		}
		out.NextOffset = end
	}
	out.Text, out.Offset = e.Body[start:end], start
	return out, nil
}

type listTypesIn struct {
	Intent string `json:"intent" jsonschema:"One sentence: what the user asked for and why this call helps. Logged for the people whose data this is."`
}

type typeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

type listTypesOut struct {
	Types []typeCount `json:"types"`
}

func (s *Server) listTypes(_ context.Context, _ listTypesIn) (listTypesOut, error) {
	entries, err := s.store.List()
	if err != nil {
		return listTypesOut{}, err
	}
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Folder]++
	}
	out := listTypesOut{Types: []typeCount{}}
	for t, n := range counts {
		out.Types = append(out.Types, typeCount{t, n})
	}
	sort.Slice(out.Types, func(i, j int) bool { return out.Types[i].Type < out.Types[j].Type })
	return out, nil
}

// --- calendars -------------------------------------------------------------

type listCalendarsIn struct {
	Intent string `json:"intent" jsonschema:"One sentence: what the user asked for and why this call helps. Logged for the people whose data this is."`
}

type calendarInfo struct {
	Key         string `json:"key" jsonschema:"owner/id; pass to list_events"`
	Owner       string `json:"owner"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Yours       bool   `json:"yours"`
}

type listCalendarsOut struct {
	Calendars []calendarInfo `json:"calendars"`
}

func (s *Server) listCalendars(_ context.Context, _ listCalendarsIn, c caller) (listCalendarsOut, error) {
	cals, err := s.cal.Calendars()
	if err != nil {
		return listCalendarsOut{}, err
	}
	out := listCalendarsOut{Calendars: []calendarInfo{}}
	for _, cal := range cals {
		out.Calendars = append(out.Calendars, calendarInfo{
			Key: cal.Key(), Owner: cal.Owner, Name: cal.Title(), Description: cal.Description,
			Yours: cal.Owner == calendarOwner(c.user),
		})
	}
	return out, nil
}

type listEventsIn struct {
	Intent    string   `json:"intent" jsonschema:"One sentence: what the user asked for and why this call helps. Logged for the people whose data this is."`
	From      string   `json:"from,omitempty" jsonschema:"start of the range: a date (YYYY-MM-DD) or time (RFC 3339); default today"`
	To        string   `json:"to,omitempty" jsonschema:"end of the range, up to and including this date or before this time; default 7 days after from"`
	Calendars []string `json:"calendars,omitempty" jsonschema:"only these calendars (keys from list_calendars); default all"`
	Query     string   `json:"query,omitempty" jsonschema:"words that must all appear in the title, location, organizer or guests (case-insensitive)"`
	Limit     int      `json:"limit,omitempty" jsonschema:"at most this many events (default 100, at most 500)"`
}

type person struct {
	Name   string `json:"name,omitempty"`
	Email  string `json:"email,omitempty"`
	Status string `json:"status,omitempty" jsonschema:"e.g. ACCEPTED, DECLINED, NEEDS-ACTION"`
}

type linkedTranscript struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Type  string `json:"type"`
}

type eventInfo struct {
	Calendar     string             `json:"calendar"`
	UID          string             `json:"uid"`
	RecurrenceID string             `json:"recurrence_id,omitempty" jsonschema:"set for one occurrence of a repeating event"`
	Title        string             `json:"title"`
	Start        string             `json:"start"`
	End          string             `json:"end"`
	AllDay       bool               `json:"all_day,omitempty"`
	Location     string             `json:"location,omitempty"`
	Description  string             `json:"description,omitempty"`
	Organizer    *person            `json:"organizer,omitempty"`
	Guests       []person           `json:"guests,omitempty" jsonschema:"everyone invited except the organizer, rooms and resources (including those who declined)"`
	Transcripts  []linkedTranscript `json:"transcripts,omitempty" jsonschema:"transcripts recorded in this event"`
}

type listEventsOut struct {
	From      string      `json:"from"`
	To        string      `json:"to"`
	Total     int         `json:"total"`
	Events    []eventInfo `json:"events"`
	Truncated bool        `json:"truncated,omitempty" jsonschema:"more events than limit: narrow the range"`
	Problems  []string    `json:"problems,omitempty" jsonschema:"calendar files that couldn't be read"`
}

const maxDescription = 2000

func (s *Server) listEvents(_ context.Context, in listEventsIn) (listEventsOut, error) {
	var from, to time.Time
	var err error
	if in.From != "" {
		if from, err = parseWhen(in.From, false); err != nil {
			return listEventsOut{}, fmt.Errorf("from: %w", err)
		}
	} else {
		now := time.Now().In(calendar.DefaultZone())
		from = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, calendar.DefaultZone())
	}
	if in.To != "" {
		if to, err = parseWhen(in.To, true); err != nil {
			return listEventsOut{}, fmt.Errorf("to: %w", err)
		}
	} else {
		to = from.AddDate(0, 0, 7)
	}
	if !to.After(from) {
		return listEventsOut{}, errors.New("to must be after from")
	}
	if to.Sub(from) > 366*24*time.Hour {
		return listEventsOut{}, errors.New("the range can be at most a year")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)

	all, err := s.cal.Calendars()
	if err != nil {
		return listEventsOut{}, err
	}
	cals := all
	if len(in.Calendars) > 0 {
		cals = nil
		for _, k := range in.Calendars {
			i := slices.IndexFunc(all, func(c calendar.Calendar) bool { return c.Key() == strings.TrimSpace(k) })
			if i < 0 {
				var keys []string
				for _, c := range all {
					keys = append(keys, c.Key())
				}
				return listEventsOut{}, fmt.Errorf("no calendar %q; there are: %s", k, strings.Join(keys, ", "))
			}
			cals = append(cals, all[i])
		}
	}
	occs, problems, err := s.cal.Occurrences(cals, from, to)
	if err != nil {
		return listEventsOut{}, err
	}

	linked, err := s.linkedTranscripts()
	if err != nil {
		return listEventsOut{}, err
	}
	out := listEventsOut{From: formatTime(from), To: formatTime(to), Events: []eventInfo{}, Problems: problems}
	words := strings.Fields(strings.ToLower(in.Query))
	for _, o := range occs {
		if len(words) > 0 && !occMatches(o, words) {
			continue
		}
		out.Total++
		if len(out.Events) >= limit {
			out.Truncated = true
			continue
		}
		ev := eventInfo{
			Calendar: o.Calendar, UID: o.UID, Title: o.Summary, AllDay: o.AllDay,
			Start: formatTime(o.Start), End: formatTime(o.End), Location: o.Location,
			Description: strings.TrimSpace(o.Description),
		}
		if o.AllDay {
			ev.Start, ev.End = o.Start.Format(time.DateOnly), o.End.Format(time.DateOnly)
		}
		if len(ev.Description) > maxDescription {
			ev.Description = ev.Description[:runeStart(ev.Description, maxDescription)] + "… (cut)"
		}
		rid := ""
		if o.Recurring() {
			rid = o.RecurrenceID.Format(time.RFC3339)
			ev.RecurrenceID = rid
		}
		if o.Organizer.Email != "" || o.Organizer.Name != "" {
			ev.Organizer = &person{Name: o.Organizer.Name, Email: o.Organizer.Email}
		}
		for _, a := range o.Attendees {
			if a.Type == "ROOM" || a.Type == "RESOURCE" || (a.Email != "" && a.Email == o.Organizer.Email) {
				continue
			}
			ev.Guests = append(ev.Guests, person{Name: a.Name, Email: a.Email, Status: a.Status})
		}
		ev.Transcripts = linked[linkKey(o.UID, rid)]
		out.Events = append(out.Events, ev)
	}
	return out, nil
}

func occMatches(o calendar.Occurrence, words []string) bool {
	var b strings.Builder
	b.WriteString(o.Summary + "\n" + o.Location + "\n" + o.Organizer.Name + " " + o.Organizer.Email + "\n")
	for _, a := range o.Attendees {
		b.WriteString(a.Name + " " + a.Email + "\n")
	}
	text := strings.ToLower(b.String())
	for _, w := range words {
		if !strings.Contains(text, w) {
			return false
		}
	}
	return true
}

func (s *Server) linkedTranscripts() (map[string][]linkedTranscript, error) {
	entries, err := s.store.List()
	if err != nil {
		return nil, err
	}
	linked := map[string][]linkedTranscript{}
	for _, e := range entries {
		if l := e.Meta.CalendarEvent; l != nil && l.UID != "" && e.ID != "" {
			k := linkKey(l.UID, l.RecurrenceID)
			linked[k] = append(linked[k], linkedTranscript{ID: e.ID, Title: e.Label(), Type: e.Folder})
		}
	}
	return linked, nil
}

// linkKey identifies an occurrence: its UID and, for a repeating event,
// when it was originally scheduled (in UTC, so offsets don't matter).
func linkKey(uid, rid string) string {
	if t, err := time.Parse(time.RFC3339, rid); err == nil {
		rid = t.UTC().Format(time.RFC3339)
	}
	return uid + "|" + rid
}

// --- notes -----------------------------------------------------------------

type noteSummary struct {
	Via       string      `json:"via,omitempty" jsonschema:"what wrote it, e.g. MCP: <app>; empty for a person"`
	ID        string      `json:"id"`
	Title     string      `json:"title" jsonschema:"empty while the note is still being saved"`
	Created   string      `json:"created"`
	Author    string      `json:"author"`
	Labels    []string    `json:"labels,omitempty" jsonschema:"see list_note_labels for what each means"`
	Kind      string      `json:"kind,omitempty" jsonschema:"goal or decision; empty for a plain note (context)"`
	Status    string      `json:"status,omitempty" jsonschema:"goal: active, achieved, paused, dropped; decision: considering, made, rejected, reversed"`
	Strength  string      `json:"strength,omitempty" jsonschema:"how firmly it's held: hard, firm, tentative"`
	Private   bool        `json:"private,omitempty" jsonschema:"only its author sees it; you're acting for them"`
	BasedOn   []notes.Ref `json:"based_on,omitempty" jsonschema:"what it rests on: note:<id> (read with get_note) or transcript:<id> (get_transcript), with a quote"`
	Pending   bool        `json:"pending,omitempty" jsonschema:"just written: its title and suggested labels are being generated and it isn't in git yet"`
	TextBytes int         `json:"text_bytes"`
	Snippet   string      `json:"snippet,omitempty"`
}

func summarizeNote(n notes.Note) noteSummary {
	return noteSummary{Via: n.Via, ID: n.ID, Title: n.Title, Created: formatTime(n.Created), Author: n.Author, Labels: n.Labels, Pending: n.Pending, TextBytes: len(n.Body),
		Kind: n.Kind, Status: n.Status, Strength: n.Strength, Private: n.Private, BasedOn: n.BasedOn}
}

type searchNotesIn struct {
	Intent  string `json:"intent" jsonschema:"One sentence: what the user asked for and why this call helps. Logged for the people whose data this is."`
	Query   string `json:"query,omitempty" jsonschema:"words that must all appear (case-insensitive) in the title, author or text"`
	Author  string `json:"author,omitempty" jsonschema:"only notes by this person (ktx username)"`
	Label   string `json:"label,omitempty" jsonschema:"only notes with this label (see list_note_labels)"`
	NoLabel string `json:"without_label,omitempty" jsonschema:"leave out notes with this label, e.g. to skip unreviewed ones"`
	Kind    string `json:"kind,omitempty" jsonschema:"only goals (goal), decisions (decision) or plain notes (plain)"`
	From    string `json:"from,omitempty" jsonschema:"only notes written on or after this date (YYYY-MM-DD) or time (RFC 3339)"`
	To      string `json:"to,omitempty" jsonschema:"only notes written up to and including this date, or before this time"`
	Limit   int    `json:"limit,omitempty" jsonschema:"how many to return (default 20, at most 100)"`
	Offset  int    `json:"offset,omitempty" jsonschema:"skip this many, for the next page"`
}

type searchNotesOut struct {
	Total      int           `json:"total"`
	Notes      []noteSummary `json:"notes"`
	NextOffset int           `json:"next_offset,omitempty"`
}

func (s *Server) searchNotes(_ context.Context, in searchNotesIn, c caller) (searchNotesOut, error) {
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	var from, to time.Time
	var err error
	if in.From != "" {
		if from, err = parseWhen(in.From, false); err != nil {
			return searchNotesOut{}, fmt.Errorf("from: %w", err)
		}
	}
	if in.To != "" {
		if to, err = parseWhen(in.To, true); err != nil {
			return searchNotesOut{}, fmt.Errorf("to: %w", err)
		}
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	offset := max(in.Offset, 0)
	all, err := s.notes.List()
	if err != nil {
		return searchNotesOut{}, err
	}
	out := searchNotesOut{Notes: []noteSummary{}}
	for _, n := range all {
		k := n.Kind
		if k == "" {
			k = "plain"
		}
		switch {
		case !n.Visible(c.user.Display()),
			kind != "" && k != kind,
			in.Author != "" && !strings.EqualFold(n.Author, strings.TrimSpace(in.Author)),
			in.Label != "" && !slices.Contains(n.Labels, strings.ToLower(strings.TrimSpace(in.Label))),
			in.NoLabel != "" && slices.Contains(n.Labels, strings.ToLower(strings.TrimSpace(in.NoLabel))),
			!from.IsZero() && n.Created.Before(from),
			!to.IsZero() && !n.Created.Before(to),
			!n.Matches(in.Query):
			continue
		}
		out.Total++
		if out.Total <= offset || len(out.Notes) >= limit {
			continue
		}
		ns := summarizeNote(n)
		ns.Snippet = snippet(n.Body, in.Query)
		out.Notes = append(out.Notes, ns)
	}
	if offset+len(out.Notes) < out.Total {
		out.NextOffset = offset + len(out.Notes)
	}
	return out, nil
}

type getNoteIn struct {
	Intent string `json:"intent" jsonschema:"One sentence: what the user asked for and why this call helps. Logged for the people whose data this is."`
	ID     string `json:"id" jsonschema:"the note id (12 hex characters), from search_notes"`
}

type getNoteOut struct {
	noteSummary
	Path     string        `json:"path,omitempty" jsonschema:"file in the company-brain repository"`
	EditedBy string        `json:"edited_by,omitempty"`
	EditedAt string        `json:"edited_at,omitempty"`
	Text     string        `json:"text"`
	LeadsTo  []noteSummary `json:"based_on_this,omitempty" jsonschema:"notes, goals and decisions based on this one"`
}

func (s *Server) getNote(_ context.Context, in getNoteIn, c caller) (getNoteOut, error) {
	id := strings.ToLower(strings.TrimSpace(in.ID))
	if !notes.ValidID(id) {
		return getNoteOut{}, fmt.Errorf("%q is not a note id (12 hex characters)", in.ID)
	}
	n, err := s.notes.Get(id)
	if err == nil && !n.Visible(c.user.Display()) {
		err = notes.ErrNotFound
	}
	if errors.Is(err, notes.ErrNotFound) {
		return getNoteOut{}, fmt.Errorf("no note with id %s", id)
	}
	if err != nil {
		return getNoteOut{}, err
	}
	out := getNoteOut{noteSummary: summarizeNote(n), Path: n.Path, EditedBy: n.EditedBy, EditedAt: n.EditedAt, Text: n.Body}
	all, err := s.notes.List()
	if err != nil {
		return getNoteOut{}, err
	}
	for _, l := range notes.BasedOnThis(all, "note:"+n.ID, c.user.Display()) {
		out.LeadsTo = append(out.LeadsTo, summarizeNote(l))
	}
	return out, nil
}

type listLabelsIn struct {
	Intent string `json:"intent" jsonschema:"One sentence: what the user asked for and why this call helps. Logged for the people whose data this is."`
}

type labelInfo struct {
	Name    string `json:"name"`
	Meaning string `json:"meaning"`
	Notes   int    `json:"notes" jsonschema:"how many notes have it"`
}

type listLabelsOut struct {
	Labels []labelInfo `json:"labels"`
}

func (s *Server) listNoteLabels(_ context.Context, _ listLabelsIn) (listLabelsOut, error) {
	defs, err := s.notes.LabelsInUse()
	if err != nil {
		return listLabelsOut{}, err
	}
	out := listLabelsOut{Labels: []labelInfo{}}
	for _, d := range defs {
		out.Labels = append(out.Labels, labelInfo{Name: d.Name, Meaning: d.Description, Notes: d.Count})
	}
	return out, nil
}

// startOf is a transcript's start in its own time zone, or the
// workspace's when it has none.
func startOf(e store.Entry) time.Time {
	if e.Meta.Timezone == "" {
		return e.Start.In(calendar.DefaultZone())
	}
	return e.LocalStart()
}

// SlopLabel is the label every note written through MCP gets. It's defined
// with slopMeaning if nobody has defined it.
const (
	SlopLabel   = "slop"
	slopMeaning = "Written by an AI agent or pasted AI output, not reviewed by a person. Don't take it as fact, decision or anyone's opinion."
)

type createNoteIn struct {
	Intent string   `json:"intent" jsonschema:"One sentence: what the user asked for and why this call helps. Logged for the people whose data this is."`
	Text   string   `json:"text" jsonschema:"the note (Markdown)"`
	Title  string   `json:"title,omitempty" jsonschema:"a short title; leave it out to have one written"`
	Labels []string `json:"labels,omitempty" jsonschema:"more labels from list_note_labels; slop is always added"`
}

type createNoteOut struct {
	ID      string   `json:"id"`
	Labels  []string `json:"labels" jsonschema:"labels so far; the model may add fitting ones when it saves the note"`
	Pending bool     `json:"pending" jsonschema:"true: being named and committed; get_note shows it meanwhile"`
}

func (s *Server) createNote(ctx context.Context, in createNoteIn, c caller) (createNoteOut, error) {
	if strings.TrimSpace(in.Text) == "" {
		return createNoteOut{}, errors.New("text is empty")
	}
	if err := s.notes.EnsureLabel(ctx, SlopLabel, slopMeaning, c.user.Display()); err != nil {
		return createNoteOut{}, err
	}
	labels := append([]string{SlopLabel}, in.Labels...)
	id, err := s.notes.SubmitNote(notes.Submission{
		Body: in.Text, Author: c.user.Display(), Labels: labels, Title: in.Title, Via: "MCP: " + c.grant.ClientName,
	})
	if err != nil {
		return createNoteOut{}, err
	}
	n, err := s.notes.Get(id)
	if err != nil {
		return createNoteOut{}, err
	}
	s.log.Info("note created over mcp", "id", id, "user", c.user.Display(), "app", c.grant.ClientName, "labels", n.Labels)
	return createNoteOut{ID: id, Labels: n.Labels, Pending: n.Pending}, nil
}
