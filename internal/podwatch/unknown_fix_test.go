package podwatch

import (
	"context"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// A job pod whose URL names no configured GitLab is told once, so that the
// controller can say why its builds get no job.
func TestWatchTellsUnknownJobPodsOnce(t *testing.T) {
	client := fake.NewClientset()
	var mu sync.Mutex
	var unknown []string
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Watch(ctx, client, "jobs", []string{"https://gitlab.com/"}, Handlers{
			Unknown: func(_ context.Context, pod, url string) {
				mu.Lock()
				unknown = append(unknown, pod+" "+url)
				mu.Unlock()
			},
		})
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	p := pod("runner-x", "", jobAt("http://gitlab-webservice.gitlab.svc:8181/g/p/-/jobs/1"))
	if _, err := client.CoreV1().Pods("jobs").Create(ctx, p, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods("jobs").Create(ctx, pod("runner-ok", "", jobAnnotations()), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		mu.Lock()
		n := len(unknown)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	p.Spec.NodeName = "node-1"
	if _, err := client.CoreV1().Pods("jobs").Update(ctx, p, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(unknown) != 1 || unknown[0] != "jobs/runner-x http://gitlab-webservice.gitlab.svc:8181/g/p/-/jobs/1" {
		t.Errorf("unknown %q", unknown)
	}
}
