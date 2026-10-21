// Command console is the ci-accelerator console: it collects the build
// daemons' history and the CI jobs' pods into its store, and serves its
// pages and a read-only JSON API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/hoobiboobi/ci-accelerator/internal/api"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/podwatch"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
	"github.com/hoobiboobi/ci-accelerator/internal/web"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

type config struct {
	data, discover, tlsDir, serverName, listen string
	jobsNamespace, kubeconfig, kubeContext     string
	gitlabTokenFile, release                   string
	daemons, gitlabURLs                        []string
	keepDays, keepOutputDays                   int
}

func main() {
	var c config
	var daemons, gitlabURLs list
	flag.StringVar(&c.data, "data", "/data", "directory of the store")
	flag.StringVar(&c.discover, "discover", "", "headless Service host:port; every address it resolves to is a daemon")
	flag.Var(&daemons, "daemon", "a daemon's address, tcp://host:port (repeatable; instead of -discover)")
	flag.StringVar(&c.tlsDir, "tls-dir", "/certs", "directory with ca.crt, tls.crt and tls.key (the client certificate)")
	flag.StringVar(&c.serverName, "server-name", "", "name in the daemons' certificate (default: the host of -discover)")
	flag.Var(&gitlabURLs, "gitlab-url", "URL of a GitLab instance, exactly as the runner has it (repeatable; also $GITLAB_URLS, comma-separated)")
	flag.StringVar(&c.jobsNamespace, "jobs-namespace", "", "namespace of the CI job pods; empty: jobs are not watched")
	flag.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig outside a cluster (default: the pod's service account)")
	flag.StringVar(&c.kubeContext, "kube-context", "", "context of -kubeconfig")
	flag.StringVar(&c.listen, "listen", ":8080", "address of the pages, the API and the health check")
	flag.StringVar(&c.gitlabTokenFile, "gitlab-token-file", "", "file with a GitLab token that can read the projects' jobs; empty: no job times from GitLab")
	flag.StringVar(&c.release, "release", "", "name of the installation, shown on the nodes page")
	flag.IntVar(&c.keepDays, "keep-days", 90, "days builds are kept")
	flag.IntVar(&c.keepOutputDays, "keep-output-days", 7, "days step output is kept")
	flag.Parse()
	c.daemons = daemons
	c.gitlabURLs = append(gitlabURLs, split(os.Getenv("GITLAB_URLS"))...)
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(c, log); err != nil {
		log.Error("console", "err", err)
		os.Exit(1)
	}
}

func split(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		out = append(out, p)
	}
	return out
}

func run(c config, log *slog.Logger) error {
	if (c.discover == "") == (len(c.daemons) == 0) {
		return errors.New("give either -discover or -daemon")
	}
	if len(c.gitlabURLs) == 0 {
		return errors.New("give at least one -gitlab-url (or $GITLAB_URLS)")
	}
	if c.keepDays < 1 || c.keepOutputDays < 1 || c.keepOutputDays > c.keepDays {
		return errors.New("-keep-days and -keep-output-days must be at least 1, output no longer than builds")
	}
	if c.serverName == "" {
		host, _, err := net.SplitHostPort(c.discover)
		if err != nil {
			return fmt.Errorf("-server-name is needed with -daemon: %w", err)
		}
		c.serverName = host
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, c.data, store.Options{Keep: time.Duration(c.keepDays) * 24 * time.Hour, KeepOutput: c.keepOutputDays})
	if err != nil {
		return err
	}
	defer st.Close()
	instances := map[string]bool{}
	for _, u := range c.gitlabURLs {
		instances[history.InstancePrefix(u)] = true
	}
	col := collector.New(st, instances, log)
	tls := collector.TLS{CA: filepath.Join(c.tlsDir, "ca.crt"), Cert: filepath.Join(c.tlsDir, "tls.crt"), Key: filepath.Join(c.tlsDir, "tls.key"), ServerName: c.serverName}
	sup := &collector.Supervisor{
		Collector: col,
		Log:       log,
		Dial: func(ctx context.Context, addr string) (collector.Source, error) {
			dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return collector.Dial(dctx, addr, tls)
		},
		Resolve: func(ctx context.Context) ([]string, error) { return resolve(ctx, c) },
	}
	col.OnLiveChange(func() { st.Announce("") })
	running := &podwatch.Running{}
	if c.jobsNamespace != "" {
		if err := watchJobs(ctx, c, st, col, running, log); err != nil {
			return err
		}
	}
	opt := web.Options{Live: col, Running: running, Release: c.release}
	if c.gitlabTokenFile != "" {
		token, err := os.ReadFile(c.gitlabTokenFile)
		if err != nil {
			return err
		}
		opt.API = &gitlab.Fetcher{Client: gitlab.New(c.gitlabURLs, strings.TrimSpace(string(token))), Store: st, Log: log}
	}
	pages, err := web.New(st, log, opt)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	apiRoutes := api.Handler(st)
	mux.Handle("GET /healthz", apiRoutes)
	mux.Handle("GET /api/", apiRoutes)
	pages.Register(mux)
	srv := &http.Server{Addr: c.listen, Handler: web.Secure(mux), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http", "err", err)
			stop()
		}
	}()
	go prune(ctx, st, log)
	sup.Run(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

// resolve returns the daemons' addresses: given, or every address the
// headless Service resolves to.
func resolve(ctx context.Context, c config) ([]string, error) {
	if c.discover == "" {
		return c.daemons, nil
	}
	host, port, err := net.SplitHostPort(c.discover)
	if err != nil {
		return nil, err
	}
	ips, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil {
		return nil, err
	}
	sort.Strings(ips)
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = "tcp://" + net.JoinHostPort(ip, port)
	}
	return out, nil
}

// watchJobs reads the CI jobs from their pods into the store.
func watchJobs(ctx context.Context, c config, st *store.Store, col *collector.Collector, running *podwatch.Running, log *slog.Logger) error {
	// In a pod: the service account. Outside: $KUBECONFIG or
	// ~/.kube/config, or the file given.
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = c.kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: c.kubeContext}).ClientConfig()
	if err != nil {
		return err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	go func() {
		err := podwatch.Watch(ctx, client, c.jobsNamespace, c.gitlabURLs, podwatch.Handlers{
			Job: func(ctx context.Context, j store.Job) {
				running.Set(j)
				if err := st.SaveJob(ctx, j); err != nil {
					log.Warn("job not stored", "job", j.JobID, "err", err)
					return
				}
				if err := col.JobKnown(ctx, j.Instance, j.JobID); err != nil {
					log.Warn("job's builds not compared", "job", j.JobID, "err", err)
				}
			},
			Gone: func(ctx context.Context, j store.Job) {
				running.Remove(j)
				if err := st.JobEnded(ctx, j.Instance, j.JobID, time.Now()); err != nil {
					log.Warn("job end not stored", "job", j.JobID, "err", err)
				}
				// The job is no longer running, whether or not the store changed.
				st.Announce(store.PipelineKey(j.Instance, j.PipelineID))
			},
		})
		if err != nil {
			log.Error("watching job pods", "err", err)
		}
	}()
	return nil
}

// prune applies retention once an hour.
func prune(ctx context.Context, st *store.Store, log *slog.Logger) {
	for {
		if n, err := st.Prune(ctx); err != nil {
			log.Warn("retention", "err", err)
		} else if n > 0 {
			log.Info("retention", "builds_deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}
