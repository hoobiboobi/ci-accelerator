package podwatch

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

func pod(name, node string, annotations map[string]string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "jobs", Labels: map[string]string{"ci-accelerator.io/component": "ci-job"}, Annotations: annotations},
		Spec:       corev1.PodSpec{NodeName: node},
	}
}

// The annotations a gitlab-runner 19.4 job pod carries with the
// ci-accelerator runner configuration.
func jobAnnotations() map[string]string {
	return map[string]string{
		"job.runner.gitlab.com/id":              "17298723831",
		"job.runner.gitlab.com/url":             "https://gitlab.com/group/app/-/jobs/17298723831",
		"job.runner.gitlab.com/name":            "build-worker",
		"job.runner.gitlab.com/ref":             "main",
		"job.runner.gitlab.com/sha":             "ea2f1a35",
		"project.runner.gitlab.com/id":          "77",
		"ci-accelerator.io/pipeline-id":         "2965068197",
		"ci-accelerator.io/pipeline-source":     "push",
		"ci-accelerator.io/pipeline-created-at": "2026-10-19T21:16:45Z",
		"ci-accelerator.io/project-path":        "group/app",
		"ci-accelerator.io/job-stage":           "build",
		"ci-accelerator.io/job-started-at":      "2026-10-19T21:16:50Z",
		"ci-accelerator.io/commit-title":        `dev: "add" $CI_JOB_TOKEN`,
		"ci-accelerator.io/commit-author":       "A Dev <a@example.com>",
		"ci-accelerator.io/merge-request-iid":   "",
	}
}

func TestJobFromPod(t *testing.T) {
	j, ok := Job(pod("p", "node-1", jobAnnotations()), []string{"https://gitlab.example.com/", "https://gitlab.com/"})
	if !ok {
		t.Fatal("not read")
	}
	want := store.Job{
		Instance: history.InstancePrefix("https://gitlab.com/"), JobID: 17298723831, PipelineID: 2965068197, ProjectID: 77,
		ProjectPath: "group/app", Name: "build-worker", Stage: "build", URL: "https://gitlab.com/group/app/-/jobs/17298723831",
		Node: "node-1", StartedAt: time.Date(2026, 10, 19, 21, 16, 50, 0, time.UTC), Ref: "main", SHA: "ea2f1a35", Source: "push",
		PipelineCreatedAt: time.Date(2026, 10, 19, 21, 16, 45, 0, time.UTC), CommitTitle: `dev: "add" $CI_JOB_TOKEN`, CommitAuthor: "A Dev <a@example.com>",
	}
	if j != want {
		t.Errorf("\n got %+v\nwant %+v", j, want)
	}
	// The prefix is derived from the URL exactly as configured.
	if j, _ := Job(pod("p", "", jobAnnotations()), []string{"https://gitlab.com"}); j.Instance != history.InstancePrefix("https://gitlab.com") {
		t.Errorf("instance %s for a URL without the slash", j.Instance)
	}
	for name, urls := range map[string][]string{"another GitLab": {"https://gitlab.example.com/"}, "a host that only starts the same": {"https://gitlab.co/"}} {
		if _, ok := Job(pod("p", "", jobAnnotations()), urls); ok {
			t.Errorf("%s: read as a job", name)
		}
	}
	a := jobAnnotations()
	delete(a, "job.runner.gitlab.com/id")
	if _, ok := Job(pod("p", "", a), []string{"https://gitlab.com/"}); ok {
		t.Error("a pod without a job id read as a job")
	}
}

func TestWatchSeesJobPods(t *testing.T) {
	client := fake.NewClientset()
	var mu sync.Mutex
	var seen, gone []store.Job
	var goneAt []time.Time
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Watch(ctx, client, "jobs", []string{"https://gitlab.com/"}, Handlers{
			Job: func(_ context.Context, j store.Job) {
				mu.Lock()
				seen = append(seen, j)
				mu.Unlock()
			},
			Gone: func(_ context.Context, j store.Job, at time.Time) {
				mu.Lock()
				gone, goneAt = append(gone, j), append(goneAt, at)
				mu.Unlock()
			},
		})
		close(done)
	}()
	wait := func(n int) {
		t.Helper()
		for i := 0; i < 300; i++ {
			mu.Lock()
			got := len(seen)
			mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("saw %d jobs, want %d", len(seen), n)
	}
	p := pod("runner-abc", "", jobAnnotations())
	if _, err := client.CoreV1().Pods("jobs").Create(ctx, p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	wait(1)
	p.Spec.NodeName = "node-2"
	if _, err := client.CoreV1().Pods("jobs").Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	wait(2)
	// Its build container starts: the job is read again with the time.
	started := time.Date(2026, 10, 28, 1, 3, 37, 0, time.UTC)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{
		{Name: "helper", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Time{Time: started.Add(time.Second)}}}},
		{Name: "build", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Time{Time: started}}}}}
	if _, err := client.CoreV1().Pods("jobs").UpdateStatus(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	wait(3)
	mu.Lock()
	if !seen[2].PodRunning.Equal(started) || !seen[0].PodRunning.IsZero() {
		t.Errorf("build container start: %v, before it: %v", seen[2].PodRunning, seen[0].PodRunning)
	}
	mu.Unlock()
	p.Status.Phase = corev1.PodSucceeded
	if _, err := client.CoreV1().Pods("jobs").UpdateStatus(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	waitGone := func(n int) {
		t.Helper()
		for i := 0; i < 300; i++ {
			mu.Lock()
			got := len(gone)
			mu.Unlock()
			if got >= n {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("%d gone, want %d", len(gone), n)
	}
	// The runner deletes the pod with a 30 s grace period: the job ended
	// when it asked, not when the pod is finally gone.
	asked := time.Now().Add(-2 * time.Second).Truncate(time.Second)
	grace := int64(30)
	p.DeletionTimestamp = &metav1.Time{Time: asked.Add(30 * time.Second)}
	p.DeletionGracePeriodSeconds = &grace
	if _, err := client.CoreV1().Pods("jobs").Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitGone(1)
	if err := client.CoreV1().Pods("jobs").Delete(ctx, p.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitGone(2)
	// The console restarts while a pod is being deleted: seen and gone.
	q := pod("runner-def", "node-1", jobAnnotations())
	q.DeletionTimestamp, q.DeletionGracePeriodSeconds = &metav1.Time{Time: asked.Add(30 * time.Second)}, &grace
	if _, err := client.CoreV1().Pods("jobs").Create(ctx, q, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitGone(3)
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 || seen[0].Node != "" || seen[1].Node != "node-2" || seen[1].JobID != 17298723831 {
		t.Errorf("jobs seen: %+v", seen)
	}
	if len(gone) != 3 || gone[0].JobID != 17298723831 || !goneAt[0].Equal(asked) || !goneAt[2].Equal(asked) {
		t.Errorf("jobs gone: %+v at %v, want the deletion asked at %v", gone, goneAt, asked)
	}
}
