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
	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// In a fleet hub every cluster's pages are this package's pages, served
// under /m/<name>/ (Options.Base) from that cluster's controller; the hub's
// own pages (Members, sign-in) are served at the root by a Server over
// Empty. Links in the pages are written from the root and moved under the
// base when a page is rendered; a link to the hub itself is written
// "/~/..." and stays at the root.

// FleetMember is one cluster as the hub's pages show it.
type FleetMember struct {
	Name string
	// State: connected, offline or removed.
	State       string
	Version     string
	Since       time.Time // connected since
	SeenAt      time.Time // last connected
	Remote      string
	Fingerprint string
	Origin      string
	AddedBy     string
	AddedAt     time.Time
	// StoreReplaced: when another store came under this name.
	StoreReplaced time.Time
	// RemovedAt: when a removed member was removed (zero: not known).
	RemovedAt time.Time
}

// FleetShell is the fleet part of every page's frame.
type FleetShell struct {
	Current string
	Members []FleetMember
	// Connected: how many of Members are connected.
	Connected int
	// Access: what the person may open of the hub's own pages.
	Access HubAccess
	// all: every member, removed ones too, as read for this page.
	all []FleetMember
}

func (s *Server) fleetShell(r *http.Request) *FleetShell {
	if s.opt.Fleet == nil {
		return nil
	}
	if s.opt.Auth != nil && viewer(r) == nil {
		// Nobody signed in (a 404 under /auth/, say): no word of the fleet.
		return nil
	}
	ms := s.opt.Fleet.Members()
	fs := &FleetShell{Current: s.opt.Fleet.Current(), Access: s.hubAccess(r), all: ms}
	if s.opt.Hub == nil {
		// A hub without its administration (tests): Members for everyone.
		fs.Access.Members = true
	}
	for _, m := range ms {
		if m.State == "removed" {
			continue
		}
		fs.Members = append(fs.Members, m)
		if m.State == "connected" {
			fs.Connected++
		}
	}
	return fs
}

// MemberRow is a row of the Members page.
type MemberRow struct {
	FleetMember
	URL        string
	StateChip  string // ok, warn, plain
	StateText  string
	Contact    Ago
	Added      Ago
	ShortPrint string
}

// MembersPage is the hub's list of clusters.
type MembersPage struct {
	Shell
	Rows      []MemberRow
	Removed   []MemberRow
	Connected int
	Total     int
	// Pending: clusters added at the hub that have not joined yet.
	Pending []string
	// Manage: the person may add and change clusters.
	Manage bool
	// Message after a change; Bad: it failed.
	Message string
	Bad     bool
	// SignIn: people sign in at this hub (without it nothing changes from
	// the pages).
	SignIn bool
}

func (s *Server) members(w http.ResponseWriter, r *http.Request) {
	if s.opt.Hub != nil && !s.hubAccess(r).Members {
		s.viewerHome(w, r)
		return
	}
	sh, err := s.shell(r, "members", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Members"
	v := MembersPage{Shell: sh, SignIn: s.opt.Auth != nil}
	v.Message, v.Bad = s.flash(w, r)
	if s.opt.Hub != nil {
		v.Manage = s.canManage(r)
	}
	now := s.now()
	var ms []FleetMember
	if sh.Fleet != nil {
		ms = append(ms, sh.Fleet.all...)
	} else {
		ms = s.opt.Fleet.Members()
	}
	v.Pending = s.pending(r, ms)
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	for _, m := range ms {
		row := MemberRow{FleetMember: m, URL: "/~/m/" + m.Name + "/", Added: agoAt(m.AddedAt, now)}
		row.ShortPrint = m.Fingerprint
		if len(row.ShortPrint) > 19 {
			row.ShortPrint = row.ShortPrint[:19] + "…"
		}
		switch m.State {
		case "connected":
			row.StateChip, row.StateText, row.Contact = "ok", "connected", Ago{Text: "now", UTC: now.UTC().Format(time.RFC3339)}
		case "removed":
			row.StateChip, row.StateText = "plain", "removed"
		default:
			row.StateChip, row.StateText = "warn", "not connected"
			if !m.SeenAt.IsZero() {
				row.Contact = agoAt(m.SeenAt, now)
			} else {
				row.Contact = Ago{Text: "never"}
			}
		}
		if m.State != "removed" && !m.StoreReplaced.IsZero() && now.Sub(m.StoreReplaced) < storeReplacedShown {
			// A warning while it is recent; the audit keeps it for good.
			row.StateChip = "warn"
			row.StateText += " · store replaced " + agoAt(m.StoreReplaced, now).Text
		}
		if m.State == "removed" {
			v.Removed = append(v.Removed, row)
			continue
		}
		v.Total++
		if m.State == "connected" {
			v.Connected++
		}
		v.Rows = append(v.Rows, row)
	}
	s.render(w, "members", http.StatusOK, v)
}

// storeReplacedShown is how long Members points out that a cluster came
// with another store.
const storeReplacedShown = 7 * 24 * time.Hour

// viewerHome is the hub's home for a person without a role: the first
// cluster connected (the switcher leads to the others); the Members page
// is for operators and owners.
func (s *Server) viewerHome(w http.ResponseWriter, r *http.Request) {
	for _, m := range s.opt.Fleet.Members() {
		if m.State == "connected" {
			http.Redirect(w, r, "/m/"+m.Name+"/", http.StatusFound)
			return
		}
	}
	sh, err := s.shell(r, "", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Nothing to show yet"
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "notfound", http.StatusOK, NotFound{Shell: sh, What: "cluster",
		Hint: "No cluster of this fleet is connected now. Its consoles show here once one is; an owner can give you a role to see more."})
}

// Ago is a past moment as the pages show it.
type Ago struct {
	Text, UTC string
}

func agoAt(t, now time.Time) Ago {
	if t.IsZero() {
		return Ago{}
	}
	return Ago{Text: sinceText(now.Sub(t)) + " ago", UTC: t.UTC().Format(time.RFC3339)}
}

func sinceText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "seconds"
	case d < time.Hour:
		return unitN(int(d/time.Minute), "minute")
	case d < 48*time.Hour:
		return unitN(int(d/time.Hour), "hour")
	}
	return unitN(int(d/(24*time.Hour)), "day")
}

func unitN(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// Empty is a Backend with nothing in it: the hub's own pages (Members,
// sign-in) show no project and no daemon.
type Empty struct{}

func (Empty) Projects(context.Context) ([]store.Project, error) { return nil, nil }
func (Empty) Builds(context.Context, store.BuildQuery) ([]store.BuildRow, error) {
	return nil, nil
}
func (Empty) Branches(context.Context, store.ProjectKey, time.Time) ([]string, error) {
	return nil, nil
}
func (Empty) Targets(context.Context, store.ProjectKey, time.Time) ([]string, error) {
	return nil, nil
}
func (Empty) TargetImages(context.Context, store.ProjectKey, time.Time) (map[string][]string, error) {
	return nil, nil
}
func (Empty) StepCounts(context.Context, store.ProjectKey, string, time.Time, int) ([]store.StepCount, error) {
	return nil, nil
}
func (Empty) ImageDays(context.Context, store.ProjectKey, string, time.Time) ([]store.ImageDay, error) {
	return nil, nil
}
func (Empty) PipelineList(context.Context, store.PipeQuery) ([]store.PipeRow, error) {
	return nil, nil
}
func (Empty) JobsOf(context.Context, string, []uint64) (map[uint64][]store.JobInfo, error) {
	return nil, nil
}
func (Empty) BuildsOf(context.Context, store.ProjectKey, []uint64) (map[uint64][]store.BuildRow, error) {
	return nil, nil
}
func (Empty) PipelineInfo(context.Context, string, uint64) (*store.Pipeline, error) { return nil, nil }
func (Empty) PipelineProject(context.Context, string, uint64) (uint64, error)       { return 0, nil }
func (Empty) PipelineJobs(context.Context, string, uint64) ([]store.JobInfo, error) {
	return nil, nil
}
func (Empty) Job(context.Context, string, uint64) (*store.JobInfo, error) { return nil, nil }
func (Empty) JobBuilds(context.Context, string, uint64) ([]int64, error)  { return nil, nil }
func (Empty) BuildPage(context.Context, int64) (*store.BuildPage, error)  { return nil, nil }
func (Empty) Refs(context.Context, []string) (map[string]store.RefInfo, error) {
	return nil, nil
}
func (Empty) Previous(context.Context, *history.Build) (*store.Stored, bool, error) {
	return nil, false, nil
}
func (Empty) Compared(context.Context, store.ProjectKey, string, string) ([]string, bool, bool, error) {
	return nil, false, false, nil
}
func (Empty) ComparedMarked(context.Context, store.ProjectKey, string, string) ([]string, bool, bool, error) {
	return nil, false, false, nil
}
func (Empty) SaveCompare(context.Context, store.ProjectKey, string, string, []string, bool) error {
	return nil
}
func (Empty) StepHistory(context.Context, int64, int, int) ([]store.StepRun, error) {
	return nil, nil
}
func (Empty) Output(context.Context, int64, int) (string, error) { return "", nil }
func (Empty) DaemonCounts(context.Context, time.Time, store.ProjectsIn) (map[string]store.DaemonCount, error) {
	return nil, nil
}
func (Empty) ProjectCache(context.Context, store.ProjectKey) ([]store.CacheObject, []store.CacheStore, error) {
	return nil, nil, nil
}
func (Empty) Elsewhere(context.Context, store.ProjectKey, time.Time, int) ([]store.Elsewhere, error) {
	return nil, nil
}
func (Empty) SettleElsewhere(context.Context, []store.Settled) error { return nil }
func (Empty) Recent(context.Context, int) ([]*store.Stored, error)   { return nil, nil }
func (Empty) Ping(context.Context) error                             { return nil }
func (Empty) Subscribe() *store.Subscription                         { return new(store.Changes).Subscribe() }
func (Empty) Unsubscribe(*store.Subscription)                        {}
func (Empty) Daemons() []collector.DaemonState                       { return nil }
func (Empty) LiveBuild(context.Context, string, string) (*collector.LiveBuild, error) {
	return nil, nil
}

// fleetJob sends a job's address (from a job log, printed by the runner of
// any cluster) to the cluster that ran it.
func (s *Server) fleetJob(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
	instance := r.PathValue("instance")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	hits, silent := s.opt.Fleet.Locate(ctx, instance, 0, id)
	// Only what the person may read of: the clusters answer for everyone.
	reads := s.reads(r, instance)
	var known, ran []FleetHit
	for _, h := range hits {
		if h.Job != nil && reads(h.Job.ProjectID) {
			known = append(known, h)
			if ranHere(*h.Job) {
				ran = append(ran, h)
			}
		}
	}
	jobPath := func(h FleetHit) string {
		return "/m/" + h.Member + "/jobs/" + url.PathEscape(instance) + "/" + strconv.FormatUint(id, 10)
	}
	if len(ran) > 1 {
		// Each cluster says what ran in it; when several claim the job, the
		// person chooses rather than the fastest answer.
		sort.Slice(ran, func(i, j int) bool { return ran[i].Member < ran[j].Member })
		sh, err := s.shell(r, "", "")
		if err != nil {
			s.fail(w, err)
			return
		}
		sh.Title = "Job #" + strconv.FormatUint(id, 10) + " in several clusters"
		v := NotFound{Shell: sh, What: "job", Hint: "More than one cluster says it ran this job. Open the one you expect:"}
		for _, h := range ran {
			v.Links = append(v.Links, Option{Value: jobPath(h), Label: h.Member})
		}
		w.Header().Set("Cache-Control", "no-store")
		s.render(w, "notfound", http.StatusOK, v)
		return
	}
	// Every cluster that read the pipeline from GitLab knows the job; the
	// one whose pod ran it is the one to open.
	if len(ran) == 1 {
		known = ran
	}
	if len(known) > 0 {
		to := jobPath(known[0])
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, to, http.StatusFound)
		return
	}
	s.fleetMissing(w, r, "job", silent)
}

// reads says, for the person on r, whether they may read a project of a
// GitLab, asking once per project.
func (s *Server) reads(r *http.Request, instance string) func(project uint64) bool {
	seen := map[uint64]bool{}
	return func(project uint64) bool {
		if s.opt.Auth == nil {
			return true
		}
		if ok, done := seen[project]; done {
			return ok
		}
		ok := project != 0 && s.may(r, instance, project)
		seen[project] = ok
		return ok
	}
}

// hitProject is the project a cluster says a pipeline is of.
func hitProject(h FleetHit) uint64 {
	if h.Pipeline != nil {
		return h.Pipeline.ProjectID
	}
	for _, j := range h.Jobs {
		return j.ProjectID
	}
	return 0
}

// FleetPipeline is a pipeline whose jobs ran in several clusters: its
// jobs grouped by cluster.
type FleetPipeline struct {
	Shell
	Instance string
	ID       uint64
	Pipeline *store.Pipeline
	Groups   []FleetGroup
	Silent   []string
	// Other: jobs no cluster of the fleet ran (another runner), as GitLab
	// shows them.
	Other []FleetJob
}

// ranHere says whether the cluster that told of a job ran it: it saw the
// job's pod.
func ranHere(j store.JobInfo) bool { return !j.PodCreated.IsZero() || j.Node != "" }

// FleetGroup is one cluster's part of a pipeline.
type FleetGroup struct {
	Member string
	URL    string
	Jobs   []FleetJob
}

// FleetJob is one job of a FleetGroup.
type FleetJob struct {
	Name, Stage, Status, URL string
	Duration                 string
}

// fleetPipeline shows a pipeline across the clusters that ran its jobs; a
// pipeline of one cluster opens that cluster's page.
func (s *Server) fleetPipeline(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
	instance := r.PathValue("instance")
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	hits, silent := s.opt.Fleet.Locate(ctx, instance, id, 0)
	// Only what the person may read of: the clusters answer for everyone,
	// and the grouped page shows the pipeline's commit, ref and jobs.
	reads := s.reads(r, instance)
	var known []FleetHit
	for _, h := range hits {
		if (h.Pipeline != nil || len(h.Jobs) > 0) && reads(hitProject(h)) {
			known = append(known, h)
		}
	}
	key := url.PathEscape(instance) + "/" + strconv.FormatUint(id, 10)
	if len(known) == 1 && len(silent) == 0 {
		http.Redirect(w, r, "/m/"+known[0].Member+"/pipelines/"+key, http.StatusFound)
		return
	}
	if len(known) == 0 {
		s.fleetMissing(w, r, "pipeline", silent)
		return
	}
	sh, err := s.shell(r, "members", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	v := FleetPipeline{Shell: sh, Instance: instance, ID: id, Silent: silent}
	sh.Title = fmt.Sprintf("Pipeline #%d", id)
	v.Shell = sh
	sort.Slice(known, func(i, j int) bool { return known[i].Member < known[j].Member })
	// A cluster that read the pipeline from GitLab knows every job of it;
	// each job belongs to the cluster whose pod ran it.
	placed := map[uint64]bool{}
	var other FleetGroup
	for _, h := range known {
		for _, j := range h.Jobs {
			if ranHere(j) {
				placed[j.ID] = true
			}
		}
	}
	for _, h := range known {
		if v.Pipeline == nil && h.Pipeline != nil {
			v.Pipeline = h.Pipeline
		}
		g := FleetGroup{Member: h.Member, URL: "/~/m/" + h.Member + "/pipelines/" + key}
		for _, j := range h.Jobs {
			if !ranHere(j) {
				if !placed[j.ID] {
					placed[j.ID] = true
					other.Jobs = append(other.Jobs, FleetJob{Name: j.Name, Stage: j.Stage, Status: j.Status, URL: webURL(j.URL)})
				}
				continue
			}
			fj := FleetJob{Name: j.Name, Stage: j.Stage, Status: j.Status,
				URL: "/~/m/" + h.Member + "/jobs/" + url.PathEscape(j.Instance) + "/" + strconv.FormatUint(j.ID, 10)}
			if j.Duration > 0 {
				fj.Duration = j.Duration.Round(time.Second).String()
			}
			g.Jobs = append(g.Jobs, fj)
		}
		if len(g.Jobs) > 0 || h.Pipeline != nil {
			v.Groups = append(v.Groups, g)
		}
	}
	v.Other = other.Jobs
	s.render(w, "fleetpipe", http.StatusOK, v)
}

func (s *Server) fleetMissing(w http.ResponseWriter, r *http.Request, what string, silent []string) {
	sh, err := s.shell(r, "", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Not found"
	hint := "No cluster of this fleet has this " + what + "."
	if len(silent) > 0 {
		hint = fmt.Sprintf("No cluster that answered has this %s; %d did not answer in time (%s): it may be there.", what, len(silent), strings.Join(silent, ", "))
	}
	s.render(w, "notfound", http.StatusNotFound, NotFound{Shell: sh, What: what, Hint: hint, Links: []Option{{Value: "/~/", Label: "Members"}}})
}

// MemberAway is the page for a cluster's address while the cluster is not
// connected (its builds stay in the cluster: 502, try again), was removed
// (410) or is unknown (404).
func (s *Server) MemberAway(w http.ResponseWriter, r *http.Request, name string) {
	sh, err := s.shell(r, "", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	status := http.StatusNotFound
	sh.Title = "No such cluster"
	hint := "No cluster of this name is registered at this hub."
	if fleet.NamePattern.MatchString(name) {
		hint = "No cluster named " + name + " is registered at this hub."
	}
	if s.opt.Fleet != nil {
		for _, m := range s.opt.Fleet.Members() {
			if m.Name != name {
				continue
			}
			switch {
			case m.State == "removed":
				status, sh.Title = http.StatusGone, name+" was removed"
				hint = "The cluster " + name + " was removed from this fleet"
				if !m.RemovedAt.IsZero() {
					hint += " on " + stampOf(m.RemovedAt, zone(r), "Jan 2, 2006 15:04").Text
				}
				hint += "."
			case m.SeenAt.IsZero():
				status, sh.Title = http.StatusBadGateway, name+" is not connected"
				hint = "The cluster " + name + " has not connected to this hub yet."
			default:
				status, sh.Title = http.StatusBadGateway, name+" is not connected"
				hint = "The cluster " + name + " is not connected to this hub now (last contact " + agoAt(m.SeenAt, s.now()).Text +
					"). Its builds are kept in the cluster; they show here again once it reconnects."
			}
		}
	}
	if status == http.StatusBadGateway {
		w.Header().Set("Retry-After", "10")
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, "notfound", status, NotFound{Shell: sh, What: "cluster", Hint: hint, Links: []Option{{Value: "/~/", Label: "Members"}}})
}
