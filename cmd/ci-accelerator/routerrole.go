package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/route"
	"github.com/hoobiboobi/ci-accelerator/internal/router"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
)

// routerMain proxies the CI jobs' connections to the build daemon where
// their project's cache is.
func routerMain(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("router", flag.ExitOnError)
	showVersion := fs.Bool("version", false, "print the version and exit")
	listen := fs.String("listen", ":1234", "address the CI jobs connect to")
	health := fs.String("health", ":8080", "address of /livez and /readyz")
	node := fs.String("node", os.Getenv("CI_ACCELERATOR_NODE_NAME"), "the node this router runs on (default $CI_ACCELERATOR_NODE_NAME)")
	namespace := fs.String("namespace", os.Getenv("POD_NAMESPACE"), "namespace of the daemons (default $POD_NAMESPACE)")
	service := fs.String("daemons-service", "", "the daemons' headless Service; its EndpointSlices list the daemons")
	port := fs.String("daemon-port", "1234", "the daemons' port")
	jobs := fs.String("jobs-namespace", "", "namespace of the CI job pods")
	var gitlabURLs list
	fs.Var(&gitlabURLs, "gitlab-url", "URL of a GitLab instance, exactly as the runner has it (repeatable; also $GITLAB_URLS)")
	controller := fs.String("controller", "", "the controller's API, https://host:port; empty: no warmth, projects go by hash")
	caFile := fs.String("controller-ca", "/api-tls/ca.crt", "CA certificate the controller's certificate chains to")
	tokenFile := fs.String("token-file", "/var/run/secrets/ci-accelerator/token", "file with the token sent to the controller")
	crossZone := fs.Bool("cross-zone", true, "send a job to another zone's daemon when its zone has none ready")
	overload := fs.Int("overload", 0, "skip a daemon running this many builds when another is free (0: never)")
	warmFor := fs.Duration("warm-for", 7*24*time.Hour, "how long a build keeps its project warm on a node")
	drain := fs.Duration("drain", 300*time.Second, "how long open connections may finish after SIGTERM")
	fs.Parse(args)
	if *showVersion {
		fmt.Println("ci-accelerator " + versionString())
		return nil
	}
	urls := append(gitlabURLs, split(os.Getenv("GITLAB_URLS"))...)
	switch {
	case *node == "":
		return errors.New("give -node (or $CI_ACCELERATOR_NODE_NAME)")
	case *namespace == "" || *service == "":
		return errors.New("give -namespace and -daemons-service")
	case *jobs == "":
		return errors.New("give -jobs-namespace")
	case len(urls) == 0:
		return errors.New("give at least one -gitlab-url (or $GITLAB_URLS)")
	}
	kube, err := kubeClient(config{})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	view := router.NewCluster(urls)
	watchErr := make(chan error, 1)
	wctx, cancelWatch := context.WithCancel(context.Background())
	defer cancelWatch()
	go func() { watchErr <- view.Watch(wctx, kube, *namespace, *service, *jobs) }()

	r := &router.Router{View: view, Node: *node, Port: *port, Log: log,
		Policy: route.Policy{CrossZone: *crossZone, Overload: *overload, WarmFor: *warmFor}}
	var rep *router.Reporter
	if *controller != "" {
		ca, err := os.ReadFile(*caFile)
		if err != nil {
			return fmt.Errorf("-controller-ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(ca) {
			return fmt.Errorf("-controller-ca %s: no certificate", *caFile)
		}
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		rc := &control.RouterClient{URL: *controller, HTTP: &http.Client{Transport: tr, Timeout: 30 * time.Second},
			Token: control.FileToken(*tokenFile)}
		rep = &router.Reporter{Send: rc.Report, Log: log}
		r.Report = func(d store.Decision) {
			view.Decided(d.Project, d.Node, d.At)
			rep.Add(d)
		}
		// Decisions keep being reported until the drain is over.
		rctx, cancelReport := context.WithCancel(context.Background())
		defer cancelReport()
		go rep.Run(rctx)
		go router.FollowWarm(ctx, rc, view, 5*time.Second, log)
	} else {
		r.Report = func(d store.Decision) { view.Decided(d.Project, d.Node, d.At) }
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		// Not ready while draining (no new jobs) or without a ready daemon.
		if ctx.Err() != nil || !view.Ready() {
			http.Error(w, "no ready build daemon known", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "ok")
	})
	hs := &http.Server{Addr: *health, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health", "err", err)
			stop()
		}
	}()
	ln, err := router.Listen(ctx, *listen)
	if err != nil {
		return err
	}
	log.Info("router", "listen", *listen, "node", *node, "daemons", *namespace+"/"+*service, "jobs", *jobs,
		"controller", *controller, "version", versionString())
	go func() {
		if err := <-watchErr; err != nil && ctx.Err() == nil {
			log.Error("watching the daemons and jobs", "err", err)
			stop()
		}
	}()
	err = r.Serve(ctx, ln, *drain)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs.Shutdown(sctx)
	var ne *net.OpError
	if errors.As(err, &ne) {
		return fmt.Errorf("listener: %w", err)
	}
	return err
}
