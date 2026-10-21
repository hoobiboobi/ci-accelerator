// Package podwatch reads CI jobs from their pods: the runner annotates every
// job pod with the job's id, name, URL, ref and commit, and the
// ci-accelerator runner configuration adds the pipeline and commit data
// from GitLab's CI variables. Pods are deleted when a job ends, so they are
// watched, not listed later.
package podwatch

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	}, true
}

// Handlers are called for job pods: Job when a pod is first seen and again
// when it is scheduled (its node becomes known), Gone when it is deleted,
// which is when its job ended.
type Handlers struct {
	Job  func(context.Context, store.Job)
	Gone func(context.Context, store.Job)
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
	_, err := pods.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) {
			if p, ok := obj.(*corev1.Pod); ok {
				call(h.Job, p)
			}
		},
		UpdateFunc: func(old, cur any) {
			o, ok1 := old.(*corev1.Pod)
			c, ok2 := cur.(*corev1.Pod)
			if ok1 && ok2 && o.Spec.NodeName != c.Spec.NodeName {
				call(h.Job, c)
			}
		},
		DeleteFunc: func(obj any) {
			if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = d.Obj
			}
			if p, ok := obj.(*corev1.Pod); ok {
				call(h.Gone, p)
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

// Running is the jobs whose pods exist now: the jobs in progress.
type Running struct {
	mu   sync.Mutex
	jobs map[string]store.Job
}

func runningKey(j store.Job) string { return fmt.Sprintf("%s/%d", j.Instance, j.JobID) }

// Set records a job pod (again, with what it says now).
func (r *Running) Set(j store.Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jobs == nil {
		r.jobs = map[string]store.Job{}
	}
	r.jobs[runningKey(j)] = j
}

// Remove forgets a job whose pod is gone.
func (r *Running) Remove(j store.Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.jobs, runningKey(j))
}

// Jobs returns the running jobs, the earliest started first.
func (r *Running) Jobs() []store.Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]store.Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].StartedAt.Equal(out[b].StartedAt) {
			return out[a].StartedAt.Before(out[b].StartedAt)
		}
		return out[a].JobID < out[b].JobID
	})
	return out
}

func number(s string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	return v
}

func timestamp(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(s))
	return t
}
