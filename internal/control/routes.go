package control

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// The build routers ask the controller where projects are warm and tell
// it where they sent jobs:
//
//	GET  /internal/v1/route/warm       Warm (ETag; If-None-Match answers 304)
//	POST /internal/v1/route/decisions  body: [store.Decision]
//
// A router keeps working on its last answer while the controller is away.

// Warm is the routers' view of the farm.
type Warm struct {
	Version  string        `json:"version"`
	Projects []WarmProject `json:"projects"`
	// Active is how many builds each node's daemon runs now.
	Active map[string]int `json:"active"`
}

// WarmProject is one project's activity by node.
type WarmProject struct {
	Project  route.Project    `json:"project"`
	Activity []route.Activity `json:"activity"`
}

// Routes serves the routers from the store and the collector.
type Routes struct {
	Store *store.Store
	Live  Live // may be nil
	Log   *slog.Logger
	// Window is how far back a build warms its node (default a week);
	// Every is how long an answer is reused (default 2 s).
	Window, Every time.Duration
	Now           func() time.Time

	mu   sync.Mutex
	last Warm
	body []byte
	at   time.Time
}

func (rt *Routes) now() time.Time {
	if rt.Now != nil {
		return rt.Now()
	}
	return time.Now()
}

// Snapshot computes (or reuses) the routers' view.
func (rt *Routes) Snapshot(r *http.Request) (Warm, []byte, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	every := rt.Every
	if every == 0 {
		every = 2 * time.Second
	}
	now := rt.now()
	if rt.body != nil && now.Sub(rt.at) < every {
		return rt.last, rt.body, nil
	}
	window := rt.Window
	if window == 0 {
		window = 7 * 24 * time.Hour
	}
	var live []string
	if rt.Live != nil {
		live = []string{}
		for _, d := range rt.Live.Daemons() {
			live = append(live, d.Name)
		}
	}
	warmth, err := rt.Store.Warm(r.Context(), now.Add(-window), live)
	if err != nil {
		return Warm{}, nil, err
	}
	w := Warm{Active: map[string]int{}}
	for p, acts := range warmth {
		sort.Slice(acts, func(i, j int) bool { return acts[i].Node < acts[j].Node })
		w.Projects = append(w.Projects, WarmProject{Project: p, Activity: acts})
	}
	sort.Slice(w.Projects, func(i, j int) bool { return w.Projects[i].Project.Key() < w.Projects[j].Project.Key() })
	if rt.Live != nil {
		for _, d := range rt.Live.Daemons() {
			if n := d.Inspection.Node; n != "" && d.Connected {
				w.Active[n] += len(d.Active)
			}
		}
	}
	b, _ := json.Marshal(w)
	sum := sha256.Sum256(b)
	w.Version = hex.EncodeToString(sum[:8])
	rt.last, rt.at = w, now
	rt.body, _ = json.Marshal(w)
	return rt.last, rt.body, nil
}

func (rt *Routes) warm(w http.ResponseWriter, r *http.Request) {
	snap, body, err := rt.Snapshot(r)
	if err != nil {
		rt.Log.Warn("routers' view not read", "err", err)
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	etag := `"` + snap.Version + `"`
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// maxDecisions bounds one report.
const maxDecisions = 1000

func (rt *Routes) decisions(w http.ResponseWriter, r *http.Request) {
	var ds []store.Decision
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&ds); err != nil {
		http.Error(w, "decisions: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(ds) > maxDecisions {
		http.Error(w, "too many decisions in one report", http.StatusRequestEntityTooLarge)
		return
	}
	now := rt.now()
	for i := range ds {
		d := &ds[i]
		if d.Node == "" || d.Daemon == "" || d.Reason == "" {
			http.Error(w, "a decision needs node, daemon and reason", http.StatusBadRequest)
			return
		}
		// A router's clock is not trusted to be ahead of the controller's.
		if d.At.IsZero() || d.At.After(now) {
			d.At = now
		}
	}
	if err := rt.Store.SaveDecisions(r.Context(), ds); err != nil {
		rt.Log.Warn("decisions not stored", "err", err)
		http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		return
	}
	rt.mu.Lock()
	rt.body = nil // the next view includes them
	rt.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// RouterClient is a router's view of the controller.
type RouterClient struct {
	URL   string
	HTTP  *http.Client
	Token func() (string, error)

	etag string
	last Warm
}

func (c *RouterClient) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if c.Token != nil {
		t, err := c.Token()
		if err != nil {
			return nil, fmt.Errorf("controller token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}
	if path == "/internal/v1/route/warm" && c.etag != "" {
		req.Header.Set("If-None-Match", c.etag)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	h := c.HTTP
	if h == nil {
		h = http.DefaultClient
	}
	return h.Do(req)
}

// Warm reads the controller's view; changed is false when it is the one
// read last time.
func (c *RouterClient) Warm(ctx context.Context) (w Warm, changed bool, err error) {
	res, err := c.do(ctx, http.MethodGet, "/internal/v1/route/warm", nil)
	if err != nil {
		return c.last, false, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNotModified:
		return c.last, false, nil
	case http.StatusOK:
	default:
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return c.last, false, fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<20)).Decode(&w); err != nil {
		return c.last, false, err
	}
	c.last, c.etag = w, res.Header.Get("ETag")
	return w, true, nil
}

// Report sends decisions.
func (c *RouterClient) Report(ctx context.Context, ds []store.Decision) error {
	b, err := json.Marshal(ds)
	if err != nil {
		return err
	}
	res, err := c.do(ctx, http.MethodPost, "/internal/v1/route/decisions", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(b)))
	}
	return nil
}
