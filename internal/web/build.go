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
	"github.com/hoobiboobi/ci-accelerator/internal/route"
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
	p, err := s.data.BuildPage(ctx, id)
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
	known, err := s.data.Refs(ctx, refs)
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
	d.Verdict = withVia(verdictFor(b, p.Reason, by, d.Built, p.Saved, rec), p.Reason, p.Via)
	d.Cat = catOf(p.Reason.Kind, b.Error, d.Built)
	d.Verdict.Icon = d.Cat.Icon
	return d, nil
}

// BuildView is the build page: the verdict first, what changed since the
// previous build of the target, the steps; the rest under Details.
type BuildView struct {
	Shell
	ID      int64
	Name    string
	Target  string
	Status  Status
	Verdict Verdict
	// At most three numbers under the verdict.
	Dur, Delta string
	Slower     bool
	Usual      string
	Ran, Steps int
	Ref, SHA   string
	Commit     string
	Node       string
	PipeURL    string
	PipeLabel  string
	GitLabURL  string
	LogURL     string
	Changes    []Change
	PrevURL    string
	PrevLabel  string
	Blocks     []Block
	Gantt      []StepRow // steps that took time, by start
	GanttSkip  int       // steps from the cache, left out of the timeline
	StepCount  int
	Tone       string
	Lines      []Line
	DfName     string
	StepViews  []StepView
	Failed     bool
	Others     []OtherBuild
	Details    []KV
	Trace      string
	// ViaLog: the reader came from the link in the job's log.
	ViaLog  bool
	LogLine string
}

// Change is one difference from the previous build of the target.
type Change struct {
	K string
	V template.HTML
}

// KV is a labelled fact.
type KV struct{ K, V string }

// OtherBuild is another build of the same job.
type OtherBuild struct {
	URL, Target, Dur string
	Status           Status
}

func (s *Server) build(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.notFound(w, r, "build")
		return
	}
	ctx := r.Context()
	p, err := s.data.BuildPage(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !s.mayBuild(r, p)) {
		s.notFound(w, r, "build")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.ensureAPI(r, p.Pipeline) {
		if p, err = s.data.BuildPage(ctx, id); err != nil {
			s.fail(w, err)
			return
		}
	}
	// Files or node: GitLab's comparison of the two commits decides it,
	// now for this build, in the background for the project's others.
	if p.Reason.Kind == history.CacheElsewhere && p.Via == "" && p.Pipeline != nil && p.Pipeline.ProjectID != 0 {
		key := store.ProjectKey{Instance: p.Pipeline.Instance, ID: p.Pipeline.ProjectID}
		if api := s.api(r); api != nil && api.Knows(key.Instance) {
			sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			n, _ := s.settle(sctx, api, key, id, 1)
			cancel()
			if n > 0 {
				if p, err = s.data.BuildPage(ctx, id); err != nil {
					s.fail(w, err)
					return
				}
			}
			s.settleLater(r, key)
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
	sh, err := s.shell(r, "builds", live)
	if err != nil {
		s.fail(w, err)
		return
	}
	project := uint64(0)
	if p.Pipeline != nil {
		project = p.Pipeline.ProjectID
		pinProject(&sh, a.Instance, project)
	}
	running := b.Completed.IsZero()
	ran := d.Built
	for _, sv := range d.Views {
		if sv.State == "failed" || sv.State == "canceled" {
			ran++
		}
	}
	v := &BuildView{ID: id, Target: b.Target, Status: statusOf(p.Reason.Kind, b.Error, d.Built, running), Verdict: d.Verdict,
		Ran: ran, Steps: len(b.Steps), Tone: d.Cat.Tone, Lines: dockerfile(p.Dockerfile, d.Views, b), DfName: p.DockerfileName,
		StepViews: d.Views, Node: shortDaemon(b.Daemon), Failed: b.Error != ""}
	if !running {
		v.Dur = duration(b.Completed.Sub(b.Started))
	}
	if r.URL.Query().Get("via") == "log" && a.State == history.Attributed {
		v.ViaLog = true
		v.LogLine = fmt.Sprintf("ci-accelerator: why this job's builds take their time: %s/jobs/%s/%d", origin(r), a.Instance, a.Job)
	}
	var job store.JobInfo
	if p.Job != nil {
		job = *p.Job
		v.Name, v.GitLabURL = job.Name, webURL(job.URL)
	}
	if v.Name == "" {
		v.Name = orDefault(b.Target)
	}
	sh.Title = v.Name
	if pl := p.Pipeline; pl != nil {
		v.PipeURL, v.PipeLabel = "/pipelines/"+store.PipelineKey(pl.Instance, pl.ID), "#"+strconv.FormatUint(pl.ID, 10)
		v.Commit, v.SHA, v.Ref = pl.CommitTitle, short(pl.SHA, 8), pl.Ref
	}
	if api := s.api(r); api != nil && job.ID != 0 && api.Knows(a.Instance) && job.ProjectID != 0 {
		v.LogURL = fmt.Sprintf("/jobs/%s/%d/log", a.Instance, job.ID)
	}
	// The usual time of builds like this one: from the cache, or rebuilt.
	var usual time.Duration
	if project != 0 {
		st, err := s.recent(ctx, store.ProjectKey{Instance: a.Instance, ID: project})
		if err != nil {
			s.fail(w, err)
			return
		}
		usual, _ = st.usual[b.Target].of(d.Built > 0)
	}
	if usual == 0 {
		if usual, err = s.data.Usual(ctx, b, project); err != nil {
			s.fail(w, err)
			return
		}
	}
	if usual > 0 && !running {
		v.Usual = duration(usual)
		if b.Error == "" && d.Built > 0 {
			v.Delta, v.Slower = delta(b.Completed.Sub(b.Started), usual)
		}
	}
	v.Blocks = blocksOf(b, d.Views)
	v.Gantt, v.GanttSkip = gantt(v.Blocks)
	v.StepCount = len(b.Steps)
	prev, _, err := s.data.Previous(ctx, b)
	if err != nil {
		s.fail(w, err)
		return
	}
	if v.Changes, err = s.changes(ctx, b, prev, p.Reason, p.Via, d); err != nil {
		s.fail(w, err)
		return
	}
	if prev != nil {
		v.PrevURL = fmt.Sprintf("/builds/%d", prev.ID)
		v.PrevLabel = "the previous build"
		if pa := prev.Build.Attribution; pa.State == history.Attributed {
			v.PrevLabel = "the previous build, pipeline #" + strconv.FormatUint(pa.Pipeline, 10)
		}
	}
	nodeHost := map[string]string{}
	// Without the daemons' live state (none known) nothing is said gone.
	if daemons := s.data.Daemons(); len(daemons) > 0 {
		for _, dm := range daemons {
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
	if a.State == history.Attributed {
		v.Trace = traceID(a)
	}
	v.Details = []KV{{"Build node", b.Daemon + paren(nodeHost[b.Daemon])}}
	if p.Route != nil {
		v.Details = append(v.Details, KV{"Routed by", routeText(*p.Route)})
	}
	if job.ID != 0 {
		jt := timesOf(job, b.Started, b.Completed)
		v.Details = append(v.Details, KV{"GitLab job", fmt.Sprintf("%s · %d · %s in all, %s of it building", job.Name, job.ID, duration(jt.Total), v.Dur)})
		if job.Waited > 0 {
			v.Details = append(v.Details, KV{"Waited for a runner", duration(job.Waited)})
		}
		if job.Runner != "" {
			v.Details = append(v.Details, KV{"Runner", job.Runner})
		}
	}
	if b.ContextBytes > 0 {
		v.Details = append(v.Details, KV{"Build context sent", bytesText(b.ContextBytes)})
	}
	if p.Saved > 0 {
		v.Details = append(v.Details, KV{"Time the cache saved, about", duration(p.Saved)})
	}
	started := when(b.Started, zone(r))
	if !strings.HasSuffix(started.Text, " UTC") {
		started.Text += " · " + started.UTC
	}
	v.Details = append(v.Details, KV{"Started", started.Text})
	if len(b.Images) > 0 {
		v.Details = append(v.Details, KV{"Pushed", strings.Join(b.Images, ", ")})
	}
	if b.Error != "" {
		v.Details = append(v.Details, KV{"Error", b.Error})
	}
	if v.Trace != "" {
		v.Details = append(v.Details, KV{"Trace id", v.Trace})
	}
	v.Details = append(v.Details, KV{"Record", b.Ref}, KV{"API", fmt.Sprintf("GET /api/v1/builds?limit=50 (this build is id %d)", id)})
	if a.State == history.Attributed {
		ids, err := s.data.JobBuilds(ctx, a.Instance, a.Job)
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
				Dur: duration(ob.Completed.Sub(ob.Started)), Status: statusOf(o.Page.Reason.Kind, ob.Error, o.Built, ob.Completed.IsZero())})
		}
	}
	v.Shell = sh
	s.render(w, "build", http.StatusOK, v)
}

// changes says what differs from the previous build of the target, as far
// as BuildKit's records tell: only what differs.
func (s *Server) changes(ctx context.Context, b *history.Build, prev *store.Stored, r history.Reason, via string, d *built) ([]Change, error) {
	if prev == nil {
		return nil, nil
	}
	pb := prev.Build
	var out []Change
	if pa, a := pb.Attribution, b.Attribution; pa.State == history.Attributed && a.State == history.Attributed && pa.Pipeline != a.Pipeline {
		before, err := s.data.PipelineInfo(ctx, pa.Instance, pa.Pipeline)
		if err != nil {
			return nil, err
		}
		now := d.Page.Pipeline
		if before != nil && now != nil && before.SHA != now.SHA && now.SHA != "" {
			out = append(out, Change{"Commit", template.HTML(fmt.Sprintf(`<span class="mono">%s</span> → <span class="mono">%s</span> %s`,
				e(short(before.SHA, 8)), e(short(now.SHA, 8)), e(now.CommitTitle)))})
		}
	}
	if pb.Daemon != b.Daemon {
		out = append(out, Change{"Node", template.HTML(fmt.Sprintf(`<span class="old">%s</span> → <span class="new">%s</span> <span class="muted">— the previous build's cache is on %s</span>`,
			e(shortDaemon(pb.Daemon)), e(shortDaemon(b.Daemon)), e(shortDaemon(pb.Daemon))))})
	}
	at := ""
	if r.Step.Name != "" {
		at = code(r.Step.Name)
	}
	switch r.Kind {
	case history.StepChanged, history.NewStep:
		if r.Old != "" && r.Old != r.New {
			out = append(out, Change{"Dockerfile", template.HTML(fmt.Sprintf(`<span class="old mono">%s</span><br><span class="new mono">%s</span>`, e(r.Old), e(r.New)))})
		} else {
			out = append(out, Change{"Dockerfile", template.HTML(at + " is new or changed")})
		}
	case history.ArgChanged:
		out = append(out, Change{"Build argument", template.HTML(fmt.Sprintf(`<code>%s</code>: <span class="old mono">%s</span> → <span class="new mono">%s</span>`, e(r.Arg), e(r.Old), e(r.New)))})
	case history.BaseImageChanged:
		out = append(out, Change{"Base image", template.HTML(fmt.Sprintf(`<span class="old mono">%s</span> → <span class="new mono">%s</span>`, e(r.Old), e(r.New)))})
	case history.FilesChanged:
		if viaVerified(via) {
			out = append(out, Change{"Files", template.HTML(orContext(r.Paths) + " — changed (GitLab's comparison of the commits)")})
		} else {
			out = append(out, Change{"Files", template.HTML(at + " builds on " + orContext(r.Paths) + " — which of them changed, the records cannot tell")})
		}
	case history.CacheElsewhere:
		why := "this node's cache no longer had the step, and BuildKit does not record the files"
		if r.Old != "" {
			why = "the previous build ran on another node, and BuildKit does not record the files"
		}
		out = append(out, Change{"Files", template.HTML(at + " reads " + orContext(r.Paths) + " — changed or not: " + why)})
	}
	prevBuilt := 0
	for _, st := range pb.Steps {
		if st.State == history.Built && !isFrom(st) {
			prevBuilt++
		}
	}
	if prevBuilt != d.Built {
		out = append(out, Change{"Steps run", template.HTML(fmt.Sprintf("%d, last time %d", d.Built, prevBuilt))})
	}
	return out, nil
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
	if errors.Is(err, store.ErrNotFound) || (err == nil && (idx < 0 || idx >= len(d.Page.Build.Steps) || !s.mayBuild(r, d.Page))) {
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
		out, err := s.data.Output(ctx, id, idx)
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
	runs, err := s.data.StepHistory(ctx, id, idx, 12)
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
		// The same step sits at another index in a build with other steps
		// (a Dockerfile that gained a step, a build that received steps).
		col := HistCol{URL: fmt.Sprintf("/builds/%d/steps/%d", run.BuildID, run.Idx), Label: duration(run.Duration), SHA: short(run.SHA, 6),
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

func paren(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}

// webURL keeps a link to GitLab only when it is one: job data comes from
// CI variables, and anything but http(s) would only become a dead link.
func webURL(u string) string {
	if strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		return u
	}
	return ""
}

func orDefault(target string) string {
	if target == "" {
		return "the default target"
	}
	return target
}

// ago is a past moment with its day, in the reader's zone.
func ago(t time.Time, loc *time.Location) string { return stampOf(t, loc, "2 Jan 15:04").Text }

// traceID is the trace id the runner gives a job.
func traceID(a history.Attribution) string {
	return fmt.Sprintf("01%s%012x%012x", a.Instance, a.Pipeline, a.Job)
}

// routeText says why the build router sent the job to the build's node.
func routeText(d store.Decision) string {
	node := d.Node
	switch d.Reason {
	case route.LocalWarm:
		return fmt.Sprintf("the build router: the job's own node, %s, which built the project before", node)
	case route.ZoneWarm:
		if d.JobNode != "" && d.JobNode != node {
			return fmt.Sprintf("the build router: %s, which built the project most recently (the job ran on %s)", node, d.JobNode)
		}
		return fmt.Sprintf("the build router: %s, which built the project most recently", node)
	case route.ZoneHash:
		return fmt.Sprintf("the build router: %s, the project's node by hash: no node had built the project in the last week", node)
	case route.CrossZone:
		return fmt.Sprintf("the build router: %s, in another zone: no daemon of the job's zone was ready", node)
	}
	return fmt.Sprintf("the build router: %s (%s)", node, d.Reason)
}
