package web

import (
	"strings"
	"testing"
	"time"
)

// An open stream's session checks do not renew the session: a stream
// cannot set the cookie again, so the next page must, or the browser's
// cookie runs out while the session lives on.
func TestPageRenewsCookieAfterStreamChecks(t *testing.T) {
	ts, _, _, g, browser, srv := signedInServer(t)
	srv.SessionCheck = 20 * time.Millisecond
	alice := browser("alice")
	res, err := alice.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	// The session's last use is now old enough to be written again.
	g.mu.Lock()
	g.skew = 11 * time.Minute
	g.mu.Unlock()
	time.Sleep(300 * time.Millisecond) // several checks of the stream
	res.Body.Close()
	page, err := alice.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page.Body.Close()
	set := strings.Join(page.Header.Values("Set-Cookie"), "\n")
	if !strings.Contains(set, "Max-Age=") {
		t.Errorf("the page after the stream's checks did not renew the cookie: %q", set)
	}
}
