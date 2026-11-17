// Package dav serves the brain's calendars over CalDAV, for Apple Calendar,
// Thunderbird and other clients.
//
//	/dav/<user>/                      principal
//	/dav/<user>/calendars/            calendar home
//	/dav/<user>/calendars/<id>/       the user's own calendar (read-write)
//	/dav/<user>/calendars/<owner>~<id>/   someone else's calendar (read-only)
//
// Clients sign in with Basic auth: the ktx username and a calendar token
// (scope caldav) made on the API tokens page. A user sees everyone's
// calendars but can only change their own.
package dav

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	"github.com/acheong08/ktx/internal/calendar"
	"github.com/acheong08/ktx/internal/state"
)

// Prefix is where the CalDAV server is mounted.
const Prefix = "/dav"

const maxEventBytes = 4 << 20

// Server is the CalDAV endpoint.
type Server struct {
	svc *calendar.Service
	db  *state.DB
	log *slog.Logger
	h   *caldav.Handler

	ensured sync.Map // usernames known to have a calendar
}

// New returns a CalDAV server for the calendars in svc.
func New(svc *calendar.Service, db *state.DB, log *slog.Logger) *Server {
	s := &Server{svc: svc, db: db, log: log}
	s.h = &caldav.Handler{Backend: &backend{s}, Prefix: Prefix}
	return s
}

// Register mounts the server and the /.well-known/caldav discovery path.
func (s *Server) Register(mux *http.ServeMux) {
	mux.Handle(Prefix+"/", s)
	mux.HandleFunc("/.well-known/caldav", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, Prefix+"/", http.StatusMovedPermanently)
	})
}

type ctxUser struct{}

func userOf(ctx context.Context) string { u, _ := ctx.Value(ctxUser{}).(string); return u }

// ServeHTTP authenticates the client, then hands over to go-webdav.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, token, ok := r.BasicAuth()
	var user state.User
	var err error = state.ErrNotFound
	if ok {
		user, err = s.db.TokenUser(token, state.ScopeCalDAV)
	}
	if err != nil || !strings.EqualFold(user.Username, name) {
		if ok {
			s.log.Warn("caldav: rejected sign-in", "username", name, "remote", r.RemoteAddr)
		}
		w.Header().Set("WWW-Authenticate", `Basic realm="ktx calendar", charset="UTF-8"`)
		http.Error(w, "use your ktx username and a calendar token", http.StatusUnauthorized)
		return
	}
	username := strings.ToLower(user.Username)
	if !calendar.ValidID(username) {
		http.Error(w, "this username can't be used for calendars", http.StatusForbidden)
		return
	}
	// LimitReader, not MaxBytesReader: go-webdav detects empty bodies with a
	// zero-length Read, which MaxBytesReader doesn't pass through. A body cut
	// off at the limit fails to parse.
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(r.Body, maxEventBytes), r.Body}
	ctx := context.WithValue(r.Context(), ctxUser{}, username)
	if _, done := s.ensured.Load(username); !done {
		// Give a new user a calendar to put events in.
		if err := s.svc.EnsureCalendar(ctx, username, "personal", "Personal"); err != nil {
			s.log.Error("caldav: creating the default calendar", "user", username, "err", err)
		} else {
			s.ensured.Store(username, true)
		}
	}
	switch r.Method {
	case "MKCALENDAR": // RFC 4791; go-webdav only knows extended MKCOL
		s.mkcalendar(w, r.WithContext(ctx))
		return
	case "PROPFIND":
		s.propfind(w, r.WithContext(ctx))
		return
	}
	s.h.ServeHTTP(w, r.WithContext(ctx))
}

// mkcalendar creates a calendar in the user's own home, taking its display
// name from the request body if there is one.
func (s *Server) mkcalendar(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `xml:"set>prop>displayname"`
		Desc string `xml:"set>prop>calendar-description"`
	}
	if b, err := io.ReadAll(r.Body); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		if err := xml.Unmarshal(b, &body); err != nil {
			http.Error(w, "malformed MKCALENDAR body", http.StatusBadRequest)
			return
		}
	}
	code, err := s.createCalendar(r.Context(), r.URL.Path, body.Name, body.Desc)
	if err != nil {
		if code == http.StatusInternalServerError {
			s.log.Error("caldav: mkcalendar", "err", err)
		}
		http.Error(w, err.Error(), code)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// createCalendar makes a calendar at p, which must be directly in the
// user's own home. It returns the HTTP status for a failure.
func (s *Server) createCalendar(ctx context.Context, p, name, desc string) (int, error) {
	user := userOf(ctx)
	id, ok := strings.CutPrefix(strings.TrimSuffix(p, "/"), strings.TrimSuffix(homePath(user), "/")+"/")
	if !ok || !calendar.ValidID(id) {
		return http.StatusForbidden, errors.New("calendars are created in your own home, named with lowercase letters, digits, '-' and '_'")
	}
	err := s.svc.CreateCalendar(ctx, calendar.Calendar{Owner: user, ID: id, Name: name, Description: desc}, user)
	switch {
	case err == nil:
		return http.StatusCreated, nil
	case errors.Is(err, calendar.ErrExists):
		return http.StatusMethodNotAllowed, err
	case errors.Is(err, calendar.ErrInvalid):
		return http.StatusBadRequest, err
	}
	return http.StatusInternalServerError, err
}

type backend struct{ s *Server }

func homePath(user string) string { return Prefix + "/" + user + "/calendars/" }

func (b *backend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return Prefix + "/" + userOf(ctx) + "/", nil
}

func (b *backend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	return homePath(userOf(ctx)), nil
}

// calPath is where user sees calendar c.
func calPath(user string, c calendar.Calendar) string {
	if c.Owner == user {
		return homePath(user) + c.ID + "/"
	}
	return homePath(user) + c.Owner + "~" + c.ID + "/"
}

func toDAV(user string, c calendar.Calendar) caldav.Calendar {
	name := c.Title()
	if c.Owner != user {
		name += " (" + c.Owner + ")"
	}
	return caldav.Calendar{
		Path: calPath(user, c), Name: name, Description: c.Description, Color: c.Color,
		MaxResourceSize: maxEventBytes, SupportedComponentSet: []string{ical.CompEvent},
		ReadOnly: c.Owner != user,
	}
}

// resolve turns a request path into the calendar it addresses and the
// event file name ("" for the calendar itself).
func (b *backend) resolve(ctx context.Context, p string) (calendar.Calendar, string, error) {
	user := userOf(ctx)
	rest, ok := strings.CutPrefix(p, homePath(user))
	if !ok {
		return calendar.Calendar{}, "", httpErr(http.StatusNotFound, "not in your calendar home")
	}
	seg, name, _ := strings.Cut(strings.TrimSuffix(rest, "/"), "/")
	owner, id := user, seg
	if o, i, shared := strings.Cut(seg, "~"); shared {
		owner, id = o, i
	}
	c, err := b.s.svc.Calendar(owner, id)
	if err != nil {
		return calendar.Calendar{}, "", mapErr(err)
	}
	if strings.Contains(name, "/") {
		return calendar.Calendar{}, "", httpErr(http.StatusNotFound, "no such event")
	}
	return c, name, nil
}

func (b *backend) CreateCalendar(ctx context.Context, cal *caldav.Calendar) error {
	if code, err := b.s.createCalendar(ctx, cal.Path, cal.Name, cal.Description); err != nil {
		return webdav.NewHTTPError(code, err)
	}
	return nil
}

func (b *backend) ListCalendars(ctx context.Context) ([]caldav.Calendar, error) {
	user := userOf(ctx)
	cals, err := b.s.svc.Calendars()
	if err != nil {
		return nil, err
	}
	var out []caldav.Calendar
	for _, c := range cals { // own calendars first
		if c.Owner == user {
			out = append(out, toDAV(user, c))
		}
	}
	for _, c := range cals {
		if c.Owner != user {
			out = append(out, toDAV(user, c))
		}
	}
	return out, nil
}

func (b *backend) GetCalendar(ctx context.Context, p string) (*caldav.Calendar, error) {
	c, name, err := b.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if name != "" {
		return nil, httpErr(http.StatusNotFound, "not a calendar")
	}
	dc := toDAV(userOf(ctx), c)
	return &dc, nil
}

func (b *backend) object(c calendar.Calendar, user string, o calendar.Object) (caldav.CalendarObject, error) {
	data, err := o.Decode()
	if err != nil {
		return caldav.CalendarObject{}, err
	}
	// ContentLength stays 0: the body is re-encoded from Data, which may
	// differ in length from the stored file.
	return caldav.CalendarObject{Path: calPath(user, c) + o.Name, ModTime: o.ModTime, ETag: o.ETag, Data: data}, nil
}

func (b *backend) GetCalendarObject(ctx context.Context, p string, _ *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	c, name, err := b.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, httpErr(http.StatusNotFound, "not an event")
	}
	o, err := b.s.svc.Object(c, name)
	if err != nil {
		return nil, mapErr(err)
	}
	co, err := b.object(c, userOf(ctx), o)
	if err != nil {
		return nil, httpErr(http.StatusInternalServerError, "stored event can't be read: "+err.Error())
	}
	return &co, nil
}

func (b *backend) ListCalendarObjects(ctx context.Context, p string, _ *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	c, name, err := b.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if name != "" {
		return nil, httpErr(http.StatusNotFound, "not a calendar")
	}
	objs, err := b.s.svc.Objects(c)
	if err != nil {
		return nil, mapErr(err)
	}
	user := userOf(ctx)
	out := make([]caldav.CalendarObject, 0, len(objs))
	for _, o := range objs {
		co, err := b.object(c, user, o)
		if err != nil {
			b.s.log.Warn("caldav: skipping unreadable event", "path", o.Path, "err", err)
			continue
		}
		out = append(out, co)
	}
	return out, nil
}

func (b *backend) QueryCalendarObjects(ctx context.Context, p string, q *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	all, err := b.ListCalendarObjects(ctx, p, nil)
	if err != nil {
		return nil, err
	}
	var out []caldav.CalendarObject
	for i := range all {
		ok, err := caldav.Match(q.CompFilter, &all[i])
		if ok || err != nil { // an event we can't evaluate is returned rather than hidden
			out = append(out, all[i])
		}
	}
	return out, nil
}

func (b *backend) PutCalendarObject(ctx context.Context, p string, data *ical.Calendar, opts *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	user := userOf(ctx)
	c, name, err := b.resolve(ctx, p)
	if err != nil {
		return nil, err
	}
	if c.Owner != user {
		return nil, httpErr(http.StatusForbidden, "this calendar is "+c.Owner+"'s; it is read-only for you")
	}
	if name == "" {
		return nil, httpErr(http.StatusMethodNotAllowed, "PUT an event, not a calendar")
	}
	var opt calendar.PutOptions
	if opts != nil {
		if opts.IfNoneMatch.IsSet() {
			opt.IfNoneMatch = true
		}
		if opts.IfMatch.IsSet() {
			if opts.IfMatch.IsWildcard() {
				if _, err := b.s.svc.Object(c, name); err != nil {
					return nil, httpErr(http.StatusPreconditionFailed, "the event doesn't exist")
				}
			} else if opt.IfMatch, err = opts.IfMatch.ETag(); err != nil {
				return nil, httpErr(http.StatusBadRequest, "malformed If-Match")
			}
		}
	}
	o, _, err := b.s.svc.Put(ctx, c, name, data, opt, user)
	if err != nil {
		return nil, mapErr(err)
	}
	return &caldav.CalendarObject{Path: calPath(user, c) + o.Name, ModTime: o.ModTime, ETag: o.ETag}, nil
}

func (b *backend) DeleteCalendarObject(ctx context.Context, p string) error {
	user := userOf(ctx)
	c, name, err := b.resolve(ctx, p)
	if err != nil {
		return err
	}
	if c.Owner != user {
		return httpErr(http.StatusForbidden, "this calendar is "+c.Owner+"'s; it is read-only for you")
	}
	if name == "" {
		return httpErr(http.StatusForbidden, "calendars can't be deleted over CalDAV")
	}
	return mapErr(b.s.svc.Delete(ctx, c, name, "", user))
}

func httpErr(code int, msg string) error { return webdav.NewHTTPError(code, errors.New(msg)) }

func mapErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, calendar.ErrNotFound):
		return webdav.NewHTTPError(http.StatusNotFound, err)
	case errors.Is(err, calendar.ErrPrecondition):
		return webdav.NewHTTPError(http.StatusPreconditionFailed, err)
	case errors.Is(err, calendar.ErrConflict):
		return webdav.NewHTTPError(http.StatusConflict, err)
	case errors.Is(err, calendar.ErrExists):
		return webdav.NewHTTPError(http.StatusMethodNotAllowed, err)
	case errors.Is(err, calendar.ErrInvalid):
		return webdav.NewHTTPError(http.StatusBadRequest, err)
	}
	return fmt.Errorf("caldav: %w", err)
}
