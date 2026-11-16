package web

import (
	"bufio"
	"context"
	"io/fs"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// An axis has a few ticks whatever its span: a span of centuries (times
// from another cluster) neither loops for ever nor fills the memory.
func TestTicksForBounded(t *testing.T) {
	for _, span := range []time.Duration{0, 90 * time.Second, 30 * time.Hour, 250 * 365 * 24 * time.Hour} {
		if n := len(ticksFor(span)); n < 1 || n > maxTicksWant {
			t.Errorf("ticksFor(%s): %d ticks", span, n)
		}
	}
	// The same ticks as before for spans niceStep covers.
	if got := ticksFor(90 * time.Second); len(got) != 5 || got[4].Label != "1:20" {
		t.Errorf("ticksFor(90s): %+v", got)
	}
	done := make(chan []Tick, 1)
	go func() { done <- ticksFor(time.Duration(math.MaxInt64)) }()
	select {
	case got := <-done:
		if len(got) > maxTicksWant {
			t.Errorf("ticksFor(MaxInt64): %d ticks", len(got))
		}
		for _, tk := range got {
			if strings.HasPrefix(tk.L, "-") {
				t.Errorf("ticksFor(MaxInt64): a tick before the start: %+v", tk)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ticksFor(MaxInt64) did not return")
	}
}

// maxTicksWant is the most ticks an axis may have.
const maxTicksWant = 13

// A page's own address in its frame (link previews) is under the base.
func TestShellPathUnderBase(t *testing.T) {
	srv, err := New(Empty{}, testLogger(), Options{Base: "/m/eu", Fleet: fakeFleet{current: "eu"}})
	if err != nil {
		t.Fatal(err)
	}
	sh, err := srv.shell(httptest.NewRequest("GET", "/builds?x=1", nil), "builds", "")
	if err != nil {
		t.Fatal(err)
	}
	if sh.Path != "/m/eu/builds" {
		t.Errorf("Path %q", sh.Path)
	}
}

// memberPages are a cluster's pages as a hub serves them, over b, with
// sign-in when person is not nil (the gate's part: the person in the
// request).
func memberPages(t *testing.T, b Backend, person *auth.Viewer) (*http.ServeMux, http.Handler) {
	t.Helper()
	opt := Options{Base: "/m/eu", Fleet: fakeFleet{current: "eu"}}
	if person != nil {
		opt.Auth = signedInSigner{}
	}
	srv, err := New(b, testLogger(), opt)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	return mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if person != nil {
			r = r.WithContext(auth.With(r.Context(), person))
		}
		mux.ServeHTTP(w, r)
	})
}

func serve(t *testing.T, h http.Handler, method, path string) (rec *httptest.ResponseRecorder) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("%s %s panics: %v", method, path, p)
			rec = httptest.NewRecorder()
			rec.Code = 0
		}
	}()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// A cluster's pages at a hub: signing in and out is the hub's, at its
// root; the page says which cluster it shows; a phone reaches the other
// clusters and the hub's pages; nothing is kept for Back.
func TestMemberPagesAtHub(t *testing.T) {
	alice := &auth.Viewer{Instance: "0a1b2c", Host: "gitlab.example.com", UserID: 7, Username: "alice"}
	mux, h := memberPages(t, Empty{}, alice)
	for _, p := range []string{"/auth/sign-in", "/auth/callback/0a1b2c"} {
		if _, pattern := mux.Handler(httptest.NewRequest("GET", p, nil)); pattern != "GET /" {
			t.Errorf("a cluster's pages serve %s (%s)", p, pattern)
		}
	}
	if _, pattern := mux.Handler(httptest.NewRequest("POST", "/auth/sign-out", nil)); strings.Contains(pattern, "auth") {
		t.Errorf("a cluster's pages serve the sign-out (%s)", pattern)
	}
	rec := serve(t, h, "GET", "/")
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("home: %d", rec.Code)
	}
	for _, want := range []string{
		`action="/auth/sign-out"`, // the hub's
		`<title>Pipelines · eu · ci-accelerator</title>`,
		`class="projmenu clmenu" id="clusterMenu"`,
		`id="mClu"`, `href="/m/us/"`, // the phone's header
	} {
		if !strings.Contains(body, want) {
			t.Errorf("home: no %q", want)
		}
	}
	if strings.Contains(body, `"/m/eu/auth/`) {
		t.Error("home: an address of the sign-in under the base")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("home with sign-in: Cache-Control %q", cc)
	}
	// At a hub a person who may read no project of this cluster does not
	// see its nodes (a box shows them to everyone signed in).
	for _, p := range []string{"/nodes", "/nodes/box-buildkit-abcde"} {
		if rec := serve(t, h, "GET", p); rec.Code != http.StatusNotFound {
			t.Errorf("%s for a person with no project here: %d", p, rec.Code)
		}
	}
	_, box := boxPages(t, alice)
	if rec := serve(t, box, "GET", "/nodes"); rec.Code != 200 {
		t.Errorf("a box's /nodes for a person with no project: %d", rec.Code)
	}
}

// boxPages are a box's pages over nothing, with sign-in.
func boxPages(t *testing.T, person *auth.Viewer) (*http.ServeMux, http.Handler) {
	t.Helper()
	srv, err := New(Empty{}, testLogger(), Options{Auth: signedInSigner{}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	return mux, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mux.ServeHTTP(w, r.WithContext(auth.With(r.Context(), person)))
	})
}

// The hub's own pages offer only what is there: on a phone Members,
// Access and Audit; no search of pipelines, no shortcuts to a cluster's
// pages; the not-found page leads to Members. A page that answers a
// post is marked so theme.js does not load it again.
func TestHubRootNavigation(t *testing.T) {
	ts, _ := hubAdmin(t, true)
	p := as(t, ts, "owner", "GET", "/", nil)
	if p.status != 200 {
		t.Fatalf("Members: %d", p.status)
	}
	bar := regexp.MustCompile(`(?s)<nav class="bbar".*?</nav>`).FindString(p.body)
	for _, want := range []string{`href="/"`, `href="/access"`, `href="/audit"`} {
		if !strings.Contains(bar, want) {
			t.Errorf("the phone's bar on Members: no %s in %q", want, bar)
		}
	}
	for _, not := range []string{`href="/insights"`, `href="/builds"`, `href="/images"`, `href="/nodes"`, `id="bSearch"`} {
		if strings.Contains(bar, not) {
			t.Errorf("the phone's bar on Members: %s", not)
		}
	}
	for _, not := range []string{`id="searchBtn"`, "<h3>Go to</h3>", `data-posted`} {
		if strings.Contains(p.body, not) {
			t.Errorf("Members: %s", not)
		}
	}
	if !strings.Contains(p.body, `data-hub="true"`) || !strings.Contains(p.body, `id="mClu"`) {
		t.Error("Members: not marked as the hub's own page, or no phone menu")
	}
	if cc := p.header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Members with sign-in: Cache-Control %q", cc)
	}
	nf := as(t, ts, "owner", "GET", "/nope", nil)
	if nf.status != 404 || strings.Contains(nf.body, ">Pipelines</a>") || !strings.Contains(nf.body, `<a class="link" href="/">Members</a>`) {
		t.Errorf("not found at the hub: %d, links %q", nf.status, regexp.MustCompile(`<a class="link"[^>]*>[^<]*</a>`).FindAllString(nf.body, -1))
	}
	// A viewer has no Members page: the bar is not there at all.
	if v := as(t, ts, "viewer", "GET", "/nope", nil); strings.Contains(v.body, `class="bbar"`) || strings.Contains(v.body, ">Members</a>") {
		t.Error("a viewer gets the hub's bar or a link to Members")
	}
	posted := as(t, ts, "owner", "POST", "/access/tokens", url.Values{"name": {"tzprobe"}, "role": {"operator"}})
	if !strings.Contains(posted.body, `data-posted="true"`) {
		t.Errorf("the page that answers a post (%d) is not marked", posted.status)
	}
}

// theme.js does not load a page that answers a post again.
func TestThemeScriptKeepsPostedPages(t *testing.T) {
	js, err := fs.ReadFile(files, "static/theme.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	guard, reload := strings.Index(s, "dataset.posted"), strings.Index(s, "location.reload()")
	if guard < 0 || reload < 0 || guard > reload {
		t.Error("theme.js reloads a page that answers a post")
	}
}

// The script on a hub's own pages fetches no search index and offers no
// cluster's pages; a stream an error closed is opened again.
func TestConsoleScriptHubAndStream(t *testing.T) {
	js, err := fs.ReadFile(files, "static/console.js")
	if err != nil {
		t.Fatal(err)
	}
	s := string(js)
	for _, want := range []string{
		`dataset.hub === "true"`, "if (HUB) index = [];", "if (HUB) return [];", "&& !HUB",
		"EventSource.CLOSED", "connect(); }", // reopened after an error closed it
		"its cluster is not connected now", // a step's panel that could not be read says so
	} {
		if !strings.Contains(s, want) {
			t.Errorf("console.js: no %q", want)
		}
	}
}

// The primary button's text has the contrast WCAG AA asks of small text
// in both themes.
func TestPrimaryButtonContrast(t *testing.T) {
	css, err := fs.ReadFile(files, "static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	rule := regexp.MustCompile(`\.btn\.pri \{[^}]*\}`).FindString(s)
	color := regexp.MustCompile(`[^-]color: ([^;]+);`).FindStringSubmatch(rule)
	if color == nil {
		t.Fatalf(".btn.pri: no colour in %q", rule)
	}
	themes := regexp.MustCompile(`--accent: (#[0-9a-fA-F]{6}); --accent-ink: (#[0-9a-fA-F]{6});`).FindAllStringSubmatch(s, -1)
	if len(themes) < 2 {
		t.Fatalf("themes: %v", themes)
	}
	for _, th := range themes {
		ink := strings.TrimSpace(color[1])
		if ink == "var(--accent-ink)" {
			ink = th[2]
		}
		if r := contrast(ink, th[1]); r < 4.5 {
			t.Errorf(".btn.pri: %s on %s: %.2f:1", ink, th[1], r)
		}
	}
}

func contrast(a, b string) float64 {
	lum := func(h string) float64 {
		h = strings.TrimPrefix(h, "#")
		if len(h) == 3 {
			h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
		}
		var c [3]float64
		for i := range c {
			v, _ := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
			x := float64(v) / 255
			if x <= 0.03928 {
				c[i] = x / 12.92
			} else {
				c[i] = math.Pow((x+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
	}
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// Below 1181 px (and with the sidebar collapsed) the cluster switcher is
// not hidden with the project switcher: it pops out beside the sidebar.
func TestClusterMenuNotHiddenWhenNarrow(t *testing.T) {
	css, err := fs.ReadFile(files, "static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range regexp.MustCompile(`[^{}]*\{[^}]*display: none[^}]*\}`).FindAllString(string(css), -1) {
		sel := rule[:strings.Index(rule, "{")]
		for _, one := range strings.Split(sel, ",") {
			if strings.Contains(one, ".projmenu") && !strings.Contains(one, ":not(.clmenu)") && !strings.Contains(one, "[hidden]") {
				t.Errorf("hides the cluster menu: %q", strings.TrimSpace(one))
			}
		}
	}
	if !regexp.MustCompile(`@media \(max-width: 1180px\) \{[^@]*\.side \.clmenu \{ position: fixed`).Match(css) {
		t.Error("no pop-out of the cluster menu below 1181 px")
	}
}

// nilBuild is a cluster that answers a build's page without its build.
type nilBuild struct{ Empty }

func (nilBuild) BuildPage(context.Context, int64) (*store.BuildPage, error) {
	return &store.BuildPage{Stored: &store.Stored{ID: 1}}, nil
}

func (nilBuild) JobBuilds(context.Context, string, uint64) ([]int64, error) {
	return []int64{1, 2}, nil
}

// An answer without its build is a build not found, not a panic.
func TestBuildPageWithoutBuild(t *testing.T) {
	_, h := memberPages(t, nilBuild{}, nil)
	for _, p := range []string{"/builds/1", "/builds/1/steps/0", "/builds/1/compare", "/builds/2/compare?with=1"} {
		if rec := serve(t, h, "GET", p); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec := serve(t, h, "GET", "/builds?a=1"); rec.Code != 200 {
		t.Errorf("/builds?a=1: %d", rec.Code)
	}
	if rec := serve(t, h, "GET", "/jobs/0a1b2c/5"); rec.Code != http.StatusNotFound {
		t.Errorf("a job whose builds have no build: %d", rec.Code)
	}
}

// withWarnings adds build checks whose documentation addresses are not
// web addresses to every build page.
type withWarnings struct{ Backend }

func (b withWarnings) BuildPage(ctx context.Context, id int64) (*store.BuildPage, error) {
	p, err := b.Backend.BuildPage(ctx, id)
	if err != nil || p == nil || p.Stored == nil || p.Build == nil {
		return p, err
	}
	cp, sc, bc := *p, *p.Stored, *p.Build
	bc.Warnings = []history.Warning{
		{Rule: "R1", Message: "m", URL: "//evil.example/a", Line: 1},
		{Rule: "R2", Message: "m", URL: "/~//evil.example/b", Line: 1},
		{Rule: "R3", Message: "m", URL: "/~/access?m=planted", Line: 1},
		{Rule: "R4", Message: "m", URL: "https://docs.docker.com/go/dockerfile/rule/r4/", Line: 1},
	}
	sc.Build = &bc
	cp.Stored = &sc
	return &cp, nil
}

// A build check's link from a cluster leads to the web, never to another
// site without a scheme or to the hub's own pages.
func TestWarningLinksFromACluster(t *testing.T) {
	_, st, ids, _ := consoleWith(t, testOpts{})
	_, h := memberPages(t, withWarnings{&control.Local{Store: st}}, nil)
	var id int64
	for _, i := range ids {
		id = i
	}
	rec := serve(t, h, "GET", "/builds/"+strconv.FormatInt(id, 10))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "R1") {
		t.Fatalf("build page: %d", rec.Code)
	}
	for _, bad := range []string{"evil.example", "planted"} {
		if strings.Contains(body, bad) {
			t.Errorf("a link to %s: %q", bad, regexp.MustCompile(`href="[^"]*`+bad+`[^"]*"`).FindAllString(body, -1))
		}
	}
	if !strings.Contains(body, `href="https://docs.docker.com/go/dockerfile/rule/r4/"`) {
		t.Error("a web address of a build check is gone")
	}
}

// A member's change keys reach the browser's stream only as pipeline
// keys: nothing it sends adds fields to the stream.
func TestEventKeysFromACluster(t *testing.T) {
	ts, st, _ := console(t)
	_, key := seedJob(t, st)
	res, err := ts.Client().Get(ts.URL + "/events")
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
	time.Sleep(50 * time.Millisecond)
	st.Announce("x\n\nevent: changed\ndata: forged\nretry: 1\n\nid: 7")
	time.Sleep(50 * time.Millisecond)
	st.Announce(key)
	timeout := time.After(2 * time.Second)
	for {
		select {
		case l, ok := <-lines:
			if !ok {
				t.Fatal("the stream ended")
			}
			if strings.Contains(l, "forged") || strings.HasPrefix(l, "retry: 1") || strings.HasPrefix(l, "id:") || l == "data: x" {
				t.Errorf("a line from a member's key: %q", l)
			}
			if l == "data: "+key {
				return
			}
		case <-timeout:
			t.Fatal("the pipeline's change did not arrive")
		}
	}
}

// The cluster switchers lead to an away cluster's last-known pages too,
// in the words of Members; they are disclosures, not menus; a collapsed
// sidebar keeps its links' names; Members has an icon of its own.
func TestClusterSwitcherAndSidebarNames(t *testing.T) {
	_, h := memberPages(t, Empty{}, nil)
	rec := serve(t, h, "GET", "/")
	body := rec.Body.String()
	if rec.Code != 200 {
		t.Fatalf("home: %d", rec.Code)
	}
	away := regexp.MustCompile(`<a class="sbtn off" href="/m/ap/" aria-current="false">(<span class="sp13"></span>)?<span class="lbl">ap</span><span class="count">not connected</span></a>`)
	if n := len(away.FindAllString(body, -1)); n != 2 {
		t.Errorf("an away cluster in the switchers: %d links, want 2 (sidebar and phone)", n)
	}
	for _, not := range []string{"offline", `aria-disabled="true"`, "aria-haspopup"} {
		if strings.Contains(body, not) {
			t.Errorf("home: %s", not)
		}
	}
	for _, name := range []string{"Pipelines", "Insights", "Builds", "Images", "Nodes", "Members"} {
		if !strings.Contains(body, `aria-label="`+name+`"`) {
			t.Errorf("the sidebar's %s link has no name of its own", name)
		}
	}
	m := regexp.MustCompile(`<a class="sbtn" href="[^"]*" aria-label="Members">(.*?)<span class="lbl">`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("no Members link in the sidebar")
	}
	if m[1] == string(icon("nodes", 15)) || m[1] != string(icon("fleet", 15)) {
		t.Errorf("Members' icon: %q", m[1])
	}
}

// A form field's border has the 3:1 a control's boundary needs against
// the panel, in both themes.
func TestFieldBorderContrast(t *testing.T) {
	css, err := fs.ReadFile(files, "static/console.css")
	if err != nil {
		t.Fatal(err)
	}
	s := string(css)
	for _, sel := range []string{`.filters select`, `.cf input, .cf select, .cf textarea`, `.filters input[type="text"]`, `.filters input[type="search"], .filters input[type="date"]`} {
		rule := regexp.MustCompile(regexp.QuoteMeta(sel) + ` \{[^}]*\}`).FindString(s)
		if !strings.Contains(rule, "border: 1px solid var(--field)") {
			t.Errorf("%s: border not var(--field): %q", sel, rule)
		}
	}
	themes := regexp.MustCompile(`(?s):root(?:\[data-theme="light"\])? \{.*?\n\}`).FindAllString(s, 2)
	if len(themes) < 2 {
		t.Fatalf("themes: %d", len(themes))
	}
	for _, th := range themes {
		panel := regexp.MustCompile(`--panel: (#[0-9a-fA-F]{6});`).FindStringSubmatch(th)
		field := regexp.MustCompile(`--field: (#[0-9a-fA-F]{6});`).FindStringSubmatch(th)
		if panel == nil || field == nil {
			t.Errorf("a theme without --panel or --field: %.60q", th)
			continue
		}
		if r := contrast(field[1], panel[1]); r < 3 {
			t.Errorf("--field %s on --panel %s: %.2f:1", field[1], panel[1], r)
		}
	}
}
