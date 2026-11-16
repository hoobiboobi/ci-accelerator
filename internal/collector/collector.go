// Package collector reads every build daemon's build history and keeps it in
// the store: each completed record becomes a build with its steps, linked to
// the builds it received work from, with the reason it was rebuilt.
package collector

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
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
// use: records are linked and stored one at a time; their blobs are read
// from the daemons concurrently, outside that order.
type Collector struct {
	store *store.Store
	cfg   history.Config
	log   *slog.Logger

	// sem is the lock of daemons and of linking and storing: a channel, so
	// that a caller with a deadline stops waiting when it passes.
	semOnce sync.Once
	sem     chan struct{}
	daemons map[string]*window

	live  live
	watch watches
}

// New returns a collector writing to s. instances are the trace-id prefixes
// of the GitLab instances the installation serves.
func New(s *store.Store, instances map[string]bool, log *slog.Logger) *Collector {
	return &Collector{
		store:   s,
		cfg:     history.Config{Instances: instances, SecretKey: s.SecretKey()},
		log:     log,
		daemons: map[string]*window{},
		live:    live{instances: instances},
	}
}

// lock takes the collector's lock, or gives up when ctx ends.
func (c *Collector) lock(ctx context.Context) error {
	c.semOnce.Do(func() { c.sem = make(chan struct{}, 1) })
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Collector) unlock() { <-c.sem }

// windowOf returns daemon name's window, made when missing. The lock is
// held.
func (c *Collector) windowOf(name string) *window {
	w := c.daemons[name]
	if w == nil {
		w = &window{builds: map[string]*history.Build{}, gens: map[string]int64{}, replayed: map[string]map[int]string{}}
		c.daemons[name] = w
	}
	return w
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
	stream := c.live.connectSource(name, src)
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
	// Records that could not be stored are tried again while the stream
	// lasts: BuildKit sends a completed record again only when it changes.
	failed := &retries{m: map[string]*pending{}}
	rctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.retry(rctx, src, failed)
	}()
	defer func() {
		stop()
		wg.Wait()
	}()
	return stream, src.Listen(ctx, func(t controlapi.BuildHistoryEventType, rec *controlapi.BuildHistoryRecord) error {
		if t != controlapi.BuildHistoryEventType_COMPLETE {
			c.live.event(name, t, rec)
			return nil // STARTED: running, nothing to store yet; DELETED: the store keeps it
		}
		// A completed build stays among the running ones until it is
		// stored: its job's page goes from the build in progress straight
		// to the stored build.
		defer c.live.event(name, t, rec)
		if err := c.record(ctx, src, rec); err != nil {
			// One bad record must not stop the daemon's stream.
			c.log.Warn("record not stored", "daemon", src.Name(), "ref", rec.Ref, "err", err)
			if ctx.Err() == nil {
				failed.add(rec)
			}
		} else {
			failed.drop(rec)
		}
		return nil
	})
}

// Variables for the tests: how often records that could not be stored
// are tried again, and how many times.
var (
	retryEvery = 30 * time.Second
	retryTries = 5
)

// maxRetries bounds the records of one stream waiting to be tried again.
const maxRetries = 1000

// retries are a stream's records that could not be stored, by ref.
type retries struct {
	mu sync.Mutex
	m  map[string]*pending
}

type pending struct {
	rec   *controlapi.BuildHistoryRecord
	tries int
}

func (r *retries) add(rec *controlapi.BuildHistoryRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.m[rec.Ref]; p != nil {
		if rec.Generation >= p.rec.Generation {
			p.rec = rec
		}
		return
	}
	if len(r.m) < maxRetries {
		r.m[rec.Ref] = &pending{rec: rec}
	}
}

// drop forgets a record stored now, unless a newer generation waits.
func (r *retries) drop(rec *controlapi.BuildHistoryRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.m[rec.Ref]; p != nil && p.rec.Generation <= rec.Generation {
		delete(r.m, rec.Ref)
	}
}

func (r *retries) list() []*pending {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*pending, 0, len(r.m))
	for _, p := range r.m {
		out = append(out, p)
	}
	return out
}

// retry tries the records in failed again every retryEvery until ctx ends;
// one that fails retryTries times more is given up (the stream read again
// after a reconnect or restart brings it back).
func (c *Collector) retry(ctx context.Context, src Source, failed *retries) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryEvery):
		}
		for _, p := range failed.list() {
			err := c.record(ctx, src, p.rec)
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				failed.drop(p.rec)
				continue
			}
			failed.mu.Lock()
			p.tries++
			over := p.tries >= retryTries
			if over && failed.m[p.rec.Ref] == p {
				delete(failed.m, p.rec.Ref)
			}
			failed.mu.Unlock()
			if over {
				c.log.Warn("record not stored, given up", "daemon", src.Name(), "ref", p.rec.Ref, "tries", p.tries+1, "err", err)
			}
		}
	}
}

// record is Record for a stream: a panic while one record is read or
// stored is that record's error, not the end of the controller (the
// daemon would send the same record again after the restart).
func (c *Collector) record(ctx context.Context, src Source, rec *controlapi.BuildHistoryRecord) (err error) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error("record panicked", "daemon", src.Name(), "ref", rec.GetRef(), "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return c.Record(ctx, src, rec)
}

// gone forgets a daemon whose pod is gone, unless a newer stream than
// stream reads it: what it knew of the daemon, and its builds in memory
// (a pod of the same name reads its history again).
func (c *Collector) gone(name string, stream uint64) {
	if !c.live.forget(name, stream) {
		return
	}
	_ = c.lock(context.Background())
	delete(c.daemons, name)
	c.unlock()
}

// Record stores one completed record and whatever its arrival changes:
// builds of the same daemon linked to it, and the reasons of its pipeline.
// The record's blobs are read from the daemon without the collector's
// lock, so a daemon that stalls holds up only its own records.
func (c *Collector) Record(ctx context.Context, src Source, rec *controlapi.BuildHistoryRecord) (err error) {
	if rec.CompletedAt == nil {
		return nil
	}
	gen := int64(rec.Generation)
	// A stream read again replays the history in the order of the record
	// ids, not in time order. A stored record older than the window has
	// nothing new to tell, and the builds it was linked with have left
	// memory: read again, it would be stored without its links.
	stale := func(newest time.Time, under string, storedGen int64) bool {
		return under != "" && storedGen >= gen && rec.GetCreatedAt().AsTime().Before(newest.Add(-Window))
	}
	if err := c.lock(ctx); err != nil {
		return err
	}
	w := c.windowOf(src.Name())
	if old, ok := w.gens[rec.Ref]; ok && old >= gen {
		c.unlock()
		return nil
	}
	newest := w.newest
	c.unlock()
	under, storedGen, err := c.store.StoredUnder(ctx, rec.Ref)
	if err != nil {
		return err
	}
	if stale(newest, under, storedGen) {
		return nil
	}
	// Said before the read: a record whose reading ends the process (out
	// of memory) is then named by the last line its log has. A record read
	// again is debug, as its "stored again" line.
	level := slog.LevelDebug
	if under == "" {
		level = slog.LevelInfo
	}
	c.log.Log(ctx, level, "reading record", "daemon", src.Name(), "ref", rec.Ref, "logs_bytes", rec.GetLogs().GetSize())
	b, err := c.readRecord(ctx, src, rec)
	if err != nil {
		return err
	}

	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.unlock()
	w = c.windowOf(src.Name())
	if old, ok := w.gens[rec.Ref]; ok && old >= gen {
		return nil // another stream of the daemon took it meanwhile
	}
	// A record stored already belongs to the daemon pod that sent it
	// first: a pod that replaced another on the same hostPath sends the old
	// pod's records again, and reasons compare the daemons builds ran on.
	// Asked again under the lock: another stream may have stored it.
	under, storedGen, err = c.store.StoredUnder(ctx, rec.Ref)
	if err != nil {
		return err
	}
	if stale(w.newest, under, storedGen) {
		return nil
	}
	replay := under != "" && storedGen >= gen
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
	// Not stored: the record is read again when it comes again (or is
	// tried again), whichever build's save failed.
	saved := false
	defer func() {
		if err != nil && !saved {
			w.forget(b.Ref)
		}
	}()
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
			return err
		}
		if x == b {
			saved = true
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
	if err := c.lock(ctx); err != nil {
		return err
	}
	defer c.unlock()
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
// other builds and stores the ones that change. The project's builds of
// other pipelines that ran meanwhile on the same daemons go with them (a
// merge request's pipeline and its branch's, of one commit): a step one of
// them executed on a witness's daemon makes that witness's cache hit no
// proof. Only this pipeline's reasons are written.
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
	others, err := c.overlapping(ctx, instance, pipeline, stored)
	if err != nil {
		return err
	}
	all := append(append(make([]*history.Build, 0, len(builds)+len(others)), builds...), others...)
	// The others' own reasons settle nothing here: they are left zero.
	allReasons := append(append(make([]history.Reason, 0, len(all)), reasons...), make([]history.Reason, len(others))...)
	settled := history.Settle(all, allReasons)
	for i, r := range settled[:len(builds)] {
		if r.Kind != reasons[i].Kind || r.By != reasons[i].By {
			if err := c.store.SetReason(ctx, builds[i].Ref, builds[i].Daemon, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// overlapMargin widens the time a pipeline's builds ran, backwards, for
// the builds of other pipelines read with them: one that started a little
// earlier may have executed a step on a daemon while they ran.
const overlapMargin = 10 * time.Minute

// maxOverlapping bounds the other pipelines' builds read for one settle.
const maxOverlapping = 200

// unchangedUntil returns when the earliest previous build that one of the
// pipeline's open reasons (cache elsewhere, or files changed by a sibling,
// which Settle may reopen) compares with started: a build of another
// pipeline that started then or earlier ran before every such change, so
// a step it executed is the cache a witness legitimately finds (the
// previous pipeline built the step on the witness's node), not the
// change's product. Zero when no open reason has a previous build.
func (c *Collector) unchangedUntil(ctx context.Context, stored []*store.Stored) time.Time {
	var at time.Time
	for _, s := range stored {
		r := s.Reason
		if r.Kind != history.CacheElsewhere && (r.Kind != history.FilesChanged || r.By == "") {
			continue
		}
		started, ok, err := c.store.PreviousStarted(ctx, s.Build)
		if err != nil {
			// Without the boundary every other pipeline's execution counts:
			// a witness proves less, never more.
			c.log.Warn("previous build not readable", "ref", s.Build.Ref, "err", err)
			return time.Time{}
		}
		if !ok {
			return time.Time{}
		}
		if at.IsZero() || started.Before(at) {
			at = started
		}
	}
	return at
}

// overlapping returns the stored builds of the pipeline's project, of
// other pipelines, that started from overlapMargin before its first build
// to the end of its last one on a daemon one of its builds ran on. None
// when the pipeline's project is not known.
func (c *Collector) overlapping(ctx context.Context, instance string, pipeline uint64, stored []*store.Stored) ([]*history.Build, error) {
	if len(stored) == 0 {
		return nil, nil
	}
	project, err := c.store.PipelineProject(ctx, instance, pipeline)
	if err != nil || project == 0 {
		return nil, err
	}
	daemons := map[string]bool{}
	mine := map[string]bool{}
	from, to := stored[0].Build.Started, stored[0].Build.Started
	for _, s := range stored {
		b := s.Build
		daemons[b.Daemon], mine[b.Ref] = true, true
		if b.Started.Before(from) {
			from = b.Started
		}
		end := b.Completed
		if end.IsZero() {
			end = b.Started
		}
		if end.After(to) {
			to = end
		}
	}
	rows, err := c.store.Builds(ctx, store.BuildQuery{Project: store.ProjectKey{Instance: instance, ID: project},
		Since: from.Add(-overlapMargin), Before: to.Add(time.Millisecond), Limit: maxOverlapping, Light: true})
	if err != nil {
		return nil, err
	}
	before := c.unchangedUntil(ctx, stored)
	want := map[int64]bool{}
	var pipelines []uint64
	seen := map[uint64]bool{}
	for _, r := range rows {
		if r.Pipeline == pipeline || r.Daemon == "" || !daemons[r.Daemon] {
			continue
		}
		if !before.IsZero() && !r.Started.After(before) {
			continue // ran before the change: its cache is what a witness may find
		}
		want[r.ID] = true
		if r.Pipeline != 0 && !seen[r.Pipeline] {
			seen[r.Pipeline] = true
			pipelines = append(pipelines, r.Pipeline)
		}
	}
	var out []*history.Build
	add := func(s *store.Stored) {
		if want[s.ID] && !mine[s.Build.Ref] {
			out = append(out, s.Build)
		}
		delete(want, s.ID)
	}
	// One read per pipeline (steps, no graphs: enough for Settle).
	for _, p := range pipelines {
		sts, err := c.store.Pipeline(ctx, instance, p)
		if err != nil {
			return nil, err
		}
		for _, s := range sts {
			if want[s.ID] {
				add(s)
			}
		}
	}
	ids := make([]int64, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		s, err := c.store.Load(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue // pruned meanwhile
		}
		if err != nil {
			return nil, err
		}
		add(s)
	}
	return out, nil
}

// maxBlob bounds a record's provenance, trace and error blobs, which are
// read whole. BuildKit bounds none of them, and any build can add spans to
// its trace. A variable for the tests.
var maxBlob = 64 << 20

// errTooLarge: a blob is larger than maxBlob.
var errTooLarge = errors.New("larger than the collector reads")

// readIdle is how long a record's blobs may make no progress before their
// reading is given up (the daemon is asked for each piece). A variable for
// the tests.
var readIdle = 2 * time.Minute

// errStalled ends the reading of a record whose daemon sent nothing for
// readIdle.
var errStalled = errors.New("collector: the daemon sent nothing for too long")

// readRecord is read with a deadline that moves with every piece read: a
// daemon that stops answering mid-blob fails the record instead of holding
// its stream for good.
func (c *Collector) readRecord(ctx context.Context, src Source, rec *controlapi.BuildHistoryRecord) (*history.Build, error) {
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	t := time.AfterFunc(readIdle, func() { cancel(errStalled) })
	defer t.Stop()
	b, err := c.read(rctx, idleSource{Source: src, t: t}, rec)
	if err != nil && errors.Is(context.Cause(rctx), errStalled) {
		return nil, fmt.Errorf("%w (%s): %v", errStalled, readIdle, err)
	}
	return b, err
}

// idleSource moves its timer on with every piece of a blob read.
type idleSource struct {
	Source
	t *time.Timer
}

func (s idleSource) Open(ctx context.Context, d *controlapi.Descriptor) (io.ReadCloser, error) {
	r, err := s.Source.Open(ctx, d)
	if err != nil {
		return nil, err
	}
	s.t.Reset(readIdle)
	return idleReader{ReadCloser: r, t: s.t}, nil
}

type idleReader struct {
	io.ReadCloser
	t *time.Timer
}

func (r idleReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if n > 0 {
		r.t.Reset(readIdle)
	}
	return n, err
}

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
		b, err := io.ReadAll(io.LimitReader(r, int64(maxBlob)+1))
		if err == nil && len(b) > maxBlob {
			err = fmt.Errorf("%s is %w (%d MiB)", d.Digest, errTooLarge, maxBlob>>20)
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
	if errors.Is(err, errTooLarge) {
		// Any build can add spans to a trace: a trace too large to read
		// leaves the build without its job, not out of the store.
		c.log.Warn("trace too large: build stored without its job", "ref", rec.Ref, "err", err)
		trace, err = nil, nil
	}
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
