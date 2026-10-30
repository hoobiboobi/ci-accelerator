package store

import (
	"database/sql"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

// A dictionary encoder is megabytes. A long-running controller writes
// every day's output with one per project, and the day files stay open for
// the whole retention: encoders must not live longer than the days still
// being written, nor be rebuilt for reading.
func TestOutputEncoderMemoryBounded(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	base := fixtures(t, s.SecretKey())[0]
	const projects, days, perDay = 10, 7, 2
	output := func(p, d, n int) string {
		var b strings.Builder
		for i := 0; b.Len() < 20<<10; i++ {
			fmt.Fprintf(&b, "#%d %d.%03d project-%d fetching package-%d@%d.%d.%d from the registry (%d kB)\n",
				i, d, (i*7919+n)%1000, p, (i*31+p)%97, d, i%13, n, (i*17+d)%500)
		}
		return b.String()
	}
	type saved struct {
		id  int64
		out string
	}
	var first []saved
	runtime.GC()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	job := uint64(1000)
	save := func(p int, out string, end time.Time) int64 {
		t.Helper()
		job++
		b := *base
		b.Ref = fmt.Sprintf("ref-%d", job)
		b.Attribution.Job = job
		b.Started = end.Add(-time.Minute)
		b.Completed = end
		b.Steps = append([]history.Step(nil), base.Steps...)
		b.Steps[0].Output = out
		if err := s.SaveJob(ctx, Job{Instance: b.Attribution.Instance, JobID: job, PipelineID: b.Attribution.Pipeline,
			ProjectID: uint64(100 + p), ProjectPath: fmt.Sprintf("example/p%d", p), Name: "build", Stage: "build"}); err != nil {
			t.Fatal(err)
		}
		id, err := s.SaveBuild(ctx, Build{Build: &b, Generation: 1})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	start := c.t
	for d := 0; d < days; d++ {
		for p := 0; p < projects; p++ {
			for n := 0; n < perDay; n++ {
				out := output(p, d, n)
				id := save(p, out, c.t)
				if d == 1 {
					first = append(first, saved{id, out})
				}
			}
		}
		c.t = c.t.Add(24 * time.Hour)
	}
	// Builds of a day with dictionaries, read back after its encoders went.
	for _, w := range first {
		if got, err := s.Output(ctx, w.id, 0); err != nil || got != w.out {
			t.Fatalf("build %d: %v, %d bytes, want %d", w.id, err, len(got), len(w.out))
		}
	}
	runtime.GC()
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	// Two days of dictionary encoders, about 3 MB each.
	if grown := int64(after.HeapAlloc) - int64(before.HeapAlloc); grown > 128<<20 {
		t.Errorf("heap grew by %d MB", grown>>20)
	}
	last := day(c.t.Add(-24 * time.Hour))
	prev := day(c.t.Add(-48 * time.Hour))
	s.mu.Lock()
	if len(s.outputs) != days {
		s.mu.Unlock()
		t.Fatalf("%d open output files, want %d", len(s.outputs), days)
	}
	dicts := 0
	for d, f := range s.outputs {
		f.mu.Lock()
		n := len(f.enc)
		f.mu.Unlock()
		dicts += n
		if n > 0 && d != last && d != prev {
			t.Errorf("file of %d keeps %d encoders", d, n)
		}
	}
	s.mu.Unlock()
	if dicts == 0 {
		t.Error("no dictionaries were used")
	}
	// A late build of an earlier day gets its encoder back from the file.
	late := output(0, 1, perDay)
	id := save(0, late, start.Add(24*time.Hour))
	if got, err := s.Output(ctx, id, 0); err != nil || got != late {
		t.Fatalf("late build: %v, %d bytes, want %d", err, len(got), len(late))
	}
	f, err := s.output(ctx, day(start.Add(24*time.Hour)), false)
	if err != nil {
		t.Fatal(err)
	}
	var dict sql.NullInt64
	if err := f.db.QueryRow("SELECT dict_id FROM outputs WHERE key = ?", outputKey(id, 0)).Scan(&dict); err != nil || !dict.Valid {
		t.Errorf("late build: dictionary %v, %v", dict, err)
	}
}

// Dropping the encoders of the days no longer written and deleting the
// files past retention run at the same time (a build stored while the
// retention runs): neither touches a file's encoders without its lock.
// Run with -race.
func TestOutputPruneWhileEncodersAreReleased(t *testing.T) {
	c := &clock{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
	s := open(t, t.TempDir(), c)
	today, old := day(c.t), day(c.t.AddDate(0, 0, -(s.opts.KeepOutput+3)))
	for range 200 {
		if _, err := s.output(ctx, old, true); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.releaseEncoders(today)
		}()
		if err := s.pruneOutputs(c.t); err != nil {
			t.Fatal(err)
		}
		<-done
	}
}
