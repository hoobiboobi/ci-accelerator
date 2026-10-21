// Package gitlab reads what the console needs from GitLab's API: a
// pipeline's jobs with their times and status. It is read lazily, while a
// page shows the pipeline: one request per pipeline, a finished pipeline
// never again, and nothing in the background. GitLab being slow, down or
// rate limiting never breaks a page; it only leaves job times out.
package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Client calls the API of the GitLab instances the installation serves.
type Client struct {
	HTTP  *http.Client
	Token string
	// bases maps an instance (trace-id prefix) to its API root.
	bases map[string]string
}

// New returns a client for the GitLab URLs the runner was given (as in the
// runner configuration) with one token for all of them.
func New(urls []string, token string) *Client {
	c := &Client{HTTP: &http.Client{Timeout: 10 * time.Second}, Token: token, bases: map[string]string{}}
	for _, u := range urls {
		c.bases[history.InstancePrefix(u)] = strings.TrimRight(u, "/") + "/api/v4"
	}
	return c
}

// Knows says whether instance is one of the client's GitLab instances.
func (c *Client) Knows(instance string) bool { return c.bases[instance] != "" }

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
	Runner         *struct {
		Description string `json:"description"`
	} `json:"runner"`
	Pipeline struct {
		ID        uint64 `json:"id"`
		ProjectID uint64 `json:"project_id"`
		Status    string `json:"status"`
		WebURL    string `json:"web_url"`
	} `json:"pipeline"`
}

// Pipeline reads a pipeline's jobs (the latest try of each).
func (c *Client) Pipeline(ctx context.Context, instance string, project, pipeline uint64) (store.APIPipeline, error) {
	out := store.APIPipeline{Instance: instance, ID: pipeline, ProjectID: project}
	base := c.bases[instance]
	if base == "" {
		return out, fmt.Errorf("gitlab: unknown instance %s", instance)
	}
	for page := 1; page > 0; {
		u := fmt.Sprintf("%s/projects/%d/pipelines/%d/jobs?per_page=100&page=%d", base, project, pipeline, page)
		jobs, next, err := c.get(ctx, u)
		if err != nil {
			return out, err
		}
		for _, j := range jobs {
			out.Status, out.WebURL = j.Pipeline.Status, j.Pipeline.WebURL
			job := store.APIJob{ID: j.ID, Name: j.Name, Stage: j.Stage, Status: j.Status, WebURL: j.WebURL,
				Queued: when(j.CreatedAt), Started: when(j.StartedAt), Finished: when(j.FinishedAt),
				Duration: seconds(j.Duration), Waited: seconds(j.QueuedDuration)}
			if j.Runner != nil {
				job.Runner = j.Runner.Description
			}
			out.Jobs = append(out.Jobs, job)
		}
		page = next
	}
	out.Read = time.Now()
	return out, nil
}

func (c *Client) get(ctx context.Context, u string) ([]apiJob, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	if c.Token != "" {
		req.Header.Set("PRIVATE-TOKEN", c.Token)
	}
	req.Header.Set("Accept", "application/json")
	res, err := c.HTTP.Do(req)
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

// Fetcher reads pipelines when pages need them: at most one request per
// pipeline at a time, a running pipeline again only after Every, a
// finished one never again, and nothing while GitLab rate limits.
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
	until    time.Time // rate limited until
	lastErr  error
}

// Ensure makes sure a pipeline's jobs are fresh enough: it starts a read
// when one is due, and waits for it at most wait. The read goes on after
// wait; the store announces the change when it lands.
func (f *Fetcher) Ensure(instance string, project, pipeline uint64, status string, read time.Time, wait time.Duration) {
	if f == nil || project == 0 || pipeline == 0 || !f.Client.Knows(instance) {
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
		f.inflight, f.last = map[string]chan struct{}{}, map[string]time.Time{}
	}
	now := time.Now()
	done, running := f.inflight[key]
	if !running {
		if now.Before(f.until) || now.Sub(f.last[key]) < every || now.Sub(read) < every {
			f.mu.Unlock()
			return
		}
		done = make(chan struct{})
		f.inflight[key], f.last[key] = done, now
		go f.read(key, instance, project, pipeline, done)
	}
	f.mu.Unlock()
	if wait > 0 {
		select {
		case <-done:
		case <-time.After(wait):
		}
	}
}

func (f *Fetcher) read(key, instance string, project, pipeline uint64, done chan struct{}) {
	defer close(done)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	p, err := f.Client.Pipeline(ctx, instance, project, pipeline)
	if err == nil && len(p.Jobs) > 0 {
		err = f.Store.SaveAPIPipeline(ctx, p)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.inflight, key)
	f.lastErr = err
	var rl *RateLimited
	if errors.As(err, &rl) {
		f.until = rl.Until
	}
	if err != nil && f.Log != nil {
		f.Log.Warn("gitlab api", "pipeline", key, "err", err)
	}
}

// Status says how the last read went, for the operations view.
func (f *Fetcher) Status() (until time.Time, err error) {
	if f == nil {
		return time.Time{}, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.until, f.lastErr
}
