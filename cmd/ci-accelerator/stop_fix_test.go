package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// startAll runs role all over data until the test replaces its GitLab URLs
// file (the role then stops as on SIGTERM); it returns the console's
// address, the stop and the role's end.
func startAll(t *testing.T, data string) (addr string, stop func(), done chan error, logs *lockedBuf) {
	t.Helper()
	dir := t.TempDir()
	urls := filepath.Join(dir, "urls")
	if err := os.WriteFile(urls, []byte("https://gitlab.example.com/"), 0o600); err != nil {
		t.Fatal(err)
	}
	urlsEvery = 20 * time.Millisecond
	logs = &lockedBuf{}
	logOut = logs
	t.Cleanup(func() { urlsEvery = 30 * time.Second; logOut = os.Stderr })
	done = make(chan error, 1)
	go func() {
		done <- controllerMain([]string{"-data", data, "-daemon", "tcp://127.0.0.1:1", "-server-name", "daemons", "-tls-dir", dir,
			"-gitlab-urls-file", urls, "-listen", "127.0.0.1:0"}, false)
	}()
	re := regexp.MustCompile(`"msg":"ready"[^\n]*"listen":"(127\.0\.0\.1:\d+)"`)
	for deadline := time.Now().Add(20 * time.Second); addr == ""; time.Sleep(20 * time.Millisecond) {
		if m := re.FindStringSubmatch(logs.String()); m != nil {
			addr = m[1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("not ready:\n%s", logs.String())
		}
	}
	return addr, func() { replace(t, urls, "https://gitlab.example.com") }, done, logs
}

// Role all stops at once with a live page open: the page's event stream
// ends with the server instead of holding the stop for its 5 s and turning
// it into a failure.
func TestRoleAllStopsWithALivePageOpen(t *testing.T) {
	addr, stop, done, logs := startAll(t, t.TempDir())
	res, err := http.Get("http://" + addr + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("/events: %s", res.Status)
	}
	go io.Copy(io.Discard, res.Body)
	start := time.Now()
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("stopped with %v", err)
		}
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("stopped after %s with a live page open", d)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("did not stop:\n%s", logs.String())
	}
}

// A box started without sign-in ends the sessions an earlier sign-in left:
// nothing would read or remove them, sealed GitLab tokens included.
func TestRoleAllWithoutSignInEndsEarlierSessions(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	st, err := store.Open(ctx, data, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.SaveSession(ctx, store.Session{ID: []byte("a"), Instance: "0a1b2c", UserID: 7, Username: "alice", Token: []byte("sealed"), Created: now, Seen: now}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	_, stop, done, logs := startAll(t, data)
	stop()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "sign-in is off") {
		t.Errorf("no word of the ended sessions:\n%s", logs.String())
	}
	st, err = store.Open(ctx, data, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Session(ctx, []byte("a")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the earlier sign-in's session is still stored: %v", err)
	}
}
