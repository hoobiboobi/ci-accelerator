// Package gitlab reads what the console needs from GitLab's API: a
// pipeline's jobs with their times and status. It is read lazily, while a
// page shows the pipeline: one request per pipeline, a finished pipeline
// never again, and nothing in the background. GitLab being slow, down or
// rate limiting never breaks a page; it only leaves job times out.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Client calls the API of the GitLab instances the installation serves.
// Every request carries a Credential: the installation's token, or the
// token of the person whose page asked.
type Client struct {
	HTTP *http.Client
	// bases maps an instance (trace-id prefix) to its API root.
	bases map[string]string
}

// New returns a client for the GitLab URLs the runner was given (as in the
// runner configuration).
func New(urls []string) *Client {
	c := &Client{HTTP: &http.Client{Timeout: 10 * time.Second}, bases: map[string]string{}}
	for _, u := range urls {
		c.bases[history.InstancePrefix(u)] = strings.TrimRight(u, "/") + "/api/v4"
	}
	return c
}

// Credential is whose token a request carries.
type Credential interface {
	// Token returns the token to send.
	Token(ctx context.Context) (string, error)
	// Renew is called when GitLab refused token: it returns a token to try
	// once more, or an error when there is none.
	Renew(ctx context.Context, refused string) (string, error)
	// Who names the credential; GitLab rate limits each one on its own.
	Who() string
}

// Token is a fixed token: a personal, project or group access token.
type Token string

// Token returns the token.
func (t Token) Token(context.Context) (string, error) { return string(t), nil }

// Renew has nothing else to offer.
func (t Token) Renew(context.Context, string) (string, error) { return "", ErrRefused }

// Who is the same for every fixed token.
func (t Token) Who() string { return "token" }

// ErrRefused is returned when GitLab does not accept the credential.
var ErrRefused = errors.New("gitlab: the token was refused")

// do sends a GET with cred's token, and once more with a renewed token
// when GitLab refuses the first. The caller closes the body.
func (c *Client) do(ctx context.Context, cred Credential, u string) (*http.Response, error) {
	return c.doRange(ctx, cred, u, "")
}

// doRange is do with an HTTP Range header (none when rng is empty).
func (c *Client) doRange(ctx context.Context, cred Credential, u, rng string) (*http.Response, error) {
	if cred == nil {
		return nil, ErrRefused
	}
	tok, err := cred.Token(ctx)
	if err != nil {
		return nil, err
	}
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		res, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusUnauthorized {
			return res, nil
		}
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		res.Body.Close()
		if try > 0 {
			return nil, ErrRefused
		}
		if tok, err = cred.Renew(ctx, tok); err != nil {
			return nil, err
		}
	}
}

// Knows says whether instance is one of the client's GitLab instances.
func (c *Client) Knows(instance string) bool { return c.bases[instance] != "" }

// Instances lists the trace-id prefixes of the GitLabs the client reads,
// sorted.
func (c *Client) Instances() []string {
	out := make([]string, 0, len(c.bases))
	for k := range c.bases {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RateLimited is returned while GitLab asks the console to wait.
type RateLimited struct{ Until time.Time }

func (e *RateLimited) Error() string {
	return "gitlab: rate limited until " + e.Until.UTC().Format(time.RFC3339)
}

type apiJob struct {
	ID             uint64   `json:"id"`
	Name           string   `json:"name"`
	Stage          string   `json:"stage"`
	Status         string   `json:"status"`
	WebURL         string   `json:"web_url"`
	CreatedAt      *string  `json:"created_at"`
	StartedAt      *string  `json:"started_at"`
	FinishedAt     *string  `json:"finished_at"`
	Duration       *float64 `json:"duration"`
	QueuedDuration *float64 `json:"queued_duration"`
	AllowFailure   bool     `json:"allow_failure"`
	User           *struct {
		Username string `json:"username"`
	} `json:"user"`
	Runner *struct {
		Description string `json:"description"`
	} `json:"runner"`
	Pipeline struct {
		ID        uint64 `json:"id"`
		ProjectID uint64 `json:"project_id"`
		Status    string `json:"status"`
		WebURL    string `json:"web_url"`
	} `json:"pipeline"`
}

// Pipeline reads a pipeline's jobs, every try of each (a retried job's
// earlier tries too: GitLab leaves them out unless asked).
func (c *Client) Pipeline(ctx context.Context, cred Credential, instance string, project, pipeline uint64) (store.APIPipeline, error) {
	out := store.APIPipeline{Instance: instance, ID: pipeline, ProjectID: project}
	base := c.bases[instance]
	if base == "" {
		return out, fmt.Errorf("gitlab: unknown instance %s", instance)
	}
	for page := 1; page > 0; {
		u := fmt.Sprintf("%s/projects/%d/pipelines/%d/jobs?per_page=100&include_retried=true&page=%d", base, project, pipeline, page)
		jobs, next, err := c.get(ctx, cred, u)
		if err != nil {
			return out, err
		}
		for _, j := range jobs {
			out.Status, out.WebURL = j.Pipeline.Status, j.Pipeline.WebURL
			job := store.APIJob{ID: j.ID, Name: j.Name, Stage: j.Stage, Status: j.Status, WebURL: j.WebURL,
				Queued: when(j.CreatedAt), Started: when(j.StartedAt), Finished: when(j.FinishedAt),
				Duration: seconds(j.Duration), Waited: seconds(j.QueuedDuration), AllowFailure: j.AllowFailure}
			if j.Runner != nil {
				job.Runner = j.Runner.Description
			}
			if j.User != nil {
				job.User = j.User.Username
			}
			out.Jobs = append(out.Jobs, job)
		}
		page = next
	}
	out.Read = time.Now()
	return out, nil
}

func (c *Client) get(ctx context.Context, cred Credential, u string) ([]apiJob, int, error) {
	res, err := c.do(ctx, cred, u)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, 0, &RateLimited{Until: retryAfter(res.Header, time.Now())}
	}
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		return nil, 0, fmt.Errorf("gitlab: %s: %s", redact(u), res.Status)
	}
	var jobs []apiJob
	if err := json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(&jobs); err != nil {
		return nil, 0, fmt.Errorf("gitlab: %s: %w", redact(u), err)
	}
	next, _ := strconv.Atoi(res.Header.Get("X-Next-Page"))
	return jobs, next, nil
}

// LogQuery picks a part of a job's log, in whole lines: the last Lines
// lines (Before and After unset), the Lines lines that end at byte Before
// (a part's Start), or the lines added since byte After (a part's End).
type LogQuery struct {
	Lines  int
	Before int64 // > 0: the lines before this byte
	After  int64 // > 0, or Since: the lines from this byte to the end
	Since  bool  // After is set (0 is a valid After)
}

// LogPart is a part of a job's log: its whole lines from byte Start to byte
// End of the log as GitLab stores it (Size bytes when read), cleaned (see
// CleanLog), not masked. Gap: more lines were added since After than one
// part holds; Text is their end and Start where it begins.
type LogPart struct {
	Text             string
	Start, End, Size int64
	Gap              bool
}

// MaxTrace bounds how much of a job's log is read when GitLab sends all of
// it (no range support: a log still being written, some installations);
// logWindow is the first window of a ranged read, grown up to logWindowMax
// while it holds fewer lines than asked.
const (
	MaxTrace     = 32 << 20
	logWindow    = 64 << 10
	logWindowMax = 4 << 20
)

// JobLogPart reads a part of a job's log. GitLab serves ranges of an
// archived log, so only the bytes of the part travel; when it answers with
// the whole log instead, the part is cut from it here.
func (c *Client) JobLogPart(ctx context.Context, cred Credential, instance string, project, job uint64, q LogQuery) (LogPart, error) {
	base := c.bases[instance]
	if base == "" {
		return LogPart{}, fmt.Errorf("gitlab: unknown instance %s", instance)
	}
	if q.Lines <= 0 {
		q.Lines = 500
	}
	r := &logReader{c: c, ctx: ctx, cred: cred, u: fmt.Sprintf("%s/projects/%d/jobs/%d/trace", base, project, job)}
	var (
		data  []byte
		from  int64
		part  LogPart
		limit = q.Before // the end of the window, or -1: the log's end
	)
	if q.Since {
		d, f, err := r.read(q.After, -1)
		if err != nil {
			return part, err
		}
		if f+int64(len(d)) == r.size && len(d) <= logWindowMax {
			end := lastLineEnd(d)
			// The line before it, for a run of empty lines across the parts.
			prev, pf, err := r.read(max(0, f-logWindow), f)
			if err != nil {
				return part, err
			}
			part = LogPart{Text: cleanPart(string(d[:end]), prevShownEmpty(prev, len(prev), pf)), Start: f, End: f + int64(end), Size: r.size}
			return part, nil
		}
		// Too much is new: the last lines of the log instead.
		limit, part.Gap = -1, true
	}
	if limit == 0 {
		limit = -1
	}
	for w := int64(logWindow); data == nil; w *= 4 {
		lo := int64(0)
		if limit > 0 {
			lo = max(0, limit-w)
		} else {
			lo = -w // the last w bytes
		}
		d, f, err := r.read(lo, limit)
		if err != nil {
			return part, err
		}
		if f == 0 || w >= logWindowMax || bytes.Count(d, []byte{'\n'}) > q.Lines {
			data, from = d, f
		}
	}
	end := lastLineEnd(data)
	if limit > 0 {
		end = len(data) // ends at a part's start: a line's start
	}
	begin := 0
	if from > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 && i < end {
			begin = i + 1 // not a piece of a line
		}
	}
	// The last Lines lines before end.
	n, i := 0, end-1
	if i >= begin && data[i] == '\n' {
		i--
	}
	for ; i >= begin; i-- {
		if data[i] == '\n' {
			if n++; n == q.Lines {
				begin = i + 1
				break
			}
		}
	}
	part.Text = cleanPart(string(data[begin:end]), prevShownEmpty(data, begin, from))
	part.Start, part.End, part.Size = from+int64(begin), from+int64(end), r.size
	return part, nil
}

// lastLineEnd is where the last whole line of b ends: a line still being
// written comes with the next read.
func lastLineEnd(b []byte) int {
	return bytes.LastIndexByte(b, '\n') + 1
}

// contentRange reads "bytes first-last/size" (or "bytes */size": first 0).
func contentRange(h string) (first, size int64, ok bool) {
	r, total, found := strings.Cut(strings.TrimPrefix(h, "bytes "), "/")
	if !found {
		return 0, 0, false
	}
	size, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	if r == "*" {
		return 0, size, true
	}
	a, _, found := strings.Cut(r, "-")
	if first, err = strconv.ParseInt(a, 10, 64); !found || err != nil {
		return 0, 0, false
	}
	return first, size, true
}

// logReader reads byte ranges of one job's log: ranged requests while
// GitLab honours them, else the whole log once, kept for the next reads.
type logReader struct {
	c    *Client
	ctx  context.Context
	cred Credential
	u    string
	all  []byte // the whole log, once GitLab sent it
	size int64
}

// read returns the bytes from from (< 0: the last -from bytes) to to (< 0:
// the log's end), and where they start.
func (r *logReader) read(from, to int64) ([]byte, int64, error) {
	if r.all == nil {
		rng := ""
		switch {
		case from < 0:
			rng = fmt.Sprintf("bytes=%d", from)
		case to < 0:
			rng = fmt.Sprintf("bytes=%d-", from)
		case to > from:
			rng = fmt.Sprintf("bytes=%d-%d", from, to-1)
		default:
			return nil, max(from, 0), nil
		}
		res, err := r.c.doRange(r.ctx, r.cred, r.u, rng)
		if err != nil {
			return nil, 0, err
		}
		defer res.Body.Close()
		switch res.StatusCode {
		case http.StatusPartialContent:
			first, size, ok := contentRange(res.Header.Get("Content-Range"))
			if !ok {
				return nil, 0, fmt.Errorf("gitlab: %s: Content-Range %q", redact(r.u), res.Header.Get("Content-Range"))
			}
			r.size = size
			b, err := io.ReadAll(io.LimitReader(res.Body, logWindowMax+1<<20))
			return b, first, err
		case http.StatusRequestedRangeNotSatisfiable: // nothing from there on
			io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
			var ok bool
			if _, r.size, ok = contentRange(res.Header.Get("Content-Range")); !ok {
				// gitlab.com's object storage leaves Content-Range out of a
				// 416 when the request accepts gzip (Go's always does): the
				// log ends where the range began (a log only grows, and
				// from is the end of a part read before).
				r.size = max(from, 0)
			}
			return nil, max(from, 0), nil
		case http.StatusOK:
			b, err := io.ReadAll(io.LimitReader(res.Body, MaxTrace))
			if err != nil {
				return nil, 0, err
			}
			r.all, r.size = b, int64(len(b))
		case http.StatusTooManyRequests:
			return nil, 0, &RateLimited{Until: retryAfter(res.Header, time.Now())}
		default:
			io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
			return nil, 0, fmt.Errorf("gitlab: %s: %s", redact(r.u), res.Status)
		}
	}
	n := int64(len(r.all))
	if from < 0 {
		from = max(0, n+from)
	}
	if to < 0 || to > n {
		to = n
	}
	if from > to {
		from = to
	}
	return r.all[from:to], from, nil
}

// CanReadJobs asks GitLab whether cred may read a project's jobs: the jobs
// list answers 200 only then (a project can be visible while its jobs are
// not: Guests, pipelines visible to members only). 403 and 404 are no;
// anything else is an error, never a yes.
func (c *Client) CanReadJobs(ctx context.Context, cred Credential, instance string, project uint64) (bool, error) {
	base := c.bases[instance]
	if base == "" || project == 0 {
		return false, nil
	}
	u := fmt.Sprintf("%s/projects/%d/jobs?per_page=1", base, project)
	res, err := c.do(ctx, cred, u)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	switch res.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusForbidden, http.StatusNotFound:
		return false, nil
	case http.StatusTooManyRequests:
		return false, &RateLimited{Until: retryAfter(res.Header, time.Now())}
	}
	return false, fmt.Errorf("gitlab: %s: %s", redact(u), res.Status)
}

// User is a GitLab account.
type User struct {
	ID       uint64 `json:"id"`
	Username string `json:"username"`
	Name     string `json:"name"`
}

// User returns the account cred belongs to.
func (c *Client) User(ctx context.Context, cred Credential, instance string) (User, error) {
	var u User
	base := c.bases[instance]
	if base == "" {
		return u, fmt.Errorf("gitlab: unknown instance %s", instance)
	}
	res, err := c.do(ctx, cred, base+"/user")
	if err != nil {
		return u, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		return u, fmt.Errorf("gitlab: /user: %s", res.Status)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&u); err != nil {
		return u, err
	}
	if u.ID == 0 || u.Username == "" {
		return u, errors.New("gitlab: /user: no account in the answer")
	}
	return u, nil
}

var (
	logSection = regexp.MustCompile(`section_(?:start|end):\d+:[A-Za-z0-9_.\-]+(?:\[[^\]\r\n]*\])?\r?(?:\x1b\[0K)?`)
	logANSI    = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	// Runners from 18.x prefix each line with its time, a stream number
	// and O or E (stdout, stderr), "+" on the pieces around section
	// markers.
	logStamp = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(?:\.\d+)?Z [0-9a-f]{2}[OE](\+?) ?`)
)

// CleanLog turns a job log as GitLab stores it into the text it shows.
func CleanLog(s string) string {
	s = logSection.ReplaceAllString(s, "")
	s = logANSI.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out []string
	for _, l := range strings.Split(s, "\n") {
		l, cont := cleanLine(l)
		// No empty piece around a section marker, no run of empty lines.
		if l == "" && (cont || (len(out) > 0 && out[len(out)-1] == "")) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// cleanLine is one line as shown (markers, colours and what a carriage
// return overwrote taken out), and whether the runner marked it as the
// continuation of the line before ("+").
func cleanLine(l string) (string, bool) {
	l = strings.TrimSuffix(l, "\r")
	l = logSection.ReplaceAllString(l, "")
	l = logANSI.ReplaceAllString(l, "")
	cont := false
	if m := logStamp.FindStringSubmatch(l); m != nil {
		l, cont = l[len(m[0]):], m[1] == "+"
	}
	if j := strings.LastIndex(l, "\r"); j >= 0 {
		l = l[j+1:]
	}
	return l, cont
}

// cleanPart cleans whole lines of a log (raw ends with a newline, or is
// empty) exactly as CleanLog cleans them within the whole log: prevEmpty
// says that the last line shown before them is empty (see prevShownEmpty),
// so parts read one by one join into what the whole log shows.
func cleanPart(raw string, prevEmpty bool) string {
	var b strings.Builder
	last := ""
	if prevEmpty {
		last = "\x00empty" // a run of empty lines goes on across the parts
	}
	shown := 0
	for _, l := range strings.SplitAfter(raw, "\n") {
		if l == "" {
			continue
		}
		c, cont := cleanLine(strings.TrimSuffix(l, "\n"))
		if c == "" && (cont || last == "" && shown > 0 || last == "\x00empty") {
			continue
		}
		b.WriteString(c)
		b.WriteByte('\n')
		last = c
		shown++
	}
	return b.String()
}

// prevShownEmpty says whether, in the whole log, the last line shown before
// byte begin of data (which starts at byte from of the log) is empty. Lines
// marked as continuations are never shown when empty and are skipped; when
// the answer lies before data, it is "no" (or the log's start).
func prevShownEmpty(data []byte, begin int, from int64) bool {
	i := begin - 1 // data[i] ends the line before begin
	for i >= 0 {
		j := bytes.LastIndexByte(data[:i], '\n') + 1
		if j == 0 && from > 0 {
			return false // that line may start before data
		}
		c, cont := cleanLine(string(data[j:i]))
		if c == "" && cont {
			i = j - 1
			continue
		}
		return c == ""
	}
	return false
}

// retryAfter reads how long GitLab asks to wait: Retry-After (seconds), or
// RateLimit-Reset (a Unix time); a minute when it says neither.
func retryAfter(h http.Header, now time.Time) time.Time {
	if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil && s >= 0 {
		return now.Add(time.Duration(s) * time.Second)
	}
	if s, err := strconv.ParseInt(h.Get("RateLimit-Reset"), 10, 64); err == nil && s > now.Unix() {
		return time.Unix(s, 0)
	}
	return now.Add(time.Minute)
}

func redact(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return "(url)"
	}
	return p.Path
}

func when(s *string) time.Time {
	if s == nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, *s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func seconds(v *float64) time.Duration {
	if v == nil || *v < 0 {
		return 0
	}
	return time.Duration(*v * float64(time.Second))
}

// Saver stores what the API returned.
type Saver interface {
	SaveAPIPipeline(ctx context.Context, p store.APIPipeline) error
}

// Fetcher reads pipelines when pages need them, with the credential of the
// page that asked: at most one request per pipeline at a time, a running
// pipeline again only after Every, a finished one never again, and nothing
// with a credential GitLab rate limits.
type Fetcher struct {
	Client *Client
	Store  Saver
	Log    *slog.Logger
	// Every is the shortest time between two reads of a running pipeline
	// (default 20 s).
	Every time.Duration

	mu       sync.Mutex
	inflight map[string]chan struct{}
	last     map[string]time.Time
	later    map[string]Credential // a read is due later, for the last page that asked
	until    map[string]time.Time  // rate limited until, by credential
	lastErr  error
}

// limited returns until when GitLab asked cred to wait; f.mu is held.
func (f *Fetcher) limited(cred Credential) time.Time {
	return f.until[cred.Who()]
}

// wait records GitLab's request to wait; f.mu is held.
func (f *Fetcher) wait(cred Credential, err error) {
	var rl *RateLimited
	if errors.As(err, &rl) {
		if f.until == nil {
			f.until = map[string]time.Time{}
		}
		f.until[cred.Who()] = rl.Until
	}
}

// Ensure makes sure a pipeline's jobs are fresh enough: it starts a read
// when one is due, and waits for it at most wait. The read goes on after
// wait; the store announces the change when it lands.
func (f *Fetcher) Ensure(cred Credential, instance string, project, pipeline uint64, status string, read time.Time, wait time.Duration) {
	if f == nil || cred == nil || project == 0 || pipeline == 0 || !f.Client.Knows(instance) {
		return
	}
	if store.Finished(status) && !read.IsZero() {
		return
	}
	key := store.PipelineKey(instance, pipeline)
	every := f.Every
	if every == 0 {
		every = 20 * time.Second
	}
	f.mu.Lock()
	if f.inflight == nil {
		f.inflight, f.last, f.later = map[string]chan struct{}{}, map[string]time.Time{}, map[string]Credential{}
	}
	now := time.Now()
	until := f.limited(cred)
	done, running := f.inflight[key]
	if !running {
		if now.Before(until) || now.Sub(f.last[key]) < every || now.Sub(read) < every {
			// Read too recently: read again when it is due, so a page
			// showing a running pipeline hears how it goes on even when
			// nothing else changes. The read's own update brings the page
			// back here; nobody asking, nothing more is read.
			_, due := f.later[key]
			if !now.Before(until) {
				f.later[key] = cred
			}
			if !due && !now.Before(until) {
				wait := every - now.Sub(f.last[key])
				if w := every - now.Sub(read); w > wait {
					wait = w
				}
				time.AfterFunc(wait, func() {
					f.mu.Lock()
					c := f.later[key]
					delete(f.later, key)
					f.mu.Unlock()
					f.Ensure(c, instance, project, pipeline, "", time.Time{}, 0)
				})
			}
			f.mu.Unlock()
			return
		}
		done = make(chan struct{})
		f.inflight[key], f.last[key] = done, now
		go f.read(key, cred, instance, project, pipeline, done)
	}
	f.mu.Unlock()
	if wait > 0 {
		select {
		case <-done:
		case <-time.After(wait):
		}
	}
}

func (f *Fetcher) read(key string, cred Credential, instance string, project, pipeline uint64, done chan struct{}) {
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p, err := f.Client.Pipeline(ctx, cred, instance, project, pipeline)
	if err == nil && len(p.Jobs) > 0 {
		err = f.Store.SaveAPIPipeline(ctx, p)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.inflight, key)
	f.lastErr = err
	f.wait(cred, err)
	if err != nil && f.Log != nil {
		f.Log.Warn("gitlab api", "pipeline", key, "err", err)
	}
}

// JobLogPart reads a part of a job's log, unless GitLab asked this
// credential to wait.
func (f *Fetcher) JobLogPart(ctx context.Context, cred Credential, instance string, project, job uint64, q LogQuery) (LogPart, error) {
	if f == nil || cred == nil || !f.Client.Knows(instance) || project == 0 {
		return LogPart{}, errors.New("gitlab: no token for this GitLab, or the job's project is not known")
	}
	f.mu.Lock()
	until := f.limited(cred)
	f.mu.Unlock()
	if time.Now().Before(until) {
		return LogPart{}, &RateLimited{Until: until}
	}
	out, err := f.Client.JobLogPart(ctx, cred, instance, project, job, q)
	f.mu.Lock()
	f.wait(cred, err)
	f.mu.Unlock()
	return out, err
}

// Status says how the last read went, for the operations view.
func (f *Fetcher) Status() (until time.Time, err error) {
	if f == nil {
		return time.Time{}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.until {
		if t.After(until) {
			until = t
		}
	}
	return until, f.lastErr
}

// MaxCompare bounds how much of a comparison is read: GitLab sends every
// file's diff along with its path.
const MaxCompare = 32 << 20

// Changed returns the files that differ between two commits of a project
// (GitLab's direct comparison, from..to), and whether the list is
// complete: GitLab cuts it when the comparison is too large or too slow,
// and so does MaxCompare.
func (c *Client) Changed(ctx context.Context, cred Credential, instance string, project uint64, from, to string) ([]string, bool, error) {
	base := c.bases[instance]
	if base == "" {
		return nil, false, fmt.Errorf("gitlab: unknown instance %s", instance)
	}
	u := fmt.Sprintf("%s/projects/%d/repository/compare?from=%s&to=%s&straight=true", base, project, url.QueryEscape(from), url.QueryEscape(to))
	res, err := c.do(ctx, cred, u)
	if err != nil {
		return nil, false, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, false, &RateLimited{Until: retryAfter(res.Header, time.Now())}
	}
	if res.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		return nil, false, fmt.Errorf("gitlab: %s: %s", redact(u), res.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, MaxCompare+1))
	if err != nil {
		return nil, false, err
	}
	if len(raw) > MaxCompare {
		return nil, false, nil
	}
	var cmp struct {
		Timeout bool `json:"compare_timeout"`
		Diffs   []struct {
			Old string `json:"old_path"`
			New string `json:"new_path"`
		} `json:"diffs"`
	}
	if err := json.Unmarshal(raw, &cmp); err != nil {
		return nil, false, fmt.Errorf("gitlab: %s: %w", redact(u), err)
	}
	seen := map[string]bool{}
	var out []string
	for _, d := range cmp.Diffs {
		for _, p := range []string{d.Old, d.New} {
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out, !cmp.Timeout, nil
}

// Changed is Client.Changed, waiting while GitLab asks the console to.
func (f *Fetcher) Changed(ctx context.Context, cred Credential, instance string, project uint64, from, to string) ([]string, bool, error) {
	if f == nil || cred == nil || !f.Client.Knows(instance) || project == 0 {
		return nil, false, errors.New("gitlab: no token for this GitLab, or the project is not known")
	}
	f.mu.Lock()
	until := f.limited(cred)
	f.mu.Unlock()
	if time.Now().Before(until) {
		return nil, false, &RateLimited{Until: until}
	}
	out, complete, err := f.Client.Changed(ctx, cred, instance, project, from, to)
	f.mu.Lock()
	f.wait(cred, err)
	f.mu.Unlock()
	return out, complete, err
}
