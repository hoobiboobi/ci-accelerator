package diskprobe

import (
	"context"
	"math"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cpuSampler reads a 4-core node whose aggregate /proc/stat cpu line is
// set by the returned function; every call is one reading, 10 s after the
// last.
func cpuSampler(t *testing.T) (*Sampler, func(cpuLine string)) {
	t.Helper()
	dir := t.TempDir()
	proc := filepath.Join(dir, "proc")
	os.MkdirAll(proc, 0o755)
	write(t, filepath.Join(proc, "meminfo"), "MemTotal: 8388608 kB\nMemAvailable: 4194304 kB\n")
	now := time.Unix(1_800_000_000, 0)
	s := NewSampler(Reader{Proc: proc})
	s.now = func() time.Time { return now }
	return s, func(cpuLine string) {
		t.Helper()
		stat := cpuLine + "\n"
		for i := 0; i < 4; i++ {
			stat += "cpu" + u(uint64(i)) + " 1 0 0 1 0 0 0 0 0 0\n"
		}
		write(t, filepath.Join(proc, "stat"), stat)
		now = now.Add(10 * time.Second)
		if err := s.Tick(); err != nil {
			t.Fatal(err)
		}
	}
}

// fetched is the load as the controller takes it from the probe.
func fetched(t *testing.T, s *Sampler) (Load, error) {
	t.Helper()
	srv := httptest.NewServer(Handler("/", func(string) (Disk, error) { return Disk{Total: 1}, nil }, s.Load))
	defer srv.Close()
	return FetchLoad(context.Background(), srv.Client(), srv.URL)
}

// A /proc/stat counter that goes back (steal after a live migration,
// iowait) makes no sample: the node's figure stays the last good one, and
// the controller does not refuse the probe's answer.
func TestNodeCPUWhenACounterGoesBack(t *testing.T) {
	// user nice system idle iowait irq softirq steal guest guest_nice
	// Readings 1-2 are a good interval (1000 busy of 2000 on 4 cores: 2),
	// 3 has a counter back, 4 is a good interval from 3 (2 cores again).
	for name, lines := range map[string][4]string{
		"steal": {"cpu 1000 0 0 3000 0 0 0 500 0 0", "cpu 2000 0 0 4000 0 0 0 500 0 0", "cpu 2050 0 0 4300 0 0 0 400 0 0",
			"cpu 3050 0 0 5300 0 0 0 400 0 0"},
		"iowait": {"cpu 1000 0 0 3000 500 0 0 0 0 0", "cpu 2000 0 0 4000 500 0 0 0 0 0", "cpu 2040 0 0 4000 470 0 0 0 0 0",
			"cpu 3040 0 0 5000 470 0 0 0 0 0"},
	} {
		s, read := cpuSampler(t)
		for _, l := range lines[:3] {
			read(l)
		}
		l, err := fetched(t, s)
		if err != nil {
			t.Errorf("%s back: the controller refuses the answer: %v", name, err)
			continue
		}
		if l.Node.CPU != 2 || len(l.Series) != 1 || l.Series[0].NodeCPU != 2 {
			t.Errorf("%s back: node cpu %v, series %+v; want the last good 2 cores and no new sample", name, l.Node.CPU, l.Series)
		}
		// The reading after it is measured from it.
		read(lines[3])
		if l, err := fetched(t, s); err != nil || len(l.Series) != 2 || l.Node.CPU != 2 {
			t.Errorf("%s back, next reading: %+v (%v)", name, l.Node, err)
		}
	}
}

// guest and guest_nice are already in user and nice: they are not counted
// twice.
func TestNodeCPUCountsGuestOnce(t *testing.T) {
	s, read := cpuSampler(t)
	read("cpu 1000 0 0 2000 0 0 0 0 1000 0")
	read("cpu 2000 0 0 3000 0 0 0 0 2000 0") // 1000 busy (all of it guest) of 2000
	l, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(l.Node.CPU-2) > 1e-9 {
		t.Errorf("node cpu %v, want 2 cores", l.Node.CPU)
	}
}
