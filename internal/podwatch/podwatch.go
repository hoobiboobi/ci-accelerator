// Package podwatch reads CI jobs from their pods: the runner annotates every
// job pod with the job's id, name, URL, ref and commit, and the
// ci-accelerator runner configuration adds the pipeline and commit data
// from GitLab's CI variables. Pods are deleted when a job ends, so they are
// watched, not listed later.
package podwatch

import (
	"context"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// Selector selects job pods: the runner configuration labels them.
const Selector = "ci-accelerator.io/component=ci-job"

// Job reads a job from its pod. gitlabURLs are the installation's GitLab
// URLs as configured, which the trace-id prefix is derived from; the job's
// URL tells which one ran it. ok is false for a pod that is not a known
// GitLab instance's job.
func Job(pod *corev1.Pod, gitlabURLs []string) (store.Job, bool) {
	a := pod.Annotations
	url := a["job.runner.gitlab.com/url"]
	instance := ""
	for _, u := range gitlabURLs {
		if strings.HasPrefix(url, strings.TrimSuffix(u, "/")+"/") {
			instance = history.InstancePrefix(u)
			break
		}
	}
	id := number(a["job.runner.gitlab.com/id"])
	if instance == "" || id == 0 {
		return store.Job{}, false
	}
	return store.Job{
		Instance:          instance,
		JobID:             id,
		PipelineID:        number(a["ci-accelerator.io/pipeline-id"]),
		ProjectID:         number(a["project.runner.gitlab.com/id"]),
		ProjectPath:       a["ci-accelerator.io/project-path"],
		Name:              a["job.runner.gitlab.com/name"],
		Stage:             a["ci-accelerator.io/job-stage"],
		URL:               url,
		Node:              pod.Spec.NodeName,
		StartedAt:         timestamp(a["ci-accelerator.io/job-started-at"]),
		Ref:               a["job.runner.gitlab.com/ref"],
		SHA:               a["job.runner.gitlab.com/sha"],
		Source:            a["ci-accelerator.io/pipeline-source"],
		PipelineCreatedAt: timestamp(a["ci-accelerator.io/pipeline-created-at"]),
		CommitTitle:       a["ci-accelerator.io/commit-title"],
		CommitAuthor:      a["ci-accelerator.io/commit-author"],
		MergeRequestIID:   number(a["ci-accelerator.io/merge-request-iid"]),
		PodCreated:        pod.CreationTimestamp.Time,
		PodRunning:        buildStarted(pod),
	}, true
}

// buildStarted is when the pod's build container started (the
// Kubernetes executor names it "build"); zero until it has.
func buildStarted(pod *corev1.Pod) time.Time {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name != "build" {
			continue
		}
		switch {
		case c.State.Running != nil:
			return c.State.Running.StartedAt.Time
		case c.State.Terminated != nil:
			return c.State.Terminated.StartedAt.Time
		}
	}
	return time.Time{}
}

// Handlers are called for job pods: Job when a pod is first seen, again
// when it is scheduled (its node becomes known) and when its build
// container starts; Gone when the runner
// starts deleting it, which is about when its job ended (at), and again
// when it is gone. A pod never says how its job ended: GitLab does.
type Handlers struct {
	Job  func(context.Context, store.Job)
	Gone func(ctx context.Context, j store.Job, at time.Time)
}

// endedAt is when deleting a pod was asked for: its deletion deadline less
// its grace period. A pod's deletion takes that long (and longer for the
// runner's cleanup), so the end of its job is the request, not the pod's
// disappearance.
func endedAt(p *corev1.Pod) (time.Time, bool) {
	if p.DeletionTimestamp == nil {
		return time.Time{}, false
	}
	t := p.DeletionTimestamp.Time
	if g := p.DeletionGracePeriodSeconds; g != nil && *g > 0 {
		t = t.Add(-time.Duration(*g) * time.Second)
	}
	if now := time.Now(); t.After(now) {
		t = now
	}
	return t, true
}

// Watch calls h for every job pod in namespace until ctx ends.
func Watch(ctx context.Context, client kubernetes.Interface, namespace string, gitlabURLs []string, h Handlers) error {
	factory := informers.NewSharedInformerFactoryWithOptions(client, 10*time.Minute,
		informers.WithNamespace(namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = Selector }))
	pods := factory.Core().V1().Pods().Informer()
	call := func(fn func(context.Context, store.Job), pod *corev1.Pod) {
		if fn == nil {
			return
		}
		if j, ok := Job(pod, gitlabURLs); ok {
			fn(ctx, j)
		}
	}
	// A pod's end is told once: when its deletion is asked for, or when it
	// disappears without that having been seen. The informer calls its
	// handlers one at a time, so the set needs no lock; a pod leaves it
	// when it is deleted.
	ended := map[string]bool{}
	gone := func(pod *corev1.Pod, final bool) {
		if h.Gone == nil {
			return
		}
		key := string(pod.UID)
		if key == "" {
			key = pod.Namespace + "/" + pod.Name
		}
		told := ended[key]
		if final {
			delete(ended, key)
		} else {
			ended[key] = true
		}
		if told {
			return
		}
		at, ok := endedAt(pod)
		if !ok {
			at = time.Now()
		}
		if j, ok := Job(pod, gitlabURLs); ok {
			h.Gone(ctx, j, at)
		}
	}
	_, err := pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if p, ok := obj.(*corev1.Pod); ok {
				call(h.Job, p)
				// Seen for the first time while being deleted (the
				// console restarted): its job is over.
				if p.DeletionTimestamp != nil {
					gone(p, false)
				}
			}
		},
		UpdateFunc: func(old, cur any) {
			o, ok1 := old.(*corev1.Pod)
			c, ok2 := cur.(*corev1.Pod)
			if !ok1 || !ok2 {
				return
			}
			switch {
			case c.DeletionTimestamp != nil && o.DeletionTimestamp == nil:
				gone(c, false)
			case o.Spec.NodeName != c.Spec.NodeName, buildStarted(o).IsZero() && !buildStarted(c).IsZero():
				call(h.Job, c)
			}
		},
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			if p, ok := obj.(*corev1.Pod); ok {
				gone(p, true)
			}
		},
	})
	if err != nil {
		return err
	}
	factory.Start(ctx.Done())
	<-ctx.Done()
	factory.Shutdown()
	return nil
}

func number(s string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return v
}

func timestamp(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(s))
	return t
}
