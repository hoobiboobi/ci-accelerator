package web

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// TestControllerAway: when the web's controller does not answer, every
// page says so (502) instead of showing an empty console.
func TestControllerAway(t *testing.T) {
	st, err := store.Open(ctx, t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctl := httptest.NewServer((&control.Server{Local: &control.Local{Store: st}}).Handler())
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c, err := control.Dial(cctx, control.ClientOptions{URL: ctl.URL})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(Secure(srv.Gate(mux)))
	defer ts.Close()
	if p := get(t, ts, "/builds"); p.status != http.StatusOK {
		t.Fatalf("with the controller: %d", p.status)
	}
	ctl.Close()
	for _, path := range []string{"/", "/builds", "/pipelines", "/images", "/images/api", "/nodes",
		"/builds/1", "/builds/1/steps/0", "/pipelines/0a1b2c/7", "/jobs/0a1b2c/7", "/search-index"} {
		p := get(t, ts, path)
		if p.status != http.StatusBadGateway {
			t.Errorf("%s with the controller away: %d, want 502", path, p.status)
		}
	}
}

// TestControllerCannotCheckTokens: a controller that cannot ask
// Kubernetes about the web's token (an API server restart) makes the
// pages say "try again", not blame the store.
func TestControllerCannotCheckTokens(t *testing.T) {
	st, err := store.Open(ctx, t.TempDir(), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var down atomic.Bool
	authorize := func(*http.Request) (control.Role, error) {
		if down.Load() {
			return "", fmt.Errorf("%w: connection refused", control.ErrReviewUnavailable)
		}
		return control.RoleWeb, nil
	}
	ctl := httptest.NewServer((&control.Server{Local: &control.Local{Store: st}, Authorize: authorize}).Handler())
	defer ctl.Close()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c, err := control.Dial(cctx, control.ClientOptions{URL: ctl.URL, Token: func() (string, error) { return "t", nil }})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(Secure(srv.Gate(mux)))
	defer ts.Close()
	down.Store(true)
	for _, path := range []string{"/", "/builds", "/nodes"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusBadGateway || res.Header.Get("Retry-After") == "" || strings.Contains(string(body), "store") {
			t.Errorf("%s: %d, Retry-After %q, %q", path, res.StatusCode, res.Header.Get("Retry-After"), body)
		}
	}
}

// TestPagesHoldNoState: the pages reach the store, GitLab and the
// sessions only through the Backend, GitLab and Signer interfaces, so the
// web can run without the controller's parts.
func TestPagesHoldNoState(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	banned := regexp.MustCompile(`\*store\.Store\b|store\.Open\(|\*auth\.Auth\b|\*gitlab\.(Fetcher|Client)\b|gitlab\.Credential\b|\bcollector\.(New|Dial|Supervisor)\b`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if m := banned.FindString(line); m != "" {
				t.Errorf("%s:%d uses %s", f, i+1, m)
			}
		}
	}
}
