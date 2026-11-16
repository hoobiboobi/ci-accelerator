package web

import (
	"bufio"
	"testing"
	"time"
)

// These tests use what the fixes added (rebase, Server.SessionCheck).

// The rewrite of a cluster's pages under its base: every address
// attribute, schemeless addresses left alone, hub links only in address
// attributes.
func TestRebase(t *testing.T) {
	in := `<a href="/x">` + `<a href="/">` + `<div data-url="/jobs/a/1/log">` + `<img src="/s">` + `<form action="/p">` +
		`<a href="//evil.example/a">` + `<a href="/~/access">` + `<a href="/~/m/us/">` +
		`<i title="/~/x" data-href="/y" data-to="/z">` + `<a href="https://gitlab.example.com/">`
	want := `<a href="/m/eu/x">` + `<a href="/m/eu/">` + `<div data-url="/m/eu/jobs/a/1/log">` + `<img src="/m/eu/s">` + `<form action="/m/eu/p">` +
		`<a href="//evil.example/a">` + `<a href="/access">` + `<a href="/m/us/">` +
		`<i title="/~/x" data-href="/y" data-to="/z">` + `<a href="https://gitlab.example.com/">`
	if got := string(rebase([]byte(in), "/m/eu", true)); got != want {
		t.Errorf("rebase:\n got %s\nwant %s", got, want)
	}
	// Outside a hub "/~/" is an address like any other.
	if got := string(rebase([]byte(`<a href="/~/x">`), "/m/eu", false)); got != `<a href="/m/eu/~/x">` {
		t.Errorf("rebase without a hub: %s", got)
	}
	// The hub's own pages (no base): only the hub links change.
	if got := string(rebase([]byte(`<a href="/~/audit"><a href="/x">`), "", true)); got != `<a href="/audit"><a href="/x">` {
		t.Errorf("rebase at the hub's root: %s", got)
	}
}

// A stream ends once its person signed out: it does not go on telling of
// changes on the session it was opened with.
func TestStreamEndsAfterSignOut(t *testing.T) {
	ts, st, _, _, browser, srv := signedInServer(t)
	srv.SessionCheck = 50 * time.Millisecond
	_, key := seedJob(t, st)
	alice := browser("alice")
	res, err := alice.Get(ts.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	<-lines // retry:
	if p := fetch(t, alice, ts, "POST", "/auth/sign-out", map[string]string{"Origin": ts.URL, "Sec-Fetch-Site": "same-origin"}); p.status != 303 {
		t.Fatalf("sign-out: %d", p.status)
	}
	timeout := time.After(3 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				return // ended
			}
			if l == "data: "+key {
				t.Fatal("heard of a pipeline after signing out")
			}
		case <-tick.C:
			st.Announce(key)
		case <-timeout:
			t.Fatal("the stream is still open after signing out")
		}
	}
}
