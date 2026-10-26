package web

import (
	"net/http"
	"net/url"
	"sync"
	"time"
	_ "time/tzdata" // the console's image has no zone files
)

// The pages show times in the reader's time zone. The browser says which
// one in a cookie (theme.js sets it before the page paints); a page knows
// no other way, so until it is set, and for a zone this build does not
// know, the pages show UTC and say so.
const zoneCookie = "tz"

// zones caches the locations readers asked for; only names the zone
// database knows are kept, so it stays as small as that database.
var zones sync.Map

// zone is the reader's time zone, UTC when the browser has not said.
func zone(r *http.Request) *time.Location {
	c, err := r.Cookie(zoneCookie)
	if err != nil || c.Value == "" || len(c.Value) > 64 {
		return time.UTC
	}
	name, err := url.QueryUnescape(c.Value)
	if err != nil {
		return time.UTC
	}
	if v, ok := zones.Load(name); ok {
		return v.(*time.Location)
	}
	loc, err := time.LoadLocation(name)
	if err != nil || name == "Local" {
		return time.UTC
	}
	zones.Store(name, loc)
	return loc
}

// Stamp is a moment as a page shows it: in the reader's zone, and the same
// moment in UTC for its tooltip (runner logs and GitLab's API are in UTC).
// It prints as its text.
type Stamp struct {
	Text string
	UTC  string
}

func (s Stamp) String() string { return s.Text }

// stampOf formats t in loc with layout; the zone's name follows when it is
// UTC, so a time is never mistaken for local when it is not.
func stampOf(t time.Time, loc *time.Location, layout string) Stamp {
	if t.IsZero() {
		return Stamp{}
	}
	text := t.In(loc).Format(layout)
	if loc == time.UTC {
		text += " UTC"
	}
	return Stamp{Text: text, UTC: t.UTC().Format("2006-01-02 15:04:05") + " UTC"}
}

// inZone is now in the reader's zone: helpers that format a time next to
// "now" (clock) show it in now's location.
func inZone(now time.Time, loc *time.Location) time.Time { return now.In(loc) }
