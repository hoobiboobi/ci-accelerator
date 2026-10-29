package diskprobe

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeKernel writes /proc and a pod cgroup as a 4-core node with 8 GiB
// reports them: busy jiffies, memory and the pod's counters are
// parameters, so two readings make a delta.
func fakeKernel(t *testing.T, dir string, busy, idle uint64, availKB uint64, podUsec, podMem uint64, psi bool) {
	t.Helper()
	proc := filepath.Join(dir, "proc")
	os.MkdirAll(filepath.Join(proc, "pressure"), 0o755)
	// user nice system idle iowait irq softirq steal guest guest_nice
	stat := "cpu  " + u(busy) + " 0 0 " + u(idle) + " 0 0 0 0 0 0\n"
	for i := 0; i < 4; i++ {
		stat += "cpu" + u(uint64(i)) + " 1 0 0 1 0 0 0 0 0 0\n"
	}
	stat += "intr 5\nctxt 7\n"
	write(t, filepath.Join(proc, "stat"), stat)
	write(t, filepath.Join(proc, "meminfo"), "MemTotal:        8388608 kB\nMemFree:          100000 kB\nMemAvailable:    "+u(availKB)+" kB\nBuffers:  1 kB\n")
	if psi {
		write(t, filepath.Join(proc, "pressure", "cpu"), "some avg10=12.50 avg60=38.00 avg300=9.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
		write(t, filepath.Join(proc, "pressure", "memory"), "some avg10=0.00 avg60=0.10 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
		write(t, filepath.Join(proc, "pressure", "io"), "some avg10=2.00 avg60=1.00 avg300=0.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n")
	} else {
		os.RemoveAll(filepath.Join(proc, "pressure"))
	}
	// cgroup v2: one tree, cgroup.controllers at its root. The pod holds
	// 500 MiB of inactive file cache on top of podMem (its working set).
	root := filepath.Join(dir, "cgroup")
	cg := filepath.Join(root, "kubelet.slice", "kubelet-kubepods.slice", "kubelet-kubepods-burstable.slice",
		"kubelet-kubepods-burstable-pode6566_4890_4825.slice")
	os.MkdirAll(cg, 0o755)
	write(t, filepath.Join(root, "cgroup.controllers"), "cpuset cpu io memory pids\n")
	write(t, filepath.Join(cg, "cpu.stat"), "usage_usec "+u(podUsec)+"\nuser_usec 1\nsystem_usec 2\n")
	write(t, filepath.Join(cg, "memory.current"), u(podMem+500<<20)+"\n")
	write(t, filepath.Join(cg, "memory.stat"), "anon 1\nfile 2\nactive_file 3\ninactive_file "+u(500<<20)+"\n")
	write(t, filepath.Join(cg, "memory.max"), "6091177984\n")
}

func u(n uint64) string { return string(append([]byte(nil), []byte(itoa(n))...)) }

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSamplerReadsNodeAndPod(t *testing.T) {
	dir := t.TempDir()
	fakeKernel(t, dir, 1000, 3000, 6*1024*1024, 50_000_000, 3<<30, true)
	cg := FindPodCgroup(filepath.Join(dir, "cgroup"), "e6566-4890-4825")
	if cg == "" {
		t.Fatal("the pod's cgroup (systemd naming) not found")
	}
	s := NewSampler(Reader{Proc: filepath.Join(dir, "proc"), Cgroup: cg})
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	if err := s.Tick(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil {
		t.Error("a load before the second reading")
	}
	// 10 s later: 1200 more jiffies, 3100 of them busy (= 2.58 of 4
	// cores); the pod used 29 s of CPU (2.9 cores) and holds 4.7 GiB.
	fakeKernel(t, dir, 1000+3100, 3000+900, 2*1024*1024, 50_000_000+29_000_000, 4700<<20, true)
	now = now.Add(10 * time.Second)
	if err := s.Tick(); err != nil {
		t.Fatal(err)
	}
	l, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if l.Cores != 4 || l.MemTotal != 8<<30 {
		t.Errorf("node: %d cores, %d bytes", l.Cores, l.MemTotal)
	}
	if l.Node.CPU < 3.09 || l.Node.CPU > 3.11 { // 3100/4000*4
		t.Errorf("node cpu %.3f, want 3.1", l.Node.CPU)
	}
	if l.Node.Mem != 6<<30 { // 8 GiB - 2 GiB available
		t.Errorf("node mem %d", l.Node.Mem)
	}
	// The pod's memory is its working set: memory.current less the
	// inactive file cache, on the node's basis (total minus available).
	if !l.PodKnown || l.Pod.CPU < 2.89 || l.Pod.CPU > 2.91 || l.Pod.Mem != 4700<<20 || l.PodMemLimit != 6091177984 {
		t.Errorf("pod: %+v known %v limit %d", l.Pod, l.PodKnown, l.PodMemLimit)
	}
	if l.Pressure == nil || l.Pressure.CPU60 != 38 || l.Pressure.CPU10 != 12.5 || l.Pressure.IO10 != 2 {
		t.Errorf("pressure: %+v", l.Pressure)
	}
	if len(l.Series) != 1 || l.Series[0].At != now.Unix() || l.Series[0].PodMem != 4700<<20 {
		t.Errorf("series: %+v", l.Series)
	}
	// The hour's window: readings older than Keep fall off.
	for i := 0; i < 400; i++ {
		now = now.Add(Interval)
		fakeKernel(t, dir, 1000+3100+uint64(i), 3000+900+uint64(i)*3, 2*1024*1024, 79_000_000+uint64(i)*1000, 4700<<20, true)
		s.Tick()
	}
	l, _ = s.Load()
	if n := len(l.Series); n < 358 || n > 362 {
		t.Errorf("%d samples kept, want about an hour", n)
	}
	if l.Series[0].At < now.Add(-Keep).Unix() {
		t.Error("a sample older than an hour kept")
	}
	// The wall clock steps back ten minutes (a time sync): the series
	// starts over instead of mixing two timelines.
	// The first reading after the step only primes the deltas again.
	now = now.Add(-10 * time.Minute)
	for i := 0; i < 3; i++ {
		s.Tick()
		now = now.Add(Interval)
	}
	l, _ = s.Load()
	if len(l.Series) != 2 || l.Series[0].At >= l.Series[1].At {
		t.Errorf("after the clock went back: %d samples, %+v", len(l.Series), l.Series)
	}
}

// On cgroup v1 every controller has its own tree; the first directory
// named after the pod would be one controller's (blkio sorts first), so
// the pod is not looked for at all; a v2 directory without the files is
// not measured either: the page says so instead of a measured 0.
func TestPodCgroupNeedsV2(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "cgroup")
	for _, c := range []string{"blkio", "cpu,cpuacct", "memory"} {
		os.MkdirAll(filepath.Join(root, c, "kubepods", "burstable", "podabc-123"), 0o755)
	}
	if got := FindPodCgroup(root, "abc-123"); got != "" {
		t.Errorf("cgroup v1: found %q", got)
	}
	fakeKernel(t, dir, 10, 10, 1024, 0, 0, false)
	empty := filepath.Join(dir, "v2empty")
	os.MkdirAll(empty, 0o755)
	s := NewSampler(Reader{Proc: filepath.Join(dir, "proc"), Cgroup: empty})
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Tick()
	now = now.Add(Interval)
	s.Tick()
	l, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if l.PodKnown || l.Pod.Mem != 0 {
		t.Errorf("a cgroup without its files measured: %+v", l)
	}
}

func TestSamplerWithoutPSIOrCgroup(t *testing.T) {
	dir := t.TempDir()
	fakeKernel(t, dir, 10, 10, 1024, 0, 0, false)
	// cgroupfs naming: the UID as is.
	cg := filepath.Join(dir, "cgroup", "kubepods", "burstable", "podabc-123")
	os.MkdirAll(cg, 0o755)
	if got := FindPodCgroup(filepath.Join(dir, "cgroup"), "abc-123"); got != cg {
		t.Errorf("cgroupfs naming: %q", got)
	}
	if got := FindPodCgroup(filepath.Join(dir, "cgroup"), "nope"); got != "" {
		t.Errorf("an unknown pod found at %q", got)
	}
	s := NewSampler(Reader{Proc: filepath.Join(dir, "proc")})
	now := time.Now()
	s.now = func() time.Time { return now }
	s.Tick()
	fakeKernel(t, dir, 20, 20, 1024, 0, 0, false)
	now = now.Add(Interval)
	if err := s.Tick(); err != nil {
		t.Fatal(err)
	}
	l, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if l.Pressure != nil {
		t.Errorf("pressure reported by a kernel without PSI: %+v", l.Pressure)
	}
	if l.Pod.CPU != 0 || l.Pod.Mem != 0 || l.PodMemLimit != 0 {
		t.Errorf("pod figures without a cgroup: %+v", l.Pod)
	}
	if l.Node.CPU < 1.99 || l.Node.CPU > 2.01 {
		t.Errorf("node cpu %.2f, want 2 (half of 4 cores busy)", l.Node.CPU)
	}
}

func TestSamplerReportsUnreadableFiles(t *testing.T) {
	s := NewSampler(Reader{Proc: filepath.Join(t.TempDir(), "none")})
	if err := s.Tick(); err == nil {
		t.Error("a missing /proc read without error")
	}
	if _, err := s.Load(); err == nil {
		t.Error("a load after a failed reading")
	}
}

func TestLoadServedAndFetched(t *testing.T) {
	want := Load{At: time.Now().Truncate(time.Second), Cores: 4, MemTotal: 8 << 30, Node: Point{1.5, 3 << 30}, Pod: Point{1.2, 2 << 30},
		Series: []Sample{{At: 1, NodeCPU: 1.5, NodeMem: 3 << 30, PodCPU: 1.2, PodMem: 2 << 30}}}
	srv := httptest.NewServer(Handler("/", func(string) (Disk, error) { return Disk{Total: 1, Free: 1, Available: 1}, nil },
		func() (Load, error) { return want, nil }))
	defer srv.Close()
	got, err := FetchLoad(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cores != 4 || got.Node != want.Node || got.Pod != want.Pod || len(got.Series) != 1 || got.Series[0] != want.Series[0] {
		t.Errorf("got %+v", got)
	}
	// Nonsense is refused; a probe without a sampler has no /load.
	for _, bad := range []Load{{}, {Cores: 4, MemTotal: 1, Node: Point{9, 0}}, {Cores: 4, MemTotal: 10, Node: Point{1, 20}}} {
		s := httptest.NewServer(Handler("/", nil, func() (Load, error) { return bad, nil }))
		if _, err := FetchLoad(context.Background(), s.Client(), s.URL); err == nil {
			t.Errorf("%+v accepted", bad)
		}
		s.Close()
	}
	none := httptest.NewServer(Handler("/", nil, nil))
	defer none.Close()
	if _, err := FetchLoad(context.Background(), none.Client(), none.URL); err == nil {
		t.Error("a probe without /load answered")
	}
}
