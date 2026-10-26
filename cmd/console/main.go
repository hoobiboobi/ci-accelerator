// Command console is the ci-accelerator console: it collects the build
// daemons' history and the CI jobs' pods into its store, and serves its
// pages and a read-only JSON API.
package main

import (
	"context"
	cryptotls "crypto/tls"
	"encoding/json"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/hoobiboobi/ci-accelerator/internal/api"
	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/podwatch"
	"github.com/hoobiboobi/ci-accelerator/internal/runnercache"
	"github.com/hoobiboobi/ci-accelerator/internal/store"
	"github.com/hoobiboobi/ci-accelerator/internal/web"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

type config struct {
	data, discover, tlsDir, serverName, listen string
	jobsNamespace, kubeconfig, kubeContext     string
	gitlabTokenFile, release, signIn           string
	caches, namespace                          string
	devAssets                                  string
	cacheEvery                                 time.Duration
	daemons, gitlabURLs                        []string
	keepDays, keepOutputDays                   int
	diskProbePort                              int

	// The controller role (split): its API for the web.
	split                     bool
	apiListen, apiTLSDir      string
	apiTokenFile, apiAudience string
	apiAllow                  []string
}

// version and commit are set at build time (-ldflags -X main.version=...);
// "dev" is a build from a working tree.
var (
	version = "dev"
	commit  = ""
)

// versionString is what the pages and -version show: "v0.2.0 (0921bbc)".
func versionString() string {
	if commit != "" {
		return version + " (" + commit + ")"
	}
	return version
}

// The console is one binary with roles:
//
//	console [flags]             controller and pages in one process
//	console controller [flags]  the store, collector and sessions, and the API the web reads
//	console web [flags]         the pages, reading the controller
//	console disk-probe [flags]  the sidecar next to a daemon that reports its cache's free space
func main() {
	role, args := "", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		role, args = args[0], args[1:]
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	var err error
	switch role {
	case "disk-probe":
		err = diskProbe(args)
	case "", "console":
		err = controllerMain(args, false, log)
	case "controller":
		err = controllerMain(args, true, log)
	case "web":
		err = webMain(args, log)
	default:
		fmt.Fprintf(os.Stderr, "console: no role %q (console, controller, web, disk-probe)\n", role)
		os.Exit(2)
	}
	if err != nil {
		log.Error("console", "role", role, "err", err)
		os.Exit(1)
	}
}

func diskProbe(args []string) error {
	fs := flag.NewFlagSet("disk-probe", flag.ExitOnError)
	dir := fs.String("dir", "/var/lib/buildkit", "directory whose filesystem is reported")
	listen := fs.String("listen", ":9180", "address of GET /disk and /healthz")
	fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("disk probe", "dir", *dir, "listen", *listen, "version", versionString())
	return diskprobe.Serve(ctx, *listen, *dir)
}

func controllerMain(args []string, splitRole bool, log *slog.Logger) error {
	var c config
	var daemons, gitlabURLs, allow list
	fs := flag.NewFlagSet("console", flag.ExitOnError)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.StringVar(&c.data, "data", "/data", "directory of the store")
	fs.StringVar(&c.discover, "discover", "", "headless Service host:port; every address it resolves to is a daemon")
	fs.Var(&daemons, "daemon", "a daemon's address, tcp://host:port (repeatable; instead of -discover)")
	fs.StringVar(&c.tlsDir, "tls-dir", "/certs", "directory with ca.crt, tls.crt and tls.key (the client certificate)")
	fs.StringVar(&c.serverName, "server-name", "", "name in the daemons' certificate (default: the host of -discover)")
	fs.Var(&gitlabURLs, "gitlab-url", "URL of a GitLab instance, exactly as the runner has it (repeatable; also $GITLAB_URLS, comma-separated)")
	fs.StringVar(&c.jobsNamespace, "jobs-namespace", "", "namespace of the CI job pods; empty: jobs are not watched")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig outside a cluster (default: the pod's service account)")
	fs.StringVar(&c.kubeContext, "kube-context", "", "context of -kubeconfig")
	fs.StringVar(&c.gitlabTokenFile, "gitlab-token-file", "", "file with a GitLab token that can read the projects' jobs; empty: no job times from GitLab")
	fs.StringVar(&c.release, "release", "", "name of the installation, shown on the nodes page")
	fs.StringVar(&c.caches, "caches", "", "JSON file listing the S3 folders the runners keep GitLab's cache: in (sizes per project on the nodes page); empty: none")
	fs.StringVar(&c.namespace, "namespace", os.Getenv("POD_NAMESPACE"), "namespace of the Secrets with the caches' keys (default $POD_NAMESPACE)")
	fs.DurationVar(&c.cacheEvery, "cache-every", 10*time.Minute, "how often the caches are listed")
	fs.StringVar(&c.signIn, "sign-in", "", "JSON file configuring sign-in through GitLab; empty: no sign-in (keep the console inside the cluster)")
	fs.IntVar(&c.diskProbePort, "disk-probe-port", 0, "port of the disk probe next to each daemon (the buildkit chart's diskProbe); 0: none")
	fs.IntVar(&c.keepDays, "keep-days", 30, "days builds are kept")
	fs.IntVar(&c.keepOutputDays, "keep-output-days", 7, "days step output is kept")
	if splitRole {
		fs.StringVar(&c.apiListen, "api-listen", ":8443", "address of the API the web reads (HTTPS)")
		fs.StringVar(&c.apiTLSDir, "api-tls-dir", "/api-tls", "directory with tls.crt and tls.key of the API (read again when they change)")
		fs.StringVar(&c.apiAudience, "api-audience", control.Audience, "audience of the callers' ServiceAccount tokens")
		fs.Var(&allow, "api-allow", "a ServiceAccount that may call the API, namespace/name (repeatable)")
		fs.StringVar(&c.apiTokenFile, "api-token-file", "", "file with a static token the callers send instead of ServiceAccount tokens (outside a cluster)")
	} else {
		fs.StringVar(&c.listen, "listen", ":8080", "address of the pages, the API and the health check")
		fs.StringVar(&c.devAssets, "dev-assets", "", "directory with templates/ and static/ (internal/web) read for every page instead of the built-in ones; for working on the pages")
	}
	fs.Parse(args)
	if *showVersion {
		fmt.Println("ci-accelerator console " + versionString())
		return nil
	}
	c.daemons = daemons
	c.gitlabURLs = append(gitlabURLs, split(os.Getenv("GITLAB_URLS"))...)
	c.split, c.apiAllow = splitRole, allow
	return run(c, log)
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
	if c.caches != "" && c.cacheEvery < 10*time.Second {
		// Listing a bucket is not free: 0s would list it without pause.
		return errors.New("-cache-every must be at least 10s")
	}
	if c.split && c.apiTokenFile == "" && len(c.apiAllow) == 0 {
		return errors.New("the controller's API needs callers: -api-allow (ServiceAccounts) or -api-token-file")
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
			d, err := collector.Dial(dctx, addr, tls)
			if err == nil && c.diskProbePort > 0 {
				d.DiskProbe(c.diskProbePort)
			}
			return d, err
		},
		Resolve: func(ctx context.Context) ([]string, error) { return resolve(ctx, c) },
	}
	col.OnLiveChange(func() { st.Announce("") })
	var kube kubernetes.Interface
	if c.jobsNamespace != "" || c.caches != "" || (c.split && c.apiTokenFile == "") {
		if kube, err = kubeClient(c); err != nil {
			return err
		}
	}
	if c.jobsNamespace != "" {
		watchJobs(ctx, c, kube, st, col, log)
	}
	if c.caches != "" {
		if _, err := loadCaches(c.caches); err != nil {
			return err
		}
		if c.namespace == "" {
			return errors.New("-caches needs -namespace (or $POD_NAMESPACE): where the Secrets with the keys are")
		}
		go scanCaches(ctx, c, kube, st, log)
	}
	var signIn *auth.Auth
	var gl *control.LocalGitLab
	if c.signIn != "" {
		if c.gitlabTokenFile != "" {
			return errors.New("-gitlab-token-file and -sign-in exclude each other: with sign-in GitLab is read with each person's own token")
		}
		client := gitlab.New(c.gitlabURLs)
		if signIn, err = loadSignIn(c.signIn, c.gitlabURLs, st, client, log); err != nil {
			return err
		}
		gl = &control.LocalGitLab{Fetcher: &gitlab.Fetcher{Client: client, Store: st, Log: log}, Auth: signIn}
		for _, ch := range signIn.Instances() {
			log.Info("sign-in through GitLab", "gitlab", ch.Host, "redirect_uri", signIn.RedirectURL(ch.Instance))
		}
	}
	if c.gitlabTokenFile != "" {
		token, err := os.ReadFile(c.gitlabTokenFile)
		if err != nil {
			return err
		}
		gl = &control.LocalGitLab{Fetcher: &gitlab.Fetcher{Client: gitlab.New(c.gitlabURLs), Store: st, Log: log},
			Token: gitlab.Token(strings.TrimSpace(string(token)))}
	}
	local := &control.Local{Store: st, Live: col}
	var srv *http.Server
	if c.split {
		authorize, err := apiAuthorizer(c, kube)
		if err != nil {
			return err
		}
		certs, err := control.ReloadingCert(filepath.Join(c.apiTLSDir, "tls.crt"), filepath.Join(c.apiTLSDir, "tls.key"))
		if err != nil {
			return err
		}
		api := &control.Server{Local: local, GitLab: gl, Auth: signIn, Log: log, Authorize: authorize,
			Config: control.ConfigFor(versionString(), c.release, gl, signIn)}
		srv = &http.Server{Addr: c.apiListen, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second,
			TLSConfig: &cryptotls.Config{MinVersion: cryptotls.VersionTLS12, GetCertificate: certs}}
		srv.RegisterOnShutdown(api.Stop)
		go func() {
			if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("controller API", "err", err)
				stop()
			}
		}()
		log.Info("controller", "api", c.apiListen, "version", versionString())
	} else {
		opt := web.Options{Release: c.release, Version: versionString()}
		if c.devAssets != "" {
			opt.Assets = os.DirFS(c.devAssets)
		}
		if gl != nil {
			opt.GitLab, opt.InstallToken = gl, gl.Token != nil
		}
		if signIn != nil {
			opt.Auth = signIn
		}
		pages, err := web.New(local, log, opt)
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		apiRoutes := api.Handler(st)
		mux.Handle("GET /healthz", apiRoutes)
		mux.Handle("GET /api/", apiRoutes)
		pages.Register(mux)
		srv = &http.Server{Addr: c.listen, Handler: web.Secure(pages.Gate(mux)), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http", "err", err)
				stop()
			}
		}()
	}
	go prune(ctx, st, signIn, log)
	sup.Run(ctx)
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}

// apiAuthorizer says who may call the controller: a static token from a
// file, or the ServiceAccounts named, checked with Kubernetes.
func apiAuthorizer(c config, kube kubernetes.Interface) (func(*http.Request) error, error) {
	if c.apiTokenFile != "" {
		b, err := os.ReadFile(c.apiTokenFile)
		if err != nil {
			return nil, err
		}
		t := strings.TrimSpace(string(b))
		if len(t) < 16 {
			return nil, errors.New("-api-token-file: the token must be at least 16 characters")
		}
		return control.StaticToken(t), nil
	}
	allow := map[string]bool{}
	for _, a := range c.apiAllow {
		ns, name, ok := strings.Cut(a, "/")
		if !ok || ns == "" || name == "" {
			return nil, fmt.Errorf("-api-allow %q: give namespace/name", a)
		}
		allow[a] = true
	}
	r := &control.TokenReviewer{Kube: kube, Audience: c.apiAudience, Allow: allow}
	return r.Authorize, nil
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

// kubeClient connects to the cluster: in a pod, with the service account;
// outside, with $KUBECONFIG or ~/.kube/config, or the file given.
func kubeClient(c config) (kubernetes.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = c.kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules,
		&clientcmd.ConfigOverrides{CurrentContext: c.kubeContext}).ClientConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// watchJobs reads the CI jobs from their pods into the store.
func watchJobs(ctx context.Context, c config, client kubernetes.Interface, st *store.Store, col *collector.Collector, log *slog.Logger) {
	go func() {
		err := podwatch.Watch(ctx, client, c.jobsNamespace, c.gitlabURLs, podwatch.Handlers{
			Job: func(ctx context.Context, j store.Job) {
				if err := st.SaveJob(ctx, j); err != nil {
					log.Warn("job not stored", "job", j.JobID, "err", err)
					return
				}
				if err := col.JobKnown(ctx, j.Instance, j.JobID); err != nil {
					log.Warn("job's builds not compared", "job", j.JobID, "err", err)
				}
			},
			Gone: func(ctx context.Context, j store.Job, at time.Time) {
				if err := st.JobEnded(ctx, j.Instance, j.JobID, at); err != nil {
					log.Warn("job end not stored", "job", j.JobID, "err", err)
				}
			},
		})
		if err != nil {
			log.Error("watching job pods", "err", err)
		}
	}()
}

// loadCaches reads the list of cache stores (the chart writes it); the
// same folder listed twice is listed once.
func loadCaches(path string) ([]runnercache.Store, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []runnercache.Store
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	var out []runnercache.Store
	for _, s := range list {
		if s.Server == "" || s.Bucket == "" || s.Secret == "" {
			return nil, fmt.Errorf("%s: a cache store needs server, bucket and secret", path)
		}
		if !seen[s.ID()] {
			seen[s.ID()] = true
			out = append(out, s)
		}
	}
	return out, nil
}

// scanCaches lists every store now and then. The list of stores and their
// keys are read each time: a mounted ConfigMap follows its changes, and a
// rotated key is used without a restart.
func scanCaches(ctx context.Context, c config, kube kubernetes.Interface, st *store.Store, log *slog.Logger) {
	for {
		stores, err := loadCaches(c.caches)
		if err != nil {
			log.Warn("cache stores not read", "err", err)
		} else {
			ids := make([]string, len(stores))
			for i, s := range stores {
				ids[i] = s.ID()
			}
			if err := st.KeepCacheStores(ctx, ids); err != nil {
				log.Warn("old cache stores not forgotten", "err", err)
			}
		}
		for _, s := range stores {
			sctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			scan, err := scanCache(sctx, s, c.namespace, kube)
			cancel()
			if ctx.Err() != nil {
				return
			}
			objs := make([]store.CacheObject, len(scan.Objects))
			for i, o := range scan.Objects {
				objs[i] = store.CacheObject{ProjectID: o.ProjectID, Key: o.Key, Bytes: o.Bytes, Modified: o.Modified}
			}
			if err != nil {
				log.Warn("cache not listed", "store", s.ID(), "err", err)
			} else {
				var bytes int64
				for _, o := range objs {
					bytes += o.Bytes
				}
				log.Info("cache listed", "store", s.ID(), "objects", len(objs), "bytes", bytes)
			}
			if err := st.ReplaceCacheScan(ctx, s.ID(), s.Instance, time.Now(), objs, scan.Lifecycle, err); err != nil {
				log.Warn("cache listing not stored", "store", s.ID(), "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cacheEvery):
		}
	}
}

func scanCache(ctx context.Context, s runnercache.Store, ns string, kube kubernetes.Interface) (runnercache.Scan, error) {
	sec, err := kube.CoreV1().Secrets(ns).Get(ctx, s.Secret, metav1.GetOptions{})
	if err != nil {
		return runnercache.Scan{}, fmt.Errorf("the keys (Secret %s): %w", s.Secret, err)
	}
	ak, sk := strings.TrimSpace(string(sec.Data["accesskey"])), strings.TrimSpace(string(sec.Data["secretkey"]))
	if ak == "" || sk == "" {
		return runnercache.Scan{}, fmt.Errorf("Secret %s has no accesskey or secretkey", s.Secret)
	}
	keys := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk, Source: "secret " + s.Secret}, nil
	})
	return runnercache.Lister{Store: s, Keys: keys}.Scan(ctx, time.Now())
}

// prune applies retention once an hour, and ends idle sessions.
func prune(ctx context.Context, st *store.Store, signIn *auth.Auth, log *slog.Logger) {
	for {
		if n, err := st.Prune(ctx); err != nil {
			log.Warn("retention", "err", err)
		} else if n > 0 {
			log.Info("retention", "builds_deleted", n)
		}
		if signIn != nil {
			if n, err := signIn.Prune(ctx); err != nil {
				log.Warn("sessions", "err", err)
			} else if n > 0 {
				log.Info("sessions", "idle_ended", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Hour):
		}
	}
}

// signInFile is the sign-in configuration the chart writes. Secrets are
// named by file (mounted Secrets), never written into it. A GitLab without
// a URL is the console's only GitLab (the runner's, from GITLAB_URLS).
type signInFile struct {
	URL            string `json:"url"`
	SessionKeyFile string `json:"sessionKeyFile"`
	GitLab         []struct {
		URL          string   `json:"url"`
		ClientIDFile string   `json:"clientIdFile"`
		SecretFile   string   `json:"secretFile"`
		Admins       []string `json:"admins"`
	} `json:"gitlab"`
}

func loadSignIn(path string, urls []string, st *store.Store, client *gitlab.Client, log *slog.Logger) (*auth.Auth, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f signInFile
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	read := func(name string) (string, error) {
		b, err := os.ReadFile(name)
		return strings.TrimSpace(string(b)), err
	}
	keyText, err := os.ReadFile(f.SessionKeyFile)
	if err != nil {
		return nil, fmt.Errorf("session key: %w", err)
	}
	key, err := auth.ParseKey(keyText)
	if err != nil {
		return nil, err
	}
	cfg := auth.Config{PublicURL: f.URL, Key: key}
	for _, g := range f.GitLab {
		if g.URL == "" {
			if len(urls) != 1 || len(f.GitLab) != 1 {
				return nil, errors.New("sign-in: name each GitLab's url: the console serves several")
			}
			g.URL = urls[0]
		}
		id, err := read(g.ClientIDFile)
		if err != nil {
			return nil, fmt.Errorf("%s: OAuth application id: %w", g.URL, err)
		}
		secret, err := read(g.SecretFile)
		if err != nil {
			return nil, fmt.Errorf("%s: OAuth application secret: %w", g.URL, err)
		}
		cfg.Instances = append(cfg.Instances, auth.Instance{URL: g.URL, ClientID: id, Secret: secret, Admins: g.Admins})
	}
	return auth.New(cfg, st, client, log)
}
