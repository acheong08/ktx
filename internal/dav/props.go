package dav

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// go-webdav answers PROPFIND with a fixed set of calendar properties. Apple
// Calendar and Thunderbird also ask for CalendarServer's getctag, which
// changes whenever anything in a calendar does and tells them when to
// re-sync, and for DAV:supported-report-set. addCalendarProps fills those
// in on the responses for calendar collections. sync-token is deliberately
// left unanswered: without it clients don't try sync-collection, which
// this server doesn't implement, and fall back to comparing ETags.

const (
	ctagEmpty   = `<getctag xmlns="http://calendarserver.org/ns/"></getctag>`
	reportEmpty = `<supported-report-set xmlns="DAV:"></supported-report-set>`
	reports     = `<supported-report-set xmlns="DAV:">` +
		`<supported-report><report><calendar-query xmlns="urn:ietf:params:xml:ns:caldav"/></report></supported-report>` +
		`<supported-report><report><calendar-multiget xmlns="urn:ietf:params:xml:ns:caldav"/></report></supported-report>` +
		`</supported-report-set>`
	respOpen = `<response xmlns="DAV:">`
	propOpen = `<propstat xmlns="DAV:"><prop xmlns="DAV:">`
	statusOK = `<status>HTTP/1.1 200 OK</status>`
	empty404 = `<propstat xmlns="DAV:"><prop xmlns="DAV:"></prop><status>HTTP/1.1 404 Not Found</status></propstat>`
)

// ctag is a hash of a calendar's metadata and the ETags of its events.
func (s *Server) ctag(ctx context.Context, href string) (string, bool) {
	c, name, err := (&backend{s}).resolve(ctx, href)
	if err != nil || name != "" {
		return "", false
	}
	objs, err := s.svc.Objects(c)
	if err != nil {
		return "", false
	}
	parts := []string{c.Key(), c.Name, c.Color, c.Description}
	for _, o := range objs {
		parts = append(parts, o.Name+":"+o.ETag)
	}
	sort.Strings(parts[4:])
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:12]), true
}

// addCalendarProps rewrites a multistatus body from go-webdav.
func (s *Server) addCalendarProps(ctx context.Context, body []byte) []byte {
	if !bytes.Contains(body, []byte(ctagEmpty)) && !bytes.Contains(body, []byte(reportEmpty)) {
		return body
	}
	chunks := strings.Split(string(body), respOpen)
	for i := 1; i < len(chunks); i++ {
		r := chunks[i]
		start, end := strings.Index(r, "<href>"), strings.Index(r, "</href>")
		if start < 0 || end < start {
			continue
		}
		href := r[start+len("<href>") : end]
		ctag, isCal := s.ctag(ctx, href)
		if !isCal {
			continue
		}
		var add string
		if strings.Contains(r, ctagEmpty) {
			r = strings.Replace(r, ctagEmpty, "", 1)
			add += `<getctag xmlns="http://calendarserver.org/ns/">` + ctag + `</getctag>`
		}
		if strings.Contains(r, reportEmpty) {
			r = strings.Replace(r, reportEmpty, "", 1)
			add += reports
		}
		if add == "" {
			continue
		}
		r = strings.Replace(r, empty404, "", 1)
		if ok := strings.Index(r, statusOK); ok >= 0 {
			if p := strings.LastIndex(r[:ok], propOpen); p >= 0 {
				r = r[:p+len(propOpen)] + add + r[p+len(propOpen):]
				chunks[i] = r
				continue
			}
		}
		after := strings.Index(r, "</href>") + len("</href>")
		r = r[:after] + propOpen + add + "</prop>" + statusOK + "</propstat>" + r[after:]
		chunks[i] = r
	}
	return []byte(strings.Join(chunks, respOpen))
}

// recorder buffers a response so it can be rewritten.
type recorder struct {
	h    http.Header
	code int
	buf  bytes.Buffer
}

func (r *recorder) Header() http.Header         { return r.h }
func (r *recorder) Write(b []byte) (int, error) { return r.buf.Write(b) }
func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

// propfind serves a PROPFIND through go-webdav, then adds the calendar
// properties it doesn't know.
func (s *Server) propfind(w http.ResponseWriter, r *http.Request) {
	rec := &recorder{h: http.Header{}}
	s.h.ServeHTTP(rec, r)
	if rec.code == 0 {
		rec.code = http.StatusOK
	}
	body := rec.buf.Bytes()
	if rec.code == http.StatusMultiStatus {
		body = s.addCalendarProps(r.Context(), body)
	}
	for k, v := range rec.h {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(rec.code)
	w.Write(body)
}
