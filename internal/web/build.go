package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// built is a build read for a page: its steps as the pages show them and
// what explains it.
type built struct {
	Page    *store.BuildPage
	Views   []StepView
	Verdict Verdict
	Cat     Cat
	Built   int
	Cached  int
	Name    func(ref string) string
}

// readBuild loads a build with the names of the builds it waited for.
func (s *Server) readBuild(ctx context.Context, id int64) (*built, error) {
	p, err := s.store.BuildPage(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.explain(ctx, p)
}

func (s *Server) explain(ctx context.Context, p *store.BuildPage) (*built, error) {
	b := p.Build
	refs := []string{p.Reason.By}
	for _, st := range b.Steps {
		refs = append(refs, st.ReceivedFrom)
	}
	known, err := s.store.Refs(ctx, refs)
	if err != nil {
		return nil, err
	}
	project := uint64(0)
	if p.Pipeline != nil {
		project = p.Pipeline.ProjectID
	}
	name := func(ref string) string {
		info, ok := known[ref]
		// Work from another project's build is named, but not whose.
		if !ok || (project != 0 && info.ProjectID != project) {
			return "another build"
		}
		if info.JobName != "" {
			return info.JobName
		}
		return "build of " + info.Target
	}
	outputs := map[int]bool{}
	if p.OutputKept {
		for i := range b.Steps {
			outputs[i] = p.Printed[i]
		}
	}
	d := &built{Page: p, Views: steps(b, p.Reason, name, outputs), Name: name}
	var rec []Received
	recIdx := map[string]int{}
	for _, v := range d.Views {
		switch v.State {
		case "built":
			d.Built++
		case "cached":
			d.Cached++
		case "received":
			i, ok := recIdx[v.From]
			if !ok {
				i = len(rec)
				recIdx[v.From] = i
				rec = append(rec, Received{From: v.From})
			}
			if n := len(rec[i].Stages); n == 0 || rec[i].Stages[n-1] != v.Stage {
				rec[i].Stages = append(rec[i].Stages, v.Stage)
			}
			rec[i].Wait += v.D
		}
	}
	by := ""
	if p.Reason.By != "" {
		by = name(p.Reason.By)
	}
	d.Verdict = verdictFor(b, p.Reason, by, d.Built, p.Saved, rec)
	d.Cat = catOf(p.Reason.Kind, b.Error, d.Built)
	d.Verdict.Icon = d.Cat.Icon
	return d, nil
}

// BuildView is the build page.
type BuildView struct {
	Shell
	ID        int64
	Name      string
	Cat       Cat
	Verdict   Verdict
	PipeURL   string
	PipeLabel string
	PrevURL   string
	NextURL   string
	GitLabURL string
	Commit    string
	SHA       string
	Ref       string
	Target    string
	Total     time.Duration
	Segs      []Seg
	BuildKit  time.Duration
	Usual     time.Duration
	Speed     string // slower, faster, usual, or ""
	Saved     time.Duration
	Cached    int
	Context   int64
	Blocks    []Block
	StepCount int
	Tone      string
	Lines     []Line
	DfName    string
	WF        *Waterfall
	Compare   *Compare
	Details   []KV
	Aside     Aside
	Steps     []StepView
	Failed    bool
	Others    []OtherBuild
	Tab       string
	// ViaLog: the reader came from the link in the job's log.
	ViaLog  bool
	LogLine string
}

// Aside is the facts column of the build page.
type Aside struct {
	Node, NodeHost, Last string
	OtherNode, SameNode  bool
	Started, Job, Build  string
	Usual, Context       string
	JobID, Trace         string
}

// KV is a labelled fact.
type KV struct{ K, V string }

// OtherBuild is another build of the same job.
type OtherBuild struct {
	URL, Target, Dur string
	Cat              Cat
}

// Compare is the build next to the previous build of its target.
type Compare struct {
	URL, Name, Pipe, SHA, Msg, From, To, Dur string
	Rows                                     []CompareRow
	Differ                                   int
}

// CompareRow is one step before and now.
type CompareRow struct {
	Name, Before, Now string
	Mark              bool
}

func (s *Server) build(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, "build")
		return
	}
	ctx := r.Context()
	p, err := s.store.BuildPage(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		s.notFound(w, r, "build")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.ensureAPI(p.Pipeline) {
		if p, err = s.store.BuildPage(ctx, id); err != nil {
			s.fail(w, err)
			return
		}
	}
	d, err := s.explain(ctx, p)
	if err != nil {
		s.fail(w, err)
		return
	}
	b := p.Build
	a := b.Attribution
	live := ""
	if a.State == history.Attributed {
		live = store.PipelineKey(a.Instance, a.Pipeline)
	}
	sh, err := s.shell(r, "pipes", live)
	if err != nil {
		s.fail(w, err)
		return
	}
	open := ""
	if a.State == history.Attributed {
		open = fmt.Sprintf("j:%s/%d", a.Instance, a.Job)
		// The list shows the build's project.
		if p.Pipeline != nil && p.Pipeline.ProjectID != 0 {
			for _, pr := range sh.Projects {
				if pr.Instance == a.Instance && pr.ID == p.Pipeline.ProjectID {
					sh.Project = pr
				}
			}
		}
	}
	lv, err := s.listFor(ctx, r, &sh, open, r.URL.Path)
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.List, sh.Split, sh.Has = lv, true, "job"
	v := &BuildView{ID: id, Cat: d.Cat, Verdict: d.Verdict, Target: b.Target, BuildKit: b.Completed.Sub(b.Started),
		Saved: p.Saved, Cached: d.Cached, Context: b.ContextBytes, Steps: d.Views, Tone: d.Cat.Tone, Lines: dockerfile(p.Dockerfile, d.Views, b),
		DfName: p.DockerfileName, Tab: "steps"}
	v.PrevURL, v.NextURL = lv.neighbours(open)
	if r.URL.Query().Get("via") == "log" && a.State == history.Attributed {
		v.ViaLog = true
		v.LogLine = fmt.Sprintf("ci-accelerator: why this job's builds take their time: %s/jobs/%s/%d", origin(r), a.Instance, a.Job)
	}
	var job store.JobInfo
	if p.Job != nil {
		job = *p.Job
		v.Name, v.GitLabURL = job.Name, job.URL
	}
	if v.Name == "" {
		v.Name = "Build of " + orDefault(b.Target)
	}
	sh.Title = v.Name
	if pl := p.Pipeline; pl != nil {
		v.PipeURL, v.PipeLabel = "/pipelines/"+store.PipelineKey(pl.Instance, pl.ID), "#"+strconv.FormatUint(pl.ID, 10)
		v.Commit, v.SHA, v.Ref = pl.CommitTitle, short(pl.SHA, 8), pl.Ref
	}
	jt := timesOf(job, b.Started, b.Completed)
	v.Total = jt.Total
	v.Segs = timeSegs(b, jt)
	project := uint64(0)
	if p.Pipeline != nil {
		project = p.Pipeline.ProjectID
	}
	if v.Usual, err = s.store.Usual(ctx, b, project); err != nil {
		s.fail(w, err)
		return
	}
	if v.Usual > 0 && b.Error == "" {
		switch r := float64(v.BuildKit) / float64(v.Usual); {
		case r > 1.3:
			v.Speed = "slower"
		case r < 0.7:
			v.Speed = "faster"
		default:
			v.Speed = "usual"
		}
	}
	v.Blocks = blocksOf(b, d.Views)
	v.StepCount = len(b.Steps)
	v.Failed = b.Error != ""
	v.WF = waterfall("", []wfJob{{Name: v.Name, Cat: d.Cat, Times: jt, Build: b, BuildID: id, Views: d.Views}}, jt.Start, true)
	prev, _, err := s.store.Previous(ctx, b)
	if err != nil {
		s.fail(w, err)
		return
	}
	v.Compare = s.compare(ctx, b, prev, d)
	nodeHost := map[string]string{}
	if s.opt.Live != nil {
		for _, dm := range s.opt.Live.Daemons() {
			nodeHost[dm.Name] = dm.Inspection.Node
		}
		// The daemon that held the cache was replaced since.
		if k := p.Reason.Kind; (k == history.OtherNode || k == history.CacheElsewhere) && p.Reason.Old != "" {
			if _, ok := nodeHost[p.Reason.Old]; !ok {
				v.Verdict.Detail += template.HTML(fmt.Sprintf(" Daemon <b>%s</b> is gone: it was replaced (a rollout or a restart), and a daemon whose cache is not on a persistent volume starts empty.",
					e(shortDaemon(p.Reason.Old))))
			}
		}
	}
	v.Aside = Aside{Node: shortDaemon(b.Daemon), NodeHost: nodeHost[b.Daemon], Started: ago(b.Started), Job: duration(jt.Total),
		Build: duration(v.BuildKit), Context: bytesText(b.ContextBytes)}
	if v.Usual > 0 {
		v.Aside.Usual = duration(v.Usual)
	}
	if prev != nil {
		v.Aside.Last = shortDaemon(prev.Build.Daemon)
		v.Aside.OtherNode, v.Aside.SameNode = prev.Build.Daemon != b.Daemon, prev.Build.Daemon == b.Daemon
	}
	if a.State == history.Attributed {
		v.Aside.JobID = strconv.FormatUint(a.Job, 10)
		v.Aside.Trace = traceID(a)
	}
	v.Details = []KV{{"Build node", b.Daemon + paren(nodeHost[b.Daemon])}, {"Record", b.Ref}}
	if prev != nil {
		v.Details = append(v.Details, KV{"Previous build of the target on", prev.Build.Daemon + paren(nodeHost[prev.Build.Daemon])})
	}
	if v.Aside.Trace != "" {
		v.Details = append(v.Details, KV{"Trace id", v.Aside.Trace})
	}
	if job.ID != 0 {
		v.Details = append(v.Details, KV{"GitLab job", fmt.Sprintf("%d · %s total, %s in BuildKit", job.ID, duration(jt.Total), duration(v.BuildKit))})
		if job.Waited > 0 {
			v.Details = append(v.Details, KV{"Waited for a runner", duration(job.Waited)})
		}
		if job.Runner != "" {
			v.Details = append(v.Details, KV{"Runner", job.Runner})
		}
	}
	v.Details = append(v.Details, KV{"Started", b.Started.UTC().Format(time.RFC3339)})
	if len(b.Images) > 0 {
		v.Details = append(v.Details, KV{"Pushed", strings.Join(b.Images, ", ")})
	}
	if b.Error != "" {
		v.Details = append(v.Details, KV{"Error", b.Error})
	}
	v.Details = append(v.Details, KV{"API", fmt.Sprintf("GET /api/v1/builds?limit=50 (this build is id %d)", id)})
	if a.State == history.Attributed {
		ids, err := s.store.JobBuilds(ctx, a.Instance, a.Job)
		if err != nil {
			s.fail(w, err)
			return
		}
		for _, oid := range ids {
			if oid == id {
				continue
			}
			o, err := s.readBuild(ctx, oid)
			if err != nil {
				s.fail(w, err)
				return
			}
			ob := o.Page.Build
			v.Others = append(v.Others, OtherBuild{URL: fmt.Sprintf("/builds/%d", oid), Target: orDefault(ob.Target),
				Dur: duration(ob.Completed.Sub(ob.Started)), Cat: o.Cat})
		}
	}
	v.Shell = sh
	s.render(w, "build", http.StatusOK, v)
}

func paren(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

func orDefault(target string) string {
	if target == "" {
		return "the default target"
	}
	return target
}

func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2 Jan 15:04 UTC")
}

// traceID is the trace id the runner gives a job.
func traceID(a history.Attribution) string {
	return fmt.Sprintf("01%s%012x%012x", a.Instance, a.Pipeline, a.Job)
}

// compare sets a build next to the previous build of its target, step by
// step.
func (s *Server) compare(ctx context.Context, b *history.Build, prev *store.Stored, d *built) *Compare {
	if prev == nil {
		return nil
	}
	pb := prev.Build
	c := &Compare{URL: fmt.Sprintf("/builds/%d", prev.ID), From: shortDaemon(pb.Daemon), To: shortDaemon(b.Daemon),
		Dur: duration(pb.Completed.Sub(pb.Started))}
	if a := pb.Attribution; a.State == history.Attributed {
		c.Pipe = "#" + shortID(a.Pipeline)
		if pl, _, _, err := s.store.PipelinePage(ctx, a.Instance, a.Pipeline); err == nil && pl != nil {
			c.SHA, c.Msg = short(pl.SHA, 7), pl.CommitTitle
		}
		if jobs, err := s.store.PipelineJobs(ctx, a.Instance, a.Pipeline); err == nil {
			for _, j := range jobs {
				if j.ID == a.Job {
					c.Name = j.Name
				}
			}
		}
	}
	state := func(st history.Step) string {
		if isFrom(st) {
			return "image"
		}
		return string(st.State)
	}
	keyOf := func(st history.Step) string { return st.Key + "|" + st.Name }
	var keys []string
	before, now := map[string]history.Step{}, map[string]history.Step{}
	for _, st := range pb.Steps {
		k := keyOf(st)
		if _, ok := before[k]; !ok {
			keys = append(keys, k)
		}
		before[k] = st
	}
	for _, st := range b.Steps {
		k := keyOf(st)
		if _, ok := before[k]; !ok {
			if _, ok := now[k]; !ok {
				keys = append(keys, k)
			}
		}
		now[k] = st
	}
	reasonKey := ""
	if d.Page.Reason.Kind != "" {
		reasonKey = keyOf(d.Page.Reason.Step)
	}
	for _, k := range keys {
		x, okA := before[k]
		y, okB := now[k]
		row := CompareRow{Mark: k == reasonKey && d.Cat.Key != "cached"}
		if okA {
			row.Name, row.Before = x.Key+" "+x.Name, state(x)
		}
		if okB {
			row.Name, row.Now = y.Key+" "+y.Name, state(y)
		}
		if !okA || !okB || row.Before != row.Now {
			c.Differ++
		}
		c.Rows = append(c.Rows, row)
	}
	return c
}

// ---- the step panel ----

// StepData is the step panel: what one step did, its output and history.
type StepData struct {
	Shell
	Full     bool // a page of its own, not the panel of the build page
	BuildID  int64
	Step     StepView
	Kind     string
	Command  string
	Sentence string
	Src      []SrcLine
	SrcTitle string
	Facts    []KV
	JobURL   string
	JobName  string
	Hint     template.HTML
	Err      string // what BuildKit said when the step failed
	// HasDockerfile: the build page has a Dockerfile tab (a failed build
	// has none).
	HasDockerfile bool
	Output        string
	OutNote       string
	Hist          []HistCol
	HistTitle     string
	HistNote      string
	Prev          int
	Next          int
}

// SrcLine is a line of the step's Dockerfile source.
type SrcLine struct {
	N    int
	HTML template.HTML
}

// HistCol is one earlier run of a step in its history.
type HistCol struct {
	URL, Label, SHA, State, H string
	Cur, Cached               bool
}

var (
	pipCached   = regexp.MustCompile(`(?m)^\s*Using cached `)
	outputCut   = regexp.MustCompile(`(?m)^\[… \d+ bytes cut …\]$`)
	pipDownload = regexp.MustCompile(`(?m)^\s*Downloading `)
)

func (s *Server) step(w http.ResponseWriter, r *http.Request) {
	id, err1 := strconv.ParseInt(r.PathValue("id"), 10, 64)
	idx, err2 := strconv.Atoi(r.PathValue("step"))
	if err1 != nil || err2 != nil {
		s.notFound(w, r, "step")
		return
	}
	ctx := r.Context()
	d, err := s.readBuild(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && (idx < 0 || idx >= len(d.Page.Build.Steps))) {
		s.notFound(w, r, "step")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	p, b := d.Page, d.Page.Build
	st, v := b.Steps[idx], d.Views[idx]
	sd := StepData{BuildID: id, Step: v, Command: st.Name, Prev: -1, Next: -1, Err: st.Error, HasDockerfile: p.Dockerfile != ""}
	if idx > 0 {
		sd.Prev = idx - 1
	}
	if idx+1 < len(b.Steps) {
		sd.Next = idx + 1
	}
	jobName := ""
	if p.Job != nil {
		jobName, sd.JobName = p.Job.Name, p.Job.Name
	}
	kind := "Step " + st.Key
	if jobName != "" {
		kind += " · " + jobName
	}
	if p.Pipeline != nil {
		kind += " · #" + shortID(p.Pipeline.ID)
	}
	sd.Kind = kind
	switch v.State {
	case "failed":
		sd.Sentence = "Failed after " + duration(v.D) + "; the build stopped here."
	case "canceled":
		sd.Sentence = "Canceled after " + duration(v.D) + ": the job was canceled while this step ran."
	case "cached":
		sd.Sentence = "Came from cache on this node — 0 s of work."
	case "received":
		sd.Sentence = "Received from " + v.From + ", which was building it at the same time — waited " + duration(v.D) + "."
	case "image":
		sd.Sentence = "The base image, resolved for this build" + map[bool]string{true: " and pulled", false: ": already on this node"}[v.D > 200*time.Millisecond] + "."
	default:
		where := "this job"
		if jobName == "" {
			where = "this build"
		}
		sd.Sentence = "Built in " + where + " — " + duration(v.D) + "."
	}
	if v.FirstRebuilt {
		sd.Sentence += " The rebuild started here."
	}
	if p.Dockerfile != "" && len(st.Lines) > 0 {
		lines := strings.Split(p.Dockerfile, "\n")
		cont := false
		for i, raw := range lines {
			n := i + 1
			in := false
			for _, rg := range st.Lines {
				in = in || (n >= rg.Start && n <= rg.End)
			}
			if in {
				sd.Src = append(sd.Src, SrcLine{N: n, HTML: highlight(raw, cont)})
			}
			cont = strings.HasSuffix(strings.TrimSpace(raw), "\\")
		}
		sd.SrcTitle = "Dockerfile, line " + v.Lines
		if p.DockerfileName != "" && p.DockerfileName != "Dockerfile" {
			sd.SrcTitle = p.DockerfileName + ", line " + v.Lines
		}
	}
	result := v.State
	if l, ok := stateLabels[v.State]; ok {
		result = l
	}
	sd.Facts = []KV{{"Result", result}, {"Duration", duration(v.D)}}
	if !st.Started.IsZero() {
		sd.Facts = append(sd.Facts, KV{"Started", "+" + duration(v.Offset) + " into the build"})
	}
	sd.Facts = append(sd.Facts, KV{"Node", shortDaemon(b.Daemon)})
	if p.Job != nil {
		sd.JobURL = fmt.Sprintf("/builds/%d", id)
	}
	if v.HasOutput {
		out, err := s.store.Output(ctx, id, idx)
		if err != nil {
			s.fail(w, err)
			return
		}
		sd.Output = out
		if strings.Contains(st.Name, "pip install") {
			uc, dl := len(pipCached.FindAllString(out, -1)), len(pipDownload.FindAllString(out, -1))
			if uc+dl > 0 {
				sd.Hint = template.HTML(fmt.Sprintf("pip printed <b>%d</b> “Using cached” and <b>%d</b> “Downloading” lines — %s",
					uc, dl, map[bool]string{true: "most packages came from the cache mount; the rest were downloaded (new or changed versions).",
						false: "most packages were downloaded: the cache mount did not have them."}[uc >= dl]))
			}
		}
		sd.OutNote = fmt.Sprintf("%d lines of output, as BuildKit recorded it (secrets masked).", strings.Count(strings.TrimRight(out, "\n"), "\n")+1)
		switch {
		case outputCut.MatchString(out):
			sd.OutNote += " A long output: its start and its end are kept, the middle is cut."
		case strings.Contains(out, "log limit"):
			sd.OutNote += " BuildKit itself stopped recording part of it (its log rate limit)."
		}
	} else {
		switch {
		case v.State == "cached":
			sd.OutNote = "A step that comes from cache prints nothing."
		case v.State == "received":
			sd.OutNote = "Its output belongs to the build that ran it."
		case v.State == "image":
			sd.OutNote = "Resolving a base image prints nothing."
		case !p.Printed[idx]:
			sd.OutNote = "This step printed nothing."
		default:
			sd.OutNote = "Output is kept for a few days; this build's is gone."
		}
	}
	runs, err := s.store.StepHistory(ctx, id, idx, 12)
	if err != nil {
		s.fail(w, err)
		return
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Started.Before(runs[j].Started) })
	var longest time.Duration
	var rebuilt []time.Duration
	for _, run := range runs {
		longest = max(longest, run.Duration)
		if run.State == history.Built {
			rebuilt = append(rebuilt, run.Duration)
		}
	}
	for _, run := range runs {
		h := 6.0
		if longest > 0 {
			h = 6 + 86*float64(run.Duration)/float64(longest)
		}
		col := HistCol{URL: fmt.Sprintf("/builds/%d/steps/%d", run.BuildID, idx), Label: duration(run.Duration), SHA: short(run.SHA, 6),
			State: string(run.State), H: strconv.FormatFloat(h, 'f', 1, 64), Cur: run.BuildID == id, Cached: run.State != history.Built}
		if col.SHA == "" {
			col.SHA = "#" + strconv.FormatInt(run.BuildID, 10)
		}
		sd.Hist = append(sd.Hist, col)
	}
	if len(runs) > 0 {
		target := b.Target
		if target == "" {
			target = "this target"
		}
		sd.HistTitle = fmt.Sprintf("History · %s · %d build%s", target, len(runs), plural(len(runs)))
		sd.HistNote = fmt.Sprintf("Rebuilt in %d of %d builds that had this step", len(rebuilt), len(runs))
		if len(rebuilt) > 0 {
			sd.HistNote += " · median " + duration(median(rebuilt)) + " when rebuilt"
		}
		sd.HistNote += ". A bar opens that build's step."
	}
	if r.URL.Query().Get("part") == "panel" {
		s.render(w, "step", http.StatusOK, sd)
		return
	}
	sh, err := s.shell(r, "pipes", "")
	if err != nil {
		s.fail(w, err)
		return
	}
	sh.Title = "Step " + st.Key
	sd.Shell, sd.Full = sh, true
	s.render(w, "steppage", http.StatusOK, sd)
}
