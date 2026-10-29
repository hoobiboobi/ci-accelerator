package diskprobe

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Load is what the daemon's node and its pod use: the latest reading and
// the readings of the last hour. The node's figures come from /proc (the
// kernel's, not namespaced); the pod's from its cgroup, which holds the
// daemon and this probe together (the probe's share is a few files read
// every Interval).
type Load struct {
	At    time.Time `json:"at"`
	Cores int       `json:"cores"`
	// MemTotal is the node's memory.
	MemTotal uint64 `json:"memTotalBytes"`
	// Node and Pod are the latest readings: cores busy over the last
	// interval and memory in use now, on the same basis for both: the
	// node's total minus available, the pod's working set (its cgroup's
	// memory.current minus inactive file cache, as the kubelet counts it).
	Node Point `json:"node"`
	Pod  Point `json:"pod"`
	// PodKnown: the pod's cgroup was found and read (cgroup v2); without
	// it the pod's figures are not measured and stay 0.
	PodKnown bool `json:"podKnown"`
	// PodMemLimit is the pod's memory limit; 0 without one.
	PodMemLimit uint64 `json:"podMemLimitBytes,omitempty"`
	// Pressure is the node's pressure stall information; nil when the
	// kernel does not report it.
	Pressure *Pressure `json:"pressure,omitempty"`
	// Series are the readings of the last hour, oldest first, Interval
	// apart (fewer right after a start).
	Series []Sample `json:"series"`
}

// Point is one reading of CPU and memory.
type Point struct {
	CPU float64 `json:"cpuCores"`
	Mem uint64  `json:"memBytes"`
}

// Sample is one reading of the node and the pod.
type Sample struct {
	At      int64   `json:"at"` // Unix seconds
	NodeCPU float64 `json:"nodeCpu"`
	NodeMem uint64  `json:"nodeMem"`
	PodCPU  float64 `json:"podCpu"`
	PodMem  uint64  `json:"podMem"`
}

// Pressure is the share of the last 10 s and 60 s in which some task
// waited for the resource (PSI "some", percent).
type Pressure struct {
	CPU10 float64 `json:"cpu10"`
	CPU60 float64 `json:"cpu60"`
	Mem10 float64 `json:"mem10"`
	Mem60 float64 `json:"mem60"`
	IO10  float64 `json:"io10"`
	IO60  float64 `json:"io60"`
}

// Interval is how often the sampler reads; Keep is how long it keeps.
const (
	Interval = 10 * time.Second
	Keep     = time.Hour
)

// Reader reads the kernel's files: Proc is the procfs ("/proc"), Cgroup
// the pod's cgroup v2 directory ("" when not found: the pod's figures are
// not measured).
type Reader struct {
	Proc   string
	Cgroup string
}

// counters is one raw reading, for the deltas between two.
type counters struct {
	at                  time.Time
	cpuBusy, cpuTotal   uint64 // /proc/stat jiffies
	cores               int
	memTotal, memAvail  uint64
	podKnown            bool
	podCPU              uint64 // cpu.stat usage_usec
	podMem, podMemLimit uint64 // working set, limit
	pressure            *Pressure
}

func (r Reader) read(now time.Time) (counters, error) {
	var c counters
	c.at = now
	f, err := os.Open(filepath.Join(r.Proc, "stat"))
	if err != nil {
		return c, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			c.cores++
			continue
		}
		// user nice system idle iowait irq softirq steal: idle and
		// iowait are the time nothing ran.
		for i, s := range fields[1:] {
			n, err := strconv.ParseUint(s, 10, 64)
			if err != nil {
				return c, fmt.Errorf("/proc/stat: %w", err)
			}
			c.cpuTotal += n
			if i != 3 && i != 4 {
				c.cpuBusy += n
			}
		}
	}
	if c.cpuTotal == 0 {
		return c, errors.New("/proc/stat: no cpu line")
	}
	mem, err := os.Open(filepath.Join(r.Proc, "meminfo"))
	if err != nil {
		return c, err
	}
	defer mem.Close()
	sc = bufio.NewScanner(mem)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			c.memTotal = kb << 10
		case "MemAvailable:":
			c.memAvail = kb << 10
		}
	}
	if c.memTotal == 0 {
		return c, errors.New("/proc/meminfo: no MemTotal")
	}
	c.pressure = r.pressure()
	if r.Cgroup != "" {
		usage, okCPU := cgroupStat(filepath.Join(r.Cgroup, "cpu.stat"), "usage_usec")
		current, okMem := readUint(filepath.Join(r.Cgroup, "memory.current"))
		if okCPU && okMem {
			c.podKnown, c.podCPU = true, usage
			// The working set: page cache the kernel can drop is not in
			// the node's figure (MemAvailable counts it as available),
			// so it is not in the pod's either.
			inactive, _ := cgroupStat(filepath.Join(r.Cgroup, "memory.stat"), "inactive_file")
			c.podMem = current - min(inactive, current)
			c.podMemLimit, _ = readUint(filepath.Join(r.Cgroup, "memory.max")) // "max" reads as 0
		}
	}
	return c, nil
}

// pressure reads /proc/pressure/{cpu,memory,io}; nil without the files
// (a kernel built without PSI, or one that hides it).
func (r Reader) pressure() *Pressure {
	var p Pressure
	found := false
	for _, kind := range []struct {
		file     string
		p10, p60 *float64
	}{{"cpu", &p.CPU10, &p.CPU60}, {"memory", &p.Mem10, &p.Mem60}, {"io", &p.IO10, &p.IO60}} {
		data, err := os.ReadFile(filepath.Join(r.Proc, "pressure", kind.file))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			if !strings.HasPrefix(line, "some ") {
				continue
			}
			found = true
			for _, kv := range strings.Fields(line)[1:] {
				k, v, _ := strings.Cut(kv, "=")
				f, err := strconv.ParseFloat(v, 64)
				if err != nil {
					continue
				}
				switch k {
				case "avg10":
					*kind.p10 = f
				case "avg60":
					*kind.p60 = f
				}
			}
		}
	}
	if !found {
		return nil
	}
	return &p
}

// cgroupStat reads one key of a cgroup's flat keyed file (cpu.stat,
// memory.stat); false when the file or the key is missing.
func cgroupStat(file, key string) (uint64, bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok && k == key {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// readUint reads a cgroup file holding one number; false when missing
// or not a number ("max").
func readUint(file string) (uint64, bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	return n, err == nil
}

// FindPodCgroup finds the pod's cgroup directory under root (the host's
// cgroup filesystem): the kubelet names it after the pod's UID, with the
// systemd driver ("kubepods-burstable-pod<uid with _>.slice") or the
// cgroupfs one ("kubepods/burstable/pod<uid>"). Empty when not found, and
// on cgroup v1 (no cgroup.controllers at the root): there each controller
// has its own tree, and the first match would be one controller's.
func FindPodCgroup(root, podUID string) string {
	if podUID == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		return ""
	}
	names := []string{"pod" + podUID, "pod" + strings.ReplaceAll(podUID, "-", "_")}
	found := ""
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found != "" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			return nil
		}
		if strings.Count(strings.TrimPrefix(path, root), string(filepath.Separator)) > 6 {
			return filepath.SkipDir
		}
		for _, n := range names {
			if strings.Contains(d.Name(), n) {
				found = path
				return filepath.SkipAll
			}
		}
		return nil
	})
	return found
}

// Sampler reads the node and the pod every Interval and keeps an hour.
type Sampler struct {
	reader Reader
	now    func() time.Time

	mu      sync.Mutex
	last    counters
	hasLast bool
	series  []Sample
	latest  Load
	err     error
}

// NewSampler makes a sampler over r.
func NewSampler(r Reader) *Sampler {
	return &Sampler{reader: r, now: time.Now}
}

// Run samples until ctx ends. The first reading only primes the deltas.
func (s *Sampler) Run(ctx context.Context, warn func(error)) {
	s.Tick()
	t := time.NewTicker(Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Tick(); err != nil && warn != nil {
				warn(err)
			}
		}
	}
}

// Tick takes one reading now.
func (s *Sampler) Tick() error {
	now := s.now()
	c, err := s.reader.read(now)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.err = err
		return err
	}
	s.err = nil
	if s.hasLast && c.at.After(s.last.at) {
		var p Sample
		p.At = c.at.Unix()
		if dt := c.cpuTotal - s.last.cpuTotal; dt > 0 && c.cpuTotal >= s.last.cpuTotal {
			p.NodeCPU = float64(c.cpuBusy-s.last.cpuBusy) / float64(dt) * float64(c.cores)
		}
		if c.podKnown && s.last.podKnown && c.podCPU >= s.last.podCPU {
			p.PodCPU = float64(c.podCPU-s.last.podCPU) / 1e6 / c.at.Sub(s.last.at).Seconds()
		}
		p.NodeMem = c.memTotal - min(c.memAvail, c.memTotal)
		p.PodMem = c.podMem
		// The wall clock went back (a time sync): the series starts over,
		// so it stays in order and its window means what it says.
		if n := len(s.series); n > 0 && p.At <= s.series[n-1].At {
			s.series = s.series[:0]
		}
		s.series = append(s.series, p)
		cut := now.Add(-Keep).Unix()
		for len(s.series) > 0 && s.series[0].At < cut {
			s.series = s.series[1:]
		}
		if max := int(Keep/Interval) + 1; len(s.series) > max {
			s.series = s.series[len(s.series)-max:]
		}
		s.latest = Load{At: c.at, Cores: c.cores, MemTotal: c.memTotal, Node: Point{p.NodeCPU, p.NodeMem},
			Pod: Point{p.PodCPU, p.PodMem}, PodKnown: c.podKnown, PodMemLimit: c.podMemLimit, Pressure: c.pressure}
	}
	s.last, s.hasLast = c, true
	return nil
}

// Load is the latest reading with the hour's series; an error before the
// second reading (nothing to compare yet) or when the files cannot be
// read.
func (s *Sampler) Load() (Load, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return Load{}, s.err
	}
	if s.latest.At.IsZero() {
		return Load{}, errors.New("no reading yet")
	}
	l := s.latest
	l.Series = append([]Sample(nil), s.series...)
	return l, nil
}

// loadHandler serves GET /load from load.
func loadHandler(mux *http.ServeMux, load func() (Load, error)) {
	mux.HandleFunc("GET /load", func(w http.ResponseWriter, r *http.Request) {
		l, err := load()
		if err != nil {
			http.Error(w, "load: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(l)
	})
}

// FetchLoad asks a probe (http://host:port) for its Load.
func FetchLoad(ctx context.Context, client *http.Client, base string) (Load, error) {
	var l Load
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/load", nil)
	if err != nil {
		return l, err
	}
	res, err := client.Do(req)
	if err != nil {
		return l, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 200))
		return l, fmt.Errorf("load probe: %s: %s", res.Status, strings.TrimSpace(string(msg)))
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 512<<10)).Decode(&l); err != nil {
		return l, fmt.Errorf("load probe: %w", err)
	}
	if l.Cores <= 0 || l.MemTotal == 0 || l.Node.CPU < 0 || l.Node.CPU > float64(l.Cores)+0.5 ||
		l.Node.Mem > l.MemTotal || l.Pod.CPU < 0 || len(l.Series) > int(Keep/Interval)+2 {
		return Load{}, fmt.Errorf("load probe: implausible numbers (cores %d, mem %d, node %+v)", l.Cores, l.MemTotal, l.Node)
	}
	return l, nil
}
