package control

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// FuzzMemberAnswers dials a member's controller as the hub does (a Client
// with Member set) to an in-process server whose configuration and call
// answers are fuzzed: their bodies (generated from the result types the
// client expects, with control characters and bidi overrides in every
// string, or raw bytes), status codes, headers, gzip encoding, a lying
// Content-Length, answers that stall after the headers and answers without
// end. Each input calls one of the client's reads the hub's pages use.
//
// Invariants:
//   - no panic;
//   - the client reads at most MaxMemberAnswer bytes of one answer (counted
//     after the transport's gzip decoding, so a gzip bomb counts in full),
//     and the server could send at most that plus the socket buffers;
//   - no string reachable from a decoded result (struct fields, slices,
//     map values and map keys, pointers) and none of the dialled
//     configuration holds a rune the hub must drop (C0 except tab and
//     newline, DEL, C1, the bidi embeddings, overrides, isolates and
//     marks); neither does an error's text, which the pages show;
//   - no request the client sends carries the session the page's context
//     holds: no X-CI-Session header, no Cookie, the value in no header;
//   - each call ends within its context's deadline (plus a grace for the
//     scheduler): a member that stalls after its headers does not hold it.
func FuzzMemberAnswers(f *testing.F) {
	// method, mode, status, header, data (drives the generator), raw.
	f.Add(uint8(0), uint8(0), uint16(200), "application/json", []byte("plain"), []byte(nil))
	// A bidi override and terminal escapes in a pipeline's strings.
	f.Add(uint8(0), uint8(0), uint16(0), "", []byte{3, 1, 4, 1, 5, 9, 2, 6, 5, 3, 5, 8, 9, 7, 9}, []byte(nil))
	// Raw answers that are not what the client expects.
	f.Add(uint8(2), uint8(0x01), uint16(0), "", []byte(nil), []byte(`{"r":[{"Name":"\u202eexe.txt","Stage":"\u001b[2J"}]}`))
	f.Add(uint8(2), uint8(0x01), uint16(0), "", []byte(nil), []byte(`{"r":[null,null]}`))
	f.Add(uint8(1), uint8(0x01), uint16(0), "", []byte(nil), []byte(`{"r":[[1,2,3]]}`))
	f.Add(uint8(5), uint8(0x01), uint16(0), "", []byte(nil), []byte(`{"r":[{"\u202ekey":["\u0007v"]}]}`))
	// An error with control characters in its message, and the 512 bytes
	// of a failing status.
	f.Add(uint8(0), uint8(0x05), uint16(0), "", []byte(nil), []byte(`{"r":[null],"err":{"msg":"\u202edenied\u001b[0m","kind":"denied"}}`))
	f.Add(uint8(0), uint8(0x01), uint16(500), "", []byte(nil), []byte("broken \x1b[31m\u202e"))
	f.Add(uint8(3), uint8(0x01), uint16(409), "", []byte(nil), []byte("other setup"))
	// A configuration with bidi text in what the hub shows.
	f.Add(uint8(4), uint8(0x02), uint16(0), "", []byte(nil), []byte(`{"Release":"\u202ebox","PublicURL":"https://x\u0000","Instances":[{"Instance":"0a\u2066","Host":"h\u009b"}],"Known":["0a1b2c"]}`))
	// Gzip, a lying Content-Length, a wrong number of results.
	f.Add(uint8(6), uint8(0x80), uint16(0), "", []byte{7, 7, 7}, []byte(nil))
	f.Add(uint8(7), uint8(0x40), uint16(0), "", []byte{1, 2, 3}, []byte(nil))
	f.Add(uint8(8), uint8(0x08), uint16(0), "", []byte{9}, []byte(nil))
	// A member that stalls after its headers (audit: no deadline ended it).
	f.Add(uint8(0), uint8(0x10), uint16(0), "", []byte{1}, []byte(nil))
	f.Add(uint8(9), uint8(0x11), uint16(0), "", []byte(nil), []byte(`{"r":[`))
	// Answers without end (audit: the hub held gigabytes), plain and gzip,
	// and a configuration without end.
	f.Add(uint8(10), uint8(0xEE), uint16(0), "", []byte(nil), []byte(nil))
	f.Add(uint8(11), uint8(0xEF), uint16(0), "", []byte(nil), []byte(nil))
	f.Add(uint8(0), uint8(0xED), uint16(0), "", []byte(nil), []byte(nil))
	// The change stream.
	f.Add(uint8(fzEvents), uint8(0x01), uint16(0), "", []byte(nil), []byte("data: [\"a\",\"\"]\n\ndata: x\n"))

	var cur atomic.Pointer[fzState]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := cur.Load()
		if st == nil {
			http.Error(w, "no input", http.StatusServiceUnavailable)
			return
		}
		st.serve(w, r)
	}))
	defer srv.Close()
	tr := &http.Transport{}
	defer tr.CloseIdleConnections()
	fzEndlessGzip() // once, before the inputs' clocks run

	f.Fuzz(func(t *testing.T, method, mode uint8, status uint16, header string, data, raw []byte) {
		m := fzMethods[int(method)%len(fzMethods)]
		// The high range (0xE0 and up) names whole scenarios; the bits are
		// flags below it.
		bits := mode
		if mode >= 0xE0 {
			bits = 0
		}
		r := &fzReader{b: data, budget: 400}
		st := &fzState{mode: mode, header: header, quit: make(chan struct{})}
		st.code = http.StatusOK
		if status >= 200 && status <= 599 {
			st.code = int(status)
		}
		// The configuration: generated unless raw.
		cfg := fzGen(r, reflect.TypeFor[Config](), 0)
		cfgv := cfg.Interface().(Config)
		cfgv.Known = append(cfgv.Known, "0a1b2c")
		st.config, _ = json.Marshal(cfgv)
		if bits&0x02 != 0 {
			st.config = raw
		}
		// The call's answer.
		switch {
		case m.name == "events":
			st.body = fzEventsBody(r)
		case bits&0x01 != 0:
			st.body = raw
		default:
			st.body = fzAnswer(r, m.outs, bits)
		}
		cur.Store(st)
		defer func() {
			close(st.quit)
			cur.Store(nil)
		}()

		hc := &http.Client{Transport: &fzCounting{base: tr, st: st}, Timeout: time.Minute}
		c := &Client{base: srv.URL, http: hc, stream: hc, member: true, reads: map[string]readAnswer{},
			log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		base := auth.WithCookie(context.Background(), fzSession)
		base = auth.With(base, &auth.Viewer{Instance: "0a1b2c", Host: "gitlab.example", UserID: 5, Username: "p"})

		// Dial's first step, once (Dial itself tries again until its
		// context ends).
		dctx, dcancel := context.WithTimeout(base, fzDeadline(mode))
		start := time.Now()
		setup, err := c.config(dctx)
		dcancel()
		fzInTime(t, "config", start, fzDeadline(mode))
		st.checkRequests(t)
		st.checkRead(t)
		if err != nil {
			fzCleanText(t, "the configuration's error", err.Error())
			return
		}
		fzCleanAll(t, "the configuration", reflect.ValueOf(c.cfg))
		c.setup = setup
		if c.setup == "" {
			c.setup = c.cfg.Setup()
		}
		c.known = map[string]bool{}
		for _, k := range c.cfg.Known {
			c.known[k] = true
		}

		limit := fzDeadline(mode)
		if m.own {
			limit = 5 * time.Second // the method's own deadline
			if mode&0x10 != 0 && mode < 0xE0 {
				return // a stall would cost the whole 5 s
			}
		}
		ctx, cancel := context.WithTimeout(base, limit)
		defer cancel()
		start = time.Now()
		outs, err := m.call(ctx, c)
		fzInTime(t, m.name, start, limit)
		st.checkRequests(t)
		st.checkRead(t)
		if (mode == 0xEE || mode == 0xEF) && err == nil && m.name != "Daemons" && m.name != "events" {
			t.Fatalf("%s: an answer without end was taken as complete", m.name)
		}
		for i, o := range outs {
			fzCleanAll(t, fmt.Sprintf("%s result %d", m.name, i+1), reflect.ValueOf(o))
		}
		if err != nil {
			fzCleanText(t, m.name+"'s error", err.Error())
		}
	})
}

const fzSession = "HUB-SESSION-COOKIE-fz-7f3a"

// fzDeadline is a call's deadline: short when the member stalls, so that
// the input stays fast and a call that outlives it shows.
func fzDeadline(mode uint8) time.Duration {
	if mode&0x10 != 0 && mode < 0xE0 {
		return 30 * time.Millisecond
	}
	return 20 * time.Second
}

// fzInTime fails when a call outlived its deadline by more than a grace.
func fzInTime(t *testing.T, what string, start time.Time, limit time.Duration) {
	t.Helper()
	// The grace grows with the deadline: a busy machine (all packages
	// tested at once) can spend seconds decoding a large answer; a call
	// with no deadline at all would still run far past it.
	if d := time.Since(start); d > limit+max(3*time.Second, limit) {
		t.Fatalf("%s took %v under a deadline of %v", what, d, limit)
	}
}

// fzState is one input's server side.
type fzState struct {
	mode         uint8
	code         int
	header       string
	config, body []byte
	quit         chan struct{}

	mu      sync.Mutex
	bad     []string // requests that carried the session
	maxRead int64    // the most the client read of one answer
	sent    int64    // the most the server sent of one answer
}

func (st *fzState) serve(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	for k, vs := range r.Header {
		for _, v := range vs {
			if k != SetupHeader && strings.Contains(v, fzSession) {
				st.bad = append(st.bad, r.URL.Path+": header "+k)
			}
		}
	}
	if r.Header.Get(SessionHeader) != "" {
		st.bad = append(st.bad, r.URL.Path+": "+SessionHeader)
	}
	if r.Header.Get("Cookie") != "" {
		st.bad = append(st.bad, r.URL.Path+": Cookie")
	}
	if b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20)); bytes.Contains(b, []byte(fzSession)) {
		st.bad = append(st.bad, r.URL.Path+": body")
	}
	st.mu.Unlock()

	call := strings.HasPrefix(r.URL.Path, "/internal/v1/call/")
	events := r.URL.Path == "/internal/v1/events"
	body, code := st.config, http.StatusOK
	if call || events {
		body, code = st.body, st.code
	}
	if r.URL.Path == "/internal/v1/config" {
		w.Header().Set(SetupHeader, st.header)
		if st.mode&0x20 != 0 && st.mode < 0xE0 {
			code = st.code
		}
	} else if st.header != "" {
		w.Header().Set("Content-Type", st.header)
	}
	switch {
	case st.mode == 0xEE && call, st.mode == 0xED && !call && !events:
		w.WriteHeader(http.StatusOK)
		prefix := `{"r":["`
		if !call {
			prefix = `{"Release":"`
		}
		n, _ := w.Write([]byte(prefix))
		sent := int64(n)
		chunk := bytes.Repeat([]byte("a"), 1<<20)
		for range 2 * MaxMemberAnswer >> 20 {
			n, err := w.Write(chunk)
			sent += int64(n)
			if err != nil {
				break
			}
		}
		st.noteSent(sent)
		return
	case st.mode == 0xEF && call:
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		w.Write(fzEndlessGzip())
		return
	}
	if st.mode >= 0xE0 {
		// The other modes of the high range: plain answers.
		w.WriteHeader(code)
		w.Write(body)
		return
	}
	if st.mode&0x80 != 0 {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write(body)
		zw.Close()
		body = b.Bytes()
		w.Header().Set("Content-Encoding", "gzip")
	}
	if st.mode&0x40 != 0 {
		n := len(body) / 2
		if len(body)%2 == 1 {
			n = len(body) * 2
		}
		w.Header().Set("Content-Length", strconv.Itoa(n))
	}
	w.WriteHeader(code)
	if st.mode&0x10 != 0 && (call || events) {
		// Headers and half the answer, then nothing until the client
		// goes away.
		w.Write(body[:len(body)/2])
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-st.quit:
		}
		return
	}
	w.Write(body)
}

func (st *fzState) noteSent(n int64) {
	st.mu.Lock()
	st.sent = max(st.sent, n)
	st.mu.Unlock()
}

func (st *fzState) checkRequests(t *testing.T) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.bad) > 0 {
		t.Fatalf("a request to the member carried the hub's session: %v", st.bad)
	}
}

// checkRead: what the client read of one answer stays within the bound;
// the server cannot have sent much more than the bound either (socket
// buffers aside).
func (st *fzState) checkRead(t *testing.T) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.maxRead > MaxMemberAnswer {
		t.Fatalf("the client read %d bytes of one answer, over MaxMemberAnswer (%d)", st.maxRead, MaxMemberAnswer)
	}
	if st.sent > MaxMemberAnswer+32<<20 {
		t.Fatalf("the member could send %d bytes of one answer, the bound is %d", st.sent, MaxMemberAnswer)
	}
}

// fzCounting counts what the client reads of every answer's body, after
// the transport decoded it.
type fzCounting struct {
	base http.RoundTripper
	st   *fzState
}

func (c *fzCounting) RoundTrip(r *http.Request) (*http.Response, error) {
	res, err := c.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	res.Body = &fzBody{ReadCloser: res.Body, st: c.st}
	return res, nil
}

type fzBody struct {
	io.ReadCloser
	st *fzState
	n  int64
}

func (b *fzBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	b.st.mu.Lock()
	b.st.maxRead = max(b.st.maxRead, b.n)
	b.st.mu.Unlock()
	return n, err
}

var (
	fzGzipOnce sync.Once
	fzGzipBlob []byte
)

// fzEndlessGzip is a gzip answer that inflates to 80 MiB of one JSON
// string that never ends (a few hundred KiB on the wire).
func fzEndlessGzip() []byte {
	fzGzipOnce.Do(func() {
		var b bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&b, gzip.BestCompression)
		zw.Write([]byte(`{"r":["`))
		chunk := bytes.Repeat([]byte("a"), 1<<20)
		for range 80 {
			zw.Write(chunk)
		}
		zw.Close()
		fzGzipBlob = b.Bytes()
	})
	return fzGzipBlob
}

// fzDropped is the rule the hub applies to a member's strings, written
// out from its documentation independently of cleanRune.
func fzDropped(r rune) bool {
	if r == '\t' || r == '\n' {
		return false
	}
	return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) ||
		(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) ||
		r == 0x200e || r == 0x200f || r == 0x061c
}

func fzCleanText(t *testing.T, where, s string) {
	t.Helper()
	if i := strings.IndexFunc(s, fzDropped); i >= 0 {
		t.Fatalf("%s holds %U, which the hub drops from what a member sends: %q", where, []rune(s[i:])[0], fzClip(s))
	}
}

// fzCleanAll checks every string reachable from v, map keys included.
func fzCleanAll(t *testing.T, where string, v reflect.Value) {
	t.Helper()
	var walk func(v reflect.Value, path string, depth int)
	walk = func(v reflect.Value, path string, depth int) {
		if !v.IsValid() || depth > 100 {
			return
		}
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem(), path, depth+1)
			}
		case reflect.String:
			fzCleanText(t, where+path, v.String())
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).IsExported() {
					walk(v.Field(i), path+"."+v.Type().Field(i).Name, depth+1)
				}
			}
		case reflect.Slice, reflect.Array:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				return
			}
			for i := 0; i < v.Len(); i++ {
				walk(v.Index(i), fmt.Sprintf("%s[%d]", path, i), depth+1)
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				walk(k, path+"{key}", depth+1)
				walk(v.MapIndex(k), path+"{value}", depth+1)
			}
		}
	}
	walk(v, "", 0)
}

func fzClip(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

// fzMethod is one read the hub's pages make through a member's client:
// the types of its results (for the generator) and the call.
type fzMethod struct {
	name string
	outs []reflect.Type
	own  bool // the method sets its own deadline
	call func(ctx context.Context, c *Client) ([]any, error)
}

func fzT[T any]() reflect.Type { return reflect.TypeFor[T]() }

var fzProject = store.ProjectKey{Instance: "0a1b2c", ID: 1}

var fzMethods = []fzMethod{
	{"PipelineInfo", []reflect.Type{fzT[*store.Pipeline]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.PipelineInfo(ctx, "0a1b2c", 7)
		return []any{v}, err
	}},
	{"PipelineProject", []reflect.Type{fzT[uint64]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.PipelineProject(ctx, "0a1b2c", 7)
		return []any{v}, err
	}},
	{"PipelineJobs", []reflect.Type{fzT[[]store.JobInfo]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.PipelineJobs(ctx, "0a1b2c", 7)
		return []any{v}, err
	}},
	{"Job", []reflect.Type{fzT[*store.JobInfo]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.Job(ctx, "0a1b2c", 9)
		return []any{v}, err
	}},
	{"Builds", []reflect.Type{fzT[[]store.BuildRow]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.Builds(ctx, store.BuildQuery{})
		return []any{v}, err
	}},
	{"TargetImages", []reflect.Type{fzT[map[string][]string]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.TargetImages(ctx, fzProject, time.Time{})
		return []any{v}, err
	}},
	{"BuildPage", []reflect.Type{fzT[*store.BuildPage]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.BuildPage(ctx, 1)
		return []any{v}, err
	}},
	{"JobsOf", []reflect.Type{fzT[map[uint64][]store.JobInfo]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.JobsOf(ctx, "0a1b2c", []uint64{7})
		return []any{v}, err
	}},
	{"Previous", []reflect.Type{fzT[*store.Stored](), fzT[bool]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, known, err := c.Previous(ctx, &history.Build{})
		return []any{v, known}, err
	}},
	{"Projects", []reflect.Type{fzT[[]store.Project]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.Projects(ctx)
		return []any{v}, err
	}},
	{"Output", []reflect.Type{fzT[string]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.Output(ctx, 1, 0)
		return []any{v}, err
	}},
	{"Recent", []reflect.Type{fzT[[]*store.Stored]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.Recent(ctx, 10)
		return []any{v}, err
	}},
	{"PipelineList", []reflect.Type{fzT[[]store.PipeRow]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.PipelineList(ctx, store.PipeQuery{})
		return []any{v}, err
	}},
	{"BuildsOf", []reflect.Type{fzT[map[uint64][]store.BuildRow]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.BuildsOf(ctx, fzProject, []uint64{7})
		return []any{v}, err
	}},
	{"JobBuilds", []reflect.Type{fzT[[]int64]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.JobBuilds(ctx, "0a1b2c", 9)
		return []any{v}, err
	}},
	{"Refs", []reflect.Type{fzT[map[string]store.RefInfo]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.Refs(ctx, []string{"r1"})
		return []any{v}, err
	}},
	{"StepHistory", []reflect.Type{fzT[[]store.StepRun]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.StepHistory(ctx, 1, 0, 10)
		return []any{v}, err
	}},
	{"DaemonCounts", []reflect.Type{fzT[map[string]store.DaemonCount]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.DaemonCounts(ctx, time.Time{}, store.ProjectsIn{})
		return []any{v}, err
	}},
	{"ProjectCache", []reflect.Type{fzT[[]store.CacheObject](), fzT[[]store.CacheStore]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		a, b, err := c.ProjectCache(ctx, fzProject)
		return []any{a, b}, err
	}},
	{"Elsewhere", []reflect.Type{fzT[[]store.Elsewhere]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.Elsewhere(ctx, fzProject, time.Time{}, 10)
		return []any{v}, err
	}},
	{"Compared", []reflect.Type{fzT[[]string](), fzT[bool](), fzT[bool]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		files, complete, ok, err := c.Compared(ctx, fzProject, "a", "b")
		return []any{files, complete, ok}, err
	}},
	{"BuildHints", []reflect.Type{fzT[map[int64][]store.StepHints]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.BuildHints(ctx, []int64{1})
		return []any{v}, err
	}},
	{"BuildOrder", []reflect.Type{fzT[map[int64][]store.OrderHint]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.BuildOrder(ctx, []int64{1})
		return []any{v}, err
	}},
	{"LiveBuild", []reflect.Type{fzT[*collector.LiveBuild]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.LiveBuild(ctx, "d1", "r1")
		return []any{v}, err
	}},
	{"JobLogPart", []reflect.Type{fzT[gitlab.LogPart]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.JobLogPart(ctx, "0a1b2c", 1, 9, gitlab.LogQuery{Lines: 10})
		return []any{v}, err
	}},
	{"Changed", []reflect.Type{fzT[[]string](), fzT[bool]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		files, complete, err := c.Changed(ctx, "0a1b2c", 1, "a", "b")
		return []any{files, complete}, err
	}},
	{"CanReadAs", []reflect.Type{fzT[bool]()}, false, func(ctx context.Context, c *Client) ([]any, error) {
		ok, err := c.CanRead(ctx, auth.From(ctx), "0a1b2c", 1)
		return []any{ok}, err
	}},
	{"Daemons", []reflect.Type{fzT[[]collector.DaemonState]()}, true, func(ctx context.Context, c *Client) ([]any, error) {
		return []any{c.Daemons()}, nil
	}},
	{"Status", []reflect.Type{fzT[time.Time]()}, true, func(ctx context.Context, c *Client) ([]any, error) {
		v, err := c.Status()
		return []any{v}, err
	}},
	{"events", nil, false, func(ctx context.Context, c *Client) ([]any, error) {
		sub := c.Subscribe()
		defer c.Unsubscribe(sub)
		err := c.follow(ctx)
		if err == io.ErrUnexpectedEOF {
			err = nil // the stream's normal end
		}
		return nil, err
	}},
}

// fzEvents is the index of the change stream in fzMethods.
const fzEvents = 29

// fzReader turns the fuzzer's bytes into choices; past their end the
// choices come from a generator seeded by them, so that a short input
// still builds deep values. budget bounds the values fzGen builds.
type fzReader struct {
	b      []byte
	i      int
	rng    uint64
	budget int
}

func (r *fzReader) byte() byte {
	if r.i < len(r.b) {
		r.i++
		return r.b[r.i-1]
	}
	if r.rng == 0 {
		r.rng = 0x9e3779b97f4a7c15
		for _, c := range r.b {
			r.rng = (r.rng ^ uint64(c)) * 0x100000001b3
		}
		r.rng |= 1
	}
	r.rng ^= r.rng >> 12
	r.rng ^= r.rng << 25
	r.rng ^= r.rng >> 27
	return byte((r.rng * 0x2545f4914f6cdd1d) >> 56)
}

// fzFragments are the pieces of the strings the generator writes: what
// the hub must drop, and what it must keep.
var fzFragments = []string{
	"", "main", "eu", "\u202e", "\u202a", "\u2066", "\u2069", "\u200e", "\u200f", "\u061c",
	"\x1b[2J", "\x00", "\x7f", "\u0085", "\u009b", "\t", "\n", "<script>alert(1)</script>",
	"\ufeff", "\u2028", "\u00e9", "0a1b2c", "\r", "\x07",
}

func fzString(r *fzReader) string {
	var b strings.Builder
	n := int(r.byte() % 4)
	for range n {
		x := r.byte()
		if x >= 0xf0 {
			// Raw bytes from the input.
			k := int(x & 0x0f)
			for range k {
				b.WriteByte(r.byte())
			}
			continue
		}
		b.WriteString(fzFragments[int(x)%len(fzFragments)])
	}
	return b.String()
}

// fzGen builds a value of type t from the reader: every exported field
// filled, collections of up to three elements, pointers mostly set.
func fzGen(r *fzReader, t reflect.Type, depth int) reflect.Value {
	v := reflect.New(t).Elem()
	if depth > 40 || r.budget <= 0 {
		return v
	}
	r.budget--
	if t == reflect.TypeFor[time.Time]() {
		v.Set(reflect.ValueOf(time.Unix(int64(r.byte())<<20, 0).UTC()))
		return v
	}
	switch t.Kind() {
	case reflect.Bool:
		v.SetBool(r.byte()&1 == 1)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(int8(r.byte())))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(uint64(r.byte()))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(r.byte()) / 4)
	case reflect.String:
		v.SetString(fzString(r))
	case reflect.Pointer:
		if r.byte()%4 != 0 {
			v.Set(fzGen(r, t.Elem(), depth+1).Addr())
		}
	case reflect.Slice:
		n := int(r.byte() % 4)
		s := reflect.MakeSlice(t, n, n)
		for i := range n {
			s.Index(i).Set(fzGen(r, t.Elem(), depth+1))
		}
		v.Set(s)
	case reflect.Array:
		for i := 0; i < t.Len(); i++ {
			v.Index(i).Set(fzGen(r, t.Elem(), depth+1))
		}
	case reflect.Map:
		m := reflect.MakeMap(t)
		for range int(r.byte() % 4) {
			m.SetMapIndex(fzGen(r, t.Key(), depth+1), fzGen(r, t.Elem(), depth+1))
		}
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() {
				v.Field(i).Set(fzGen(r, t.Field(i).Type, depth+1))
			}
		}
	case reflect.Interface:
		if t.NumMethod() == 0 && r.byte()%2 == 1 {
			v.Set(reflect.ValueOf(fzString(r)))
		}
	}
	return v
}

var fzKinds = []string{"", "notfound", "norows", "signedout", "denied", "othersignin", "signin", "notrevoked", "notoken", "ratelimited", "other"}

// fzAnswer is a call's answer: one generated value per result (one more
// or one fewer when mode says so), and an error when mode says so.
func fzAnswer(r *fzReader, outs []reflect.Type, mode uint8) []byte {
	var a answer
	for _, t := range outs {
		b, err := json.Marshal(fzGen(r, t, 0).Interface())
		if err != nil {
			b = []byte("null")
		}
		a.R = append(a.R, b)
	}
	if mode&0x08 != 0 {
		if len(a.R) > 0 && r.byte()%2 == 0 {
			a.R = a.R[1:]
		} else {
			a.R = append(a.R, json.RawMessage(`"extra"`))
		}
	}
	if mode&0x04 != 0 {
		a.Err = &wireErr{Msg: fzString(r), Kind: fzKinds[int(r.byte())%len(fzKinds)], Until: time.Unix(int64(r.byte())<<20, 0).UTC()}
	}
	b, _ := json.Marshal(a)
	return b
}

// fzEventsBody is a change stream: lines of keys, comments and junk.
func fzEventsBody(r *fzReader) []byte {
	var b bytes.Buffer
	for range int(r.byte() % 8) {
		switch r.byte() % 3 {
		case 0:
			keys := []string{fzString(r), ""}
			j, _ := json.Marshal(keys[:1+int(r.byte()%2)])
			b.WriteString("data: " + string(j) + "\n\n")
		case 1:
			b.WriteString(": ping\n\n")
		default:
			b.WriteString("data: " + fzString(r) + "\n")
		}
	}
	return b.Bytes()
}
