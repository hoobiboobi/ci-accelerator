package podwatch

import (
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/history"
)

func jobAt(url string) map[string]string {
	a := jobAnnotations()
	a["job.runner.gitlab.com/url"] = url
	return a
}

// A job URL names its GitLab whatever the case of the host name and with or
// without the scheme's default port; the instance stays the hash of the URL
// as configured.
func TestJobURLMatchesTheGitLabWrittenAnotherWay(t *testing.T) {
	for _, tc := range []struct{ configured, jobURL string }{
		{"https://GitLab.Example.com/", "https://gitlab.example.com/g/p/-/jobs/1001"},
		{"https://gitlab.example.com", "https://GITLAB.EXAMPLE.COM/g/p/-/jobs/1001"},
		{"https://gitlab.example.com:443/", "https://gitlab.example.com/g/p/-/jobs/1001"},
		{"https://gitlab.example.com/", "https://gitlab.example.com:443/g/p/-/jobs/1001"},
		{"http://gitlab.example.com:80", "HTTP://gitlab.example.com/g/p/-/jobs/1001"},
	} {
		j, ok := Job(pod("p", "", jobAt(tc.jobURL)), []string{tc.configured})
		if !ok || j.Instance != history.InstancePrefix(tc.configured) || j.ProjectPath != "g/p" {
			t.Errorf("%s for %s: %v %q %q", tc.jobURL, tc.configured, ok, j.Instance, j.ProjectPath)
		}
	}
	// Another port, scheme or host is another GitLab.
	for _, other := range []string{"https://gitlab.example.com:8443/", "http://gitlab.example.com/", "https://gitlab.example.org/"} {
		if _, ok := Job(pod("p", "", jobAt("https://gitlab.example.com/g/p/-/jobs/1001")), []string{other}); ok {
			t.Errorf("read as a job of %s", other)
		}
	}
}

// Of a GitLab at the root of a host and one under a path of it, a job of
// the second is its own, in either order.
func TestJobTakesTheLongestGitLabURL(t *testing.T) {
	root, sub := "https://example.com/", "https://example.com/gitlab/"
	for _, urls := range [][]string{{root, sub}, {sub, root}} {
		j, ok := Job(pod("p", "", jobAt("https://example.com/gitlab/g/p/-/jobs/1001")), urls)
		if !ok || j.Instance != history.InstancePrefix(sub) || j.ProjectPath != "g/p" {
			t.Errorf("%v: %v %q %q", urls, ok, j.Instance, j.ProjectPath)
		}
		j, ok = Job(pod("p", "", jobAt("https://example.com/g/p/-/jobs/1001")), urls)
		if !ok || j.Instance != history.InstancePrefix(root) || j.ProjectPath != "g/p" {
			t.Errorf("%v, root: %v %q %q", urls, ok, j.Instance, j.ProjectPath)
		}
	}
}
