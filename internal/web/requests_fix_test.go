package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// The test signers name no GitLab (the shell asks with several GitLabs).
func (signedInSigner) Instances() []auth.Choice { return nil }
func (gateSigner) Instances() []auth.Choice     { return nil }

// openStore is an empty store at testNow.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(ctx, t.TempDir(), store.Options{Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// serveBackend is the pages over b, without sign-in.
func serveBackend(t *testing.T, b Backend) *httptest.Server {
	t.Helper()
	testLogs = &lockedBuffer{}
	srv, err := New(b, testLogger(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return testNow }
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(Secure(mux))
	t.Cleanup(ts.Close)
	return ts
}

// silentController is a backend whose controller does not answer for the
// daemons (control.Client while the controller restarts).
type silentController struct{ Backend }

func (silentController) Daemons() []collector.DaemonState { return nil }
func (silentController) DaemonsKnown() ([]collector.DaemonState, bool) {
	return nil, false
}

// While the controller does not answer, the Nodes pages say so; they do
// not say that the daemons are gone or were replaced.
func TestNodesWhenTheControllerIsSilent(t *testing.T) {
	st := openStore(t)
	savePipeline(t, st, 600, testNow.Add(-time.Hour)) // built on bk-daemon-aaaaa
	ts := serveBackend(t, silentController{&control.Local{Store: st}})
	const say = "did not answer; the daemons are not known now"
	list := get(t, ts, "/nodes")
	if !strings.Contains(list.body, say) {
		t.Errorf("the Nodes page does not say the controller did not answer")
	}
	if strings.Contains(list.body, "No daemon reached yet") {
		t.Errorf("the Nodes page says no daemon is reached")
	}
	if strings.Contains(list.body, "replaced since") {
		t.Errorf("the Nodes page says the daemons were replaced")
	}
	one := get(t, ts, "/nodes/bk-daemon-aaaaa")
	if one.status != http.StatusServiceUnavailable || !strings.Contains(one.body, say) || strings.Contains(one.body, "No daemon named") {
		t.Errorf("a node's page: %d, says the daemon is gone %v", one.status, strings.Contains(one.body, "No daemon named"))
	}
	if !strings.Contains(one.body, `data-live-nodes="true"`) {
		t.Errorf("a node's page does not update when the controller answers again")
	}
}

// One daemon whose cache cannot be listed dates the page by its oldest
// reading and its own cache figures; its disk and load are current.
func TestNodesStaleCacheReading(t *testing.T) {
	old := testNow.Add(-3 * time.Hour)
	daemons := live{
		{Name: "box-buildkit-aaaaa", Connected: true, InspectedAt: testNow, Inspection: collector.Inspection{Node: "node-1", Usage: collector.Usage{Total: 1 << 30}}},
		{Name: "box-buildkit-zzzzz", Connected: true, InspectedAt: old, InspectErr: "listing the cache: message too large",
			Inspection: collector.Inspection{Node: "node-2", Usage: collector.Usage{Total: 2 << 30}}},
	}
	ts, _ := emptyConsole(t, testOpts{Live: daemons})
	at := stampOf(old, zone(httptest.NewRequest("GET", "/", nil)), "15:04:05").Text
	list := get(t, ts, "/nodes")
	body := html.UnescapeString(list.body)
	for _, want := range []string{"the oldest reading at " + at, "zzzzz on node-2 (cache figures from " + at + ")",
		"the cache figures come back once the daemon's cache can be listed again; disk and load are read each minute"} {
		if !strings.Contains(body, want) {
			t.Errorf("the Nodes page lacks %q", want)
		}
	}
	if one := html.UnescapeString(get(t, ts, "/nodes/box-buildkit-zzzzz").body); !strings.Contains(one, "cache figures from "+at) {
		t.Errorf("the node's page does not date its cache figures")
	}
}

// A job without builds is shown running on its pipeline's card only while
// the job page would say so: a pod's word counts for podTrust only.
func TestPipelineCardRunningBoundedByPodTrust(t *testing.T) {
	testLogs = &lockedBuffer{}
	srv, err := New(Empty{}, testLogger(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv.now = func() time.Time { return testNow }
	for _, c := range []struct {
		ago     time.Duration
		running bool
	}{{48 * time.Hour, false}, {10 * time.Minute, true}} {
		start := testNow.Add(-c.ago)
		p := store.PipeRow{Instance: "0a1b2c", ID: 7, Created: start.Add(-time.Minute)}
		jobs := []store.JobInfo{{ID: 70, Name: "lint", Stage: "test", Started: start, PodCreated: start, PodRunning: start}}
		card := srv.pipeCard(p, jobs, nil, time.UTC)
		got := false
		for _, l := range card.Lanes {
			for _, j := range l.Jobs {
				got = got || j.Class == "jw-running"
			}
		}
		if got != c.running {
			t.Errorf("a pod running for %v: shown running %v", c.ago, got)
		}
	}
}

// The build list offers older builds only when there is one more than a
// page.
func TestBuildListOlderOnlyWhenOlder(t *testing.T) {
	older := regexp.MustCompile(`href="(/builds\?before=\d+&amp;bid=\d+)"`)
	for _, n := range []int{pageSize, pageSize + 1} {
		ts, st := emptyConsole(t, testOpts{})
		for i := 0; i < n; i++ {
			savePipeline(t, st, uint64(700+i), testNow.Add(-time.Duration(i+1)*time.Minute))
		}
		first := get(t, ts, "/builds")
		if got := strings.Count(first.body, `class="brow`); got != min(n, pageSize) {
			t.Errorf("%d builds: %d on the first page", n, got)
		}
		m := older.FindStringSubmatch(first.body)
		if (m != nil) != (n > pageSize) {
			t.Errorf("%d builds: Older link %v", n, m)
			continue
		}
		if m != nil {
			if next := get(t, ts, html.UnescapeString(m[1])); strings.Count(next.body, `class="brow`) != 1 {
				t.Errorf("%d builds: the Older page lists %d", n, strings.Count(next.body, `class="brow`))
			}
		}
	}
}

// endlessBuilds answers every page of the build list with a full page of
// another image's builds, up to 100 reads.
type endlessBuilds struct {
	Backend
	reads atomic.Int64
}

func (b *endlessBuilds) Builds(ctx context.Context, q store.BuildQuery) ([]store.BuildRow, error) {
	if q.Limit != pageSize+1 {
		return b.Backend.Builds(ctx, q)
	}
	n := b.reads.Add(1)
	if n > 100 {
		return nil, nil
	}
	before := q.Before
	if before.IsZero() {
		before = testNow
	}
	rows := make([]store.BuildRow, pageSize+1)
	for i := range rows {
		s := before.Add(-time.Duration(i+1) * time.Second)
		rows[i] = store.BuildRow{ID: 1_000_000 + n*1000 + int64(i), Images: []string{"registry.example.com/other"}, Started: s, Completed: s.Add(time.Second)}
	}
	return rows, nil
}

// Picking the second build of a pair without a target reads on for builds
// of its image, at most maxReads pages.
func TestBuildListReadOnIsBounded(t *testing.T) {
	st := openStore(t)
	inst := history.InstancePrefix("https://gitlab.example.com/")
	start := testNow.Add(-time.Hour)
	if err := st.SaveJob(ctx, store.Job{Instance: inst, JobID: 9100, PipelineID: 910, ProjectID: 42, ProjectPath: "group/app", Name: "b", Stage: "build",
		Ref: "main", SHA: "0123456789abcdef0123456789abcdef01234567", URL: "https://gitlab.example.com/group/app/-/jobs/9100"}); err != nil {
		t.Fatal(err)
	}
	id, err := st.SaveBuild(ctx, store.Build{Generation: 1, Build: &history.Build{Ref: "anchor", Daemon: "bk-daemon-aaaaa", Started: start, Completed: start.Add(time.Second),
		Images:      []string{"registry.example.com/app"},
		Attribution: history.Attribution{State: history.Attributed, Instance: inst, Pipeline: 910, Job: 9100},
		Steps:       []history.Step{{Key: "[1/1]", Name: "RUN make", State: history.Built, Started: start, Completed: start.Add(time.Second), Line: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	b := &endlessBuilds{Backend: &control.Local{Store: st}}
	ts := serveBackend(t, b)
	p := get(t, ts, fmt.Sprintf("/builds?pick=1&a=%d", id))
	if p.status != http.StatusOK {
		t.Fatalf("status %d", p.status)
	}
	// It reads on (no build of the image on the first page), and stops.
	if n := b.reads.Load(); n < 2 || n > 20 {
		t.Errorf("%d reads of the build list for one page", n)
	}
}

// slowPages holds every build page read until released, and counts the
// reads at once.
type slowPages struct {
	Backend
	in, most atomic.Int64
	release  chan struct{}
}

func (b *slowPages) BuildPage(ctx context.Context, id int64) (*store.BuildPage, error) {
	n := b.in.Add(1)
	for m := b.most.Load(); n > m && !b.most.CompareAndSwap(m, n); m = b.most.Load() {
	}
	<-b.release
	b.in.Add(-1)
	return b.Backend.BuildPage(ctx, id)
}

// At most MaxHeavy build pages are read at once; the others wait.
func TestHeavyReadsBounded(t *testing.T) {
	st := openStore(t)
	savePipeline(t, st, 600, testNow.Add(-time.Hour))
	b := &slowPages{Backend: &control.Local{Store: st}, release: make(chan struct{})}
	ts := serveBackend(t, b)
	var wg sync.WaitGroup
	for i := 0; i < 2*MaxHeavy; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := ts.Client().Get(ts.URL + "/builds/1")
			if err == nil {
				res.Body.Close()
			}
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for b.in.Load() < MaxHeavy && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond) // time for more to come in, were they let
	most := b.most.Load()
	close(b.release)
	wg.Wait()
	if most != MaxHeavy {
		t.Errorf("%d build pages read at once, want %d", most, MaxHeavy)
	}
}

// The gate's refusal of the JSON API to someone who is not an admin is
// not kept by any cache, as the API's own answers are not.
func TestGateRefusalNotStored(t *testing.T) {
	testLogs = &lockedBuffer{}
	srv, err := New(Empty{}, testLogger(), Options{Auth: gateSigner{}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	req := httptest.NewRequest("GET", "/api/v1/builds", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookie, Value: "x"})
	rec := httptest.NewRecorder()
	srv.Gate(mux).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("status %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

// A step whose Dockerfile line is the same but whose source changed is
// worded as such, never as a changed Dockerfile.
func TestSourceChangeWording(t *testing.T) {
	b := &history.Build{Target: "api", Started: testNow, Completed: testNow.Add(time.Second)}
	r := history.Reason{Kind: history.StepChanged, Step: history.Step{Name: "COPY . /src", Line: 3},
		Old: "git://gitlab.example.com/group/lib.git#aaaa", New: "git://gitlab.example.com/group/lib.git#bbbb"}
	v := verdictFor(b, r, "", 2, 0, nil)
	if v.Kicker != "Source changed" || !strings.Contains(string(v.Title), "the step&#39;s source changed") && !strings.Contains(string(v.Title), "the step's source changed") {
		t.Errorf("verdict: %q %q", v.Kicker, v.Title)
	}
	if !strings.Contains(string(v.Detail), "lib.git#aaaa") || !strings.Contains(string(v.Detail), "lib.git#bbbb") {
		t.Errorf("the addresses are not shown: %q", v.Detail)
	}
	// A changed line keeps its words.
	r.Old, r.New = "COPY . /src", "COPY . /app"
	if v := verdictFor(b, r, "", 2, 0, nil); v.Kicker != "Dockerfile changed" {
		t.Errorf("a changed line: %q", v.Kicker)
	}
}
