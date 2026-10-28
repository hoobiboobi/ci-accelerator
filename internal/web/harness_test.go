package web

import (
	"context"
	"io/fs"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// testOpts are a test console's parts, as the controller has them.
type testOpts struct {
	Live    control.Live
	API     *gitlab.Fetcher
	Token   gitlab.Credential
	Auth    *auth.Auth
	Release string
	Version string
	Assets  fs.FS
}

// split runs the pages against a controller in another "process" (its API
// over HTTP) instead of in this one: WEB_TEST_SPLIT=1 (make go-test runs
// both).
var split = os.Getenv("WEB_TEST_SPLIT") != ""

// wire makes the pages' backend, GitLab reader and sign-in from o: the
// controller's parts themselves, or a client of a controller serving them.
func (o testOpts) wire(t *testing.T, st *store.Store) (Backend, Options) {
	t.Helper()
	local := &control.Local{Store: st, Live: o.Live}
	opt := Options{Release: o.Release, Version: o.Version, Assets: o.Assets, InstallToken: o.Token != nil && o.Auth == nil}
	var gl *control.LocalGitLab
	if o.API != nil {
		gl = &control.LocalGitLab{Fetcher: o.API, Token: o.Token, Auth: o.Auth}
		opt.GitLab = gl
	}
	if o.Auth != nil {
		opt.Auth = o.Auth
	}
	if !split {
		return local, opt
	}
	srv := &control.Server{Local: local, GitLab: gl, Auth: o.Auth, Config: control.ConfigFor(o.Version, o.Release, gl, o.Auth),
		Authorize: control.StaticToken("test-token"), Log: testLogger()}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := control.Dial(cctx, control.ClientOptions{URL: ts.URL, HTTP: ts.Client(), Token: func() (string, error) { return "test-token", nil }})
	if err != nil {
		t.Fatal(err)
	}
	go c.Run(cctx)
	if gl != nil {
		opt.GitLab = c
	}
	if o.Auth != nil {
		opt.Auth = c
	}
	return c, opt
}
