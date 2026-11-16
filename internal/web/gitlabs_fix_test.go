package web

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// With projects of two GitLabs the shell says so, and names each GitLab
// by its host where sign-in knows it.
func TestShellNamesTheGitLabs(t *testing.T) {
	sh := Shell{hosts: map[string]string{"0a1b2c": "gitlab.example.com"}}
	if sh.InstHost("0a1b2c") != "gitlab.example.com" || sh.InstHost("ffffff") != "GitLab ffffff" {
		t.Errorf("hosts: %q, %q", sh.InstHost("0a1b2c"), sh.InstHost("ffffff"))
	}
	st := openStore(t)
	savePipeline(t, st, 600, testNow.Add(-time.Hour))
	testLogs = &lockedBuffer{}
	srv, err := New(&control.Local{Store: st}, testLogger(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := srv.shell(httptest.NewRequest("GET", "/", nil), "", "")
	if err != nil || got.ManyGitLabs {
		t.Errorf("one GitLab: many %v, err %v", got.ManyGitLabs, err)
	}
	other := history.InstancePrefix("https://gitlab.other.example/")
	start := testNow.Add(-time.Hour)
	if err := st.SaveJob(ctx, store.Job{Instance: other, JobID: 10, PipelineID: 1, ProjectID: 42, ProjectPath: "group/app", Name: "b", Stage: "build",
		Ref: "main", URL: "https://gitlab.other.example/group/app/-/jobs/10"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveBuild(ctx, store.Build{Generation: 1, Build: &history.Build{Ref: "other", Daemon: "bk-daemon-aaaaa", Target: "api", Started: start, Completed: start.Add(time.Second),
		Attribution: history.Attribution{State: history.Attributed, Instance: other, Pipeline: 1, Job: 10}}}); err != nil {
		t.Fatal(err)
	}
	if got, err = srv.shell(httptest.NewRequest("GET", "/", nil), "", ""); err != nil || !got.ManyGitLabs {
		t.Errorf("two GitLabs: many %v, err %v", got.ManyGitLabs, err)
	}
	// Both project menus name each project's GitLab.
	page := get(t, serveBackend(t, &control.Local{Store: st}), "/")
	if n := strings.Count(page.body, `<span class="count">GitLab `+other+`</span>`); n != 2 {
		t.Errorf("the project menus name the other GitLab %d times, want 2", n)
	}
}
