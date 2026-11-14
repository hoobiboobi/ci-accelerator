package web

import (
	"bytes"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// orderConsole is a console holding the TradingAgents builds of the lab
// run (internal/store/testdata/order): cold, then a one-line edit of
// main.py that installed the dependencies again; the edit settled by
// GitLab's comparison. It returns the edit's id.
func orderConsole(t *testing.T) (*httptest.Server, int64) {
	t.Helper()
	testLogs = &lockedBuffer{}
	st, err := store.Open(ctx, t.TempDir(), store.Options{Now: func() time.Time { return time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	load := func(name string) *history.Build {
		d := filepath.Join("..", "store", "testdata", "order", name)
		read := func(f string) []byte {
			b, err := os.ReadFile(filepath.Join(d, f))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		rec, err := history.ParseRecord(read("record.json"))
		if err != nil {
			t.Fatal(err)
		}
		progress, err := history.DecodeProgress(bytes.NewReader(read("logs.bin")))
		if err != nil {
			t.Fatal(err)
		}
		b, err := history.NewBuild(rec, progress, [][]byte{read("provenance.json")}, nil, nil, history.Config{SecretKey: st.SecretKey()})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	cold, edit := load("tradingagents-1"), load("tradingagents-2")
	if _, err := st.SaveBuild(ctx, store.Build{Build: cold, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	r, _ := history.FirstRebuild(cold, edit)
	id, err := st.SaveBuild(ctx, store.Build{Build: edit, Generation: 1, Reason: r})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SettleElsewhere(ctx, []store.Settled{{ID: id, Kind: history.FilesChanged, Paths: []string{"main.py"}, Via: "gitlab d97865d7..0612237d"}}); err != nil {
		t.Fatal(err)
	}
	backend, opt := testOpts{}.wire(t, st)
	srv, err := New(backend, testLogger(), opt)
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return edit.Completed.Add(time.Minute) }
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(Secure(srv.Gate(mux)))
	t.Cleanup(ts.Close)
	return ts, id
}

// The build page of a rebuild that installed the dependencies again only
// because of the order: the verdict says what it cost here, the
// Speed-ups card gives the fragment, the step list marks the step.
func TestOrderOnTheBuildPage(t *testing.T) {
	ts, id := orderConsole(t)
	p := get(t, ts, fmt.Sprintf("/builds/%d", id))
	if p.status != http.StatusOK {
		t.Fatalf("status %d", p.status)
	}
	p.safe(t)
	body := html.UnescapeString(p.body)
	for _, want := range []string{
		// The verdict's line: the measured share and the two steps.
		"<b>" + duration(30430*time.Millisecond) + " of " + duration(36800*time.Millisecond) + "</b> went to installing the dependencies again",
		"none of the files they come from changed (<code>pyproject.toml</code>, <code>setup.py</code> and <code>setup.cfg</code>)",
		"<code>RUN pip install --no-cache-dir .</code> (line 11) comes after <code>COPY . .</code> (line 10)",
		`<svg class="ometer"`,
		// The Speed-ups card.
		`<li class="sp order" id="speedup-2">`,
		"Install the dependencies before copying the code",
		`<span class="chip spk">order</span>`,
		"step [builder 5/5] · lines 10–11",
		"Installed again without a change to those files once in the last 30 days of this image's builds: " + duration(30430*time.Millisecond) + ".",
		" && pip install --no-cache-dir -r /tmp/requirements.txt",
		// The step list.
		`<tr class="st order"`,
		"installed the dependencies again only because the code is copied before it: speed-up 2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("build page lacks %q", want)
		}
	}
	// The general tip about COPY . . gives way to the measured one.
	if strings.Contains(body, "copies the whole repository, so any change to any file rebuilds") {
		t.Error("the general COPY . . tip is still there")
	}
	// The step's own page shows the speed-up.
	m := regexp.MustCompile(`<tr class="st order" data-of="\d+" data-step="(\d+)"`).FindStringSubmatch(p.body)
	if m == nil {
		t.Fatal("no marked step row")
	}
	sp := get(t, ts, fmt.Sprintf("/builds/%d/steps/%s", id, m[1]))
	sb := html.UnescapeString(sp.body)
	if sp.status != http.StatusOK || !strings.Contains(sb, "Install the dependencies before copying the code") || !strings.Contains(sb, "Add a cache mount for pip") {
		t.Errorf("step page: status %d, want both speed-ups", sp.status)
	}
}

func TestFixedVerdict(t *testing.T) {
	got := string(fixedVerdict(&store.OrderFixed{Managers: []string{"pip"}, Step: 4, Name: "RUN pip install -r /tmp/requirements.txt", Line: 11,
		Before: 41, Took: 36800 * time.Millisecond, Wasted: 30400 * time.Millisecond, ThisBuild: 9100 * time.Millisecond}))
	for _, want := range []string{"order fixed", "<b>The dependencies came from the cache:</b> pip installs them before the code is copied now (line 11).",
		"This build took <b>" + duration(9100*time.Millisecond) + "</b>;",
		`<a class="link" href="/builds/41">build 41</a>, the last that installed again, took <b>` + duration(36800*time.Millisecond) + "</b>, " + duration(30400*time.Millisecond) + " of it installing the dependencies."} {
		if !strings.Contains(got, want) {
			t.Errorf("%s\nlacks %q", got, want)
		}
	}
}
