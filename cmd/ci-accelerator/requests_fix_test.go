package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// One cache folder listed for two GitLabs is refused: the runners keep
// their archives under project/<id>/, so the projects of one id would mix.
// Listed twice for the same GitLab, it is still read once.
func TestLoadCachesRefusesOneFolderForTwoGitLabs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "caches.json")
	a := `{"server":"s3.example.com","bucket":"ci-cache","region":"eu-1","path":"shared","instance":"aaaaaa","secret":"s"}`
	b := `{"server":"s3.example.com","bucket":"ci-cache","region":"eu-1","path":"/shared/","instance":"bbbbbb","secret":"s"}`
	if err := os.WriteFile(path, []byte("["+a+","+b+"]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadCaches(path); err == nil || !strings.Contains(err.Error(), "two GitLabs") ||
		!strings.Contains(err.Error(), "aaaaaa") || !strings.Contains(err.Error(), "bbbbbb") {
		t.Errorf("one folder for two GitLabs: %+v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte("["+a+","+a+"]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadCaches(path); err != nil || len(got) != 1 {
		t.Errorf("one folder twice for one GitLab: %+v, %v", got, err)
	}
}

// A job pod whose URL names no configured GitLab is said once at the
// warning level: otherwise every build would get no job without a word.
func TestWatchJobsWarnsOfAJobPodOfNoGitLab(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, err := store.Open(ctx, t.TempDir(), store.Options{Keep: 24 * time.Hour, KeepOutput: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var buf lockedBuf
	log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	urls := []string{"https://gitlab.example.com/"}
	col := collector.New(st, map[string]bool{history.InstancePrefix(urls[0]): true}, log)
	client := fake.NewClientset()
	// The job watch's goroutines read klog's logger, which the next test's
	// start sets: they end before this test does.
	watched := watchJobs(ctx, config{jobsNamespace: "jobs", gitlabURLs: urls}, client, st, col, log)
	defer func() {
		cancel()
		select {
		case <-watched:
		case <-time.After(10 * time.Second):
			t.Error("the job watch did not stop")
		}
	}()
	for _, name := range []string{"runner-a", "runner-b"} {
		p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "jobs",
			Labels: map[string]string{"ci-accelerator.io/component": "ci-job"},
			Annotations: map[string]string{"job.runner.gitlab.com/id": "1",
				"job.runner.gitlab.com/url": "http://gitlab-webservice.gitlab.svc:8181/g/p/-/jobs/1"}}}
		if _, err := client.CoreV1().Pods("jobs").Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	const msg = "starts with none of the GitLab URLs"
	for deadline := time.Now().Add(5 * time.Second); strings.Count(buf.String(), msg) < 2; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("job pods of no GitLab not told twice:\n%s", buf.String())
		}
	}
	if n := strings.Count(buf.String(), `"level":"WARN"`); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "gitlab-webservice.gitlab.svc:8181") {
		t.Errorf("the pod's URL is not named:\n%s", buf.String())
	}
}

// A changed URL file that the next start would refuse (a second GitLab
// next to the installation's token) keeps the running controller, with a
// warning: ending it would leave none. A change it can start with still
// ends it.
func TestControllerKeepsRunningOnRefusedGitLabURLs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "urls")
	if err := os.WriteFile(path, []byte("https://gitlab.example.com/"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("glpat-not-a-real-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	urlsEvery = 20 * time.Millisecond
	defer func() { urlsEvery = 30 * time.Second }()
	var buf lockedBuf
	logOut = &buf
	defer func() { logOut = os.Stderr }()
	done := make(chan error, 1)
	go func() {
		done <- controllerMain([]string{"-data", t.TempDir(), "-daemon", "tcp://127.0.0.1:1", "-server-name", "daemons", "-tls-dir", dir,
			"-gitlab-urls-file", path, "-gitlab-token-file", token, "-listen", "127.0.0.1:0"}, false)
	}()
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(buf.String(), `"msg":"ready"`); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no ready line:\n%s", buf.String())
		}
	}
	replace(t, path, "https://gitlab.example.com/\nhttps://other.example.com/")
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(buf.String(), "would be refused"); time.Sleep(20 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("ended on URLs its next start refuses (%v):\n%s", err, buf.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no warning:\n%s", buf.String())
		}
	}
	select {
	case err := <-done:
		t.Fatalf("ended on URLs its next start refuses (%v):\n%s", err, buf.String())
	case <-time.After(200 * time.Millisecond):
	}
	replace(t, path, "https://other.example.com/")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("still running with the old URLs:\n%s", buf.String())
	}
}
