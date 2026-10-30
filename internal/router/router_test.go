package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"

	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// backend is a daemon stand-in: it reads a request to its end, answers
// "<name>:<REQUEST>" and closes. hold keeps it from answering until closed.
type backend struct {
	name string
	ln   net.Listener
	hold chan struct{}
	mu   sync.Mutex
	got  []string
}

func newBackend(t *testing.T, name string) *backend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{name: name, ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req, _ := io.ReadAll(c)
				b.mu.Lock()
				b.got = append(b.got, string(req))
				hold := b.hold
				b.mu.Unlock()
				if hold != nil {
					<-hold
				}
				c.Write([]byte(b.name + ":" + strings.ToUpper(string(req))))
			}()
		}
	}()
	return b
}

type view struct {
	mu      sync.Mutex
	daemons []route.Daemon
	jobs    map[string]Job
	warm    map[route.Project][]route.Activity
	zones   map[string]string // nodes known without a daemon now
}

func (v *view) Daemons() []route.Daemon { return v.daemons }
func (v *view) Zone(node string) (string, bool) {
	for _, d := range v.daemons {
		if d.Node == node {
			return d.Zone, true
		}
	}
	z, ok := v.zones[node]
	return z, ok
}
func (v *view) Job(ip string) (Job, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	j, ok := v.jobs[ip]
	return j, ok
}
func (v *view) Warm(p route.Project) []route.Activity { return v.warm[p] }

var projA = route.Project{Instance: "0a1b2c", ID: 7}

// setup starts a router over two daemons on nodes n1 and n2; the client
// (127.0.0.1) is a job of projA on n1 unless jobs says otherwise.
func setup(t *testing.T, v *view) (addr string, b1, b2 *backend, reports *[]store.Decision, cancel context.CancelFunc, done chan error) {
	t.Helper()
	b1, b2 = newBackend(t, "d1"), newBackend(t, "d2")
	if v.daemons == nil {
		v.daemons = []route.Daemon{
			{Name: "bk-1", Node: "n1", Addr: "10.9.0.1", Ready: true},
			{Name: "bk-2", Node: "n2", Addr: "10.9.0.2", Ready: true},
		}
	}
	real := map[string]string{"10.9.0.1:1234": b1.ln.Addr().String(), "10.9.0.2:1234": b2.ln.Addr().String()}
	var mu sync.Mutex
	reports = &[]store.Decision{}
	r := &Router{View: v, Node: "n1", Port: "1234", IdentityWait: -1,
		Report: func(d store.Decision) { mu.Lock(); *reports = append(*reports, d); mu.Unlock() },
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", real[addr])
		}}
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- r.Serve(ctx, ln, 2*time.Second) }()
	t.Cleanup(cancel)
	return ln.Addr().String(), b1, b2, reports, cancel, done
}

// exchange sends req, closes its sending half and reads the answer.
func exchange(t *testing.T, addr, req string) (string, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(req)); err != nil {
		return "", err
	}
	c.(*net.TCPConn).CloseWrite()
	b, err := io.ReadAll(c)
	return string(b), err
}

func TestBytesBothWaysWithHalfClose(t *testing.T) {
	big := strings.Repeat("x", 4<<20)
	addr, _, _, _, _, _ := setup(t, &view{})
	got, err := exchange(t, addr, big)
	if err != nil || got != "d1:"+strings.ToUpper(big) {
		t.Fatalf("answer of %d bytes (%v)", len(got), err)
	}
}

func TestRoutesByProject(t *testing.T) {
	v := &view{jobs: map[string]Job{"127.0.0.1": {Project: projA, JobID: 99, Node: "n1"}},
		warm: map[route.Project][]route.Activity{projA: {{Node: "n2", At: time.Now().Add(-time.Hour)}}}}
	addr, b1, b2, reports, _, _ := setup(t, v)
	got, err := exchange(t, addr, "build")
	if err != nil || got != "d2:BUILD" {
		t.Fatalf("got %q (%v): the project is warm on n2", got, err)
	}
	if len(b1.got) != 0 || len(b2.got) != 1 {
		t.Errorf("daemon 1 saw %v, daemon 2 %v", b1.got, b2.got)
	}
	if len(*reports) != 1 {
		t.Fatalf("reports %+v", *reports)
	}
	d := (*reports)[0]
	if d.Project != projA || d.JobID != 99 || d.Node != "n2" || d.Daemon != "bk-2" || d.Reason != route.ZoneWarm || d.JobNode != "n1" {
		t.Errorf("decision %+v", d)
	}
}

func TestUnknownPeerGoesToItsNode(t *testing.T) {
	v := &view{daemons: []route.Daemon{{Name: "bk-1", Node: "n1", Addr: "10.9.0.1", Ready: true}, {Name: "bk-2", Node: "n2", Addr: "10.9.0.2", Ready: true}}}
	addr, _, _, reports, _, _ := setup(t, v)
	if got, err := exchange(t, addr, "x"); err != nil || got != "d1:X" {
		t.Fatalf("got %q (%v)", got, err)
	}
	if len(*reports) != 0 {
		t.Errorf("unknown connections reported: %+v", *reports)
	}
}

func TestNoDaemonClosesTheConnection(t *testing.T) {
	v := &view{daemons: []route.Daemon{{Name: "bk-1", Node: "n1", Addr: "10.9.0.1", Ready: false}}}
	addr, _, _, _, _, _ := setup(t, v)
	got, err := exchange(t, addr, "x")
	if got != "" {
		t.Errorf("an answer without a daemon: %q (%v)", got, err)
	}
}

// A daemon that dies mid-build ends the job's connection: buildctl fails
// at once instead of waiting.
func TestDaemonGoneEndsTheClient(t *testing.T) {
	addr, b1, _, _, _, _ := setup(t, &view{})
	b1.mu.Lock()
	b1.hold = make(chan struct{})
	b1.mu.Unlock()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("long build"))
	c.(*net.TCPConn).CloseWrite()
	time.Sleep(100 * time.Millisecond)
	b1.ln.Close()
	// Close every open backend connection: the daemon is gone.
	close(b1.hold)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = io.ReadAll(c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("the client still waits after its daemon went away")
	}
}

// On SIGTERM the router stops taking connections and lets the open ones
// finish: a build in progress completes through it.
func TestDrain(t *testing.T) {
	addr, b1, _, _, cancel, done := setup(t, &view{})
	b1.mu.Lock()
	b1.hold = make(chan struct{})
	b1.mu.Unlock()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("running"))
	c.(*net.TCPConn).CloseWrite()
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
	if _, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
		t.Error("a new connection was accepted while draining")
	}
	select {
	case <-done:
		t.Fatal("stopped with a build open")
	case <-time.After(300 * time.Millisecond):
	}
	close(b1.hold)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "d1:RUNNING" {
		t.Fatalf("the open build got %q (%v)", got, err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("did not stop after the last connection")
	}
}

// A connection still open when the drain time is over is closed.
func TestDrainTimeLimit(t *testing.T) {
	addr, b1, _, _, cancel, done := setup(t, &view{})
	b1.mu.Lock()
	b1.hold = make(chan struct{})
	b1.mu.Unlock()
	defer close(b1.hold)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("forever"))
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the drain never ended")
	}
	if d := time.Since(start); d < 1500*time.Millisecond {
		t.Errorf("stopped after %s, before the drain time", d)
	}
}

func TestReporterKeepsWhileAway(t *testing.T) {
	var mu sync.Mutex
	var sent []store.Decision
	fail := true
	r := &Reporter{Every: 20 * time.Millisecond, Max: 3, Send: func(_ context.Context, ds []store.Decision) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			return errors.New("away")
		}
		sent = append(sent, ds...)
		return nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	for i := 1; i <= 5; i++ {
		r.Add(store.Decision{JobID: uint64(i), Reason: route.ZoneWarm})
	}
	time.Sleep(100 * time.Millisecond)
	if r.Pending() != 3 {
		t.Fatalf("pending %d, want the newest 3", r.Pending())
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	time.Sleep(100 * time.Millisecond)
	cancel()
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 3 || sent[0].JobID != 3 || sent[2].JobID != 5 || r.Pending() != 0 {
		t.Errorf("sent %+v, pending %d", sent, r.Pending())
	}
}

func TestReporterSendsColdProjectsAtOnce(t *testing.T) {
	got := make(chan int, 4)
	r := &Reporter{Every: time.Hour, Send: func(_ context.Context, ds []store.Decision) error { got <- len(ds); return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	time.Sleep(20 * time.Millisecond)
	r.Add(store.Decision{Reason: route.LocalWarm})
	r.Add(store.Decision{Reason: route.ZoneHash})
	select {
	case n := <-got:
		if n != 2 {
			t.Errorf("sent %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a cold project's decision waited for the interval")
	}
}

// syncBuffer is a log destination read while the logger writes to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) count(text string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Count(s.b.String(), text)
}

// The router takes the controller's view, keeps it on "not modified" and
// while the controller is away (warning once), and takes the next one when
// the controller is back.
func TestFollowWarm(t *testing.T) {
	p := route.Project{Instance: "0a1b2c", ID: 7}
	views := map[string]control.Warm{
		"v1": {Version: "v1", Projects: []control.WarmProject{{Project: p, Activity: []route.Activity{{Node: "n2", At: time.Now()}}}},
			Active: map[string]int{"n2": 1}},
		"v2": {Version: "v2", Projects: []control.WarmProject{{Project: p, Activity: []route.Activity{{Node: "n3", At: time.Now()}}}}},
	}
	var mu sync.Mutex
	serve, requests := "v1", map[string]int{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path != "/internal/v1/route/warm" || r.Header.Get("Authorization") != "Bearer router" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		etag := `"` + serve + `"`
		switch {
		case serve == "down":
			requests["down"]++
			http.Error(w, "store unavailable", http.StatusServiceUnavailable)
		case r.Header.Get("If-None-Match") == etag:
			requests["304"]++
			w.WriteHeader(http.StatusNotModified)
		default:
			requests[serve]++
			w.Header().Set("ETag", etag)
			json.NewEncoder(w).Encode(views[serve])
		}
	}))
	defer ts.Close()
	set := func(s string) { mu.Lock(); serve = s; mu.Unlock() }
	seen := func(k string) int { mu.Lock(); defer mu.Unlock(); return requests[k] }
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%s: never", what)
			}
		}
	}
	var logs syncBuffer
	c := NewCluster(nil)
	rc := &control.RouterClient{URL: ts.URL, Token: func() (string, error) { return "router", nil }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go FollowWarm(ctx, rc, c, 5*time.Millisecond, slog.New(slog.NewTextHandler(&logs, nil)))
	on := func() string {
		w := c.Warm(p)
		if len(w) != 1 {
			return fmt.Sprint(w)
		}
		return w[0].Node
	}

	waitFor("the first view read", func() bool { return seen("v1") == 1 && seen("304") > 0 })
	if got := on(); got != "n2" {
		t.Fatalf("after the first view the project is warm on %s, want n2", got)
	}
	waitFor("not modified answers", func() bool { return seen("304") >= 3 })
	if got := on(); got != "n2" || seen("v1") != 1 {
		t.Errorf("after 304s warm on %s, %d full reads", got, seen("v1"))
	}

	set("down")
	waitFor("the controller away", func() bool { return seen("down") >= 3 })
	if got := on(); got != "n2" {
		t.Errorf("with the controller away the project is warm on %s, want the last view's n2", got)
	}
	if n := logs.count("controller not reached"); n != 1 {
		t.Errorf("warned %d times while the controller was away, want once", n)
	}

	set("v2")
	waitFor("the next view", func() bool { return on() == "n3" })
	waitFor("the log line", func() bool { return logs.count("controller reached again") == 1 })
}

func ptr[T any](v T) *T { return &v }

func TestClusterFromKubernetes(t *testing.T) {
	gitlab := "https://gitlab.example.com/"
	slice := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "box-buildkit-headless-x", Namespace: "box",
			Labels: map[string]string{discoveryv1.LabelServiceName: "box-buildkit-headless"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.1.0.1"}, NodeName: ptr("n1"), Zone: ptr("a"), TargetRef: &corev1.ObjectReference{Name: "bk-1"},
				Conditions: discoveryv1.EndpointConditions{Ready: ptr(true)}},
			{Addresses: []string{"10.1.0.2"}, NodeName: ptr("n2"), Zone: ptr("a"), TargetRef: &corev1.ObjectReference{Name: "bk-2"},
				Conditions: discoveryv1.EndpointConditions{Ready: ptr(false)}},
			{Addresses: []string{"10.1.0.3"}, NodeName: ptr("n3"), TargetRef: &corev1.ObjectReference{Name: "bk-3"},
				Conditions: discoveryv1.EndpointConditions{Ready: ptr(true), Terminating: ptr(true)}},
		},
	}
	other := slice.DeepCopy()
	other.Name, other.Labels = "unrelated", map[string]string{discoveryv1.LabelServiceName: "other"}
	pod := func(name, ip, url, project string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "jobs", UID: types.UID("uid-" + name),
			Labels: map[string]string{"ci-accelerator.io/component": "ci-job"},
			Annotations: map[string]string{"job.runner.gitlab.com/url": url, "job.runner.gitlab.com/id": "5",
				"project.runner.gitlab.com/id": project}},
			Spec: corev1.PodSpec{NodeName: "n2"}, Status: corev1.PodStatus{PodIP: ip, Phase: corev1.PodRunning}}
	}
	kube := fake.NewClientset(slice, other,
		pod("job-a", "10.2.0.5", gitlab+"g/p/-/jobs/5", "7"),
		pod("job-other-gitlab", "10.2.0.6", "https://elsewhere.example.com/g/p/-/jobs/5", "7"))
	c := NewCluster([]string{gitlab})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Watch(ctx, kube, "box", "box-buildkit-headless", "jobs")
	deadline := time.Now().Add(5 * time.Second)
	for !c.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("never ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	ds := c.Daemons()
	if len(ds) != 3 || !ds[0].Ready || ds[1].Ready || ds[2].Ready || ds[0].Zone != "a" || ds[2].Zone != "" || ds[1].Addr != "10.1.0.2" {
		t.Fatalf("daemons %+v", ds)
	}
	j, ok := c.Job("10.2.0.5")
	want := route.Project{Instance: history.InstancePrefix(gitlab), ID: 7}
	if !ok || j.Project != want || j.JobID != 5 || j.Node != "n2" {
		t.Fatalf("job %+v %v", j, ok)
	}
	if _, ok := c.Job("10.2.0.6"); ok {
		t.Error("a job of another GitLab is known")
	}
	// The pod ends: its address is no longer that job's.
	if err := kube.CoreV1().Pods("jobs").Delete(ctx, "job-a", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, ok := c.Job("10.2.0.5"); ok; _, ok = c.Job("10.2.0.5") {
		if time.Now().After(deadline) {
			t.Fatal("a deleted pod's job is still known")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Own decisions count at once and give way to the controller's view.
	now := time.Now()
	c.Decided(want, "n1", now)
	if w := c.Warm(want); len(w) != 1 || w[0].Node != "n1" {
		t.Fatalf("own warmth %+v", w)
	}
	c.SetWarm(control.Warm{Projects: []control.WarmProject{{Project: want, Activity: []route.Activity{{Node: "n1", At: now}}}},
		Active: map[string]int{"n1": 4}}, now.Add(time.Minute))
	if w := c.Warm(want); len(w) != 1 {
		t.Errorf("after the controller's view %+v", w)
	}
	if d := c.Daemons(); d[0].Active != 4 {
		t.Errorf("active builds %+v", d[0])
	}
	// A node's zone outlives its daemon's endpoint (a rollout, an
	// eviction): jobs on it still belong to their zone.
	gone := slice.DeepCopy()
	gone.Endpoints = gone.Endpoints[1:]
	if _, err := kube.DiscoveryV1().EndpointSlices("box").Update(context.Background(), gone, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for len(c.Daemons()) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("the endpoint never went")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if z, ok := c.Zone("n1"); !ok || z != "a" {
		t.Errorf("zone of n1 after its daemon went: %q %v", z, ok)
	}
	if z, ok := c.Zone("n3"); !ok || z != "" {
		t.Errorf("zone of n3 (no label): %q %v", z, ok)
	}
	if _, ok := c.Zone("n9"); ok {
		t.Error("a node never seen has a zone")
	}
}

// A job's pod shows up in the watch a moment after its first connection:
// the connection waits for it instead of going to its node's daemon.
func TestWaitsForTheJobsPod(t *testing.T) {
	v := &view{jobs: map[string]Job{}, warm: map[route.Project][]route.Activity{projA: {{Node: "n2", At: time.Now()}}}}
	r := &Router{View: v, Node: "n1", IdentityWait: 2 * time.Second}
	go func() {
		time.Sleep(300 * time.Millisecond)
		v.mu.Lock()
		v.jobs["10.1.1.1"] = Job{Project: projA, JobID: 7, Node: "n1"}
		v.mu.Unlock()
	}()
	v.daemons = []route.Daemon{{Name: "bk-1", Node: "n1", Addr: "a", Ready: true}, {Name: "bk-2", Node: "n2", Addr: "b", Ready: true}}
	start := time.Now()
	r.awaitJob(context.Background(), "10.1.1.1")
	d, _, err := r.Decide("10.1.1.1")
	if err != nil || d.Daemon.Node != "n2" || time.Since(start) > 1500*time.Millisecond {
		t.Errorf("got %+v %v after %s", d, err, time.Since(start))
	}
	// A pod that never shows up waits the whole time, then goes local.
	start = time.Now()
	r.awaitJob(context.Background(), "10.9.9.9")
	if d, _, _ := r.Decide("10.9.9.9"); d.Reason != route.UnknownLocal || time.Since(start) < 2*time.Second {
		t.Errorf("unknown: %+v after %s", d, time.Since(start))
	}
}

// One zone label per node (kind, or zones close together): with zones
// kept, a job stays in its zone; ignored, it follows the warm node.
func TestZones(t *testing.T) {
	v := &view{jobs: map[string]Job{"10.1.1.1": {Project: projA, JobID: 7, Node: "n1"}},
		warm:    map[route.Project][]route.Activity{projA: {{Node: "n2", At: time.Now()}}},
		daemons: []route.Daemon{{Name: "bk-1", Node: "n1", Zone: "a", Addr: "a", Ready: true}, {Name: "bk-2", Node: "n2", Zone: "b", Addr: "b", Ready: true}}}
	r := &Router{View: v, Node: "n1"}
	if d, _, _ := r.Decide("10.1.1.1"); d.Daemon.Node != "n1" || d.Reason != route.ZoneHash {
		t.Errorf("zones kept: %+v", d)
	}
	r.IgnoreZones = true
	if d, _, _ := r.Decide("10.1.1.1"); d.Daemon.Node != "n2" || d.Reason != route.ZoneWarm {
		t.Errorf("zones ignored: %+v", d)
	}
}

// A job's node whose daemon is gone (between a rollout's old and new pod,
// a daemon evicted or pending) keeps its zone: the job stays in it, or is
// refused when crossing zones is not allowed.
func TestZoneOfANodeWithoutADaemon(t *testing.T) {
	v := &view{jobs: map[string]Job{"10.1.1.1": {Project: projA, JobID: 7, Node: "a1"}},
		daemons: []route.Daemon{{Name: "bk-a2", Node: "a2", Zone: "a", Addr: "a2", Ready: true},
			{Name: "bk-b1", Node: "b1", Zone: "b", Addr: "b1", Ready: true}, {Name: "bk-b2", Node: "b2", Zone: "b", Addr: "b2", Ready: true}},
		zones: map[string]string{"a1": "a"}}
	r := &Router{View: v, Node: "a1"}
	if d, _, err := r.Decide("10.1.1.1"); err != nil || d.Daemon.Node != "a2" || d.Reason != route.ZoneHash {
		t.Errorf("zone a has a daemon: %+v, %v", d, err)
	}
	v.daemons = v.daemons[1:]
	if d, _, err := r.Decide("10.1.1.1"); !errors.Is(err, route.ErrNoDaemon) {
		t.Errorf("zone a has none, crossing not allowed: %+v, %v", d, err)
	}
	r.Policy.CrossZone = true
	if d, rec, err := r.Decide("10.1.1.1"); err != nil || d.Reason != route.CrossZone || rec.Reason != route.CrossZone {
		t.Errorf("zone a has none, crossing allowed: %+v, %v", d, err)
	}
	// A node never seen with a daemon (a runner tier on any node, a node
	// that is no build node): its zone is not known, so every ready daemon
	// is its zone. The project's warm daemon takes it, whichever zone that
	// is, and it is never refused for crossing zones.
	v.zones = nil
	v.daemons = append(v.daemons, route.Daemon{Name: "bk-a2", Node: "a2", Zone: "a", Addr: "a2", Ready: true})
	for _, node := range []string{"a2", "b1", "b2"} {
		v.warm = map[route.Project][]route.Activity{projA: {{Node: node, At: time.Now().Add(-time.Hour)}}}
		for _, cross := range []bool{true, false} {
			r.Policy.CrossZone = cross
			if d, rec, err := r.Decide("10.1.1.1"); err != nil || d.Daemon.Node != node || d.Reason != route.ZoneWarm || rec.Reason != route.ZoneWarm {
				t.Errorf("zone unknown, warm on %s, crossZone=%v: %+v, %v", node, cross, d, err)
			}
		}
	}
	v.warm = nil
	// Zones ignored: every daemon is one zone.
	r.IgnoreZones = true
	if d, _, err := r.Decide("10.1.1.1"); err != nil || d.Reason != route.ZoneHash {
		t.Errorf("zones ignored: %+v, %v", d, err)
	}
}

// A connection accepted just before SIGTERM, still waiting for its pod in
// the watch, is routed and served, not dropped.
func TestDrainServesAConnectionStillWaitingForItsPod(t *testing.T) {
	v := &view{jobs: map[string]Job{}}
	b1 := newBackend(t, "d1")
	v.daemons = []route.Daemon{{Name: "bk-1", Node: "n1", Addr: "10.9.0.1", Ready: true}}
	r := &Router{View: v, Node: "n1", Port: "1234", IdentityWait: time.Second,
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", b1.ln.Addr().String())
		}}
	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Serve(ctx, ln, 5*time.Second) }()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(50 * time.Millisecond)
	cancel() // SIGTERM while the connection waits for its pod
	c.Write([]byte("late"))
	c.(*net.TCPConn).CloseWrite()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "d1:LATE" {
		t.Fatalf("the connection got %q (%v)", got, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not end")
	}
}

// Decisions dropped (the queue full) while a batch is on its way never
// cost decisions that were not sent.
func TestReporterKeepsUnsentWhenDroppingDuringASend(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var sent []uint64
	r := &Reporter{Every: time.Hour, Max: 4, Send: func(_ context.Context, ds []store.Decision) error {
		<-release
		mu.Lock()
		defer mu.Unlock()
		for _, d := range ds {
			sent = append(sent, d.JobID)
		}
		return nil
	}}
	for i := 1; i <= 4; i++ {
		r.Add(store.Decision{JobID: uint64(i)})
	}
	go r.flush(context.Background()) // sends 1-4, held
	time.Sleep(50 * time.Millisecond)
	for i := 5; i <= 7; i++ {
		r.Add(store.Decision{JobID: uint64(i)}) // drops 1, 2, 3
	}
	close(release)
	time.Sleep(50 * time.Millisecond)
	r.mu.Lock()
	var left []uint64
	for _, d := range r.pending {
		left = append(left, d.JobID)
	}
	r.mu.Unlock()
	if fmt.Sprint(left) != "[5 6 7]" {
		t.Errorf("left after the send %v, want [5 6 7]", left)
	}
}

// The GitLab URLs change while the router runs (the runners' gitlabUrl
// was corrected): the pods it already knows are read again with them.
func TestClusterNewGitLabURLs(t *testing.T) {
	old, now := "https://gitlab.example.com/", "https://gitlab.example.com"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "job-a", Namespace: "jobs", UID: "uid-a",
		Labels: map[string]string{"ci-accelerator.io/component": "ci-job"},
		Annotations: map[string]string{"job.runner.gitlab.com/url": "https://gitlab.example.com/g/p/-/jobs/5",
			"job.runner.gitlab.com/id": "5", "project.runner.gitlab.com/id": "7"}},
		Spec: corev1.PodSpec{NodeName: "n2"}, Status: corev1.PodStatus{PodIP: "10.2.0.5", Phase: corev1.PodRunning}}
	kube := fake.NewClientset(pod)
	c := NewCluster([]string{old})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Watch(ctx, kube, "box", "box-buildkit-headless", "jobs")
	deadline := time.Now().Add(5 * time.Second)
	for _, ok := c.Job("10.2.0.5"); !ok; _, ok = c.Job("10.2.0.5") {
		if time.Now().After(deadline) {
			t.Fatal("the job never known")
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.SetGitLabs([]string{now})
	j, ok := c.Job("10.2.0.5")
	if want := history.InstancePrefix(now); !ok || j.Project.Instance != want {
		t.Errorf("job %+v %v, want instance %s", j, ok, want)
	}
	c.SetGitLabs([]string{"https://elsewhere.example.com/"})
	if j, ok := c.Job("10.2.0.5"); ok {
		t.Errorf("a job of a GitLab no longer configured is known: %+v", j)
	}
}

// racingStore is a pod store whose pod is deleted while it is listed: the
// watch drops a pod from its store first and runs the delete handler
// after, so a list can hold a pod whose deletion is being handled.
type racingStore struct {
	cache.Store
	onList func()
}

func (s *racingStore) List() []any {
	out := s.Store.List()
	s.onList()
	return out
}

// Reading the pods again for new GitLab URLs never brings back a job pod
// deleted meanwhile: its address would count as that job's until another
// pod took it, and a new pod there would be routed as the old job.
func TestClusterNewGitLabURLsWhileAPodGoes(t *testing.T) {
	url := "https://gitlab.example.com"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "job-a", Namespace: "jobs", UID: "uid-a",
		Labels: map[string]string{"ci-accelerator.io/component": "ci-job"},
		Annotations: map[string]string{"job.runner.gitlab.com/url": url + "/g/p/-/jobs/5",
			"job.runner.gitlab.com/id": "5", "project.runner.gitlab.com/id": "7"}},
		Spec: corev1.PodSpec{NodeName: "n2"}, Status: corev1.PodStatus{PodIP: "10.2.0.5", Phase: corev1.PodRunning}}
	c := NewCluster([]string{url})
	c.setPod(pod)
	if _, ok := c.Job("10.2.0.5"); !ok {
		t.Fatal("the job is not known")
	}
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := store.Add(pod); err != nil {
		t.Fatal(err)
	}
	handled := make(chan struct{})
	c.pods = &racingStore{Store: store, onList: func() {
		_ = store.Delete(pod)
		go func() {
			c.deletePod(pod)
			close(handled)
		}()
		// Give the handler its chance to run before the listed pods are
		// read; it may have to wait for them instead.
		select {
		case <-handled:
		case <-time.After(200 * time.Millisecond):
		}
	}}
	c.SetGitLabs([]string{url})
	<-handled
	if j, ok := c.Job("10.2.0.5"); ok {
		t.Errorf("a deleted job pod is known again: %+v", j)
	}
}
