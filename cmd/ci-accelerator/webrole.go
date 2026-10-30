package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/api"
	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/logging"
	"github.com/hoobiboobi/ci-accelerator/internal/web"
)

// webMain serves the pages, reading everything from the controller. It
// keeps no state: any number of them can run.
func webMain(args []string) error {
	fs := flag.NewFlagSet("web", flag.ExitOnError)
	var lf logging.Flags
	lf.Register(fs)
	showVersion := fs.Bool("version", false, "print the version and exit")
	listen := fs.String("listen", ":8080", "address of the pages, the JSON API and the health check")
	controller := fs.String("controller", "", "the controller's API, https://host:port")
	caFile := fs.String("controller-ca", "/api-tls/ca.crt", "CA certificate the controller's certificate chains to")
	serverName := fs.String("controller-server-name", "", "name in the controller's certificate (default: the host of -controller)")
	tokenFile := fs.String("token-file", "/var/run/secrets/ci-accelerator/token", "file with the token sent to the controller (a projected ServiceAccount token, renewed in place)")
	devAssets := fs.String("dev-assets", "", "directory with templates/ and static/ read for every page instead of the built-in ones")
	fs.Parse(args)
	if *showVersion {
		fmt.Println("ci-accelerator " + versionString())
		return nil
	}
	if *controller == "" {
		return errors.New("give -controller")
	}
	log, err := roleLog(lf, "web")
	if err != nil {
		return err
	}
	started(log, "listen", *listen, "controller", *controller, "log_level", lf.Level)
	ca, err := os.ReadFile(*caFile)
	if err != nil {
		return fmt.Errorf("-controller-ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return fmt.Errorf("-controller-ca %s: no certificate", *caFile)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: *serverName, MinVersion: tls.VersionTLS12}
	tr.MaxIdleConnsPerHost = 64
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The address answers at once; until the controller has said how it is
	// set up, everything but the liveness check is 503.
	var handler atomic.Pointer[http.Handler]
	starting := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/livez" {
			fmt.Fprint(w, "ok")
			return
		}
		w.Header().Set("Retry-After", "5")
		http.Error(w, "The console is starting: its controller has not answered yet.", http.StatusServiceUnavailable)
	}))
	handler.Store(&starting)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, ErrorLog: logging.ErrorLog(log),
		Handler: web.AccessLog(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { (*handler.Load()).ServeHTTP(w, r) }))}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
			stop()
		}
	}()

	// Calls end within two minutes (a page waits less); the change stream
	// is long-lived and has no overall timeout.
	client, err := control.Dial(ctx, control.ClientOptions{URL: *controller, HTTP: &http.Client{Transport: tr, Timeout: 2 * time.Minute},
		Stream: &http.Client{Transport: tr}, Token: control.FileToken(*tokenFile), Log: log})
	if err != nil {
		return err
	}
	// A controller set up otherwise than when the web started (sign-in
	// turned on, say) makes the web start again rather than serve under
	// the old setup.
	changed := make(chan struct{})
	client.OnSetupChange(func() { close(changed) })
	cfg := client.Config()
	if cfg.Version != versionString() {
		log.Warn("the web and the controller are of different versions", "web", versionString(), "controller", cfg.Version)
	}
	go client.Run(ctx)
	opt := web.Options{Release: cfg.Release, Version: versionString(), InstallToken: cfg.InstallToken, PublicURL: publicURL(log)}
	if *devAssets != "" {
		opt.Assets = os.DirFS(*devAssets)
	}
	if cfg.GitLab {
		opt.GitLab = client
	}
	if cfg.SignIn {
		opt.Auth = client
	}
	pages, err := web.New(client, log, opt)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	// Liveness and readiness: the web runs and has the controller's setup;
	// the controller being away makes pages answer 502, not the web unready.
	ok := func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }
	mux.HandleFunc("GET /healthz", ok)
	mux.HandleFunc("GET /livez", ok)
	mux.Handle("GET /api/", api.Handler(client))
	pages.Register(mux)
	ready := web.Secure(pages.Gate(mux))
	handler.Store(&ready)
	log.Info("ready", "listen", ln.Addr().String(), "controller_version", cfg.Version, "sign_in", cfg.SignIn, "gitlab", cfg.GitLab)
	var err2 error
	select {
	case <-ctx.Done():
	case <-changed:
		err2 = errors.New("the controller's setup changed: starting again")
	}
	log.Info("stopping")
	begin := time.Now()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	log.Info("stopped", "dur_ms", time.Since(begin).Milliseconds())
	return err2
}
