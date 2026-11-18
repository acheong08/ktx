package web

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/acheong08/ktx/internal/calendar"
)

// Settings → General: the workspace's time zone.

type generalView struct {
	Chrome chrome
	Zone   string
	Zones  []string
	Now    string // the time now in Zone, so a wrong choice is obvious
	Flash  flash
}

func (u *UI) generalView(r *http.Request) generalView {
	loc := calendar.DefaultZone()
	return generalView{Chrome: u.chrome(r, "general"), Zone: loc.String(), Zones: commonZones,
		Now: time.Now().In(loc).Format("Mon 2 Jan, 15:04 MST")}
}

func (u *UI) generalPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, r, http.StatusOK, generalPage(u.generalView(r)))
}

func (u *UI) saveGeneral(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("timezone"))
	loc, err := time.LoadLocation(name)
	if name == "" || name == "Local" || err != nil {
		v := u.generalView(r)
		v.Zone, v.Flash = name, flash{"error", "“" + name + "” isn't a time zone. Use a name like America/Los_Angeles or Europe/London."}
		u.render(w, r, http.StatusUnprocessableEntity, generalPage(v))
		return
	}
	if err := u.db.SetTimeZone(loc.String(), actor(r)); err != nil {
		u.fail(w, r, err)
		return
	}
	changed := loc.String() != calendar.DefaultZone().String()
	calendar.SetDefaultZone(loc)
	u.log.Info("time zone set", "zone", loc.String(), "user", actor(r))
	if changed && u.cal != nil {
		// Calendar times without a zone now mean something else; match
		// transcripts to events again.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if n, err := u.cal.RelinkAll(ctx); err != nil {
				u.log.Error("relinking after a time zone change", "err", err)
			} else if n > 0 {
				u.log.Info("relinked transcripts after a time zone change", "changed", n)
			}
		}()
	}
	v := u.generalView(r)
	v.Flash = flash{"ok", "Times are now shown in " + loc.String() + "."}
	u.render(w, r, http.StatusOK, generalPage(v))
}
