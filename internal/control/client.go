package control

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// ErrUnavailable: the controller did not answer. The pages tell it apart
// by its Unavailable method (they do not import this package).
var ErrUnavailable error = unavailable{}

type unavailable struct{}

func (unavailable) Error() string     { return "the console's controller is not reachable" }
func (unavailable) Unavailable() bool { return true }

// Client is the web's view of the controller: the pages' backend, GitLab
// reader and sign-in, all served by the controller's API.
type Client struct {
	base   string
	http   *http.Client
	stream *http.Client
	token  func() (string, error)
	log    *slog.Logger
	cfg    Config
	known  map[string]bool

	hub store.Changes

	liveMu  sync.Mutex
	live    []collector.DaemonState
	liveAt  time.Time
	liveOld bool // a change was announced since

	readMu sync.Mutex
	reads  map[string]readAnswer

	setup     string
	onChange  func()
	changeOne sync.Once
}

// OnSetupChange calls f once when the controller's setup is no longer the
// one the client started with (sign-in turned on, a token added): the web
// must start again with the new one.
func (c *Client) OnSetupChange(f func()) { c.onChange = f }

func (c *Client) setupChanged() {
	c.changeOne.Do(func() {
		c.log.Error("the controller's setup changed; the web starts again to take it")
		if c.onChange != nil {
			go c.onChange()
		}
	})
}

type readAnswer struct {
	ok    bool
	until time.Time
}

// ClientOptions say how to reach the controller.
type ClientOptions struct {
	// URL is the controller's address, https://host:port.
	URL string
	// HTTP is the client to call it with (its TLS settings trust the
	// controller's certificate); Stream the one for the change stream,
	// without an overall timeout (default: HTTP).
	HTTP, Stream *http.Client
	// Token gives the bearer token of every call; nil: none (tests).
	Token func() (string, error)
	Log   *slog.Logger
}

// Dial reads the controller's configuration, trying again until it
// answers or ctx ends.
func Dial(ctx context.Context, o ClientOptions) (*Client, error) {
	c := &Client{base: strings.TrimRight(o.URL, "/"), http: o.HTTP, token: o.Token, log: o.Log, reads: map[string]readAnswer{}}
	if c.http == nil {
		c.http = http.DefaultClient
	}
	c.stream = o.Stream
	if c.stream == nil {
		c.stream = c.http
	}
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	delay := 500 * time.Millisecond
	for {
		err := c.get(ctx, "/internal/v1/config", &c.cfg)
		if err == nil {
			break
		}
		c.log.Info("waiting for the controller", "err", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
	c.setup = c.cfg.Setup()
	c.known = map[string]bool{}
	for _, k := range c.cfg.Known {
		c.known[k] = true
	}
	return c, nil
}

// Config is the controller's setup as it said when the client dialled.
func (c *Client) Config() Config { return c.cfg }

func (c *Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return nil, err
	}
	if c.token != nil {
		t, err := c.token()
		if err != nil {
			return nil, fmt.Errorf("controller token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}
	if raw := auth.CookieFrom(ctx); raw != "" {
		req.Header.Set(SessionHeader, raw)
	}
	if c.setup != "" {
		req.Header.Set(SetupHeader, c.setup)
	}
	return req, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// call calls method with args and decodes its results into outs. Results
// are decoded even when the method also returned an error.
func (c *Client) call(ctx context.Context, method string, args []any, outs ...any) error {
	if args == nil {
		args = []any{}
	}
	b, err := json.Marshal(args)
	if err != nil {
		return err
	}
	req, err := c.request(ctx, http.MethodPost, "/internal/v1/call/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		err := fmt.Errorf("controller: %s: %s", res.Status, strings.TrimSpace(string(msg)))
		if res.StatusCode == http.StatusConflict {
			c.setupChanged()
		}
		if res.StatusCode >= 500 || res.StatusCode == http.StatusConflict {
			err = fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return err
	}
	var ans answer
	if err := json.NewDecoder(res.Body).Decode(&ans); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnavailable, method, err)
	}
	if len(ans.R) != len(outs) {
		return fmt.Errorf("controller: %s gave %d results, not %d (another version?)", method, len(ans.R), len(outs))
	}
	for i, r := range ans.R {
		if err := json.Unmarshal(r, outs[i]); err != nil {
			return fmt.Errorf("controller: %s result %d: %v", method, i+1, err)
		}
	}
	return fromWire(ans.Err)
}

func fromWire(e *wireErr) error {
	if e == nil {
		return nil
	}
	var base error
	switch e.Kind {
	case "notfound":
		base = store.ErrNotFound
	case "norows":
		base = sql.ErrNoRows
	case "signedout":
		base = auth.ErrSignedOut
	case "denied":
		base = auth.ErrDenied
	case "signin":
		base = auth.ErrSignIn
	case "notoken":
		base = ErrNoToken
	case "ratelimited":
		return &gitlab.RateLimited{Until: e.Until}
	default:
		return errors.New(e.Msg)
	}
	if e.Msg == base.Error() {
		return base
	}
	return fmt.Errorf("%w (%s)", base, e.Msg)
}

// ---- changes ----

// Run follows the controller's changes until ctx ends, connecting again
// whenever the stream breaks. On every connection it announces "*": what
// changed while it was away is unknown.
func (c *Client) Run(ctx context.Context) {
	delay := time.Second
	for ctx.Err() == nil {
		err := c.follow(ctx)
		if ctx.Err() != nil {
			return
		}
		c.log.Warn("the controller's change stream ended", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 15*time.Second)
	}
}

func (c *Client) follow(ctx context.Context) error {
	req, err := c.request(ctx, http.MethodGet, "/internal/v1/events", nil)
	if err != nil {
		return err
	}
	res, err := c.stream.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusConflict {
		c.setupChanged()
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("events: %s", res.Status)
	}
	c.markLive()
	c.hub.Announce("*", "")
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var keys []string
		if err := json.Unmarshal([]byte(data), &keys); err != nil {
			continue
		}
		for _, k := range keys {
			if k == "" {
				c.markLive()
			}
		}
		c.hub.Announce(keys...)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func (c *Client) markLive() {
	c.liveMu.Lock()
	c.liveOld = true
	c.liveMu.Unlock()
}

// Subscribe receives the controller's changes.
func (c *Client) Subscribe() *store.Subscription { return c.hub.Subscribe() }

// Unsubscribe ends sub.
func (c *Client) Unsubscribe(sub *store.Subscription) { c.hub.Unsubscribe(sub) }

// ---- the store's reads ----

func (c *Client) Projects(ctx context.Context) (out []store.Project, err error) {
	err = c.call(ctx, "Projects", nil, &out)
	return
}

func (c *Client) Builds(ctx context.Context, q store.BuildQuery) (out []store.BuildRow, err error) {
	err = c.call(ctx, "Builds", []any{q}, &out)
	return
}

func (c *Client) Branches(ctx context.Context, p store.ProjectKey, since time.Time) (out []string, err error) {
	err = c.call(ctx, "Branches", []any{p, since}, &out)
	return
}

func (c *Client) Targets(ctx context.Context, p store.ProjectKey, since time.Time) (out []string, err error) {
	err = c.call(ctx, "Targets", []any{p, since}, &out)
	return
}

func (c *Client) StepCounts(ctx context.Context, p store.ProjectKey, target string, since time.Time, limit int) (out []store.StepCount, err error) {
	err = c.call(ctx, "StepCounts", []any{p, target, since, limit}, &out)
	return
}

func (c *Client) ImageDays(ctx context.Context, p store.ProjectKey, target string, since time.Time) (out []store.ImageDay, err error) {
	err = c.call(ctx, "ImageDays", []any{p, target, since}, &out)
	return
}

func (c *Client) PipelineList(ctx context.Context, q store.PipeQuery) (out []store.PipeRow, err error) {
	err = c.call(ctx, "PipelineList", []any{q}, &out)
	return
}

func (c *Client) JobsOf(ctx context.Context, instance string, pipelines []uint64) (out map[uint64][]store.JobInfo, err error) {
	err = c.call(ctx, "JobsOf", []any{instance, pipelines}, &out)
	return
}

func (c *Client) BuildsOf(ctx context.Context, p store.ProjectKey, pipelines []uint64) (out map[uint64][]store.BuildRow, err error) {
	err = c.call(ctx, "BuildsOf", []any{p, pipelines}, &out)
	return
}

func (c *Client) PipelineInfo(ctx context.Context, instance string, id uint64) (out *store.Pipeline, err error) {
	err = c.call(ctx, "PipelineInfo", []any{instance, id}, &out)
	return
}

func (c *Client) PipelineProject(ctx context.Context, instance string, pipeline uint64) (out uint64, err error) {
	err = c.call(ctx, "PipelineProject", []any{instance, pipeline}, &out)
	return
}

func (c *Client) PipelineJobs(ctx context.Context, instance string, pipeline uint64) (out []store.JobInfo, err error) {
	err = c.call(ctx, "PipelineJobs", []any{instance, pipeline}, &out)
	return
}

func (c *Client) Job(ctx context.Context, instance string, id uint64) (out *store.JobInfo, err error) {
	err = c.call(ctx, "Job", []any{instance, id}, &out)
	return
}

func (c *Client) JobBuilds(ctx context.Context, instance string, job uint64) (out []int64, err error) {
	err = c.call(ctx, "JobBuilds", []any{instance, job}, &out)
	return
}

func (c *Client) BuildPage(ctx context.Context, id int64) (out *store.BuildPage, err error) {
	err = c.call(ctx, "BuildPage", []any{id}, &out)
	return
}

func (c *Client) Refs(ctx context.Context, refs []string) (out map[string]store.RefInfo, err error) {
	err = c.call(ctx, "Refs", []any{refs}, &out)
	return
}

func (c *Client) Previous(ctx context.Context, b *history.Build) (prev *store.Stored, known bool, err error) {
	err = c.call(ctx, "Previous", []any{b}, &prev, &known)
	return
}

func (c *Client) StepHistory(ctx context.Context, buildID int64, step int, limit int) (out []store.StepRun, err error) {
	err = c.call(ctx, "StepHistory", []any{buildID, step, limit}, &out)
	return
}

func (c *Client) Output(ctx context.Context, buildID int64, step int) (out string, err error) {
	err = c.call(ctx, "Output", []any{buildID, step}, &out)
	return
}

func (c *Client) DaemonCounts(ctx context.Context, since time.Time) (out map[string]store.DaemonCount, err error) {
	err = c.call(ctx, "DaemonCounts", []any{since}, &out)
	return
}

func (c *Client) ProjectCache(ctx context.Context, p store.ProjectKey) (objs []store.CacheObject, stores []store.CacheStore, err error) {
	err = c.call(ctx, "ProjectCache", []any{p}, &objs, &stores)
	return
}

func (c *Client) Elsewhere(ctx context.Context, p store.ProjectKey, since time.Time, limit int) (out []store.Elsewhere, err error) {
	err = c.call(ctx, "Elsewhere", []any{p, since, limit}, &out)
	return
}

func (c *Client) SettleElsewhere(ctx context.Context, settled []store.Settled) error {
	return c.call(ctx, "SettleElsewhere", []any{settled})
}

func (c *Client) Recent(ctx context.Context, limit int) (out []*store.Stored, err error) {
	err = c.call(ctx, "Recent", []any{limit}, &out)
	return
}

func (c *Client) Ping(ctx context.Context) error { return c.call(ctx, "Ping", nil) }

// liveFor is how long the daemons' state is reused while no change is
// announced: a page reads it several times.
const liveFor = 2 * time.Second

// Daemons is the collector's view of the daemons, as the controller has
// it; empty while the controller does not answer.
func (c *Client) Daemons() []collector.DaemonState {
	c.liveMu.Lock()
	if !c.liveOld && time.Since(c.liveAt) < liveFor {
		out := c.live
		c.liveMu.Unlock()
		return out
	}
	c.liveMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []collector.DaemonState
	if err := c.call(ctx, "Daemons", nil, &out); err != nil {
		c.log.Warn("daemons from the controller", "err", err)
		return nil
	}
	c.liveMu.Lock()
	c.live, c.liveAt, c.liveOld = out, time.Now(), false
	c.liveMu.Unlock()
	return out
}

// ---- GitLab ----

// Knows says whether instance is one of the console's GitLabs.
func (c *Client) Knows(instance string) bool { return c.known[instance] }

func (c *Client) Ensure(ctx context.Context, instance string, project, pipeline uint64, status string, read time.Time, wait time.Duration) {
	if err := c.call(ctx, "Ensure", []any{instance, project, pipeline, status, read, wait}); err != nil && ctx.Err() == nil {
		c.log.Warn("reading a pipeline from GitLab through the controller", "err", err)
	}
}

func (c *Client) JobLogPart(ctx context.Context, instance string, project, job uint64, q gitlab.LogQuery) (out gitlab.LogPart, err error) {
	err = c.call(ctx, "JobLogPart", []any{instance, project, job, q}, &out)
	return
}

func (c *Client) Changed(ctx context.Context, instance string, project uint64, from, to string) (files []string, complete bool, err error) {
	err = c.call(ctx, "Changed", []any{instance, project, from, to}, &files, &complete)
	return
}

func (c *Client) Status() (until time.Time, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = c.call(ctx, "Status", nil, &until)
	return until, err
}

// ---- sign-in ----

// PublicURL is where browsers reach the console.
func (c *Client) PublicURL() string { return c.cfg.PublicURL }

// Instances lists the GitLabs people can sign in with.
func (c *Client) Instances() []auth.Choice { return c.cfg.Instances }

// Begin sends the browser to GitLab to sign in; the controller holds the
// key that seals the sign-in's state.
func (c *Client) Begin(w http.ResponseWriter, r *http.Request, instance, to string) error {
	var authURL, login string
	if err := c.call(r.Context(), "BeginWith", []any{instance, to}, &authURL, &login); err != nil {
		return err
	}
	auth.SetLoginCookie(w, login)
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, authURL, http.StatusFound)
	return nil
}

// Finish completes a sign-in on GitLab's callback.
func (c *Client) Finish(w http.ResponseWriter, r *http.Request, instance string) (string, error) {
	login, old := "", ""
	if ck, err := r.Cookie(auth.LoginCookie); err == nil {
		login = ck.Value
	}
	auth.ClearLoginCookie(w)
	if ck, err := r.Cookie(auth.SessionCookie); err == nil {
		old = ck.Value
	}
	to, session := "/", ""
	q := url.Values{}
	for _, k := range []string{"state", "code", "error"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	err := c.call(r.Context(), "FinishWith", []any{instance, login, q, old}, &to, &session)
	if err != nil {
		return to, err
	}
	auth.SetSessionCookie(w, session, c.cfg.Idle)
	return to, nil
}

// Load returns the person signed in on r, or nil. The viewer it returns
// carries the person's fields only: GitLab is read by the controller.
func (c *Client) Load(w http.ResponseWriter, r *http.Request) (*auth.Viewer, error) {
	ck, err := r.Cookie(auth.SessionCookie)
	if err != nil {
		return nil, nil
	}
	var v *auth.Viewer
	var touched bool
	if err := c.call(r.Context(), "LoadCookie", []any{ck.Value}, &v, &touched); err != nil {
		return nil, err
	}
	if touched {
		auth.SetSessionCookie(w, ck.Value, c.cfg.Idle)
	}
	return v, nil
}

// SignOut ends the session on r.
func (c *Client) SignOut(w http.ResponseWriter, r *http.Request) error {
	auth.ClearSessionCookie(w)
	ck, err := r.Cookie(auth.SessionCookie)
	if err != nil {
		return nil
	}
	c.forget(ck.Value)
	return c.call(r.Context(), "EndCookie", []any{ck.Value})
}

// readFor is how long the web keeps the controller's answer on what a
// person may read; the controller keeps GitLab's for longer.
const readFor = 30 * time.Second

// CanRead says whether the person of the session ctx carries may see a
// project's builds and jobs.
func (c *Client) CanRead(ctx context.Context, v *auth.Viewer, instance string, project uint64) (bool, error) {
	if v == nil {
		return false, nil
	}
	if v.Admin {
		return true, nil
	}
	raw := auth.CookieFrom(ctx)
	sum := sha256.Sum256([]byte(raw))
	key := fmt.Sprintf("%x/%s/%d", sum[:8], instance, project)
	c.readMu.Lock()
	if a, ok := c.reads[key]; ok && time.Now().Before(a.until) {
		c.readMu.Unlock()
		return a.ok, nil
	}
	c.readMu.Unlock()
	var ok bool
	if err := c.call(ctx, "CanReadAs", []any{instance, project}, &ok); err != nil {
		return false, err
	}
	c.readMu.Lock()
	if len(c.reads) > 20000 {
		clear(c.reads)
	}
	c.reads[key] = readAnswer{ok: ok, until: time.Now().Add(readFor)}
	c.readMu.Unlock()
	return ok, nil
}

// forget drops what the web kept about a session that ends.
func (c *Client) forget(raw string) {
	sum := sha256.Sum256([]byte(raw))
	prefix := fmt.Sprintf("%x/", sum[:8])
	c.readMu.Lock()
	for k := range c.reads {
		if strings.HasPrefix(k, prefix) {
			delete(c.reads, k)
		}
	}
	c.readMu.Unlock()
}
