// Package mcpserver is ktx's MCP server (/mcp): read-only tools for agents
// to search and read transcripts and calendars, as the person who
// authorized them over OAuth (package oauth).
//
// Agents see what the person sees in the web UI: every transcript and every
// calendar. Every call must say why it's being made (the intent argument);
// intents are logged with the user and the tool, not stored.
package mcpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/notes"
	"github.com/acheong08/ktx/internal/state"
	"github.com/acheong08/ktx/internal/store"
)

// Verifier checks access tokens (oauth.Provider).
type Verifier interface {
	Verify(token string) (state.User, state.OAuthGrant, error)
	ResourceMetadataURL() string
}

// Server serves /mcp.
type Server struct {
	store   *store.Store
	cal     *calendar.Service // nil: no calendar tools
	notes   *notes.Service    // nil: no note tools
	tokens  Verifier
	log     *slog.Logger
	version string
	handler http.Handler
}

// New builds the MCP server. cal and nt may be nil.
func New(st *store.Store, cal *calendar.Service, nt *notes.Service, tokens Verifier, version string, log *slog.Logger) *Server {
	s := &Server{store: st, cal: cal, notes: nt, tokens: tokens, log: log, version: version}
	s.handler = mcp.NewStreamableHTTPHandler(s.server, &mcp.StreamableHTTPOptions{
		Stateless:    true, // every tool is a plain read; nothing to keep between requests
		JSONResponse: true,
		// ktx sits behind Caddy on 127.0.0.1, so every request looks local;
		// requests are authenticated by bearer token instead.
		DisableLocalhostProtection: true,
	})
	return s
}

// Register adds /mcp to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("/mcp", s.serveHTTP)
}

type ctxKey struct{}

type caller struct {
	user  state.User
	grant state.OAuthGrant
}

func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID")
	h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
	h.Set("Access-Control-Expose-Headers", "WWW-Authenticate, Mcp-Session-Id")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		s.unauthorized(w, "")
		return
	}
	user, grant, err := s.tokens.Verify(token)
	if err != nil {
		s.unauthorized(w, "invalid_token")
		return
	}
	ctx := withCaller(r.Context(), caller{user: user, grant: grant})
	s.handler.ServeHTTP(w, r.WithContext(ctx))
}

func (s *Server) unauthorized(w http.ResponseWriter, code string) {
	v := fmt.Sprintf(`Bearer resource_metadata=%q, scope="mcp"`, s.tokens.ResourceMetadataURL())
	if code != "" {
		v += fmt.Sprintf(`, error=%q`, code)
	}
	w.Header().Set("WWW-Authenticate", v)
	http.Error(w, "missing or invalid access token", http.StatusUnauthorized)
}

// server builds the MCP server for one request, for the caller.
func (s *Server) server(r *http.Request) *mcp.Server {
	c, _ := callerFrom(r.Context())
	srv := mcp.NewServer(&mcp.Implementation{Name: "ktx", Title: "ktx (Melzi company brain)", Version: s.version},
		&mcp.ServerOptions{Instructions: s.instructions(c.user)})
	s.addTools(srv, c)
	return srv
}

func (s *Server) instructions(u state.User) string {
	var b strings.Builder
	b.WriteString("ktx is Melzi's company brain: transcripts of calls and meetings (from Granola, Pocket, and uploads), " +
		"the team's notes, and the team's calendars. Transcripts are linked to the calendar event they were recorded in, " +
		"and carry the event's guests as participants.\n\n")
	fmt.Fprintf(&b, "You are acting for %s", u.Display())
	if u.Name != "" && u.Name != u.Display() {
		fmt.Fprintf(&b, " (%s", u.Name)
		if u.Email != "" {
			fmt.Fprintf(&b, ", %s", u.Email)
		}
		b.WriteString(")")
	} else if u.Email != "" {
		fmt.Fprintf(&b, " (%s)", u.Email)
	}
	b.WriteString(". ")
	if s.cal != nil {
		fmt.Fprintf(&b, "Their own calendars are owned by %q. ", calendarOwner(u))
	}
	b.WriteString("Access is read-only.\n\n" +
		"Every tool takes an `intent`: one sentence on what the user asked for and why this call helps. It is logged.\n\n" +
		"Start with search_transcripts (filters: words, type, participant, dates) and read one with get_transcript. " +
		"Notes (search_notes, get_note) are free-form things people wrote down, separate from transcripts. " +
		"Notes can have labels; list_note_labels says what each means (some may mark notes as unreviewed or unreliable). " +
		"Use list_events to see meetings in a time range and which transcripts belong to them. " +
		"Dates without an offset are in " + calendar.DefaultZone.String() + ".")
	return b.String()
}

// calendarOwner is the owner folder of a user's calendars (as in the web UI
// and CalDAV).
func calendarOwner(u state.User) string { return strings.ToLower(u.Username) }

func withCaller(ctx context.Context, c caller) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

func callerFrom(ctx context.Context) (caller, bool) {
	c, ok := ctx.Value(ctxKey{}).(caller)
	return c, ok
}
