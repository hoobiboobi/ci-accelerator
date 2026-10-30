package router

import (
	"context"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/podwatch"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
)

// Cluster is the View from Kubernetes and the controller: the daemons from
// the EndpointSlices of their headless Service (address, node, zone,
// readiness), the jobs from their pods, warmth from the controller plus
// the router's own recent decisions.
type Cluster struct {
	mu      sync.RWMutex
	slices  map[string][]route.Daemon // by EndpointSlice name
	zones   map[string]string         // node -> zone, kept when its daemon goes
	jobs    map[string]Job            // by pod IP
	owner   map[string]string         // pod IP -> UID of the job pod holding it
	warm    map[route.Project][]route.Activity
	active  map[string]int
	own     map[route.Project][]route.Activity // decisions of this router not yet in warm
	synced  bool
	gitlabs []string
	pods    cache.Store // the job pods as the watch has them (set by Watch)
}

// NewCluster returns an empty view for the jobs of gitlabURLs.
func NewCluster(gitlabURLs []string) *Cluster {
	return &Cluster{slices: map[string][]route.Daemon{}, zones: map[string]string{}, jobs: map[string]Job{}, owner: map[string]string{}, warm: map[route.Project][]route.Activity{},
		own: map[route.Project][]route.Activity{}, gitlabs: gitlabURLs}
}

// SetGitLabs replaces the GitLab URLs jobs are told by, and reads the
// pods already known again with them. The pods are listed and read under
// the lock: a pod that leaves the store meanwhile has its handler waiting
// for the lock, so the stale copy read here is undone after, not left.
func (c *Cluster) SetGitLabs(gitlabURLs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gitlabs = gitlabURLs
	if c.pods != nil {
		for _, obj := range c.pods.List() {
			c.setPodLocked(obj)
		}
	}
}

// Daemons is every daemon of the slices, ready or not, with the builds
// each node runs as the controller last said.
func (c *Cluster) Daemons() []route.Daemon {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []route.Daemon
	for _, ds := range c.slices {
		for _, d := range ds {
			d.Active = c.active[d.Node]
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Zone is the zone a daemon on node last showed; known is false for a
// node never seen with one.
func (c *Cluster) Zone(node string) (zone string, known bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	zone, known = c.zones[node]
	return zone, known
}

// Ready says whether the view is complete and has a ready daemon.
func (c *Cluster) Ready() bool {
	c.mu.RLock()
	synced := c.synced
	c.mu.RUnlock()
	if !synced {
		return false
	}
	for _, d := range c.Daemons() {
		if d.Ready {
			return true
		}
	}
	return false
}

// Job is the job whose pod has ip.
func (c *Cluster) Job(ip string) (Job, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	j, ok := c.jobs[ip]
	return j, ok
}

// Warm is the project's activity: the controller's and this router's own.
func (c *Cluster) Warm(p route.Project) []route.Activity {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return append(append([]route.Activity(nil), c.warm[p]...), c.own[p]...)
}

// Decided notes a decision of this router at once: the project's next job
// goes the same way before the controller has heard of it.
func (c *Cluster) Decided(p route.Project, node string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.own[p] = append(c.own[p], route.Activity{Node: node, At: at})
	if n := len(c.own[p]); n > 16 {
		c.own[p] = c.own[p][n-16:]
	}
}

// SetWarm takes the controller's view; it includes this router's reported
// decisions, so the own ones older than the view are dropped.
func (c *Cluster) SetWarm(w control.Warm, at time.Time) {
	m := make(map[route.Project][]route.Activity, len(w.Projects))
	for _, p := range w.Projects {
		m[p.Project] = p.Activity
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.warm, c.active = m, w.Active
	for p, acts := range c.own {
		var keep []route.Activity
		for _, a := range acts {
			if a.At.After(at.Add(-10 * time.Second)) {
				keep = append(keep, a)
			}
		}
		if len(keep) == 0 {
			delete(c.own, p)
		} else {
			c.own[p] = keep
		}
	}
}

// daemonsOf reads a slice's endpoints; port names the daemons' port.
func daemonsOf(s *discoveryv1.EndpointSlice) []route.Daemon {
	var out []route.Daemon
	for _, e := range s.Endpoints {
		if len(e.Addresses) == 0 || e.TargetRef == nil || e.NodeName == nil {
			continue
		}
		d := route.Daemon{Name: e.TargetRef.Name, Node: *e.NodeName, Addr: e.Addresses[0],
			// Ready is nil for "unknown", which the API means as ready.
			Ready: e.Conditions.Ready == nil || *e.Conditions.Ready}
		if e.Conditions.Terminating != nil && *e.Conditions.Terminating {
			d.Ready = false
		}
		if e.Zone != nil {
			d.Zone = *e.Zone
		}
		out = append(out, d)
	}
	return out
}

func jobOf(p *corev1.Pod, gitlabs []string) (string, Job, bool) {
	if p.Status.PodIP == "" || p.DeletionTimestamp != nil {
		return "", Job{}, false
	}
	switch p.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return "", Job{}, false
	}
	j, ok := podwatch.Job(p, gitlabs)
	if !ok || j.ProjectID == 0 {
		return "", Job{}, false
	}
	return p.Status.PodIP, Job{Project: route.Project{Instance: j.Instance, ID: j.ProjectID}, JobID: j.JobID, Node: p.Spec.NodeName}, true
}

// setPod takes a job pod as the watch saw it.
func (c *Cluster) setPod(obj any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setPodLocked(obj)
}

// setPodLocked is setPod with c.mu held. Pod IPs are reused: the IP's
// entry is the pod's only while it holds it.
func (c *Cluster) setPodLocked(obj any) {
	p, ok := obj.(*corev1.Pod)
	if !ok {
		return
	}
	ip, j, ok := jobOf(p, c.gitlabs)
	if ok {
		c.jobs[ip], c.owner[ip] = j, string(p.UID)
		return
	}
	for ip, uid := range c.owner {
		if uid == string(p.UID) {
			delete(c.jobs, ip)
			delete(c.owner, ip)
		}
	}
}

// deletePod takes a job pod the watch saw deleted.
func (c *Cluster) deletePod(obj any) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = d.Obj
	}
	if p, ok := obj.(*corev1.Pod); ok {
		p = p.DeepCopy()
		now := metav1.Now()
		p.DeletionTimestamp = &now
		c.setPod(p)
	}
}

// Watch fills the view from Kubernetes until ctx ends: the EndpointSlices
// of service in namespace, and the job pods of jobsNamespace.
func (c *Cluster) Watch(ctx context.Context, kube kubernetes.Interface, namespace, service, jobsNamespace string) error {
	eps := informers.NewSharedInformerFactoryWithOptions(kube, 10*time.Minute, informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = discoveryv1.LabelServiceName + "=" + service }))
	slices := eps.Discovery().V1().EndpointSlices().Informer()
	setSlice := func(obj any) {
		if s, ok := obj.(*discoveryv1.EndpointSlice); ok {
			ds := daemonsOf(s)
			c.mu.Lock()
			c.slices[s.Name] = ds
			for _, d := range ds {
				c.zones[d.Node] = d.Zone
			}
			c.mu.Unlock()
		}
	}
	slices.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    setSlice,
		UpdateFunc: func(_, obj any) { setSlice(obj) },
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			if s, ok := obj.(*discoveryv1.EndpointSlice); ok {
				c.mu.Lock()
				delete(c.slices, s.Name)
				c.mu.Unlock()
			}
		},
	})
	pods := informers.NewSharedInformerFactoryWithOptions(kube, 10*time.Minute, informers.WithNamespace(jobsNamespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = podwatch.Selector }))
	podInf := pods.Core().V1().Pods().Informer()
	podInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.setPod,
		UpdateFunc: func(_, obj any) { c.setPod(obj) },
		DeleteFunc: c.deletePod,
	})
	c.mu.Lock()
	c.pods = podInf.GetStore()
	c.mu.Unlock()
	eps.Start(ctx.Done())
	pods.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), slices.HasSynced, podInf.HasSynced) {
		return ctx.Err()
	}
	c.mu.Lock()
	c.synced = true
	c.mu.Unlock()
	<-ctx.Done()
	return nil
}
