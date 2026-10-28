package collector

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	controlapi "github.com/moby/buildkit/api/services/control"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

var ctx = context.Background()

var instances = map[string]bool{history.InstancePrefix("https://gitlab.example.com/"): true}

// fake is a daemon that replays recorded history.
type fake struct {
	name  string
	recs  []*controlapi.BuildHistoryRecord
	blobs map[string][]byte
}

func (f *fake) Name() string { return f.name }

func (f *fake) Listen(ctx context.Context, fn func(controlapi.BuildHistoryEventType, *controlapi.BuildHistoryRecord) error) error {
	for _, r := range f.recs {
		if err := fn(controlapi.BuildHistoryEventType_COMPLETE, r); err != nil {
			return err
		}
	}
	return nil
}

func (f *fake) Blob(ctx context.Context, d *controlapi.Descriptor) ([]byte, error) {
	b, ok := f.blobs[d.Digest]
	if !ok {
		return nil, fmt.Errorf("no blob %s", d.Digest)
	}
	return b, nil
}

// recorded reads the history package's fixtures (one daemon) as records
// and blobs, oldest first.
func recorded(t *testing.T) *fake {
	t.Helper()
	dirs, err := filepath.Glob("../history/testdata/records/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	f := &fake{name: "daemon-a", blobs: map[string][]byte{}}
	for _, d := range dirs {
		read := func(n string) []byte {
			b, err := os.ReadFile(filepath.Join(d, n))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			return b
		}
		rec, err := history.ParseRecord(read("record.json"))
		if err != nil {
			t.Fatal(err)
		}
		for desc, file := range map[*controlapi.Descriptor]string{rec.Logs: "logs.bin", provenance(rec): "provenance.json", rec.Trace: "trace.json", rec.ExternalError: "error.bin"} {
			if desc != nil {
				f.blobs[desc.Digest] = read(file)
			}
		}
		f.recs = append(f.recs, rec)
	}
	sort.Slice(f.recs, func(i, j int) bool { return f.recs[i].CreatedAt.AsTime().Before(f.recs[j].CreatedAt.AsTime()) })
	return f
}

func newCollector(t *testing.T) (*Collector, *store.Store) {
	t.Helper()
	s, err := store.Open(ctx, t.TempDir(), store.Options{Now: func() time.Time { return time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC) }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, instances, slog.New(slog.NewTextHandler(io.Discard, nil))), s
}

// jobs are the fixture's CI jobs, all of one project.
func jobs(t *testing.T, f *fake) []store.Job {
	t.Helper()
	seen := map[uint64]bool{}
	var out []store.Job
	for _, r := range f.recs {
		b, err := history.NewBuild(r, nil, nil, f.blobs[r.Trace.GetDigest()], nil, history.Config{Instances: instances})
		if err != nil {
			t.Fatal(err)
		}
		if a := b.Attribution; a.State == history.Attributed && !seen[a.Job] {
			seen[a.Job] = true
			out = append(out, store.Job{Instance: a.Instance, JobID: a.Job, PipelineID: a.Pipeline, ProjectID: 42, Name: "build"})
		}
	}
	return out
}

// dump is everything the console shows about the stored builds, by ref.
func dump(t *testing.T, s *store.Store) string {
	t.Helper()
	builds, err := s.Recent(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(builds, func(i, j int) bool { return builds[i].Build.Ref < builds[j].Build.Ref })
	var sb strings.Builder
	for _, b := range builds {
		r := b.Reason
		fmt.Fprintf(&sb, "%s %s %s job=%d reason=%s@%s %q→%q %s %v by=%s\n", b.Build.Ref, b.Build.Daemon, b.Build.Attribution.State,
			b.Build.Attribution.Job, r.Kind, r.Step.Key, r.Old, r.New, r.Arg, r.Paths, r.By)
		for _, st := range b.Build.Steps {
			fmt.Fprintf(&sb, "  %s %s from=%s %s %dms\n", st.Key, st.State, st.ReceivedFrom, st.Identity, st.Duration().Milliseconds())
		}
	}
	return sb.String()
}

// The collector stores what the model computes: the same links and the
// same reasons as the recorded builds linked and compared in memory.
func TestCollectorStoresWhatTheModelComputes(t *testing.T) {
	f := recorded(t)
	c, s := newCollector(t)
	for _, j := range jobs(t, f) {
		if err := s.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Run(ctx, f); err != nil {
		t.Fatal(err)
	}
	var mem []*history.Build
	for _, r := range f.recs {
		b, err := c.read(ctx, f, r)
		if err != nil {
			t.Fatal(err)
		}
		b.Daemon = f.name
		mem = append(mem, b)
	}
	history.Link(mem)
	stored, err := s.Recent(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	byRef := map[string]*store.Stored{}
	for _, b := range stored {
		byRef[b.Build.Ref] = b
	}
	if len(byRef) != len(mem) {
		t.Fatalf("%d builds stored, want %d", len(byRef), len(mem))
	}
	received := 0
	for i, b := range mem {
		st := byRef[b.Ref]
		for k, step := range b.Steps {
			got := st.Build.Steps[k]
			if got.State != step.State || got.ReceivedFrom != step.ReceivedFrom {
				t.Errorf("%s %s: %s from %q, want %s from %q", b.Ref, step.Key, got.State, got.ReceivedFrom, step.State, step.ReceivedFrom)
			}
			if step.ReceivedFrom != "" {
				received++
			}
		}
		var prev *history.Build
		for _, p := range mem[:i] {
			if p.Target == b.Target && p.Attribution.State == history.Attributed && b.Attribution.State == history.Attributed {
				prev = p
			}
		}
		want := history.Reason{}
		if b.Attribution.State == history.Attributed {
			want, _ = history.FirstRebuild(prev, b)
		}
		if st.Reason.Kind != want.Kind || st.Reason.Step.Key != want.Step.Key || st.Reason.Old != want.Old || st.Reason.New != want.New {
			t.Errorf("%s: reason %s at %s, want %s at %s", b.Ref, st.Reason.Kind, st.Reason.Step.Key, want.Kind, want.Step.Key)
		}
	}
	if received == 0 {
		t.Error("no received step in the fixtures: linking is not exercised")
	}
}

// What ends up stored does not depend on the order records and jobs
// arrive in, nor on a record arriving first without its trace.
func TestArrivalOrderDoesNotMatter(t *testing.T) {
	f := recorded(t)
	run := func(order []int, jobsFirst, traceLater bool) string {
		c, s := newCollector(t)
		js := jobs(t, f)
		if jobsFirst {
			for _, j := range js {
				if err := s.SaveJob(ctx, j); err != nil {
					t.Fatal(err)
				}
			}
		}
		for _, i := range order {
			r := f.recs[i]
			if traceLater {
				early := proto.Clone(r).(*controlapi.BuildHistoryRecord)
				early.Trace, early.Generation = nil, 1
				if err := c.Record(ctx, f, early); err != nil {
					t.Fatal(err)
				}
				r = proto.Clone(r).(*controlapi.BuildHistoryRecord)
				r.Generation = 2
			}
			if err := c.Record(ctx, f, r); err != nil {
				t.Fatal(err)
			}
		}
		if !jobsFirst {
			for k := range js {
				j := js[len(js)-1-k] // newest first: each job's next build is already stored
				if err := s.SaveJob(ctx, j); err != nil {
					t.Fatal(err)
				}
				if err := c.JobKnown(ctx, j.Instance, j.JobID); err != nil {
					t.Fatal(err)
				}
			}
		}
		return dump(t, s)
	}
	n := len(f.recs)
	inOrder, reverse, shuffled := make([]int, n), make([]int, n), make([]int, n)
	for i := range n {
		inOrder[i], reverse[i], shuffled[i] = i, n-1-i, (i*7+3)%n
	}
	want := run(inOrder, true, false)
	for name, got := range map[string]string{
		"reverse":              run(reverse, true, false),
		"shuffled":             run(shuffled, true, false),
		"trace attached later": run(inOrder, true, true),
		"jobs after records":   run(shuffled, false, false),
	} {
		if got != want {
			t.Errorf("%s: stored differently\n got:\n%s\nwant:\n%s", name, got, want)
		}
	}
	if !strings.Contains(want, "from=") || !strings.Contains(want, "reason=cache-elsewhere") {
		t.Fatalf("the dump shows no link or reason:\n%s", want)
	}
}

func TestProvenanceDescriptor(t *testing.T) {
	att := func(digest, pt string) *controlapi.Descriptor {
		return &controlapi.Descriptor{Digest: digest, Annotations: map[string]string{"in-toto.io/predicate-type": pt}}
	}
	rec := &controlapi.BuildHistoryRecord{Results: map[string]*controlapi.BuildResultInfo{
		"linux/arm64": {Attestations: []*controlapi.Descriptor{att("sha256:arm", "https://slsa.dev/provenance/v1")}},
		"linux/amd64": {Attestations: []*controlapi.Descriptor{att("sha256:sbom", "https://spdx.dev/Document"), att("sha256:amd", "https://slsa.dev/provenance/v1")}},
	}}
	if d := provenance(rec); d == nil || d.Digest != "sha256:amd" {
		t.Errorf("multi-platform: %v", d)
	}
	if d := provenance(&controlapi.BuildHistoryRecord{}); d != nil {
		t.Errorf("no result: %v", d)
	}
}

// A pipeline's "cache elsewhere" reason is settled from what another of its
// builds found in the cache of another daemon.
func TestSettleStoredPipeline(t *testing.T) {
	c, s := newCollector(t)
	t0 := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	att := history.Attribution{State: history.Attributed, Instance: "051b90", Pipeline: 7}
	step := func(state history.StepState) history.Step {
		return history.Step{Key: "[app 1/2]", Name: "COPY common/ ./common", Identity: "00000000000000aa", State: state, Started: t0, Completed: t0.Add(time.Second)}
	}
	worker := &history.Build{Ref: "worker", Daemon: "node-b", Attribution: att, Target: "worker", Started: t0, Completed: t0.Add(time.Minute), Steps: []history.Step{step(history.Built)}}
	worker.Attribution.Job = 71
	api := &history.Build{Ref: "api", Daemon: "node-c", Attribution: att, Target: "api", Started: t0, Completed: t0.Add(time.Minute), Steps: []history.Step{step(history.Cached)}}
	api.Attribution.Job = 72
	elsewhere := history.Reason{Kind: history.CacheElsewhere, Step: worker.Steps[0], Old: "node-a", New: "node-b", Paths: []string{"common"}}
	if _, err := s.SaveBuild(ctx, store.Build{Build: worker, Generation: 1, Reason: elsewhere}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveBuild(ctx, store.Build{Build: api, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if err := c.settle(ctx, "051b90", 7); err != nil {
		t.Fatal(err)
	}
	got, err := s.Pipeline(ctx, "051b90", 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range got {
		if b.Build.Ref == "worker" && (b.Reason.Kind != history.OtherNode || b.Reason.By != "api" || b.Reason.Step.Key != "[app 1/2]") {
			t.Errorf("worker: %+v", b.Reason)
		}
	}
}

// A record already stored in the same or a newer generation is not read
// again.
func TestStaleRecordIsNotReadAgain(t *testing.T) {
	f := recorded(t)
	c, _ := newCollector(t)
	r := f.recs[0]
	if err := c.Record(ctx, f, r); err != nil {
		t.Fatal(err)
	}
	gone := &fake{name: f.name, blobs: map[string][]byte{}}
	if err := c.Record(ctx, gone, r); err != nil {
		t.Errorf("the same generation was read again: %v", err)
	}
	older := proto.Clone(r).(*controlapi.BuildHistoryRecord)
	older.Generation = r.Generation - 1
	if err := c.Record(ctx, gone, older); err != nil {
		t.Errorf("an older generation was read: %v", err)
	}
}

// Builds older than the window, by the newest build's start, leave memory.
func TestWindowKeepsRecentBuilds(t *testing.T) {
	f := recorded(t)
	c, _ := newCollector(t)
	first, last := f.recs[0], proto.Clone(f.recs[len(f.recs)-1]).(*controlapi.BuildHistoryRecord)
	if err := c.Record(ctx, f, first); err != nil {
		t.Fatal(err)
	}
	last.CreatedAt = timestamppb.New(first.CreatedAt.AsTime().Add(Window + time.Minute))
	last.CompletedAt = timestamppb.New(last.CreatedAt.AsTime().Add(time.Minute))
	if err := c.Record(ctx, f, last); err != nil {
		t.Fatal(err)
	}
	w := c.daemons[f.name]
	if _, ok := w.builds[first.Ref]; ok || len(w.builds) != 1 {
		t.Errorf("window holds %d builds, the first among them: %v", len(w.builds), ok)
	}
}

// A reconnecting stream replays old records after newer ones: an old one
// is stored all the same, and does not stay in memory.
func TestOldRecordAfterANewerOneIsStored(t *testing.T) {
	f := recorded(t)
	c, s := newCollector(t)
	first, last := f.recs[0], proto.Clone(f.recs[len(f.recs)-1]).(*controlapi.BuildHistoryRecord)
	last.CreatedAt = timestamppb.New(first.CreatedAt.AsTime().Add(Window + time.Minute))
	last.CompletedAt = timestamppb.New(last.CreatedAt.AsTime().Add(time.Minute))
	if err := c.Record(ctx, f, last); err != nil {
		t.Fatal(err)
	}
	if err := c.Record(ctx, f, first); err != nil {
		t.Fatal(err)
	}
	builds, err := s.Recent(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range builds {
		if b.Build.Ref == first.Ref {
			found = true
		}
	}
	if !found {
		t.Errorf("the old record was dropped; stored: %d builds", len(builds))
	}
	if _, ok := c.daemons[f.name].builds[first.Ref]; ok {
		t.Error("the old record stays in memory")
	}
}

// A build new to the store is one info line; the same record read again
// after a restart (a new collector over the same store) is a debug line.
func TestStoredBuildsAreLoggedOnce(t *testing.T) {
	f := recorded(t)
	c, s := newCollector(t)
	var buf bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := f.recs[0]
	if err := c.Record(ctx, f, r); err != nil {
		t.Fatal(err)
	}
	again := New(s, instances, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	if err := again.Record(ctx, f, r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Count(out, `level=INFO msg="build stored"`) != 1 || strings.Count(out, `level=DEBUG msg="build stored again"`) != 1 ||
		!strings.Contains(out, "build="+r.Ref) || !strings.Contains(out, "daemon="+f.name) {
		t.Errorf("log:\n%s", out)
	}
}

// BuildKit sends a record first without the client's trace, then again
// with it: the build is logged when stored and when it gets its job.
func TestBuildAttributedLater(t *testing.T) {
	f := recorded(t)
	c, _ := newCollector(t)
	var buf bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&buf, nil))
	r := f.recs[0]
	early := proto.Clone(r).(*controlapi.BuildHistoryRecord)
	early.Trace, early.Generation = nil, r.Generation-1
	for _, rec := range []*controlapi.BuildHistoryRecord{early, r} {
		if err := c.Record(ctx, f, rec); err != nil {
			t.Fatal(err)
		}
	}
	out := buf.String()
	if !strings.Contains(out, `msg="build stored"`) || !strings.Contains(out, "attribution=unattributed") ||
		!strings.Contains(out, `msg="build attributed"`) || !strings.Contains(out, " job=") {
		t.Errorf("log:\n%s", out)
	}
}

// A daemon pod that replaced another on the same hostPath sends the old
// pod's records again (its history is on that disk): they stay the builds
// they were, under the pod that ran them, with the same reasons; 0.3.0 and
// 0.3.1 stored each again under the new pod. A record new to the store is
// the new pod's.
func TestReplacedDaemonReplaysHistory(t *testing.T) {
	f := recorded(t)
	c, s := newCollector(t)
	for _, j := range jobs(t, f) {
		if err := s.SaveJob(ctx, j); err != nil {
			t.Fatal(err)
		}
	}
	n := len(f.recs)
	old := &fake{name: f.name, recs: f.recs[:n-1], blobs: f.blobs}
	if err := c.Run(ctx, old); err != nil {
		t.Fatal(err)
	}
	before := dump(t, s)
	replaced := &fake{name: "daemon-a-replaced", recs: f.recs, blobs: f.blobs}
	if err := c.Run(ctx, replaced); err != nil {
		t.Fatal(err)
	}
	builds, err := s.Recent(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(builds) != n {
		t.Fatalf("%d builds stored after the replay, want %d", len(builds), n)
	}
	last := f.recs[n-1].Ref
	for _, b := range builds {
		want := f.name
		if b.Build.Ref == last {
			want = replaced.name
		}
		if b.Build.Daemon != want {
			t.Errorf("%s stored under %s, want %s", b.Build.Ref, b.Build.Daemon, want)
		}
	}
	// The old pod's builds are what they were before the replay.
	after := dump(t, s)
	for _, line := range strings.Split(strings.TrimSpace(before), "\n") {
		if !strings.Contains(after, line+"\n") {
			t.Errorf("after the replay the store no longer has\n  %s\nall:\n%s", line, after)
			break
		}
	}
}
