package dav

import (
	"encoding/xml"
	"errors"
	"html"
	"io"
	"net/http"
	"strings"

	"github.com/acheong08/ktx/internal/calendar"
)

// PROPPATCH on one of your own calendars changes its name, colour or
// description (go-webdav doesn't implement PROPPATCH). The calendar's id,
// and so its path, never change. Other properties clients set alongside,
// such as Apple's calendar-order, are accepted and ignored, so a rename
// isn't refused because of them.

const (
	nsDAV    = "DAV:"
	nsCalDAV = "urn:ietf:params:xml:ns:caldav"
	nsApple  = "http://apple.com/ns/ical/"
)

type propElem struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
}

type propList struct {
	Prop struct {
		Any []propElem `xml:",any"`
	} `xml:"DAV: prop"`
}

type propertyUpdate struct {
	XMLName xml.Name   `xml:"DAV: propertyupdate"`
	Set     []propList `xml:"DAV: set"`
	Remove  []propList `xml:"DAV: remove"`
}

func (s *Server) proppatch(w http.ResponseWriter, r *http.Request) {
	user := userOf(r.Context())
	c, name, err := (&backend{s}).resolve(r.Context(), r.URL.Path)
	switch {
	case err != nil:
		http.Error(w, "no such calendar", http.StatusNotFound)
		return
	case name != "":
		http.Error(w, "event properties can't be changed", http.StatusForbidden)
		return
	case c.Owner != user:
		http.Error(w, "this calendar is "+c.Owner+"'s; it is read-only for you", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "can't read the request", http.StatusBadRequest)
		return
	}
	var pu propertyUpdate
	if err := xml.Unmarshal(body, &pu); err != nil {
		http.Error(w, "malformed PROPPATCH body", http.StatusBadRequest)
		return
	}
	var touched []xml.Name
	apply := func(p propElem, value string) {
		touched = append(touched, p.XMLName)
		switch p.XMLName {
		case xml.Name{Space: nsDAV, Local: "displayname"}:
			c.Name = strings.TrimSpace(value)
		case xml.Name{Space: nsApple, Local: "calendar-color"}:
			c.Color = strings.TrimSpace(value)
		case xml.Name{Space: nsCalDAV, Local: "calendar-description"}:
			c.Description = strings.TrimSpace(value)
		}
	}
	for _, set := range pu.Set {
		for _, p := range set.Prop.Any {
			apply(p, p.Value)
		}
	}
	for _, rm := range pu.Remove {
		for _, p := range rm.Prop.Any {
			apply(p, "")
		}
	}
	status := "HTTP/1.1 200 OK"
	code := http.StatusMultiStatus
	if err := s.svc.UpdateCalendar(r.Context(), c, user); err != nil {
		if !errors.Is(err, calendar.ErrInvalid) {
			s.log.Error("caldav: proppatch", "calendar", c.Key(), "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		status = "HTTP/1.1 409 Conflict" // RFC 4918: a value the server won't store
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<multistatus xmlns="DAV:"><response><href>` + html.EscapeString(r.URL.Path) + `</href><propstat><prop>`)
	for _, n := range touched {
		b.WriteString(`<` + n.Local + ` xmlns="` + html.EscapeString(n.Space) + `"/>`)
	}
	b.WriteString(`</prop><status>` + status + `</status></propstat></response></multistatus>`)
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(code)
	io.WriteString(w, b.String())
}
