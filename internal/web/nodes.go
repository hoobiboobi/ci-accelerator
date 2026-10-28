package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// ---- build nodes ----

// NodesView is the build farm at a glance: one row per daemon, the ones
// that need something first, and below what is the same on every node.
type NodesView struct {
	Shell
	Release      string
	Ready, Total int
	// Builds and Running sum up the daemons shown, 24 hours.
	Builds, Running int
	// Attention counts the daemons that need something: not reachable,
	// a disk warning, or their cache could not be read.
	Attention int
	Filtered  bool // only those (?show=attention)
	Hidden    int  // the daemons the filter leaves out
	// BuildKit is its version when every daemon runs the same one;
	// Mixed: they do not, and the table shows each.
	BuildKit string
	Mixed    bool
	Probe    bool // a daemon has a disk probe: the Disk column
	Router   bool // the build routers made decisions: the Routed column
	Rows     []NodeRow
	Notes    []NodeNote
	Where    *WhereView
	Dups     []Dup
	DupMore  []Dup
	DupBytes int64
	GC       []GCRow
	Caps     []KV
	Cache    *BucketView
	// Builds served by daemons that are gone (replaced by a rollout):
	// they count in the projects' totals, not in any row.
	GoneBuilds, GoneDaemons int
	// RouterLine sums up the build routers' decisions of the last 24
	// hours; empty when they made none (no router).
	RouterLine string
	APIStatus  string
	Measured   string
}

// NodeRow is one daemon in the table, and the head of its own page.
type NodeRow struct {
	Name, Short, Node, Version string
	URL                        string
	Ready                      bool
	Down                       string // how long it has not been reachable
	// Level is why it needs attention: "bad" (not reachable), "crit" or
	// "warn" (its disk, or its cache could not be read); "" when nothing.
	Level                 string
	Builds, Farm, Unclear int
	Running               int
	Stuck                 string // the oldest running build's time, when over an hour
	Projects              int
	Routed, Warm          int
	WarmShare             string // Warm of Routed, "98 %"
	Cache                 int64
	Inspected             bool
	Err                   string
	Segs                  []NSeg // the cache by kind
	Disk                  *DiskView
}

// NodeNote is one problem, with the nodes it is on.
type NodeNote struct {
	Level, Title, Text string
	Nodes              []string
}

// WhereView is where the project the pages show built in the last 24
// hours: the daemons, and how many did not build it.
type WhereView struct {
	Project string
	Rows    []WhereRow
	Others  int
	Gone    int // its builds on daemons replaced since
}

// WhereRow is one daemon that built the project.
type WhereRow struct {
	Short, Node, URL      string
	Builds, Farm, Unclear int
	Last                  Stamp
}

// NodeView is one daemon's page.
type NodeView struct {
	Shell
	Row         NodeRow
	Persists    bool
	RunningNote string
	Measured    string
	Mounts      []MountRow
	MoreMounts  int
	// Built are the projects it built for (Shell.Projects is the
	// sidebar's), More the rest after the first nodeProjects, folded.
	Built      []NodeProject
	More       []NodeProject
	Unreadable int // projects the reader may not see
	RoutedRows []KV
	Active     []ActiveRow
	GC         []GCRow
	Caps       []KV
	// Replaced counts this node's builds on daemons replaced since.
	Replaced int
}

// MountRow is one cache mount of a daemon.
type MountRow struct{ Name, Size, Until string }

// NodeProject is a project a daemon built for.
type NodeProject struct {
	Name, URL string
	Builds    int
	Share     string // of the most builds, for the bar
	Current   bool   // the project the pages show
}

// ActiveRow is a build running on a daemon.
type ActiveRow struct {
	Target, For string
	Stuck       bool
}

// nodeProjects is how many projects a node's page lists before folding
// the rest; nodeMounts how many cache mounts.
const (
	nodeProjects = 25
	nodeMounts   = 10
	dupsShown    = 5
)

// stuckAfter is when a running build is worth a question mark.
const stuckAfter = time.Hour

// nodeRow is what the table and a node's page say about a daemon.
func (s *Server) nodeRow(d collector.DaemonState, c store.DaemonCount, loc *time.Location) NodeRow {
	row := NodeRow{Name: d.Name, Short: shortDaemon(d.Name), Node: d.Inspection.Node, Version: d.Inspection.Version,
		URL: nodeURL(d.Name), Ready: d.Connected, Builds: c.Builds, Farm: c.Farm, Unclear: c.Unclear,
		Running: len(d.Active), Projects: c.ProjectCount, Err: d.InspectErr, Inspected: !d.InspectedAt.IsZero()}
	if row.Node == "" {
		row.Node = c.Node
	}
	if !d.Connected {
		row.Down = duration(s.now().Sub(d.Since).Truncate(time.Minute))
		if s.now().Sub(d.Since) < time.Minute {
			row.Down = "under a minute"
		}
	}
	if len(d.Active) > 0 {
		if oldest := s.now().Sub(d.Active[0].Started); oldest > stuckAfter {
			row.Stuck = duration(oldest.Truncate(time.Minute))
		}
	}
	if n, warm, _ := routed(c.Routed); n > 0 {
		row.Routed, row.Warm = n, warm
		row.WarmShare = fmt.Sprintf("%d %%", warm*100/n)
	}
	u := d.Inspection.Usage
	row.Cache = u.Total
	for _, sg := range []struct {
		class, label string
		n            int64
	}{{"image", "Base images", u.Images}, {"built", "Build layers", u.Layers}, {"received", "Cache mounts", u.Mounts},
		{"xfer", "Contexts", u.Contexts}, {"runner", "Other", u.Other}} {
		if sg.n > 0 && u.Total > 0 {
			row.Segs = append(row.Segs, NSeg{Class: sg.class, Label: sg.label, Size: bytesText(sg.n),
				Flex: strconv.FormatFloat(100*float64(sg.n)/float64(u.Total), 'f', 2, 64)})
		}
	}
	row.Disk = diskView(d.Inspection)
	switch {
	case !d.Connected:
		row.Level = "bad"
	case row.Disk != nil && row.Disk.Level != "":
		row.Level = row.Disk.Level
	case d.InspectErr != "":
		row.Level = "warn"
	}
	return row
}

// nodeURL is a daemon's page.
func nodeURL(name string) string { return "/nodes/" + url.PathEscape(name) }

// levelRank orders the table: not reachable, a full disk, a warning, the rest.
func levelRank(l string) int {
	switch l {
	case "bad":
		return 0
	case "crit":
		return 1
	case "warn":
		return 2
	}
	return 3
}

// nodeLabel names a daemon for a list: "4hxsv on node-1".
func nodeLabel(r NodeRow) string {
	if r.Node == "" {
		return r.Short
	}
	return r.Short + " on " + r.Node
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	loc := zone(r)
	ctx := r.Context()
	sh, err := s.shell(r, "nodes", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.LiveNodes = true
	sh.Title = "Build nodes"
	v := &NodesView{Release: s.opt.Release, Filtered: r.URL.Query().Get("show") == "attention"}
	in := store.ProjectsIn{}
	if sh.Project.ID != 0 && !sh.NoProjects {
		in.Only = sh.Project.ProjectKey
	}
	counts, err := s.data.DaemonCounts(ctx, s.now().Add(-24*time.Hour), in)
	if err != nil {
		s.fail(w, err)
		return
	}
	daemons := s.data.Daemons()
	shown := map[string]bool{}
	for _, d := range daemons {
		shown[d.Name] = true
	}
	all := map[string]int{}
	for name, c := range counts {
		if !shown[name] && c.Builds > 0 {
			v.GoneBuilds += c.Builds
			v.GoneDaemons++
		}
		for k, n := range c.Routed {
			all[k] += n
		}
	}
	if n, warm, _ := routed(all); n > 0 {
		v.Router = true
		v.RouterLine = fmt.Sprintf("The build router, 24 hours: %d build connection%s, %d to a node that had built the project before (%d %%).",
			n, plural(n), warm, warm*100/n)
	}
	images := map[string][]int64{}
	versions := map[string]bool{}
	var gc []collector.GCRule
	var rows []NodeRow
	for _, d := range daemons {
		c := counts[d.Name]
		row := s.nodeRow(d, c, loc)
		v.Total++
		if d.Connected {
			v.Ready++
		}
		v.Builds += row.Builds
		v.Running += row.Running
		if row.Level != "" {
			v.Attention++
		}
		if row.Version != "" {
			versions[row.Version] = true
		}
		if d.Inspection.Disk != nil || d.Inspection.DiskErr != "" {
			v.Probe = true
		}
		for name, n := range d.Inspection.Usage.ImageSizes {
			images[name] = append(images[name], n)
		}
		if len(gc) == 0 {
			gc = d.Inspection.GC
		}
		if !d.InspectedAt.IsZero() {
			v.Measured = "Read from the running daemons at " + stampOf(d.InspectedAt, loc, "15:04:05").Text
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := levelRank(rows[i].Level), levelRank(rows[j].Level); a != b {
			return a < b
		}
		return rows[i].Short < rows[j].Short
	})
	v.Notes = nodeNotes(rows)
	if len(versions) == 1 {
		for ver := range versions {
			v.BuildKit = ver
		}
	}
	v.Mixed = len(versions) > 1
	for _, row := range rows {
		if v.Filtered && row.Level == "" {
			v.Hidden++
			continue
		}
		v.Rows = append(v.Rows, row)
	}
	if in.Only.ID != 0 {
		v.Where = where(sh.Project, daemons, counts, rows, loc)
	}
	v.Dups, v.DupMore, v.DupBytes = duplicates(images)
	v.GC, v.Caps = gcRows(gc)
	if v.Cache, err = s.bucket(ctx, sh.Project.ProjectKey, loc); err != nil {
		s.fail(w, err)
		return
	}
	v.APIStatus = s.apiStatus(loc)
	v.Shell = sh
	s.render(w, "nodes", http.StatusOK, v)
}

// nodeNotes says once what is wrong, naming the nodes it is wrong on, so
// fifty nodes with the same warning give one line.
func nodeNotes(rows []NodeRow) []NodeNote {
	var down, unread []string
	type key struct{ level, text string }
	byText := map[key][]string{}
	var order []key
	for _, r := range rows {
		switch {
		case !r.Ready:
			down = append(down, nodeLabel(r)+" (for "+r.Down+")")
		case r.Err != "":
			unread = append(unread, nodeLabel(r))
		}
		// A daemon that is not reachable has its last reading only.
		if r.Disk == nil || !r.Ready {
			continue
		}
		for _, n := range r.Disk.Notes {
			k := key{n.Level, n.Text}
			if _, ok := byText[k]; !ok {
				order = append(order, k)
			}
			byText[k] = append(byText[k], nodeLabel(r))
		}
	}
	var out []NodeNote
	if len(down) > 0 {
		title := "A daemon is not reachable"
		if len(down) > 1 {
			title = fmt.Sprintf("%d daemons are not reachable", len(down))
		}
		out = append(out, NodeNote{Level: "warn", Title: title, Nodes: down,
			Text: "The console cannot read their build history; their jobs' builds appear once they are back. Check the daemon pods and the NetworkPolicy."})
	}
	sort.SliceStable(order, func(i, j int) bool { return levelRank(order[i].level) < levelRank(order[j].level) })
	for _, k := range order {
		title := "Disk space"
		if k.level == "crit" {
			title = "The disk is nearly full"
		}
		out = append(out, NodeNote{Level: k.level, Title: title, Text: k.text, Nodes: byText[k]})
	}
	if len(unread) > 0 {
		out = append(out, NodeNote{Level: "warn", Title: "A daemon's cache could not be read", Nodes: unread,
			Text: "Its row shows the builds the console stored; the cache and disk figures come back with the next reading (each minute)."})
	}
	return out
}

// where lists the daemons that built the project in the last 24 hours.
func where(p store.Project, daemons []collector.DaemonState, counts map[string]store.DaemonCount, rows []NodeRow, loc *time.Location) *WhereView {
	v := &WhereView{Project: projName(p.Path, p.ID)}
	byName := map[string]NodeRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	live := map[string]bool{}
	for _, d := range daemons {
		live[d.Name] = true
		c := counts[d.Name]
		if len(c.Projects) == 0 {
			v.Others++
			continue
		}
		pb, row := c.Projects[0], byName[d.Name]
		v.Rows = append(v.Rows, WhereRow{Short: row.Short, Node: row.Node, URL: row.URL, Builds: pb.Builds, Farm: pb.Farm,
			Unclear: pb.Unclear, Last: stampOf(pb.Last, loc, "15:04")})
	}
	for name, c := range counts {
		if !live[name] && len(c.Projects) > 0 {
			v.Gone += c.Projects[0].Builds
		}
	}
	sort.SliceStable(v.Rows, func(i, j int) bool {
		if v.Rows[i].Builds != v.Rows[j].Builds {
			return v.Rows[i].Builds > v.Rows[j].Builds
		}
		return v.Rows[i].Short < v.Rows[j].Short
	})
	return v
}

// duplicates are the base images more than one daemon holds, the most
// bytes of copies first: the first few, the rest, and the copies' bytes.
func duplicates(images map[string][]int64) (shown, more []Dup, copies int64) {
	var all []Dup
	for name, sizes := range images {
		if len(sizes) < 2 {
			continue
		}
		d := Dup{Name: name, Nodes: len(sizes), Size: sizes[0]}
		for _, n := range sizes[1:] {
			d.Copies += n
		}
		copies += d.Copies
		all = append(all, d)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Copies != all[j].Copies {
			return all[i].Copies > all[j].Copies
		}
		return all[i].Name < all[j].Name
	})
	if len(all) > dupsShown {
		return all[:dupsShown], all[dupsShown:], copies
	}
	return all, nil, copies
}

// gcRows is a daemon's cache policies in words, and the caps every
// capped policy shares.
func gcRows(gc []collector.GCRule) (rows []GCRow, caps []KV) {
	var capped collector.GCRule
	for _, rule := range gc {
		rows = append(rows, gcRow(rule))
		if len(rule.Filters) == 0 && rule.MaxUsed > capped.MaxUsed {
			capped = rule
		}
	}
	if capped.MaxUsed > 0 {
		caps = []KV{{"Keep cache up to", bytesText(capped.MaxUsed) + " per node"}}
		if capped.Reserved > 0 {
			caps = append(caps, KV{"Always keep", bytesText(capped.Reserved)})
		}
		if capped.MinFree > 0 {
			caps = append(caps, KV{"Keep free on disk", bytesText(capped.MinFree)})
		}
	}
	return rows, caps
}

// apiStatus is GitLab's API as the console last found it.
func (s *Server) apiStatus(loc *time.Location) string {
	if s.opt.GitLab == nil {
		return ""
	}
	until, err := s.opt.GitLab.Status()
	if err == nil {
		return "GitLab API: reachable"
	}
	out := "GitLab API: " + err.Error()
	if until.After(s.now()) {
		out += " (paused until " + stampOf(until, loc, "15:04").Text + ")"
	}
	return out
}

// node is one daemon's page: its disk and cache, the projects it built
// for, what the build routers sent it and what runs on it now.
func (s *Server) node(w http.ResponseWriter, r *http.Request) {
	loc := zone(r)
	ctx := r.Context()
	name := r.PathValue("daemon")
	var d *collector.DaemonState
	daemons := s.data.Daemons()
	for i := range daemons {
		if daemons[i].Name == name {
			d = &daemons[i]
		}
	}
	sh, err := s.shell(r, "nodes", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	since := s.now().Add(-24 * time.Hour)
	counts, err := s.data.DaemonCounts(ctx, since, store.ProjectsIn{All: d != nil})
	if err != nil {
		s.fail(w, err)
		return
	}
	if d == nil {
		s.nodeGone(w, r, sh, name, counts[name], daemons, loc)
		return
	}
	sh.LiveNodes = true
	sh.Title = shortDaemon(name) + " · Build nodes"
	c := counts[name]
	v := &NodeView{Row: s.nodeRow(*d, c, loc), Persists: d.Inspection.Persists}
	if !d.InspectedAt.IsZero() {
		v.Measured = "read at " + stampOf(d.InspectedAt, loc, "15:04:05").Text
	}
	if len(d.Active) == 0 {
		v.RunningNote = "none"
	} else {
		v.RunningNote = "the oldest for " + duration(s.now().Sub(d.Active[0].Started).Truncate(time.Second))
	}
	for _, a := range d.Active {
		age := s.now().Sub(a.Started)
		v.Active = append(v.Active, ActiveRow{Target: a.Target, For: duration(age.Truncate(time.Second)), Stuck: age > stuckAfter})
	}
	keep := mountKeep(d.Inspection.GC)
	for i, m := range d.Inspection.Usage.MountList {
		if i == nodeMounts {
			v.MoreMounts = len(d.Inspection.Usage.MountList) - nodeMounts
			break
		}
		row := MountRow{Name: mountName(m.Description), Size: bytesText(m.Size)}
		if keep > 0 && !m.LastUsed.IsZero() {
			row.Until = m.LastUsed.Add(keep).In(loc).Format("2 Jan")
		}
		v.Mounts = append(v.Mounts, row)
	}
	// The projects the reader may see: the shell asked GitLab about every
	// project already, so this page asks nothing more.
	v.Built, v.More, v.Unreadable = builtFor(c.Projects, sh.Projects, sh.Project.ProjectKey)
	if n, _, _ := routed(c.Routed); n > 0 {
		v.RoutedRows = routedRows(c.Routed)
	}
	v.GC, v.Caps = gcRows(d.Inspection.GC)
	for other, oc := range counts {
		if other != name && oc.Node != "" && oc.Node == v.Row.Node && !isLive(daemons, other) {
			v.Replaced += oc.Builds
		}
	}
	v.Shell = sh
	s.render(w, "node", http.StatusOK, v)
}

// builtFor lists the projects a daemon built for that the reader may see
// (readable), the current one first, then the most builds first: the
// first nodeProjects, the rest, and how many the reader may not see.
func builtFor(built []store.ProjectBuilds, readable []store.Project, current store.ProjectKey) (shown, more []NodeProject, unreadable int) {
	may := map[store.ProjectKey]store.Project{}
	for _, p := range readable {
		may[p.ProjectKey] = p
	}
	var ps []NodeProject
	top := 1
	for _, pb := range built {
		p, ok := may[pb.ProjectKey]
		if !ok {
			unreadable++
			continue
		}
		top = max(top, pb.Builds)
		name := p.Path
		if name == "" {
			name = "project " + strconv.FormatUint(p.ID, 10)
		}
		np := NodeProject{Name: name, URL: "/projects/" + p.Instance + "/" + strconv.FormatUint(p.ID, 10) + "?to=/builds", Builds: pb.Builds,
			Current: p.ProjectKey == current}
		if np.Current {
			ps = append([]NodeProject{np}, ps...)
		} else {
			ps = append(ps, np)
		}
	}
	for i := range ps {
		ps[i].Share = strconv.FormatFloat(100*float64(ps[i].Builds)/float64(top), 'f', 1, 64)
	}
	if len(ps) > nodeProjects {
		return ps[:nodeProjects], ps[nodeProjects:], unreadable
	}
	return ps, nil, unreadable
}

// isLive says whether a daemon of that name is known now.
func isLive(daemons []collector.DaemonState, name string) bool {
	for _, d := range daemons {
		if d.Name == name {
			return true
		}
	}
	return false
}

// nodeGone answers for a daemon that is not there now: replaced by a
// rollout, most likely; its node's daemon now, when there is one.
func (s *Server) nodeGone(w http.ResponseWriter, r *http.Request, sh Shell, name string, c store.DaemonCount, daemons []collector.DaemonState, loc *time.Location) {
	sh.Title = "Not found"
	nf := NotFound{Shell: sh, What: "node",
		Hint: "No daemon named " + name + " is reached now. A rollout or a restart replaces a daemon with a new one; its builds still count in the projects' figures."}
	for _, d := range daemons {
		row := NodeRow{Short: shortDaemon(d.Name), Node: d.Inspection.Node}
		label := nodeLabel(row)
		if c.Node != "" && d.Inspection.Node == c.Node {
			label += " — the same node"
			nf.Links = append([]Option{{Label: label, Value: nodeURL(d.Name)}}, nf.Links...)
			continue
		}
		nf.Links = append(nf.Links, Option{Label: label, Value: nodeURL(d.Name)})
	}
	s.render(w, "notfound", http.StatusNotFound, nf)
}

// routedRows is a daemon's router decisions by why, with their shares.
func routedRows(m map[string]int) []KV {
	n, warm, _ := routed(m)
	var hash, zone, unknown int
	for k, c := range m {
		switch route.Reason(k) {
		case route.LocalWarm, route.ZoneWarm:
		case route.ZoneHash:
			hash += c
		case route.CrossZone:
			zone += c
		default:
			unknown += c
		}
	}
	share := func(c int) string {
		if c == 0 {
			return "0"
		}
		return fmt.Sprintf("%d · %s %%", c, strconv.FormatFloat(100*float64(c)/float64(n), 'f', 1, 64))
	}
	return []KV{{"To a node that had built the project", share(warm)}, {"By hash: no node had built the project", share(hash)},
		{"From another zone", share(zone)}, {"Job not known", share(unknown)}}
}

// DiskView is a daemon's disk: what fills it and how close it is to the
// limits that delete cache (BuildKit's GC) or evict pods (the kubelet).
type DiskView struct {
	Segs  []NSeg
	Rows  []KV
	Level string // "", "warn" or "crit": the worst of Notes
	Notes []DiskNote
	Err   string
	// Avail is the share available to pods ("61 %"): the kubelet's
	// eviction thresholds are on it.
	Avail string
}

// DiskNote is one warning about a disk.
type DiskNote struct{ Level, Text string }

// The kubelet's default hard eviction thresholds for the node's disks.
const (
	evictNodefs  = 0.10
	evictImagefs = 0.15
)

// diskView reads a daemon's disk against its GC floor: free space under
// minFreeSpace makes BuildKit delete cache after each build; available
// space under the kubelet's thresholds evicts pods if the cache shares the
// node's disk (emptyDir, or hostPath on the root disk).
func diskView(in collector.Inspection) *DiskView {
	if in.Disk == nil {
		if in.DiskErr != "" {
			return &DiskView{Err: in.DiskErr}
		}
		return nil
	}
	d := in.Disk
	v := &DiskView{}
	total := float64(d.Total)
	used := d.Total - d.Free
	cache := uint64(max(in.Usage.Total, 0))
	if cache > used {
		cache = used
	}
	pct := func(n uint64) string { return strconv.FormatFloat(100*float64(n)/total, 'f', 2, 64) }
	for _, sg := range []struct {
		class, label string
		n            uint64
	}{{"built", "Cache", cache}, {"runner", "Other files", used - cache}, {"free", "Free", d.Free}} {
		if sg.n > 0 {
			v.Segs = append(v.Segs, NSeg{Class: sg.class, Label: sg.label, Size: bytesText(int64(sg.n)), Flex: pct(sg.n)})
		}
	}
	share := func(n uint64) string { return strconv.FormatFloat(100*float64(n)/total, 'f', 0, 64) + " %" }
	v.Rows = append(v.Rows, KV{"Disk", bytesText(int64(d.Total))},
		KV{"Free", bytesText(int64(d.Free)) + " (" + share(d.Free) + ")"},
		KV{"Available to pods", bytesText(int64(d.Available)) + " (" + share(d.Available) + ")"})
	if d.Inodes > 0 {
		v.Rows = append(v.Rows, KV{"Inodes free", strconv.FormatFloat(100*float64(d.InodesFree)/float64(d.Inodes), 'f', 0, 64) + " %"})
	}
	var minFree int64
	for _, r := range in.GC {
		minFree = max(minFree, r.MinFree)
	}
	raise := func(level, note string) {
		if level == "crit" || v.Level == "" {
			v.Level = level
		}
		v.Notes = append(v.Notes, DiskNote{level, note})
	}
	v.Avail = share(d.Available)
	avail := float64(d.Available) / total
	switch {
	case avail < evictNodefs:
		raise("crit", "Under 10 % available: if the cache shares the node's disk, the kubelet is evicting pods now (the daemon last, by its priority).")
	case avail < evictImagefs:
		raise("warn", "Under 15 % available: close to the kubelet's eviction thresholds if the cache shares the node's disk.")
	}
	if minFree > 0 && int64(d.Free) < minFree {
		raise("warn", "Less free than the garbage collection keeps ("+bytesText(minFree)+"): BuildKit deletes cache after each build until it is.")
	}
	if d.Inodes > 0 && float64(d.InodesFree)/float64(d.Inodes) < 0.05 {
		raise("warn", "Under 5 % of inodes free: the disk fills up by file count before bytes.")
	}
	return v
}

// NSeg is a part of a daemon's cache.
type NSeg struct {
	Class, Label, Size, Flex string
}

// Dup is a base image held by several daemons.
type Dup struct {
	Name   string
	Nodes  int
	Size   int64
	Copies int64 // the bytes of all but one
}

// GCRow is one cache policy in words.
type GCRow struct{ What, Rule string }

// BucketView is what the runners' S3 cache holds for the project.
type BucketView struct {
	Bytes   int64
	Keys    int
	Scanned string
	NoRule  bool
	Err     string
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

// bucket sums what the runners' S3 cache holds for a project; nil when no
// cache is listed.
func (s *Server) bucket(ctx context.Context, p store.ProjectKey, loc *time.Location) (*BucketView, error) {
	objs, stores, err := s.data.ProjectCache(ctx, p)
	if err != nil || len(stores) == 0 {
		return nil, err
	}
	v := &BucketView{Keys: len(objs)}
	for _, o := range objs {
		v.Bytes += o.Bytes
	}
	var at time.Time
	for _, st := range stores {
		if st.Scanned.After(at) {
			at = st.Scanned
		}
		if st.Lifecycle != nil && !*st.Lifecycle {
			v.NoRule = true
		}
		if st.Err != "" {
			v.Err = st.Err
		}
	}
	v.Scanned = ago(at, loc)
	return v, nil
}

// routed sums up build router decisions by reason: how many, how many
// went where the project's cache was, and both in words with the rest by
// why ("12 — 9 to the project's cache, 3 by hash").
func routed(m map[string]int) (total, warm int, text string) {
	var hash, zone, unknown int
	for k, n := range m {
		total += n
		switch route.Reason(k) {
		case route.LocalWarm, route.ZoneWarm:
			warm += n
		case route.ZoneHash:
			hash += n
		case route.CrossZone:
			zone += n
		default: // route.UnknownLocal, route.UnknownHash, a reason of a newer router
			unknown += n
		}
	}
	var parts []string
	for _, p := range []struct {
		n    int
		what string
	}{{warm, "to the project's cache"}, {hash, "by hash: no cache held the project"}, {zone, "from another zone"}, {unknown, "job not known"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.what))
		}
	}
	if total == 0 {
		return 0, 0, ""
	}
	return total, warm, fmt.Sprintf("%d — %s", total, strings.Join(parts, ", "))
}
