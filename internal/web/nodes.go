package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

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
	Cache     *BucketView
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
	Builds, Farm, Unclear                  int
	Running                                int
	RunningNote                            string
	Total, Images, Layers, Mounts          int64
	Segs                                   []NSeg
	MountRows                              []KV
	Inspected                              bool
	// Disk is the cache's filesystem, from the disk probe; nil without one.
	Disk *DiskView
}

// DiskView is a daemon's disk: what fills it and how close it is to the
// limits that delete cache (BuildKit's GC) or evict pods (the kubelet).
type DiskView struct {
	Segs  []NSeg
	Rows  []KV
	Level string // "", "warn" or "crit"
	Notes []string
	Err   string
}

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
		v.Notes = append(v.Notes, note)
	}
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
	Name  string
	Nodes int
	Size  int64
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

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sh, err := s.shell(r, "nodes", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.LiveNodes = true
	sh.Title = "Build nodes"
	v := &NodesView{Release: s.opt.Release}
	counts, err := s.store.DaemonCounts(ctx, s.now().Add(-24*time.Hour))
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
			Ready: d.Connected, Builds: c.Builds, Farm: c.Farm, Unclear: c.Unclear, Running: len(d.Active), Err: d.InspectErr,
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
		card.Disk = diskView(d.Inspection)
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
	if v.Cache, err = s.bucket(ctx, sh.Project.ProjectKey); err != nil {
		s.fail(w, err)
		return
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

// bucket sums what the runners' S3 cache holds for a project; nil when no
// cache is listed.
func (s *Server) bucket(ctx context.Context, p store.ProjectKey) (*BucketView, error) {
	objs, stores, err := s.store.ProjectCache(ctx, p)
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
	v.Scanned = ago(at)
	return v, nil
}
