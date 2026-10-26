package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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
