package web

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// TestUI runs the browser checks of test/ui against the console serving
// the fixtures, with a running job and two daemons, while one pipeline
// keeps changing (its page must update without losing what the reader
// typed). It needs Node >= 22 and Chrome: `make ui-test` (UI_TEST=1).
func TestUI(t *testing.T) {
	if os.Getenv("UI_TEST") == "" {
		t.Skip("browser checks: make ui-test")
	}
	t.Parallel()
	node := os.Getenv("NODE")
	if node == "" {
		node = "node"
	}
	started := testNow.Add(-42 * time.Second)
	daemons := live{{Name: "box-buildkit-abcde", Connected: true, InspectedAt: time.Now(),
		Inspection: collector.Inspection{Node: "node-1", Version: "v0.33.1",
			GC:    []collector.GCRule{{Filters: []string{"type==exec.cachemount"}, Keep: 336 * time.Hour}, {MaxUsed: 40 << 30, MinFree: 16 << 30}},
			Usage: collector.Usage{Total: 300 << 20, Images: 200 << 20, Layers: 90 << 20, Mounts: 10 << 20}},
		Active: []collector.Active{{Daemon: "box-buildkit-abcde", Target: "api" + evil, Started: started}}},
		{Name: "box-buildkit-fghij", Connected: false}}
	f, _ := fakeGitLab(t, "Running with gitlab-runner\n$ helm upgrade app\ndeployed "+evil+"\n")
	ts, st, _, _ := consoleWith(t, Options{Live: daemons, Release: "box", API: f, Token: gitlab.Token("t")})
	f.Store = st
	// A job that built nothing, with a log to read.
	_, key := seedJob(t, st)
	browse(t, node, st, ts.URL, key)
}

// browse runs the browser checks against base while the pipeline key keeps
// changing; extra are more arguments for ui.mjs.
func browse(t *testing.T, node string, st *store.Store, base, key string, extra ...string) {
	t.Helper()
	stop, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop.Done():
				return
			case <-tick.C:
				st.Announce(key)
			}
		}
	}()
	if dir := os.Getenv("UI_SHOTS"); dir != "" {
		extra = append(extra, "--shots", filepath.Join(dir, t.Name()))
	}
	run, end := context.WithTimeout(ctx, 9*time.Minute)
	defer end()
	cmd := exec.CommandContext(run, node, append([]string{"../../test/ui/ui.mjs", "--base", base, "--live", "/pipelines/" + key}, extra...)...)
	out, err := cmd.CombinedOutput()
	t.Logf("\n%s", out)
	if err != nil {
		t.Fatalf("browser checks: %v", err)
	}
}

// TestUISignedIn runs the browser checks as a person signed in through a
// (fake) GitLab who may read the project, then signs out.
func TestUISignedIn(t *testing.T) {
	if os.Getenv("UI_TEST") == "" {
		t.Skip("browser checks: make ui-test")
	}
	t.Parallel()
	node := os.Getenv("NODE")
	if node == "" {
		node = "node"
	}
	g := newSignInGitLab(t)
	to, _ := url.Parse(g.srv.URL)
	via := &http.Client{Transport: rewrite{to}, Timeout: 10 * time.Second}
	const gl = "https://gitlab.example.com/"
	inst := history.InstancePrefix(gl)
	ts, st, _, _ := consoleBuild(t, func(st *store.Store) Options {
		client := gitlab.New([]string{gl})
		client.HTTP = via
		a, err := auth.New(auth.Config{PublicURL: "http://localhost", Key: make([]byte, 32), HTTP: via,
			Instances: []auth.Instance{{URL: gl, ClientID: "cid", Secret: "sec"}}}, st, client, nil)
		if err != nil {
			t.Fatal(err)
		}
		return Options{Auth: a, API: &gitlab.Fetcher{Client: client, Store: st}}
	}, httptest.NewServer)
	_, key := seedJob(t, st)
	// Sign in as alice the way a browser would; Go's cookie jar keeps no
	// Secure cookie over http, so the cookies are passed by hand.
	c := ts.Client()
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := c.Get(ts.URL + "/auth/sign-in/" + inst + "?to=/")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	loc, _ := url.Parse(res.Header.Get("Location"))
	req, _ := http.NewRequest("GET", ts.URL+"/auth/callback/"+inst+"?code=alice&state="+url.QueryEscape(loc.Query().Get("state")), nil)
	for _, ck := range res.Cookies() {
		req.AddCookie(ck)
	}
	res, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	var session string
	for _, ck := range res.Cookies() {
		if ck.Name == auth.SessionCookie {
			session = ck.Name + "=" + ck.Value
		}
	}
	if session == "" {
		t.Fatalf("no session: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// Chrome keeps Secure cookies for http only on localhost.
	base := strings.Replace(ts.URL, "127.0.0.1", "localhost", 1)
	browse(t, node, st, base, key, "--cookie", session)
}

// TestDevServer serves the fixtures at $CONSOLE_DEV with the templates and
// static files read from disk for every page, until it is stopped: for
// working on the pages (`make console-dev`).
func TestDevServer(t *testing.T) {
	addr := os.Getenv("CONSOLE_DEV")
	if addr == "" {
		t.Skip("a server for working on the pages: make console-dev")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	start := func(h http.Handler) *httptest.Server {
		s := httptest.NewUnstartedServer(h)
		s.Listener.Close()
		s.Listener = ln
		s.Start()
		return s
	}
	daemons := live{{Name: "box-buildkit-abcde", Connected: true, InspectedAt: time.Now(),
		Inspection: collector.Inspection{Node: "node-1", Version: "v0.33.1"}}}
	f, _ := fakeGitLab(t, "Running with gitlab-runner\n$ make\nok\n")
	_, st, _, _ := consoleBuild(t, func(*store.Store) Options {
		return Options{Live: daemons, Release: "dev", API: f, Token: gitlab.Token("t"), Assets: os.DirFS(".")}
	}, start)
	f.Store = st
	seedJob(t, st)
	t.Logf("the console with the fixtures: http://%s/ (pages read from internal/web; Ctrl-C stops)", addr)
	select {}
}
