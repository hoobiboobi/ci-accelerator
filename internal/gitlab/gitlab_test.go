package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

const page1 = `[{"id":11,"name":"build-api","stage":"build","status":"success","web_url":"https://gl/x/-/jobs/11",
 "created_at":"2026-10-20T21:04:40.739Z","started_at":"2026-10-20T21:04:46.595Z","finished_at":"2026-10-20T21:04:55.373Z",
 "duration":8.777798,"queued_duration":5.380261,"runner":{"description":"test-runner"},
 "pipeline":{"id":7,"project_id":42,"status":"running","web_url":"https://gl/x/-/pipelines/7"}}]`
const page2 = `[{"id":12,"name":"deploy","stage":"deploy","status":"pending","web_url":"https://gl/x/-/jobs/12",
 "created_at":"2026-10-20T21:04:40Z","started_at":null,"finished_at":null,"duration":null,"queued_duration":null,"runner":null,
 "pipeline":{"id":7,"project_id":42,"status":"running","web_url":"https://gl/x/-/pipelines/7"}}]`

func TestPipeline(t *testing.T) {
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		if r.URL.Path != "/api/v4/projects/42/pipelines/7/jobs" || r.URL.Query().Get("per_page") != "100" || r.URL.Query().Get("include_retried") != "true" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, page1)
			return
		}
		fmt.Fprint(w, page2)
	}))
	defer srv.Close()
	c := New([]string{srv.URL + "/"})
	inst := history.InstancePrefix(srv.URL + "/")
	p, err := c.Pipeline(context.Background(), Token("glpat-secret"), inst, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Jobs) != 2 || p.Status != "running" || p.WebURL != "https://gl/x/-/pipelines/7" || p.Instance != inst || p.Read.IsZero() {
		t.Fatalf("%+v", p)
	}
	j := p.Jobs[0]
	if j.ID != 11 || j.Name != "build-api" || j.Runner != "test-runner" || j.Duration != 8777798*time.Microsecond ||
		j.Waited.Round(time.Millisecond) != 5380*time.Millisecond || j.Started.Format(time.RFC3339Nano) != "2026-10-20T21:04:46.595Z" ||
		j.Finished.IsZero() || j.Queued.IsZero() {
		t.Errorf("job %+v", j)
	}
	if k := p.Jobs[1]; !k.Started.IsZero() || k.Duration != 0 || k.Runner != "" || k.Status != "pending" {
		t.Errorf("pending job %+v", k)
	}
	if len(tokens) != 2 || tokens[0] != "Bearer glpat-secret" {
		t.Errorf("tokens %v", tokens)
	}
	if _, err := c.Pipeline(context.Background(), Token("t"), "abcdef", 42, 7); err == nil {
		t.Error("unknown instance read")
	}
}

func TestRateLimitAndErrors(t *testing.T) {
	var status atomic.Int32
	status.Store(429)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch status.Load() {
		case 429:
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(429)
		case 401:
			w.WriteHeader(401)
		default:
			fmt.Fprint(w, "not json")
		}
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	inst := history.InstancePrefix(srv.URL)
	_, err := c.Pipeline(context.Background(), Token("t"), inst, 1, 2)
	rl, ok := err.(*RateLimited)
	if !ok || time.Until(rl.Until) < 25*time.Second || time.Until(rl.Until) > 31*time.Second {
		t.Errorf("429: %v", err)
	}
	status.Store(401)
	if _, err := c.Pipeline(context.Background(), Token("t"), inst, 1, 2); !errors.Is(err, ErrRefused) {
		t.Errorf("401: %v", err)
	}
	status.Store(200)
	if _, err := c.Pipeline(context.Background(), Token("t"), inst, 1, 2); err == nil {
		t.Error("bad JSON accepted")
	}
	now := time.Unix(1000, 0)
	h := http.Header{}
	h.Set("RateLimit-Reset", "1090")
	if got := retryAfter(h, now); !got.Equal(time.Unix(1090, 0)) {
		t.Errorf("reset: %v", got)
	}
	if got := retryAfter(http.Header{}, now); got.Sub(now) != time.Minute {
		t.Errorf("default: %v", got)
	}
}

type saver struct {
	mu    sync.Mutex
	saved []store.APIPipeline
}

func (s *saver) SaveAPIPipeline(_ context.Context, p store.APIPipeline) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saved = append(s.saved, p)
	return nil
}

func (s *saver) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.saved)
}

// Pages ask for a pipeline as often as they render; GitLab is asked once
// per pipeline at a time, a running one again only after Every, a
// finished one never, and nothing while it rate limits.
func TestFetcher(t *testing.T) {
	var calls atomic.Int32
	limited := atomic.Bool{}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-release
		if limited.Load() {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, page1)
	}))
	defer srv.Close()
	sv := &saver{}
	f := &Fetcher{Client: New([]string{srv.URL}), Store: sv, Every: time.Hour}
	inst := history.InstancePrefix(srv.URL)
	for i := 0; i < 5; i++ {
		f.Ensure(Token("t"), inst, 42, 7, "", time.Time{}, 0)
	}
	close(release)
	f.Ensure(Token("t"), inst, 42, 7, "", time.Time{}, 2*time.Second)
	if calls.Load() != 1 || sv.count() != 1 {
		t.Fatalf("%d calls, %d saved", calls.Load(), sv.count())
	}
	f.Ensure(Token("t"), inst, 42, 7, "running", time.Now(), time.Second)
	f.Ensure(Token("t"), inst, 42, 8, "success", time.Now(), time.Second)
	f.Ensure(Token("t"), inst, 42, 8, "success", time.Now().Add(-2*time.Hour), time.Second) // finished and read: never again
	f.Ensure(Token("t"), "abcdef", 42, 9, "", time.Time{}, time.Second)
	f.Ensure(Token("t"), inst, 0, 9, "", time.Time{}, time.Second)
	if calls.Load() != 1 {
		t.Fatalf("read again: %d calls", calls.Load())
	}
	// A finished pipeline that was never read is read once.
	f.Ensure(Token("t"), inst, 42, 10, "success", time.Time{}, 2*time.Second)
	if calls.Load() != 2 {
		t.Fatalf("finished, unread: %d calls", calls.Load())
	}
	limited.Store(true)
	f.Ensure(Token("t"), inst, 42, 11, "", time.Time{}, 2*time.Second)
	until, err := f.Status()
	if err == nil || time.Until(until) < 50*time.Second {
		t.Fatalf("rate limit not kept: %v %v", until, err)
	}
	f.Ensure(Token("t"), inst, 42, 12, "", time.Time{}, time.Second)
	if calls.Load() != 3 {
		t.Errorf("read while rate limited: %d calls", calls.Load())
	}
	var nilF *Fetcher
	nilF.Ensure(Token("t"), inst, 42, 7, "", time.Time{}, time.Second)
}

// A page showing a running pipeline asks again before the interval is
// over: the read is done when it is due, without another ask.
func TestFetcherReadsAgainWhenDue(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, page1)
	}))
	defer srv.Close()
	sv := &saver{}
	f := &Fetcher{Client: New([]string{srv.URL}), Store: sv, Every: 150 * time.Millisecond}
	inst := history.InstancePrefix(srv.URL)
	f.Ensure(Token("t"), inst, 42, 7, "", time.Time{}, time.Second)
	f.Ensure(Token("t"), inst, 42, 7, "running", time.Now(), 0)
	f.Ensure(Token("t"), inst, 42, 7, "running", time.Now(), 0)
	if calls.Load() != 1 {
		t.Fatalf("read again at once: %d calls", calls.Load())
	}
	time.Sleep(400 * time.Millisecond)
	if calls.Load() != 2 {
		t.Errorf("%d calls after the interval, want 2 (one later read for the two asks)", calls.Load())
	}
}

func TestJobLog(t *testing.T) {
	var asked string
	log := "Running with gitlab-runner 19.4.1\n" +
		"section_start:1761071234:prepare_script[collapsed=true]\r\x1b[0K\x1b[0K\x1b[36;1mPreparing environment\x1b[0;m\n" +
		"section_end:1761071235:prepare_script\r\x1b[0K\n" +
		"Downloading 10%\rDownloading 55%\rDownloading 100%\n" +
		"\x1b[32;1m$ helm upgrade app ./chart\x1b[0;m\r\n" +
		"deployed\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Path + " " + r.Header.Get("Authorization")
		fmt.Fprint(w, log)
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	inst := history.InstancePrefix(srv.URL)
	got, err := c.JobLogPart(context.Background(), Token("tok"), inst, 42, 77, LogQuery{Lines: 3})
	if err != nil {
		t.Fatal(err)
	}
	if asked != "/api/v4/projects/42/jobs/77/trace Bearer tok" {
		t.Errorf("asked %q", asked)
	}
	if want := "Downloading 100%\n$ helm upgrade app ./chart\ndeployed\n"; got.Text != want {
		t.Errorf("tail:\n%q\nwant\n%q", got.Text, want)
	}
	all, _ := c.JobLogPart(context.Background(), Token("tok"), inst, 42, 77, LogQuery{Lines: 100})
	if all.Start != 0 || strings.Contains(all.Text, "section_") || strings.Contains(all.Text, "\x1b") || strings.Contains(all.Text, "\r") || !strings.Contains(all.Text, "Preparing environment") {
		t.Errorf("not cleaned: %+v", all)
	}
	// A runner 19 log: time, stream and O/E before every line, "+"
	// continuing the line before.
	stamped := "2026-10-21T20:44:53.051755Z 01O lint found problems\n" +
		"2026-10-21T20:44:56.083603Z 00O+\n" +
		"2026-10-21T20:44:56.083612Z 00O+Cleaning up project directory\n" +
		"2026-10-21T20:44:56.558351Z 01O \n" +
		"2026-10-21T20:44:56.558416Z 00O \n" +
		"2026-10-21T20:44:56.590669Z 00E ERROR: Job failed: exit code 1\n"
	if got, want := CleanLog(stamped), "lint found problems\nCleaning up project directory\n\nERROR: Job failed: exit code 1\n"; got != want {
		t.Errorf("stamped log:\n%q\nwant\n%q", got, want)
	}
	f := &Fetcher{Client: c}
	f.until = map[string]time.Time{"token": time.Now().Add(time.Minute)}
	if _, err := f.JobLogPart(context.Background(), Token("tok"), inst, 42, 77, LogQuery{Lines: 10}); err == nil {
		t.Error("read while rate limited")
	}
	if _, err := (&Fetcher{Client: c}).JobLogPart(context.Background(), Token("tok"), inst, 0, 77, LogQuery{Lines: 10}); err == nil {
		t.Error("read without a project")
	}
}

// A long log read in parts, from its end back to its start, is the whole
// log, whether GitLab serves byte ranges (only the parts travel) or always
// the whole log; lines added later come as one more part, or, when too many
// are new, as the end of the log with a gap.
func TestJobLogParts(t *testing.T) {
	var mu sync.Mutex
	var b strings.Builder
	for i := 1; i <= 6000; i++ {
		fmt.Fprintf(&b, "2026-10-21T20:44:53.051755Z 01O line %05d of the job log\n", i)
	}
	logText := b.String()
	clean := func(from, to int) string { // lines from..to, 1-based, as cleaned
		var s strings.Builder
		for i := from; i <= to; i++ {
			fmt.Fprintf(&s, "line %05d of the job log\n", i)
		}
		return s.String()
	}
	for _, ranges := range []bool{true, false} {
		var bytesSent atomic.Int64
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			body := logText
			mu.Unlock()
			if !ranges || r.Header.Get("Range") == "" {
				bytesSent.Add(int64(len(body)))
				fmt.Fprint(w, body)
				return
			}
			http.ServeContent(w, r, "trace", time.Time{}, &countingReader{strings.NewReader(body), &bytesSent})
		}))
		c := New([]string{srv.URL})
		inst := history.InstancePrefix(srv.URL)
		read := func(q LogQuery) LogPart {
			t.Helper()
			p, err := c.JobLogPart(context.Background(), Token("tok"), inst, 42, 77, q)
			if err != nil {
				t.Fatalf("ranges=%v %+v: %v", ranges, q, err)
			}
			return p
		}
		end := read(LogQuery{Lines: 500})
		if end.Text != clean(5501, 6000) || end.End != end.Size || end.Size != int64(len(logText)) || end.Gap {
			t.Fatalf("ranges=%v: the end: %d bytes from %d, %q...", ranges, len(end.Text), end.Start, end.Text[:min(40, len(end.Text))])
		}
		whole, p := end.Text, end
		for p.Start > 0 {
			p = read(LogQuery{Lines: 500, Before: p.Start})
			whole = p.Text + whole
		}
		if whole != clean(1, 6000) {
			t.Errorf("ranges=%v: the parts do not make the log (%d of %d bytes)", ranges, len(whole), len(clean(1, 6000)))
		}
		if ranges && bytesSent.Load() > 3*int64(len(logText)) {
			t.Errorf("ranges: %d bytes travelled for a %d byte log read in parts", bytesSent.Load(), len(logText))
		}
		// Nothing new yet; then two lines, one still being written.
		if p := read(LogQuery{Lines: 500, After: end.End, Since: true}); p.Text != "" || p.End != end.End {
			t.Errorf("ranges=%v: nothing new: %+v", ranges, p)
		}
		mu.Lock()
		logText += "2026-10-21T20:44:54Z 01O line 06001 of the job log\n2026-10-21T20:44:54Z 01O line 06002 of the jo"
		mu.Unlock()
		more := read(LogQuery{Lines: 500, After: end.End, Since: true})
		if more.Text != clean(6001, 6001) || more.Start != end.End || more.Gap {
			t.Errorf("ranges=%v: new lines: %+v", ranges, more)
		}
		// Far more than one part is new: the end, marked as a gap.
		mu.Lock()
		logText = logText[:len(logText)-len("2026-10-21T20:44:54Z 01O line 06002 of the jo")]
		var big strings.Builder
		for i := 6002; i <= 6002+120000; i++ {
			fmt.Fprintf(&big, "2026-10-21T20:44:55Z 01O line %06d of the job log\n", i)
		}
		logText += big.String()
		mu.Unlock()
		gap := read(LogQuery{Lines: 3, After: more.End, Since: true})
		if !gap.Gap || gap.End != gap.Size || !strings.HasSuffix(gap.Text, "line 126002 of the job log\n") || strings.Count(gap.Text, "\n") != 3 {
			t.Errorf("ranges=%v: a gap: %+v", ranges, gap)
		}
		mu.Lock()
		logText = b.String()
		mu.Unlock()
		srv.Close()
	}
}

type countingReader struct {
	*strings.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.Reader.Read(p)
	c.n.Add(int64(k))
	return k, err
}

func TestAllowFailureRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[{"id":1,"name":"flaky","status":"failed","allow_failure":true,"pipeline":{"id":7,"status":"success"}}]`)
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	p, err := c.Pipeline(context.Background(), Token("t"), history.InstancePrefix(srv.URL), 42, 7)
	if err != nil || len(p.Jobs) != 1 || !p.Jobs[0].AllowFailure {
		t.Errorf("%+v %v", p, err)
	}
}

// person is a credential whose token GitLab may refuse once; Renew gives
// the next one.
type person struct {
	who     string
	tokens  []string
	renewed atomic.Int32
}

func (p *person) Token(context.Context) (string, error) { return p.tokens[0], nil }
func (p *person) Renew(_ context.Context, refused string) (string, error) {
	n := p.renewed.Add(1)
	if int(n) >= len(p.tokens) {
		return "", ErrRefused
	}
	return p.tokens[n], nil
}
func (p *person) Who() string { return p.who }

func TestRenewOnRefusal(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"id":7,"username":"alice","name":"Alice"}`)
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	inst := history.InstancePrefix(srv.URL)
	u, err := c.User(context.Background(), &person{who: "alice", tokens: []string{"stale", "fresh"}}, inst)
	if err != nil || u.ID != 7 || u.Username != "alice" || u.Name != "Alice" {
		t.Fatalf("user %+v, %v", u, err)
	}
	if len(seen) != 2 || seen[0] != "Bearer stale" || seen[1] != "Bearer fresh" {
		t.Errorf("tokens sent %v", seen)
	}
	// Refused again after the renewal: refused, and asked only twice.
	seen = nil
	if _, err := c.User(context.Background(), &person{who: "bob", tokens: []string{"stale", "also-stale", "x"}}, inst); !errors.Is(err, ErrRefused) {
		t.Errorf("twice refused: %v", err)
	}
	if len(seen) != 2 {
		t.Errorf("asked %d times", len(seen))
	}
	if _, err := c.User(context.Background(), nil, inst); !errors.Is(err, ErrRefused) {
		t.Errorf("no credential: %v", err)
	}
}

func TestCanReadJobs(t *testing.T) {
	codes := map[string]int{"/api/v4/projects/1/jobs": 200, "/api/v4/projects/2/jobs": 403, "/api/v4/projects/3/jobs": 404,
		"/api/v4/projects/4/jobs": 429, "/api/v4/projects/5/jobs": 500}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "1" {
			t.Errorf("asked %s", r.URL)
		}
		if codes[r.URL.Path] == 429 {
			w.Header().Set("Retry-After", "5")
		}
		w.WriteHeader(codes[r.URL.Path])
		fmt.Fprint(w, "[]")
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	inst := history.InstancePrefix(srv.URL)
	for project, want := range map[uint64]bool{1: true, 2: false, 3: false} {
		if ok, err := c.CanReadJobs(context.Background(), Token("t"), inst, project); ok != want || err != nil {
			t.Errorf("project %d: %v, %v", project, ok, err)
		}
	}
	var rl *RateLimited
	if ok, err := c.CanReadJobs(context.Background(), Token("t"), inst, 4); ok || !errors.As(err, &rl) {
		t.Errorf("429: %v, %v", ok, err)
	}
	if ok, err := c.CanReadJobs(context.Background(), Token("t"), inst, 5); ok || err == nil {
		t.Errorf("500 must be an error, never a yes: %v, %v", ok, err)
	}
	if ok, err := c.CanReadJobs(context.Background(), Token("t"), "abcdef", 1); ok || err != nil {
		t.Errorf("unknown instance: %v, %v", ok, err)
	}
}

func TestRateLimitPerCredential(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer busy" {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, "done\n")
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	inst := history.InstancePrefix(srv.URL)
	f := &Fetcher{Client: c}
	busy, other := &person{who: "busy", tokens: []string{"busy"}}, &person{who: "other", tokens: []string{"ok"}}
	if _, err := f.JobLogPart(context.Background(), busy, inst, 42, 1, LogQuery{Lines: 10}); err == nil {
		t.Fatal("429 not reported")
	}
	if _, err := f.JobLogPart(context.Background(), other, inst, 42, 1, LogQuery{Lines: 10}); err != nil {
		t.Errorf("another person waits for the busy one: %v", err)
	}
	var rl *RateLimited
	if _, err := f.JobLogPart(context.Background(), busy, inst, 42, 1, LogQuery{Lines: 10}); !errors.As(err, &rl) {
		t.Errorf("busy person asked GitLab again: %v", err)
	}
	if until, _ := f.Status(); time.Until(until) < 50*time.Second {
		t.Errorf("status until %v", until)
	}
}

// readTail keeps the end of a log much larger than what it keeps, from
// the start of a line.

// Empty lines are shown at most once in a row, and a runner's empty
// continuation pieces never: that depends on the line before, so a part
// must know the line before it. Parts of every size join into exactly what
// the whole log shows.
func TestJobLogPartsKeepEmptyLines(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 400; i++ {
		switch {
		case i%37 == 0:
			b.WriteString("2026-10-21T20:44:56Z 00O+\n") // an empty continuation piece
		case i%11 == 0:
			b.WriteString("\n\n\n") // a run of empty lines
		case i%7 == 0:
			b.WriteString("2026-10-21T20:44:56Z 01O \r\n") // an empty stamped line, CRLF
		default:
			fmt.Fprintf(&b, "2026-10-21T20:44:56Z 01O line %03d\n", i)
		}
	}
	log := b.String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "trace", time.Time{}, strings.NewReader(log))
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	inst := history.InstancePrefix(srv.URL)
	read := func(q LogQuery) LogPart {
		t.Helper()
		p, err := c.JobLogPart(context.Background(), Token("tok"), inst, 42, 77, q)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	whole := read(LogQuery{Lines: 1 << 20})
	if whole.Start != 0 || whole.Text != CleanLog(log) {
		t.Fatalf("the whole log as a part differs from CleanLog:\n%q\nvs\n%q", whole.Text[:120], CleanLog(log)[:120])
	}
	for _, n := range []int{1, 2, 3, 5, 13, 50} {
		p := read(LogQuery{Lines: n})
		joined := p.Text
		for p.Start > 0 {
			p = read(LogQuery{Lines: n, Before: p.Start})
			joined = p.Text + joined
		}
		if joined != whole.Text {
			t.Errorf("parts of %d lines: %d bytes joined, %d whole", n, len(joined), len(whole.Text))
		}
	}
	// New lines after a part that ends with an empty line.
	cut := strings.LastIndex(log[:len(log)-1], "\n\n\n") + 2
	first := read(LogQuery{Lines: 1 << 20, Before: int64(cut)})
	more := read(LogQuery{Lines: 1 << 20, After: int64(cut), Since: true})
	if first.Text+more.Text != whole.Text {
		t.Errorf("a part and the lines after it: %d + %d bytes, whole %d", len(first.Text), len(more.Text), len(whole.Text))
	}
}

// gitlab.com answers a range past the log's end with 416 and, when the
// request accepts gzip, no Content-Range (seen 2026-10-28): nothing new,
// not "too much is new".
func TestJobLogNothingNewWithoutContentRange(t *testing.T) {
	const logText = "2026-10-28T00:00:00Z 01O one\n2026-10-28T00:00:01Z 01O two\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "bytes="+strconv.Itoa(len(logText))+"-" {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			fmt.Fprint(w, `<?xml version='1.0' encoding='UTF-8'?><Error><Code>InvalidRange</Code></Error>`)
			return
		}
		http.ServeContent(w, r, "trace", time.Time{}, strings.NewReader(logText))
	}))
	defer srv.Close()
	c := New([]string{srv.URL})
	p, err := c.JobLogPart(context.Background(), Token("tok"), history.InstancePrefix(srv.URL), 42, 77,
		LogQuery{Lines: 500, After: int64(len(logText)), Since: true})
	if err != nil || p.Gap || p.Text != "" || p.End != int64(len(logText)) || p.Size != int64(len(logText)) {
		t.Errorf("nothing new: %+v, %v", p, err)
	}
}
