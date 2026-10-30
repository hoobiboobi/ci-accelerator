package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

type indexItem struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Sub   string `json:"sub"`
	URL   string `json:"url"`
	Icon  string `json:"icon"`
}

// index lists what the search can jump to: the pages, the project's
// images, its latest pipelines with builds, the nodes.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.shell(r, "", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	items := []indexItem{
		{Kind: "Page", Label: "Pipelines", URL: "/", Icon: "pipes"},
		{Kind: "Page", Label: "Insights", URL: "/insights", Icon: "overview"},
		{Kind: "Page", Label: "Builds", URL: "/builds", Icon: "builds"},
		{Kind: "Page", Label: "Images", URL: "/images", Icon: "images"},
		{Kind: "Page", Label: "Nodes", URL: "/nodes", Icon: "nodes"},
	}
	targets, err := s.data.Targets(ctx, sh.Project.ProjectKey, s.now().Add(-30*24*time.Hour))
	if err != nil {
		s.fail(w, err)
		return
	}
	names := s.imageNames(ctx, sh.Project.ProjectKey, s.now().Add(-30*24*time.Hour))
	for _, t := range targets {
		items = append(items, indexItem{Kind: "Image", Label: imageName(t, names[t]), URL: "/images/" + url.PathEscape(imageKey(t)), Icon: "images"})
	}
	rows, err := s.data.Builds(ctx, store.BuildQuery{Project: sh.Project.ProjectKey, Limit: 500})
	if err != nil {
		s.fail(w, err)
		return
	}
	seen := map[uint64]bool{}
	for _, b := range rows {
		if b.Pipeline == 0 || seen[b.Pipeline] {
			continue
		}
		seen[b.Pipeline] = true
		title := b.Title
		if title == "" {
			title = fmt.Sprintf("Pipeline #%d", b.Pipeline)
		}
		items = append(items, indexItem{Kind: "Pipeline", Label: title, URL: "/pipelines/" + store.PipelineKey(b.Instance, b.Pipeline), Icon: "pipe",
			Sub: joinSet(" · ", fmt.Sprintf("#%d", b.Pipeline), short(b.SHA, 7), b.Ref)})
	}
	for _, d := range s.data.Daemons() {
		items = append(items, indexItem{Kind: "Node", Label: shortDaemon(d.Name), URL: nodeURL(d.Name), Icon: "nodes", Sub: d.Name + paren(d.Inspection.Node)})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"items": items})
}

// ---- live updates ----

// events streams which pipelines changed (server-sent events): with
// ?pipeline=<instance>/<id> only that pipeline's changes, with
// ?project=<instance>/<id> only that project's. A page on a pipeline
// fetches itself again; a page on a project says something is new.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	want := r.URL.Query().Get("pipeline")
	wantProject, byProject := store.ParseProjectKey(r.URL.Query().Get("project"))
	projectOf := map[string]uint64{}
	// The daemons' live state changes every time a build starts or ends
	// and on every inspection that finds something new: only pages that
	// show it ask for it.
	nodes := r.URL.Query().Get("nodes") != ""
	// A page on the daemons alone (Nodes) hears of nothing else.
	onlyNodes := nodes && want == "" && !byProject
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		http.Error(w, "too many live pages open", http.StatusServiceUnavailable)
		return
	}
	sub := s.data.Subscribe()
	defer s.data.Unsubscribe(sub)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// The browser waits this long before it reconnects.
	fmt.Fprint(w, "retry: 5000\n\n")
	flusher.Flush()
	beat := time.NewTicker(s.Heartbeat)
	defer beat.Stop()
	// With sign-in, a person hears only of pipelines they may see. The
	// answers are kept for a minute (GitLab's own answer may change), and
	// a failed lookup is asked again with the next change.
	type answer struct {
		ok bool
		at time.Time
	}
	allowed := map[string]answer{}
	hear := func(k string) bool {
		if s.opt.Auth == nil {
			return true
		}
		if a, seen := allowed[k]; seen && time.Since(a.at) < time.Minute {
			return a.ok
		}
		pk, valid := store.ParseProjectKey(k)
		if !valid {
			return false
		}
		project, err := s.data.PipelineProject(r.Context(), pk.Instance, pk.ID)
		if err != nil {
			return false
		}
		ok, known := s.mayKnown(r, pk.Instance, project)
		if known {
			if len(allowed) > 2000 {
				clear(allowed)
			}
			allowed[k] = answer{ok, time.Now()}
		}
		return ok
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
		case <-sub.C:
			for _, k := range sub.Take() {
				switch {
				case k == "*":
					// The source of changes was away: whatever the page
					// shows may have changed.
					fmt.Fprint(w, "event: changed\ndata: *\n\n")
				case k == "":
					if nodes {
						fmt.Fprint(w, "event: changed\ndata: live\n\n")
					}
				case byProject:
					pk, valid := store.ParseProjectKey(k)
					if !valid {
						continue
					}
					project, known := projectOf[k]
					if !known {
						var err error
						if project, err = s.data.PipelineProject(r.Context(), pk.Instance, pk.ID); err != nil {
							// Asked again with the pipeline's next change.
							continue
						}
						if len(projectOf) > 2000 {
							clear(projectOf)
						}
						projectOf[k] = project
					}
					if pk.Instance == wantProject.Instance && project == wantProject.ID && hear(k) {
						fmt.Fprintf(w, "event: changed\ndata: %s\n\n", k)
					}
				case onlyNodes:
				case (want == "" || k == want) && hear(k):
					fmt.Fprintf(w, "event: changed\ndata: %s\n\n", k)
				}
			}
		}
		flusher.Flush()
	}
}

// origin is the console's address as the browser reached it.
func origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// joinSet joins the parts that are not empty.
func joinSet(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
