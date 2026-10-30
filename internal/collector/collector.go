// Package collector reads every build daemon's build history and keeps it in
// the store: each completed record becomes a build with its steps, linked to
// the builds it received work from, with the reason it was rebuilt.
package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"github.com/moby/buildkit/client"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Window is how far back, in build start times, the builds of one daemon
// are kept in memory to link received steps to the build that executed
// them. Builds share work while they run at the same time; after a restart
// the window is read again from the daemon's history.
const Window = 2 * time.Hour

// Source is one daemon's build history.
type Source interface {
	// Name is the daemon's name (its pod), the key of its cache.
	Name() string
	// Listen calls fn with every record the daemon has, then with every
	// event as it happens, until ctx ends or the stream breaks.
	Listen(ctx context.Context, fn func(controlapi.BuildHistoryEventType, *controlapi.BuildHistoryRecord) error) error
	// Open reads one of a record's blobs.
	Open(ctx context.Context, d *controlapi.Descriptor) (io.ReadCloser, error)
}

// Collector turns records into stored builds. It is safe for concurrent
// use: records are processed one at a time.
type Collector struct {
	store *store.Store
	cfg   history.Config
	log   *slog.Logger

	mu      sync.Mutex
	daemons map[string]*window

	live live
}

// New returns a collector writing to s. instances are the trace-id prefixes
// of the GitLab instances the installation serves.
func New(s *store.Store, instances map[string]bool, log *slog.Logger) *Collector {
	return &Collector{
		store:   s,
		cfg:     history.Config{Instances: instances, SecretKey: s.SecretKey()},
		log:     log,
		daemons: map[string]*window{},
	}
}

// window holds a daemon's recent builds and the record generation each was
// read from.
type window struct {
	builds map[string]*history.Build
	gens   map[string]int64
	newest time.Time
	// replayed are the builds read again from a record the store has, with
	// the links they were stored with (step index -> build): such a build
	// is stored again only once linking has found all of them.
	replayed map[string]map[int]string
}

// forget drops a build from the window.
func (w *window) forget(ref string) {
	delete(w.builds, ref)
	delete(w.gens, ref)
	delete(w.replayed, ref)
}

// relinked reports whether b, read again from a stored record, has every
// link it was stored with: before that, storing it again would drop links
// to builds the stream has not replayed yet, and the reason with them.
func (w *window) relinked(b *history.Build) bool {
	want, ok := w.replayed[b.Ref]
	if !ok {
		return true
	}
	for i, from := range want {
		if i >= len(b.Steps) || b.Steps[i].ReceivedFrom != from {
			return false
		}
	}
	return true
}

// Run reads src until ctx ends or its stream breaks.
func (c *Collector) Run(ctx context.Context, src Source) error {
	_, err := c.run(ctx, src)
	return err
}

// run is Run, and returns the number of the stream it read.
func (c *Collector) run(ctx context.Context, src Source) (uint64, error) {
	name := src.Name()
	stream := c.live.connect(name)
	defer func() {
		if errors.Is(context.Cause(ctx), ErrGone) {
			c.gone(name, stream)
		} else {
			c.live.disconnected(name, stream)
		}
	}()
	if in, ok := src.(Inspector); ok {
		ictx, cancel := context.WithCancel(ctx)
		defer cancel()
		go c.inspect(ictx, name, in)
	}
	return stream, src.Listen(ctx, func(t controlapi.BuildHistoryEventType, rec *controlapi.BuildHistoryRecord) error {
		c.live.event(name, t, rec)
		if t != controlapi.BuildHistoryEventType_COMPLETE {
			return nil // STARTED: running, nothing to store yet; DELETED: the store keeps it
		}
		if err := c.Record(ctx, src, rec); err != nil {
			// One bad record must not stop the daemon's stream.
			c.log.Warn("record not stored", "daemon", src.Name(), "ref", rec.Ref, "err", err)
		}
		return nil
	})
}

// gone forgets a daemon whose pod is gone, unless a newer stream than
// stream reads it: what it knew of the daemon, and its builds in memory
// (a pod of the same name reads its history again).
func (c *Collector) gone(name string, stream uint64) {
	if !c.live.forget(name, stream) {
		return
	}
	c.mu.Lock()
	delete(c.daemons, name)
	c.mu.Unlock()
}

// Record stores one completed record and whatever its arrival changes:
// builds of the same daemon linked to it, and the reasons of its pipeline.
func (c *Collector) Record(ctx context.Context, src Source, rec *controlapi.BuildHistoryRecord) error {
	if rec.CompletedAt == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.daemons[src.Name()]
	if w == nil {
		w = &window{builds: map[string]*history.Build{}, gens: map[string]int64{}, replayed: map[string]map[int]string{}}
		c.daemons[src.Name()] = w
	}
	gen := int64(rec.Generation)
	if old, ok := w.gens[rec.Ref]; ok && old >= gen {
		return nil
	}
	// A record stored already belongs to the daemon pod that sent it
	// first: a pod that replaced another on the same hostPath sends the old
	// pod's records again, and reasons compare the daemons builds ran on.
	under, storedGen, err := c.store.StoredUnder(ctx, rec.Ref)
	if err != nil {
		return err
	}
	// A stream read again replays the history in the order of the record
	// ids, not in time order. A stored record older than the window has
	// nothing new to tell, and the builds it was linked with have left
	// memory: read again, it would be stored without its links.
	replay := under != "" && storedGen >= gen
	if replay && rec.GetCreatedAt().AsTime().Before(w.newest.Add(-Window)) {
		return nil
	}
	b, err := c.read(ctx, src, rec)
	if err != nil {
		return err
	}
	if replay {
		links, err := c.store.ReceivedFrom(ctx, b.Ref)
		if err != nil {
			return err
		}
		w.replayed[b.Ref] = links
	} else {
		delete(w.replayed, b.Ref)
	}
	b.Daemon = src.Name()
	if under != "" {
		b.Daemon = under
	}

	before := map[string]string{}
	for ref, x := range w.builds {
		before[ref] = fingerprint(x)
	}
	// BuildKit completes a record before the client's trace is in it, and
	// sends it again a few seconds later with it: that is when a build
	// gets its pipeline and job.
	gotJob := false
	if old := w.builds[b.Ref]; old != nil {
		gotJob = old.Attribution.State != history.Attributed && b.Attribution.State == history.Attributed
	}
	w.builds[b.Ref], w.gens[b.Ref] = b, gen
	if b.Started.After(w.newest) {
		w.newest = b.Started
	}
	// Builds older than the window leave memory; the one that just
	// arrived is stored all the same (a reconnecting stream replays old
	// records after newer ones) and leaves after.
	old := b.Started.Before(w.newest.Add(-Window))
	for ref, x := range w.builds {
		if x != b && x.Started.Before(w.newest.Add(-Window)) {
			w.forget(ref)
			delete(before, ref)
		}
	}
	defer func() {
		if old {
			w.forget(b.Ref)
		}
	}()
	list := make([]*history.Build, 0, len(w.builds))
	for _, x := range w.builds {
		list = append(list, x)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Started.Before(list[j].Started) })
	history.Link(list)

	// A record read again (the stream starts over after a restart) is
	// logged quietly: only a build new to the store is news.
	known := under != ""
	// Save in start order, so that a build that executed a step is stored
	// before the builds that received it.
	type pipelineKey struct {
		instance string
		id       uint64
	}
	pipelines := map[pipelineKey]bool{}
	for _, x := range list {
		if x != b {
			if f, ok := before[x.Ref]; !ok || f == fingerprint(x) {
				continue
			}
		}
		if !w.relinked(x) {
			continue
		}
		r, err := c.save(ctx, x, w.gens[x.Ref])
		if err != nil {
			if x == b {
				// Not stored: the record is read again when it comes again.
				w.forget(b.Ref)
			}
			return err
		}
		// Stored with all its links: linking does not take links away.
		delete(w.replayed, x.Ref)
		c.logStored(x, r, x == b && !known, x == b && gotJob)
		if a := x.Attribution; a.State == history.Attributed {
			pipelines[pipelineKey{a.Instance, a.Pipeline}] = true
		}
		// Records arrive out of order (one stream per daemon, history read
		// again after a restart): the next build of the target may have
		// been compared with an older one.
		changed, err := c.recompute(ctx, x)
		if err != nil {
			return err
		}
		for _, n := range changed {
			pipelines[pipelineKey{n.Attribution.Instance, n.Attribution.Pipeline}] = true
		}
	}
	for p := range pipelines {
		if err := c.settle(ctx, p.instance, p.id); err != nil {
			return err
		}
	}
	return nil
}

// JobKnown computes the reasons of a job's builds again once its project is
// known (the job's pod was seen after its builds were stored).
func (c *Collector) JobKnown(ctx context.Context, instance string, job uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ids, err := c.store.JobBuilds(ctx, instance, job)
	if err != nil {
		return err
	}
	var pipeline uint64
	for _, id := range ids {
		cur, err := c.store.Load(ctx, id)
		if err != nil {
			return err
		}
		r, err := c.reason(ctx, cur.Build)
		if err != nil {
			return err
		}
		if err := c.store.SetReason(ctx, cur.Build.Ref, cur.Build.Daemon, r); err != nil {
			return err
		}
		pipeline = cur.Build.Attribution.Pipeline
		// Its project now known, it is also the previous build of the next.
		changed, err := c.recompute(ctx, cur.Build)
		if err != nil {
			return err
		}
		for _, n := range changed {
			if err := c.settle(ctx, n.Attribution.Instance, n.Attribution.Pipeline); err != nil {
				return err
			}
		}
	}
	if pipeline != 0 {
		if err := c.settle(ctx, instance, pipeline); err != nil {
			return err
		}
	}
	return c.store.CompareDone(ctx, instance, job)
}

// CompareDue compares the builds that took their project after they were
// stored and whose comparison has not been done (JobKnown failed, or the
// controller stopped before it), each job with its own time.
func (c *Collector) CompareDue(ctx context.Context) error {
	due, err := c.store.DueJobs(ctx, 100)
	if err != nil {
		return err
	}
	for _, j := range due {
		jctx, cancel := context.WithTimeout(ctx, time.Minute)
		err := c.JobKnown(jctx, j.Instance, j.Job)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.log.Warn("job's builds not compared", "job", j.Job, "err", err)
		}
	}
	return nil
}

// recompute compares the builds after b (same project and target) again,
// now that b is stored, and returns those whose reason changed: the failed
// builds up to the next that succeeded, and that one (store.Next). Builds
// of one pipeline never compare with each other, so EVERY build of the
// next pipeline that built this target has b as its previous build.
func (c *Collector) recompute(ctx context.Context, b *history.Build) ([]*history.Build, error) {
	next, err := c.store.Next(ctx, b)
	if err != nil || len(next) == 0 {
		return nil, err
	}
	candidates := next
	listed := map[string]bool{}
	for _, n := range next {
		listed[n.Build.Ref] = true
	}
	if p := next[len(next)-1].Build.Attribution; p.State == history.Attributed {
		// Pipeline() does not restore the graphs; Load the siblings.
		sts, err := c.store.Pipeline(ctx, p.Instance, p.Pipeline)
		if err != nil {
			return nil, err
		}
		for _, st := range sts {
			if st.Build.Target != b.Target || st.Build.Ref == b.Ref || listed[st.Build.Ref] {
				continue
			}
			loaded, err := c.store.Load(ctx, st.ID)
			if err != nil {
				return nil, err
			}
			candidates = append(candidates, loaded)
		}
	}
	var changed []*history.Build
	for _, cand := range candidates {
		r, err := c.reason(ctx, cand.Build)
		if err != nil {
			return nil, err
		}
		if r.Kind == cand.Reason.Kind && r.Step.Key == cand.Reason.Step.Key && r.Old == cand.Reason.Old && r.New == cand.Reason.New {
			continue
		}
		if err := c.store.SetReason(ctx, cand.Build.Ref, cand.Build.Daemon, r); err != nil {
			return nil, err
		}
		changed = append(changed, cand.Build)
	}
	return changed, nil
}

func (c *Collector) save(ctx context.Context, b *history.Build, gen int64) (history.Reason, error) {
	r, err := c.reason(ctx, b)
	if err != nil {
		return r, err
	}
	_, err = c.store.SaveBuild(ctx, store.Build{Build: b, Generation: gen, Reason: r})
	return r, err
}

// logStored says a build was stored: one line per build new to the store
// and one when it gets its job (info), and one per build stored again, read
// again after a restart or changed by what another one received (debug).
func (c *Collector) logStored(b *history.Build, r history.Reason, arrived, gotJob bool) {
	var built, cached, received int
	for _, st := range b.Steps {
		switch st.State {
		case history.Built:
			built++
		case history.Cached:
			cached++
		case history.Received:
			received++
		}
	}
	args := []any{"build", b.Ref, "daemon", b.Daemon, "target", b.Target, "steps", len(b.Steps), "built", built, "cached", cached,
		"received", received, "dur_ms", b.Completed.Sub(b.Started).Milliseconds(), "failed", b.Error != ""}
	if a := b.Attribution; a.State == history.Attributed {
		args = append(args, "gitlab", a.Instance, "pipeline", a.Pipeline, "job", a.Job)
	} else {
		args = append(args, "attribution", string(a.State))
	}
	if r.Kind != "" {
		args = append(args, "reason", string(r.Kind))
	}
	switch {
	case arrived:
		c.log.Info("build stored", args...)
	case gotJob:
		c.log.Info("build attributed", args...)
	default:
		c.log.Debug("build stored again", args...)
	}
}

// reason compares a build with the previous one of its project and target.
// A build whose project is not known (not attributed, or its job not seen
// yet) gets none: it is computed again when its job arrives (JobKnown).
func (c *Collector) reason(ctx context.Context, b *history.Build) (history.Reason, error) {
	prev, known, err := c.store.Previous(ctx, b)
	if err != nil {
		// A previous build that cannot be restored (for example at the edge
		// of retention) leaves this one without a reason, not unstored.
		c.log.Warn("previous build not readable", "ref", b.Ref, "err", err)
		return history.Reason{}, nil
	}
	if !known {
		return history.Reason{}, nil
	}
	var p *history.Build
	if prev != nil {
		p = prev.Build
	}
	r, ok := history.FirstRebuild(p, b)
	if !ok && b.Error != "" {
		r, _ = history.FailedStep(p, b)
	}
	return r, nil
}

// settle decides the "cache elsewhere" reasons of a pipeline with its
// other builds and stores the ones that change.
func (c *Collector) settle(ctx context.Context, instance string, pipeline uint64) error {
	stored, err := c.store.Pipeline(ctx, instance, pipeline)
	if err != nil {
		return err
	}
	builds := make([]*history.Build, len(stored))
	reasons := make([]history.Reason, len(stored))
	for i, s := range stored {
		builds[i], reasons[i] = s.Build, s.Reason
	}
	for i, r := range history.Settle(builds, reasons) {
		if r.Kind != reasons[i].Kind || r.By != reasons[i].By {
			if err := c.store.SetReason(ctx, builds[i].Ref, builds[i].Daemon, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// maxBlob bounds a record's provenance, trace and error blobs, which are
// read whole. BuildKit bounds none of them, and any build can add spans to
// its trace.
const maxBlob = 64 << 20

// read fetches a record's blobs and assembles the build. The progress blob
// is read as it streams: BuildKit does not bound it, and only the head and
// tail of each step's log are kept.
func (c *Collector) read(ctx context.Context, src Source, rec *controlapi.BuildHistoryRecord) (*history.Build, error) {
	blob := func(d *controlapi.Descriptor) ([]byte, error) {
		if d == nil || d.Digest == "" {
			return nil, nil
		}
		r, err := src.Open(ctx, d)
		if err != nil {
			return nil, err
		}
		defer r.Close()
		b, err := io.ReadAll(io.LimitReader(r, maxBlob+1))
		if err == nil && len(b) > maxBlob {
			err = fmt.Errorf("%s is larger than %d MiB", d.Digest, maxBlob>>20)
		}
		return b, err
	}
	var progress []*client.SolveStatus
	if d := rec.Logs; d != nil && d.Digest != "" {
		r, err := src.Open(ctx, d)
		if err != nil {
			return nil, fmt.Errorf("logs: %w", err)
		}
		progress, err = history.DecodeProgress(r)
		r.Close()
		if err != nil {
			return nil, err
		}
	}
	var prov [][]byte
	for _, d := range provenance(rec) {
		p, err := blob(d)
		if err != nil {
			return nil, fmt.Errorf("provenance: %w", err)
		}
		prov = append(prov, p)
	}
	trace, err := blob(rec.Trace)
	if err != nil {
		return nil, fmt.Errorf("trace: %w", err)
	}
	var errBlob []byte
	if rec.Error != nil {
		// What failed and where in the Dockerfile; a build is stored
		// without it rather than not at all.
		errBlob, _ = blob(rec.ExternalError)
	}
	return history.NewBuild(rec, progress, prov, trace, errBlob, c.cfg)
}

// provenance picks the SLSA provenance attestation of each of the record's
// results: a build for several platforms has one result per platform, and
// each one's provenance covers only that platform's steps. Results are in
// platform name order.
func provenance(rec *controlapi.BuildHistoryRecord) []*controlapi.Descriptor {
	results := []*controlapi.BuildResultInfo{rec.Result}
	var platforms []string
	for p := range rec.Results {
		platforms = append(platforms, p)
	}
	sort.Strings(platforms)
	for _, p := range platforms {
		results = append(results, rec.Results[p])
	}
	var out []*controlapi.Descriptor
	for _, r := range results {
		if r == nil || len(r.Attestations) == 0 {
			continue
		}
		att := r.Attestations[0]
		for _, a := range r.Attestations {
			if strings.Contains(a.Annotations["in-toto.io/predicate-type"], "slsa.dev/provenance") {
				att = a
				break
			}
		}
		out = append(out, att)
	}
	return out
}

// fingerprint is what linking can change in a build.
func fingerprint(b *history.Build) string {
	var sb strings.Builder
	for _, s := range b.Steps {
		fmt.Fprintf(&sb, "%s|%s|%s|%d|%d;", s.Key, s.State, s.ReceivedFrom, s.Started.UnixNano(), s.Completed.UnixNano())
	}
	return sb.String()
}
