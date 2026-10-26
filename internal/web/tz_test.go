package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

func TestZoneFromTheCookie(t *testing.T) {
	for cookie, want := range map[string]string{
		"":                            "UTC",
		"Europe/Moscow":               "Europe/Moscow",
		url.QueryEscape("Asia/Tokyo"): "Asia/Tokyo",
		"Etc/UTC":                     "Etc/UTC",
		"Not/AZone":                   "UTC",
		"Local":                       "UTC", // the server's zone is not the reader's
		"%zz":                         "UTC",
		strings.Repeat("A", 80):       "UTC",
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: zoneCookie, Value: cookie})
		}
		if got := zone(r).String(); got != want {
			t.Errorf("cookie %q: %s, want %s", cookie, got, want)
		}
	}
}

// Lists show times in the reader's zone; UTC says it is UTC; the tooltip
// always has the moment in UTC.
func TestTimesInTheReadersZone(t *testing.T) {
	at := time.Date(2026, 10, 26, 16, 28, 3, 0, time.UTC)
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	now := at.Add(time.Hour)
	if got := clock(at, now.In(tokyo)); got != "01:28" {
		t.Errorf("Tokyo, same day there: %q", got)
	}
	if got := clock(at, now); got != "16:28 UTC" {
		t.Errorf("UTC: %q", got)
	}
	if got := clock(at, now.Add(48*time.Hour).In(tokyo)); got != "27 Oct 01:28" {
		t.Errorf("Tokyo, another day: %q", got)
	}
	if st := clockStamp(at, now.In(tokyo)); st.UTC != "2026-10-26 16:28:03 UTC" || st.String() != "01:28" {
		t.Errorf("stamp: %+v", st)
	}
	if st := when(at, tokyo); st.Text != "27 Oct 01:28:03" {
		t.Errorf("when: %+v", st)
	}
	if st := when(time.Time{}, tokyo); st.Text != "" || st.UTC != "" {
		t.Errorf("no time: %+v", st)
	}
}

// A day picked in the pipelines filter is the reader's day.
func TestDateFilterInTheReadersZone(t *testing.T) {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	var v PipelinesView
	var pq store.PipeQuery
	pipeFilter(url.Values{"period": {"custom"}, "from": {"2026-10-26"}, "to": {"2026-10-26"}}, time.Now().In(tokyo), &v, &pq)
	if want := time.Date(2026, 10, 25, 15, 0, 0, 0, time.UTC); !pq.Since.Equal(want) {
		t.Errorf("from: %v, want %v", pq.Since.UTC(), want)
	}
	if want := time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC); !pq.Until.Equal(want) {
		t.Errorf("to (included): %v, want %v", pq.Until.UTC(), want)
	}
}

// A page names the zone it shows times in; theme.js compares it with the
// browser's. Times follow the cookie, and without it say UTC.
func TestPagesShowTheReadersZone(t *testing.T) {
	ts, st, _ := console(t)
	keys, _ := pipelines(t, st)
	path := "/pipelines/" + keys[0]
	stamp := regexp.MustCompile(`<span title="(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC)">([^<]*)</span></div></header>`)
	for _, tz := range []string{"", "Asia/Tokyo"} {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if tz != "" {
			req.AddCookie(&http.Cookie{Name: zoneCookie, Value: url.QueryEscape(tz)})
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		buf := make([]byte, 32<<10)
		for {
			n, err := res.Body.Read(buf)
			b.WriteString(string(buf[:n]))
			if err != nil {
				break
			}
		}
		res.Body.Close()
		body := b.String()
		want := tz
		if want == "" {
			want = "UTC"
		}
		if !strings.Contains(body, `<html lang="en" data-tz="`+want+`">`) {
			t.Errorf("%s: the page does not name its zone %s", tz, want)
		}
		m := stamp.FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("%s: no start time with its UTC tooltip in the header", tz)
		}
		utc, _ := time.Parse("2006-01-02 15:04:05 MST", m[1])
		loc, _ := time.LoadLocation(want)
		shown := utc.In(loc).Format("15:04")
		if !strings.Contains(m[2], shown) || strings.HasSuffix(m[2], " UTC") != (tz == "") {
			t.Errorf("%s: shown %q for %s (want %s in %s)", tz, m[2], m[1], shown, want)
		}
	}
}
