package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// ---- home ----

// HomeView is the start page: how developers arrive, the latest pipeline,
// and the jobs rebuilt for reasons other than their code.
type HomeView struct {
	Shell
	Latest  *ListPipe
	Look    []LookRow
	Example *Example
}

// LookRow is a job worth a look.
type LookRow struct {
	URL, Name, Sub, Dur string
	Cat                 Cat
}

// Example is the line a job log carries, built from a real job.
type Example struct {
	Took, URL, Push string
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.shell(r, "pipes", "all")
	if err != nil {
		s.fail(w, err)
		return
	}
	lv, err := s.listFor(ctx, r, &sh, "", "/")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.List, sh.Split, sh.Has, sh.Title = lv, true, "none", "Builds, explained"
	v := &HomeView{}
	for i := range lv.Pipes {
		if len(lv.Pipes[i].Jobs) > 0 {
			v.Latest = &lv.Pipes[i]
			break
		}
	}
	for _, p := range lv.Pipes {
		for _, j := range p.Jobs {
			if (j.Cat.Key == "node" || j.Cat.Key == "unknown") && len(v.Look) < 5 {
				v.Look = append(v.Look, LookRow{URL: j.URL, Name: j.Name, Sub: p.Short + " · " + p.Title, Dur: j.Dur, Cat: j.Cat})
			}
		}
	}
	for _, p := range lv.Pipes {
		for _, j := range p.Jobs {
			if v.Example == nil && strings.HasPrefix(j.URL, "/builds/") {
				v.Example = &Example{Took: j.Dur, URL: origin(r) + "/jobs/" + strings.TrimPrefix(j.Key, "j:")}
			}
		}
	}
	v.Shell = sh
	s.render(w, "home", http.StatusOK, v)
}

// origin is the console's address as the browser reached it.
func origin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// ---- project insights ----

// InsightsView is the project's trends: how much the cache saves, why
// builds were rebuilt, pipeline times and each step's history.
type InsightsView struct {
	Shell
	PipeN, BuildN int
	Range         string
	Median, First time.Duration
	Reused        int
	ReusedAll     int
	Saved         time.Duration
	NodeN         int
	UnclearN      int
	Bars          []HBar
	Pipes         []VBar
	Targets       []TargetTab
	Target        string
	Cols          []MCol
	Rows          []MRow
	Empty         bool
}

// HBar is a reason with how many builds it explains.
type HBar struct {
	Key, Label, Icon, URL, W string
	N                        int
	T                        time.Duration
}

// VBar is one pipeline's time.
type VBar struct {
	URL, Label, H, SHA, Title string
}

// TargetTab is a target of the step history.
type TargetTab struct {
	Name, URL string
	On        bool
}

// MCol is a build in the step history.
type MCol struct{ SHA, URL, Title string }

// MRow is a step across builds.
type MRow struct {
	Key, Name string
	Cells     []MCell
	Reused    int
}

// MCell is a step in one build.
type MCell struct{ State, Label string }

func (s *Server) insights(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.shell(r, "insights", "all")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Project insights"
	v := &InsightsView{}
	if sh.Project.ID == 0 {
		v.Empty, v.Shell = true, sh
		s.render(w, "insights", http.StatusOK, v)
		return
	}
	builds, err := s.store.ProjectBuilds(ctx, sh.Project.ProjectKey, 400)
	if err != nil {
		s.fail(w, err)
		return
	}
	list, err := s.store.List(ctx, sh.Project.ProjectKey, 30)
	if err != nil {
		s.fail(w, err)
		return
	}
	v.BuildN = len(builds)
	// Pipelines oldest first, with their times.
	var durs []time.Duration
	for i := len(list) - 1; i >= 0; i-- {
		p := list[i]
		var start, end time.Time
		for _, j := range p.Jobs {
			jt := timesOf(j.JobInfo, j.Started, j.Completed)
			if start.IsZero() || jt.Start.Before(start) {
				start = jt.Start
			}
			if jt.End.After(end) {
				end = jt.End
			}
		}
		d := end.Sub(start)
		durs = append(durs, d)
		title := p.CommitTitle
		if title == "" {
			title = "#" + strconv.FormatUint(p.ID, 10)
		}
		v.Pipes = append(v.Pipes, VBar{URL: "/pipelines/" + store.PipelineKey(p.Instance, p.ID), Label: duration(d),
			SHA: short(p.SHA, 6), Title: "#" + strconv.FormatUint(p.ID, 10) + " · " + title})
	}
	v.PipeN = len(list)
	var longest time.Duration
	for _, d := range durs {
		longest = max(longest, d)
	}
	for i := range v.Pipes {
		h := 2.0
		if longest > 0 {
			h = 2 + 98*float64(durs[i])/float64(longest)
		}
		v.Pipes[i].H = strconv.FormatFloat(h, 'f', 1, 64)
		if v.Pipes[i].SHA == "" {
			v.Pipes[i].SHA = strings.TrimPrefix(strings.SplitN(v.Pipes[i].Title, " ", 2)[0], "#")
		}
	}
	v.Median = median(durs)
	if len(durs) > 0 {
		v.First = durs[0]
	}
	if len(builds) > 0 {
		v.Range = builds[0].Build.Started.UTC().Format("2 Jan") + " – " + builds[len(builds)-1].Build.Started.UTC().Format("2 Jan 2006")
	}
	// Why builds were rebuilt.
	order := []string{"node", "unknown", "files", "step", "first", "cached", "failed", "canceled"}
	labels := map[string]string{"node": "Other node — not your change", "unknown": "Files or node — unclear", "files": "Files changed",
		"step": "Dockerfile changed", "first": "First build", "cached": "Nothing rebuilt", "failed": "Failed", "canceled": "Canceled"}
	tally := map[string]*HBar{}
	firstPipe := uint64(0)
	if len(list) > 0 {
		firstPipe = list[len(list)-1].ID
	}
	var all, reusedAll, later, reusedLater int
	targets := map[string]bool{}
	for _, st := range builds {
		b := st.Build
		built := 0
		var spent time.Duration
		for _, step := range b.Steps {
			if isFrom(step) {
				continue
			}
			all++
			if step.State != history.Built {
				reusedAll++
			} else {
				built++
				spent += step.Duration()
			}
			if b.Attribution.Pipeline != firstPipe {
				later++
				if step.State != history.Built {
					reusedLater++
				}
			}
		}
		c := catOf(st.Reason.Kind, b.Error, built)
		k := c.Key
		if _, ok := labels[k]; !ok {
			k = "first"
		}
		hb := tally[k]
		if hb == nil {
			hb = &HBar{Key: k, Label: labels[k], Icon: catIcons[k]}
			tally[k] = hb
		}
		hb.N++
		hb.T += spent
		v.Saved += st.Saved
		targets[b.Target] = true
	}
	if t := tally["node"]; t != nil {
		v.NodeN = t.N
	}
	if t := tally["unknown"]; t != nil {
		v.UnclearN = t.N
	}
	if all > 0 {
		v.ReusedAll = 100 * reusedAll / all
	}
	if later > 0 {
		v.Reused = 100 * reusedLater / later
	} else {
		v.Reused = v.ReusedAll
	}
	maxN := 0
	for _, k := range order {
		if t := tally[k]; t != nil {
			maxN = max(maxN, t.N)
		}
	}
	for _, k := range order {
		t := tally[k]
		if t == nil {
			continue
		}
		t.W = strconv.FormatFloat(100*float64(t.N)/float64(maxN), 'f', 1, 64)
		t.URL = withFilter("/", k)
		v.Bars = append(v.Bars, *t)
	}
	// Step history of one target.
	names := sortedKeys(targets)
	v.Target = r.URL.Query().Get("target")
	if !targets[v.Target] && len(names) > 0 {
		v.Target = names[0]
		for _, n := range names {
			if n == "worker" || n == "api" {
				v.Target = n
				break
			}
		}
	}
	for _, n := range names {
		label := n
		if label == "" {
			label = "(default)"
		}
		v.Targets = append(v.Targets, TargetTab{Name: label, URL: "/insights?target=" + n, On: n == v.Target})
	}
	var tb []*store.Stored
	for _, st := range builds {
		if st.Build.Target == v.Target {
			tb = append(tb, st)
		}
	}
	if len(tb) > 12 {
		tb = tb[len(tb)-12:]
	}
	shas := map[uint64]string{}
	for _, p := range list {
		shas[p.ID] = short(p.SHA, 6)
	}
	var keys []string
	seen := map[string]bool{}
	for _, st := range tb {
		col := MCol{URL: fmt.Sprintf("/builds/%d", st.ID), SHA: shas[st.Build.Attribution.Pipeline]}
		if col.SHA == "" {
			col.SHA = "#" + strconv.FormatInt(st.ID, 10)
		}
		col.Title = fmt.Sprintf("Pipeline #%d", st.Build.Attribution.Pipeline)
		v.Cols = append(v.Cols, col)
		for _, step := range st.Build.Steps {
			if isFrom(step) {
				continue
			}
			k := step.Key + "|" + step.Name
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	for _, k := range keys {
		key, name, _ := strings.Cut(k, "|")
		row := MRow{Key: key, Name: name}
		present, reused := 0, 0
		for i, st := range tb {
			cell := MCell{State: "none", Label: "step not in this build"}
			for _, step := range st.Build.Steps {
				if step.Key+"|"+step.Name == k {
					cell.State = string(step.State)
					cell.Label = fmt.Sprintf("%s · %s · %s", v.Cols[i].SHA, stateLabels[cell.State], duration(step.Duration()))
					present++
					if step.State != history.Built {
						reused++
					}
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		if present > 0 {
			row.Reused = 100 * reused / present
		}
		v.Rows = append(v.Rows, row)
	}
	v.Shell = sh
	s.render(w, "insights", http.StatusOK, v)
}

var catIcons = map[string]string{"node": "v_node", "unknown": "v_unknown", "files": "v_files", "step": "v_step",
	"first": "v_first", "cached": "v_cached", "failed": "warn", "canceled": "x"}

// ---- build nodes ----

// NodesView is the operations view: every daemon, what its cache holds
// and how it is collected.
type NodesView struct {
	Shell
	Release   string
	Ready     int
	Total     int
	Cards     []NodeCard
	Dups      []Dup
	DupBytes  int64
	GC        []GCRow
	Caps      []KV
	ProjRows  []ProjRow
	APIStatus string
	Measured  string
	// Builds served by daemons that are gone (replaced by a rollout):
	// they count in the projects' totals, not on any card.
	GoneBuilds, GoneDaemons int
}

// NodeCard is one daemon.
type NodeCard struct {
	Name, Short, Node, Version, Err, Since string
	Ready                                  bool
	Builds, OtherNode, Unclear             int
	Running                                int
	RunningNote                            string
	Total, Images, Layers, Mounts          int64
	Segs                                   []NSeg
	MountRows                              []KV
	Inspected                              bool
}

// NSeg is a part of a daemon's cache.
type NSeg struct {
	Class, Label, Size, Flex string
}

// Dup is a base image held by several daemons.
type Dup struct {
	Name  string
	Nodes int
	Size  int64
}

// GCRow is one cache policy in words.
type GCRow struct{ What, Rule string }

// ProjRow is a project's builds on the box.
type ProjRow struct {
	Path, Builds, Pipelines, Last string
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.shell(r, "nodes", "all")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.LiveNodes = true
	sh.Title = "Build nodes"
	v := &NodesView{Release: s.opt.Release}
	counts, err := s.store.DaemonCounts(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	var daemons []collector.DaemonState
	if s.opt.Live != nil {
		daemons = s.opt.Live.Daemons()
	}
	images := map[string][]int64{}
	var gc []collector.GCRule
	shown := map[string]bool{}
	for _, d := range daemons {
		shown[d.Name] = true
	}
	for name, c := range counts {
		if !shown[name] && c.Builds > 0 {
			v.GoneBuilds += c.Builds
			v.GoneDaemons++
		}
	}
	for _, d := range daemons {
		c := counts[d.Name]
		card := NodeCard{Name: d.Name, Short: shortDaemon(d.Name), Node: d.Inspection.Node, Version: d.Inspection.Version,
			Ready: d.Connected, Builds: c.Builds, OtherNode: c.OtherNode, Unclear: c.Unclear, Running: len(d.Active), Err: d.InspectErr,
			Inspected: !d.InspectedAt.IsZero()}
		if d.Connected {
			v.Ready++
		} else {
			card.Since = "unreachable since " + ago(d.Since)
		}
		v.Total++
		if card.Running == 0 {
			card.RunningNote = "none"
		} else {
			oldest := s.now().Sub(d.Active[0].Started)
			card.RunningNote = "the oldest for " + duration(oldest.Truncate(time.Second))
			if oldest > time.Hour {
				card.RunningNote += " — stuck?"
			}
		}
		u := d.Inspection.Usage
		card.Total, card.Images, card.Layers, card.Mounts = u.Total, u.Images, u.Layers, u.Mounts
		for _, sg := range []struct {
			class, label string
			n            int64
		}{{"image", "Base images", u.Images}, {"built", "Build layers", u.Layers}, {"received", "Cache mounts", u.Mounts},
			{"xfer", "Contexts", u.Contexts}, {"runner", "Other", u.Other}} {
			if sg.n > 0 && u.Total > 0 {
				card.Segs = append(card.Segs, NSeg{Class: sg.class, Label: sg.label, Size: bytesText(sg.n),
					Flex: strconv.FormatFloat(100*float64(sg.n)/float64(u.Total), 'f', 2, 64)})
			}
		}
		keep := mountKeep(d.Inspection.GC)
		for i, m := range u.MountList {
			if i == 3 {
				break
			}
			val := bytesText(m.Size)
			if keep > 0 && !m.LastUsed.IsZero() {
				val += " · kept until " + m.LastUsed.Add(keep).UTC().Format("2 Jan")
			}
			card.MountRows = append(card.MountRows, KV{mountName(m.Description), val})
		}
		for name, n := range u.ImageSizes {
			images[name] = append(images[name], n)
		}
		if len(gc) == 0 {
			gc = d.Inspection.GC
		}
		if !d.InspectedAt.IsZero() {
			v.Measured = "Read from the running daemons at " + d.InspectedAt.UTC().Format("15:04:05 UTC")
		}
		v.Cards = append(v.Cards, card)
	}
	for _, name := range sortedKeys(images) {
		sizes := images[name]
		if len(sizes) < 2 {
			continue
		}
		dup := Dup{Name: name, Nodes: len(sizes), Size: sizes[0]}
		var sum int64
		for _, n := range sizes {
			sum += n
		}
		v.DupBytes += sum - sizes[0]
		v.Dups = append(v.Dups, dup)
	}
	var capped collector.GCRule
	for _, rule := range gc {
		v.GC = append(v.GC, gcRow(rule))
		if len(rule.Filters) == 0 && rule.MaxUsed > capped.MaxUsed {
			capped = rule
		}
	}
	if capped.MaxUsed > 0 {
		v.Caps = []KV{{"Keep cache up to", bytesText(capped.MaxUsed) + " per node"}}
		if capped.Reserved > 0 {
			v.Caps = append(v.Caps, KV{"Always keep", bytesText(capped.Reserved)})
		}
		if capped.MinFree > 0 {
			v.Caps = append(v.Caps, KV{"Keep free on disk", bytesText(capped.MinFree)})
		}
	}
	for _, p := range sh.Projects {
		path := p.Path
		if path == "" {
			path = fmt.Sprintf("project %d", p.ID)
		}
		v.ProjRows = append(v.ProjRows, ProjRow{Path: path, Builds: strconv.Itoa(p.Builds), Pipelines: strconv.Itoa(p.Pipelines), Last: ago(p.Last)})
	}
	if s.opt.API != nil {
		if until, err := s.opt.API.Status(); err != nil {
			v.APIStatus = "GitLab API: " + err.Error()
			if until.After(s.now()) {
				v.APIStatus += " (paused until " + until.UTC().Format("15:04 UTC") + ")"
			}
		} else {
			v.APIStatus = "GitLab API: reachable"
		}
	}
	v.Shell = sh
	s.render(w, "nodes", http.StatusOK, v)
}

// mountKeep is how long cache mounts are kept after their last use.
func mountKeep(rules []collector.GCRule) time.Duration {
	for _, r := range rules {
		for _, f := range r.Filters {
			if strings.Contains(f, "exec.cachemount") {
				return r.Keep
			}
		}
	}
	return 0
}

// mountName shortens a cache mount's description to its path.
func mountName(desc string) string {
	d := strings.TrimPrefix(desc, "cached mount ")
	if i := strings.Index(d, " from exec"); i > 0 {
		d = d[:i]
	}
	return d + " cache mount"
}

func gcRow(r collector.GCRule) GCRow {
	what := "Other cache, by size"
	if r.Keep > 0 {
		what = "Other cache, by age"
	}
	for _, f := range r.Filters {
		switch {
		case strings.Contains(f, "exec.cachemount"):
			what = "Cache mounts"
		case strings.Contains(f, "source.local") || strings.Contains(f, "source.git.checkout"):
			what = "Build contexts and checkouts"
		}
	}
	if r.All && len(r.Filters) == 0 {
		what = "All cache, shared too"
	}
	var parts []string
	if r.Keep > 0 {
		parts = append(parts, keepText(r.Keep)+" after last use")
	}
	if r.MaxUsed > 0 {
		parts = append(parts, "max "+bytesText(r.MaxUsed))
	}
	if r.MinFree > 0 {
		parts = append(parts, "keep "+bytesText(r.MinFree)+" free")
	}
	if len(parts) == 0 {
		parts = append(parts, "no limit")
	}
	return GCRow{What: what, Rule: strings.Join(parts, ", ")}
}

func keepText(d time.Duration) string {
	if d >= 48*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
	return fmt.Sprintf("%d h", int(d.Hours()))
}

// ---- search index ----

type indexItem struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	Sub   string `json:"sub"`
	URL   string `json:"url"`
	Icon  string `json:"icon"`
}

// index lists what the search can jump to: jobs and pipelines of the
// project, and the pages.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.shell(r, "", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	list, err := s.store.List(ctx, sh.Project.ProjectKey, 60)
	if err != nil {
		s.fail(w, err)
		return
	}
	var items []indexItem
	for _, p := range list {
		title := p.CommitTitle
		if title == "" {
			title = "Pipeline #" + strconv.FormatUint(p.ID, 10)
		}
		for _, j := range p.Jobs {
			c := catOf(j.Reason, j.Error, j.Built)
			items = append(items, indexItem{Kind: "Job", Label: j.Name, URL: fmt.Sprintf("/builds/%d", j.BuildID), Icon: c.Icon,
				Sub: fmt.Sprintf("#%d · %s · %s · %s", p.ID, short(p.SHA, 7), title, c.Label)})
		}
		items = append(items, indexItem{Kind: "Pipeline", Label: title, URL: "/pipelines/" + store.PipelineKey(p.Instance, p.ID), Icon: "pipe",
			Sub: fmt.Sprintf("#%d · %s · %s", p.ID, short(p.SHA, 7), p.Ref)})
	}
	if s.opt.Live != nil {
		for _, d := range s.opt.Live.Daemons() {
			items = append(items, indexItem{Kind: "Node", Label: shortDaemon(d.Name), URL: "/nodes", Icon: "nodes", Sub: d.Name + paren(d.Inspection.Node)})
		}
	}
	items = append(items,
		indexItem{Kind: "Page", Label: "Pipelines", URL: "/", Icon: "pipes"},
		indexItem{Kind: "Page", Label: "Project insights", URL: "/insights", Icon: "insights"},
		indexItem{Kind: "Page", Label: "Build nodes", URL: "/nodes", Icon: "nodes"})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{"items": items})
}

// ---- live updates ----

// events streams which pipelines changed (server-sent events). With
// ?pipeline=<instance>/<id> only that pipeline's changes are sent; other
// pages get every change. A page that receives one fetches itself again.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	want := r.URL.Query().Get("pipeline")
	// The daemons' live state changes every time a build starts or ends
	// and on every inspection that finds something new: only pages that
	// show it ask for it.
	nodes := r.URL.Query().Get("nodes") != ""
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
	sub := s.store.Subscribe()
	defer s.store.Unsubscribe(sub)
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
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": ping\n\n")
		case <-sub.C:
			for _, k := range sub.Take() {
				switch {
				case k == "":
					if nodes {
						fmt.Fprint(w, "event: changed\ndata: live\n\n")
					}
				case want == "" || k == want:
					fmt.Fprintf(w, "event: changed\ndata: %s\n\n", k)
				}
			}
		}
		flusher.Flush()
	}
}

var _ = sort.Strings
