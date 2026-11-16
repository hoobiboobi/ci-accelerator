// Command ci-accelerator is the control plane of ci-accelerator: the
// controller collects the build daemons' history and the CI jobs' pods into
// its store, and the web serves the console (its pages and a read-only JSON
// API) from it.
package main

import (
	"context"
	cryptotls "crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"github.com/hoobiboobi/ci-accelerator/internal/api"
	"github.com/hoobiboobi/ci-accelerator/internal/auth"
	"github.com/hoobiboobi/ci-accelerator/internal/collector"
	"github.com/hoobiboobi/ci-accelerator/internal/control"
	"github.com/hoobiboobi/ci-accelerator/internal/diskprobe"
	"github.com/hoobiboobi/ci-accelerator/internal/fleet"
	"github.com/hoobiboobi/ci-accelerator/internal/gitlab"
	"github.com/hoobiboobi/ci-accelerator/internal/history"
	"github.com/hoobiboobi/ci-accelerator/internal/logging"
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
	// discoverAll is a headless Service that lists every daemon pod,
	// ready or not: a controller that starts while a daemon drains finds
	// it there.
	discoverAll                            string
	jobsNamespace, kubeconfig, kubeContext string
	gitlabTokenFile, release, signIn       string
	logLevel                               string
	// A member of a fleet: the hub's console address, this box's name
	// there, its key and the hub's public keys (files), and the CA of the
	// hub's address ("" for the system's).
	fleetHub, fleetName, fleetKey, fleetHubKeys, fleetHubCA string
	// A member joining with a join token: the token's file, and the
	// Secret of its namespace the member keeps the key it makes in.
	fleetJoin, fleetKeySecret     string
	caches, namespace             string
	devAssets                     string
	gitlabURLsFile, gitlabURLsWas string
	// gitlabURLsFixed are the URLs of the flags and $GITLAB_URLS, which a
	// changed -gitlab-urls-file does not change.
	gitlabURLsFixed          []string
	cacheEvery               time.Duration
	daemons, gitlabURLs      []string
	keepDays, keepOutputDays int
	// maxStoreMiB bounds the store's database (0: no bound).
	maxStoreMiB int64

	// The controller role (split): its API for the web.
	split                     bool
	apiListen, apiTLSDir      string
	apiTokenFile, apiAudience string
	apiAllow, apiAllowRouter  []string
	warmFor                   time.Duration
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

// publicURL is where browsers reach the console, from the environment
// (the chart sets it from global.console.url), for the pages' link
// previews; "" when unset or not an http(s) URL.
func publicURL(log *slog.Logger) string {
	u := os.Getenv(web.PublicURLEnv)
	if u == "" {
		return ""
	}
	o := web.Origin(u)
	if o == "" {
		log.Warn("not an http(s) URL with a host; link previews use the request's host", "env", web.PublicURLEnv, "value", u)
	}
	return o
}

// One binary with roles:
//
//	ci-accelerator [all] [flags]        controller and web in one process (development)
//	ci-accelerator controller [flags]   the store, collector and sessions, and the API the web reads
//	ci-accelerator web [flags]          the console's pages, reading the controller
//	ci-accelerator router [flags]       the proxy that sends each CI job to the daemon where its project is warm
//	ci-accelerator disk-probe [flags]   the sidecar next to a daemon that reports its cache's free space
//	ci-accelerator hub [flags]          a fleet hub: one console for the boxes that join it
//	ci-accelerator fleet-keygen <file>  a key pair for a fleet member or hub (<file> and <file>.pub)
func main() {
	role, args := "", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		role, args = args[0], args[1:]
	}
	// Until a role has read its flags, and for the error it ends with.
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: logging.Redact}))
	var err error
	switch role {
	case "disk-probe":
		err = diskProbe(args)
	case "", "all":
		err = controllerMain(args, false)
	case "controller":
		err = controllerMain(args, true)
	case "web":
		err = webMain(args)
	case "router":
		err = routerMain(args)
	case "hub":
		err = hubMain(args)
	case "fleet-keygen":
		err = fleetKeygen(args)
	default:
		fmt.Fprintf(os.Stderr, "ci-accelerator: no role %q\n%s", role, rolesUsage)
		os.Exit(2)
	}
	if err != nil {
		log.Error("ci-accelerator", "role", role, "err", err)
		os.Exit(1)
	}
}

// rolesUsage is the binary's help: its roles (main's comment).
const rolesUsage = `Usage: ci-accelerator [role] [flags]  (ci-accelerator <role> -h: the role's flags)
  all           controller and web in one process (the default; development)
  controller    the store, collector and sessions, and the API the web reads
  web           the console's pages, reading the controller
  router        the proxy that sends each CI job to the daemon where its project is warm
  disk-probe    the sidecar next to a daemon that reports its cache's free space
  hub           a fleet hub: one console for the boxes that join it (ci-accelerator hub help: its commands)
  fleet-keygen  <file>: a key pair for a fleet member or hub (<file> and <file>.pub)
`

// logOut is where the roles log (the tests read it).
var logOut io.Writer = os.Stderr

// roleLog is a role's logger from its flags; client-go's own log (klog)
// goes to it too, so every line of a pod is one JSON object.
func roleLog(f logging.Flags, role string) (*slog.Logger, error) {
	log, err := logging.New(logOut, f, role)
	if err != nil {
		return nil, err
	}
	klog.SetSlogLogger(log.With("source", "client-go"))
	return log, nil
}

// started logs a role's start: its version and the settings it runs with
// (never a secret: files of secrets by whether they are given).
func started(log *slog.Logger, cfg ...any) {
	log.Info("starting", "version", versionString(), "go", runtime.Version(), slog.Group("config", cfg...))
}

func diskProbe(args []string) error {
	fs := flag.NewFlagSet("disk-probe", flag.ExitOnError)
	dir := fs.String("dir", "/var/lib/buildkit", "directory whose filesystem is reported")
	listen := fs.String("listen", ":9180", "address of GET /disk, /load and /healthz")
	// Environment, not flags, for what the chart passes since 0.3.3: a
	// probe image of an earlier release (pinned or mirrored) ignores an
	// unknown variable, where an unknown flag would stop it.
	cgroup := fs.String("cgroup", os.Getenv("CI_ACCELERATOR_CGROUP_ROOT"), "the host's cgroup filesystem, mounted read-only (default $CI_ACCELERATOR_CGROUP_ROOT); with it, /load reports the node's and the pod's CPU and memory (the pod from $CI_ACCELERATOR_POD_UID's cgroup)")
	var lf logging.Flags
	lf.Register(fs)
	fs.Parse(args)
	log, err := roleLog(lf, "disk-probe")
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var sampler *diskprobe.Sampler
	podCgroup := ""
	if *cgroup != "" {
		podCgroup = diskprobe.FindPodCgroup(*cgroup, os.Getenv("CI_ACCELERATOR_POD_UID"))
		if podCgroup == "" {
			log.Warn("the pod's cgroup v2 directory not found: /load reports the node only", "cgroup", *cgroup, "pod_uid", os.Getenv("CI_ACCELERATOR_POD_UID"))
		}
		sampler = diskprobe.NewSampler(diskprobe.Reader{Proc: "/proc", Cgroup: podCgroup})
	}
	started(log, "dir", *dir, "listen", *listen, "cgroup", *cgroup, "pod_cgroup", podCgroup, "log_level", lf.Level)
	return diskprobe.Serve(ctx, *listen, *dir, sampler, log)
}

func controllerMain(args []string, splitRole bool) error {
	var c config
	var daemons, gitlabURLs, allow, allowRouter list
	fs := flag.NewFlagSet("controller", flag.ExitOnError)
	fs.Usage = func() {
		name := "all"
		if splitRole {
			name = "controller"
		}
		fmt.Fprintf(fs.Output(), "%s\nFlags of %s:\n", rolesUsage, name)
		fs.PrintDefaults()
	}
	var lf logging.Flags
	lf.Register(fs)
	showVersion := fs.Bool("version", false, "print the version and exit")
	fs.StringVar(&c.data, "data", "/data", "directory of the store")
	fs.StringVar(&c.discover, "discover", "", "headless Service host:port; every address it resolves to is a daemon")
	// Also from an environment variable: a controller image older than
	// the chart would stop on an unknown flag.
	fs.StringVar(&c.discoverAll, "discover-all", os.Getenv(discoverAllEnv), "headless Service host:port listing every daemon pod, ready or not (stopping daemons too); its addresses are read as well (default $"+discoverAllEnv+")")
	fs.Var(&daemons, "daemon", "a daemon's address, tcp://host:port (repeatable; instead of -discover)")
	fs.StringVar(&c.tlsDir, "tls-dir", "/certs", "directory with ca.crt, tls.crt and tls.key (the client certificate)")
	fs.StringVar(&c.serverName, "server-name", "", "name in the daemons' certificate (default: the host of -discover)")
	fs.Var(&gitlabURLs, "gitlab-url", "URL of a GitLab instance, exactly as the runner has it (repeatable; also $GITLAB_URLS, comma-separated)")
	fs.StringVar(&c.gitlabURLsFile, "gitlab-urls-file", "", "file listing GitLab URLs as $GITLAB_URLS does (a mounted ConfigMap); when it changes the controller ends, to start again with them")
	fs.StringVar(&c.jobsNamespace, "jobs-namespace", "", "namespace of the CI job pods; empty: jobs are not watched")
	fs.StringVar(&c.kubeconfig, "kubeconfig", "", "kubeconfig outside a cluster (default: the pod's service account)")
	fs.StringVar(&c.kubeContext, "kube-context", "", "context of -kubeconfig")
	fs.StringVar(&c.gitlabTokenFile, "gitlab-token-file", "", "file with a GitLab token that can read the projects' jobs; empty: no job times from GitLab")
	fs.StringVar(&c.release, "release", "", "name of the installation, shown on the nodes page")
	fs.StringVar(&c.caches, "caches", "", "JSON file listing the S3 folders the runners keep GitLab's cache: in (sizes per project on the nodes page); empty: none")
	fs.StringVar(&c.namespace, "namespace", os.Getenv("POD_NAMESPACE"), "namespace of the Secrets with the caches' keys (default $POD_NAMESPACE)")
	fs.DurationVar(&c.cacheEvery, "cache-every", 10*time.Minute, "how often the caches are listed")
	fs.StringVar(&c.signIn, "sign-in", "", "JSON file configuring sign-in through GitLab; empty: no sign-in (keep the console inside the cluster)")
	fs.IntVar(&c.keepDays, "keep-days", 30, "days builds are kept")
	fs.IntVar(&c.keepOutputDays, "keep-output-days", 7, "days step output is kept")
	maxStore, _ := strconv.ParseInt(os.Getenv(maxStoreEnv), 10, 64)
	fs.Int64Var(&c.maxStoreMiB, "max-store-mib", maxStore, "MiB the store's database may hold before the oldest builds go early; 0: no bound (default $"+maxStoreEnv+")")
	if splitRole {
		fs.StringVar(&c.apiListen, "api-listen", ":8443", "address of the API the web reads (HTTPS)")
		fs.StringVar(&c.apiTLSDir, "api-tls-dir", "/api-tls", "directory with tls.crt and tls.key of the API (read again when they change)")
		fs.StringVar(&c.apiAudience, "api-audience", control.Audience, "audience of the callers' ServiceAccount tokens")
		fs.Var(&allow, "api-allow", "a ServiceAccount of the web, which may read the pages' data, namespace/name (repeatable)")
		fs.Var(&allowRouter, "api-allow-router", "a ServiceAccount of the build routers, which may read where projects are warm and report decisions, namespace/name (repeatable)")
		fs.DurationVar(&c.warmFor, "warm-for", 7*24*time.Hour, "how long a build keeps its project warm on its node, for the build router")
		fs.StringVar(&c.apiTokenFile, "api-token-file", "", "file with a static token the callers send instead of ServiceAccount tokens (outside a cluster)")
		fs.StringVar(&c.fleetHub, "fleet-hub", "", "a fleet hub's console address (https://...): this box joins it as a member and serves the hub its API over an outgoing connection")
		fs.StringVar(&c.fleetName, "fleet-name", "", "this box's name in the fleet")
		fs.StringVar(&c.fleetKey, "fleet-key-file", "", "this member's private key (PEM, PKCS#8 or SEC1; Ed25519 or ECDSA P-256/P-384)")
		fs.StringVar(&c.fleetHubKeys, "fleet-hub-keys-file", "", "the hub's public keys this member trusts (PEM, one or more)")
		fs.StringVar(&c.fleetHubCA, "fleet-hub-ca-file", "", "CA certificates of the hub's address (its Ingress); empty: the system's")
		fs.StringVar(&c.fleetJoin, "fleet-join-file", "", "a join token issued by the hub: when -fleet-key-file holds no key, the member makes one, keeps it in -fleet-key-secret and trades the token for its registration")
		fs.StringVar(&c.fleetKeySecret, "fleet-key-secret", "", "the Secret (in -namespace) a member that joins with a token keeps its key in")
	} else {
		fs.StringVar(&c.listen, "listen", ":8080", "address of the pages, the API and the health check")
		fs.StringVar(&c.devAssets, "dev-assets", "", "directory with templates/ and static/ (internal/web) read for every page instead of the built-in ones; for working on the pages")
	}
	fs.Parse(args)
	if *showVersion {
		fmt.Println("ci-accelerator " + versionString())
		return nil
	}
	c.daemons = daemons
	c.gitlabURLs = append(gitlabURLs, split(os.Getenv("GITLAB_URLS"))...)
	c.gitlabURLsFixed = append([]string(nil), c.gitlabURLs...)
	if c.gitlabURLsFile != "" {
		was, urls, err := readURLsFile(c.gitlabURLsFile)
		if err != nil {
			return fmt.Errorf("-gitlab-urls-file: %w", err)
		}
		c.gitlabURLs, c.gitlabURLsWas = append(c.gitlabURLs, urls...), was
	}
	c.split, c.apiAllow, c.apiAllowRouter = splitRole, allow, allowRouter
	c.logLevel = lf.Level
	role := "all"
	if splitRole {
		role = "controller"
	}
	log, err := roleLog(lf, role)
	if err != nil {
		return err
	}
	return run(c, log)
}

func split(s string) []string {
	var out []string
	return append(out, strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' })...)
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
	if c.maxStoreMiB < 0 {
		return errors.New("-max-store-mib must not be negative")
	}
	if c.caches != "" && c.cacheEvery < 10*time.Second {
		// Listing a bucket is not free: 0s would list it without pause.
		return errors.New("-cache-every must be at least 10s")
	}
	if c.fleetHub != "" && (c.fleetName == "" || c.fleetKey == "" || c.fleetHubKeys == "") {
		return errors.New("-fleet-hub needs -fleet-name, -fleet-key-file and -fleet-hub-keys-file")
	}
	if c.fleetJoin != "" && c.fleetKeySecret == "" {
		return errors.New("-fleet-join-file needs -fleet-key-secret: where the member keeps the key it makes")
	}
	if c.fleetHub != "" {
		if err := hubAddress(c.fleetHub, c.fleetJoin != ""); err != nil {
			return err
		}
	}
	if c.fleetHub != "" && c.namespace == "" {
		return errors.New("-fleet-hub needs -namespace (or $POD_NAMESPACE): where the member writes the Secrets its hub delivers, and keeps a key it makes")
	}
	if c.split && c.apiTokenFile == "" && len(c.apiAllow) == 0 && c.fleetHub == "" {
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
	gitlabRead := "none"
	switch {
	case c.signIn != "":
		gitlabRead = "each person's own (sign-in)"
	case c.gitlabTokenFile != "":
		gitlabRead = "the installation's"
	}
	cfg := []any{"data", c.data, "discover", c.discover, "discover_all", c.discoverAll, "daemons", len(c.daemons), "jobs_namespace", c.jobsNamespace,
		"gitlab", hosts(c.gitlabURLs), "gitlab_read", gitlabRead, "runner_caches", c.caches != "", "keep_days", c.keepDays,
		"keep_output_days", c.keepOutputDays, "max_store_mib", c.maxStoreMiB, "release", c.release, "log_level", c.logLevel}
	if c.fleetHub != "" {
		cfg = append(cfg, "fleet_hub", c.fleetHub, "fleet_name", c.fleetName)
	}
	if c.split {
		cfg = append(cfg, "api_listen", c.apiListen, "api_allow", []string(c.apiAllow), "api_allow_router", []string(c.apiAllowRouter),
			"warm_for", c.warmFor.String())
	} else {
		cfg = append(cfg, "listen", c.listen)
	}
	started(log, cfg...)
	if c.gitlabURLsFile != "" {
		// The instances are fixed at start (the collector, the job watch,
		// GitLab's client): a new URL starts the controller again.
		running := c.gitlabURLs
		go followURLsFile(ctx, c.gitlabURLsFile, c.gitlabURLsWas, urlsEvery, func(fromFile []string) {
			urls := append(append([]string(nil), c.gitlabURLsFixed...), fromFile...)
			if slices.Equal(urls, running) {
				log.Info("the GitLab URLs are the running ones again: no new start", "gitlab", hosts(urls))
				return
			}
			// Ending now would leave no controller at all: the next start
			// refuses these URLs.
			if err := urlsRefused(c, urls); err != nil {
				log.Warn("the GitLab URLs changed, but a start with them would be refused: running on with the old ones",
					"gitlab", hosts(urls), "running", hosts(running), "reason", err.Error())
				return
			}
			log.Info("the GitLab URLs changed: starting again", "gitlab", hosts(urls))
			stop()
		})
	}

	// An environment variable, not a flag: a controller image older than
	// the chart would stop on an unknown flag.
	trustGit := os.Getenv(store.TrustGitComparisonEnv) == "true"
	if trustGit {
		log.Info("rebuilds with no change in the repository count as the build farm's", "env", store.TrustGitComparisonEnv)
	}
	st, err := store.Open(ctx, c.data, store.Options{Keep: time.Duration(c.keepDays) * 24 * time.Hour, KeepOutput: c.keepOutputDays,
		MaxBytes: c.maxStoreMiB << 20, TrustGitComparison: trustGit})
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
		Resolve: func(ctx context.Context) ([]string, error) { return resolve(ctx, c, log) },
	}
	col.OnLiveChange(func() { st.Announce("") })
	var kube kubernetes.Interface
	if c.jobsNamespace != "" || c.caches != "" || (c.split && c.apiTokenFile == "") || c.fleetHub != "" {
		if kube, err = kubeClient(c); err != nil {
			if c.fleetHub != "" {
				return fmt.Errorf("a fleet member runs in Kubernetes (it writes the Secrets its hub delivers there): %w", err)
			}
			return err
		}
	}
	if c.fleetHub != "" {
		if err := endSessions(ctx, st, log); err != nil {
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
		client.Log = log
		if signIn, err = loadSignIn(c.signIn, c.gitlabURLs, st, client, log); err != nil {
			return err
		}
		gl = &control.LocalGitLab{Fetcher: &gitlab.Fetcher{Client: client, Store: st, Known: col.JobKnown, Log: log}, Auth: signIn}
		for _, ch := range signIn.Instances() {
			log.Info("sign-in through GitLab", "gitlab", ch.Host, "redirect_uri", signIn.RedirectURL(ch.Instance))
		}
	} else if c.fleetHub == "" {
		// Nothing reads or ends the sessions of an earlier sign-in now:
		// their sealed GitLab tokens would stay in the store for good.
		if err := endSignInSessions(ctx, st, log); err != nil {
			return err
		}
	}
	if c.gitlabTokenFile != "" {
		if len(c.gitlabURLs) > 1 {
			return errOneTokenSeveralGitLabs
		}
		token, err := gitlabToken(ctx, c, log)
		if err != nil {
			return err
		}
		client := gitlab.New(c.gitlabURLs)
		client.Log = log
		gl = &control.LocalGitLab{Fetcher: &gitlab.Fetcher{Client: client, Store: st, Known: col.JobKnown, Log: log},
			Token: token}
	}
	// "Files or node" is settled without anyone opening a page: with the
	// installation's token by asking GitLab, with sign-in (no token of its
	// own) from the comparisons the pages stored, which never change.
	var changer web.Changer
	if gl != nil && gl.Token != nil {
		changer = gl
	}
	go settleRebuilds(ctx, st, changer, log)
	go compareDue(ctx, col, log)
	local := &control.Local{Store: st, Live: col}
	var srv *http.Server
	if c.split {
		authorize, err := apiAuthorizer(c, kube)
		if err != nil {
			return err
		}
		certs, err := control.ReloadingCertLog(filepath.Join(c.apiTLSDir, "tls.crt"), filepath.Join(c.apiTLSDir, "tls.key"), log)
		if err != nil {
			return err
		}
		api := &control.Server{Local: local, GitLab: gl, Auth: signIn, Log: log,
			Authorize: overLinkAuthorizer(fleet.Over, authorize),
			Routes:    &control.Routes{Store: st, Live: col, Log: log, Window: c.warmFor},
			Config:    control.ConfigFor(versionString(), c.release, gl, signIn)}
		if c.fleetHub != "" {
			// The hub reads the controller's API, and delivers the Secrets
			// the box's values name.
			served := http.NewServeMux()
			served.Handle("PUT "+SecretsPath, secretWriter(kube, c.namespace, fleet.Over, log))
			served.Handle("/", api.Handler())
			d, outer, err := fleetDialer(c, st, served, log)
			if err != nil {
				return err
			}
			go runMember(ctx, d, c, kube, outer, log)
		}
		srv = &http.Server{Addr: c.apiListen, Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: idleTimeout, ErrorLog: logging.ErrorLog(log),
			TLSConfig: &cryptotls.Config{MinVersion: cryptotls.VersionTLS12, GetCertificate: certs}}
		srv.RegisterOnShutdown(api.Stop)
		ln, err := net.Listen("tcp", c.apiListen)
		if err != nil {
			return err
		}
		go func() {
			if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("controller API", "err", err)
				stop()
			}
		}()
		log.Info("ready", "api_listen", ln.Addr().String())
	} else {
		opt := web.Options{Release: c.release, Version: versionString(), PublicURL: publicURL(log)}
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
		apiRoutes := api.Handler(st, log)
		mux.Handle("GET /healthz", apiRoutes)
		mux.Handle("GET /api/", web.Heavy(apiRoutes))
		pages.Register(mux)
		srv = &http.Server{Addr: c.listen, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: idleTimeout, ErrorLog: logging.ErrorLog(log)}
		srv.Handler = endStreamsOnShutdown(srv, web.AccessLog(log, web.Secure(web.Compress(pages.Gate(mux)))))
		ln, err := net.Listen("tcp", c.listen)
		if err != nil {
			return err
		}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http", "err", err)
				stop()
			}
		}()
		log.Info("ready", "listen", ln.Addr().String())
	}
	go prune(ctx, st, signIn, log)
	sup.Run(ctx)
	log.Info("stopping")
	begin := time.Now()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Requests still open after the wait are cut: a stop, not a failure.
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("requests still open when stopping were cut", "err", err)
	}
	log.Info("stopped", "dur_ms", time.Since(begin).Milliseconds())
	return nil
}

// idleTimeout closes a kept-alive connection no request used for this
// long: idle connections are not held without bound.
var idleTimeout = 2 * time.Minute

// endStreamsOnShutdown ends the pages' event streams (/events) when srv
// shuts down: Shutdown waits for every open request, and a stream ends
// only with its request's context.
func endStreamsOnShutdown(srv *http.Server, h http.Handler) http.Handler {
	stopping, stop := context.WithCancel(context.Background())
	srv.RegisterOnShutdown(stop)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" && !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			h.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		defer context.AfterFunc(stopping, cancel)()
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

// gitlabToken is the installation's GitLab token, read from its file at
// every use: a fleet hub delivers its member's token after the member
// started. A box on its own refuses to start without the file, and starts
// with an empty one (as before fleets: GitLab then refuses the reads).
func gitlabToken(ctx context.Context, c config, log *slog.Logger) (gitlab.FileToken, error) {
	token := gitlab.FileToken(c.gitlabTokenFile)
	_, err := token.Token(ctx)
	switch {
	case err == nil:
	case c.fleetHub != "":
		log.Info("fleet: no GitLab token yet; read once the hub has delivered it", "file", c.gitlabTokenFile)
	default:
		if _, rerr := os.ReadFile(c.gitlabTokenFile); rerr != nil {
			return "", err
		}
		log.Warn("the GitLab token file is empty: GitLab's job times and logs are not read until it holds a token", "file", c.gitlabTokenFile)
	}
	return token, nil
}

// hosts are the GitLab instances' hosts, for the log.
func hosts(urls []string) []string {
	var out []string
	for _, u := range urls {
		if p, err := url.Parse(u); err == nil && p.Host != "" {
			out = append(out, p.Host)
		}
	}
	return out
}

// apiAuthorizer says who may call the controller: a static token from a
// file, or the ServiceAccounts named, checked with Kubernetes.
func apiAuthorizer(c config, kube kubernetes.Interface) (func(*http.Request) (control.Role, error), error) {
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
	allow := map[string]control.Role{}
	for role, list := range map[control.Role][]string{control.RoleWeb: c.apiAllow, control.RoleRouter: c.apiAllowRouter} {
		for _, a := range list {
			ns, name, ok := strings.Cut(a, "/")
			if !ok || ns == "" || name == "" {
				return nil, fmt.Errorf("-api-allow %q: give namespace/name", a)
			}
			if allow[a] != "" {
				return nil, fmt.Errorf("-api-allow %q: one ServiceAccount, one role", a)
			}
			allow[a] = role
		}
	}
	r := &control.TokenReviewer{Kube: kube, Audience: c.apiAudience, Allow: allow}
	return r.Authorize, nil
}

// discoverAllEnv carries -discover-all from the chart.
const discoverAllEnv = "CI_ACCELERATOR_DISCOVER_ALL"

// maxStoreEnv carries -max-store-mib from the chart: in the environment,
// so an older image that knows no such flag still starts.
const maxStoreEnv = "CI_ACCELERATOR_MAX_STORE_MIB"

// lookupHost resolves a Service's name; a variable for the tests.
var lookupHost = net.DefaultResolver.LookupHost

// resolve returns the daemons' addresses: given, or every address the
// headless Service resolves to, and with -discover-all also the daemons
// that are not ready (a stopping daemon is read to the end). A failed
// lookup of -discover-all alone is no error: an older BuildKit chart has
// no such Service.
func resolve(ctx context.Context, c config, log *slog.Logger) ([]string, error) {
	if c.discover == "" {
		return c.daemons, nil
	}
	out, err := lookupAddrs(ctx, c.discover)
	if err != nil {
		return nil, err
	}
	if c.discoverAll == "" {
		return out, nil
	}
	all, err := lookupAddrs(ctx, c.discoverAll)
	if err != nil {
		log.Debug("the daemons that are not ready not listed", "discover_all", c.discoverAll, "err", err)
		return out, nil
	}
	out = append(out, all...)
	sort.Strings(out)
	return slices.Compact(out), nil
}

// lookupAddrs is every address of a Service's host:port, as tcp://ip:port,
// sorted.
func lookupAddrs(ctx context.Context, hostPort string) ([]string, error) {
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		return nil, err
	}
	ips, err := lookupHost(ctx, host)
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

// watchJobs reads the CI jobs from their pods into the store until ctx
// ends; the channel closes once the watch and its informers have stopped.
func watchJobs(ctx context.Context, c config, client kubernetes.Interface, st *store.Store, col *collector.Collector, log *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := podwatch.Watch(ctx, client, c.jobsNamespace, c.gitlabURLs, podwatch.Handlers{
			Job: func(ctx context.Context, j store.Job) {
				if err := st.SaveJob(ctx, j); err != nil {
					log.Warn("job not stored", "job", j.JobID, "err", err)
					return
				}
				// Once when its build container starts (and again for a
				// running job after a restart); the pod's earlier events
				// at debug.
				args := []any{"job", j.JobID, "pipeline", j.PipelineID, "project", j.ProjectID, "name", j.Name, "node", j.Node}
				if !j.PodRunning.IsZero() {
					log.Info("job started", args...)
				} else {
					log.Debug("job pod seen", args...)
				}
				if err := col.JobKnown(ctx, j.Instance, j.JobID); err != nil {
					log.Warn("job's builds not compared", "job", j.JobID, "err", err)
				}
			},
			Unknown: unknownJobLog(log, c.gitlabURLs),
			Gone: func(ctx context.Context, j store.Job, at time.Time) {
				if err := st.JobEnded(ctx, j.Instance, j.JobID, at); err != nil {
					log.Warn("job end not stored", "job", j.JobID, "err", err)
					return
				}
				log.Info("job ended", "job", j.JobID, "pipeline", j.PipelineID, "project", j.ProjectID)
			},
		})
		if err != nil {
			log.Error("watching job pods", "err", err)
		}
	}()
	return done
}

// unknownJobLog tells of a job pod whose URL starts with none of the
// GitLab URLs: a warning for the first, then at the debug level.
func unknownJobLog(log *slog.Logger, gitlabURLs []string) func(ctx context.Context, pod, url string) {
	var told atomic.Bool
	return func(ctx context.Context, pod, url string) {
		level := slog.LevelDebug
		if told.CompareAndSwap(false, true) {
			level = slog.LevelWarn
		}
		log.Log(ctx, level, "a job pod's URL starts with none of the GitLab URLs: its builds get no job", "pod", pod, "url", url, "gitlab_urls", gitlabURLs)
	}
}

// loadCaches reads the list of cache stores (the chart writes it); the
// same folder listed twice for one GitLab is listed once. The same folder
// for two GitLabs is refused: the runners keep their archives under
// project/<id>/, so two GitLabs' projects of one id would be one.
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
	seen := map[string]runnercache.Store{}
	var out []runnercache.Store
	for _, s := range list {
		if s.Server == "" || s.Bucket == "" || s.Secret == "" {
			return nil, fmt.Errorf("%s: a cache store needs server, bucket and secret", path)
		}
		first, ok := seen[s.ID()]
		switch {
		case !ok:
			seen[s.ID()] = s
			out = append(out, s)
		case first.Instance != s.Instance:
			return nil, fmt.Errorf("%s: the cache folder %s is listed for two GitLabs (instances %s and %s): their projects' archives would mix; give each GitLab its own path",
				path, s.ID(), first.Instance, s.Instance)
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
		return runnercache.Scan{}, fmt.Errorf("the Secret %s has no accesskey or secretkey", s.Secret)
	}
	keys := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: ak, SecretAccessKey: sk, Source: "secret " + s.Secret}, nil
	})
	return runnercache.Lister{Store: s, Keys: keys}.Scan(ctx, time.Now())
}

// compareDue compares, once a minute, the builds that took their project
// after they were stored and whose comparison failed or was cut off by a
// restart.
func compareDue(ctx context.Context, col *collector.Collector, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		if err := col.CompareDue(ctx); err != nil && ctx.Err() == nil {
			log.Warn("comparing builds whose job became known", "err", err)
		}
	}
}

// settleRebuilds settles the rebuilds whose cache was elsewhere with
// GitLab's comparison of the commits once a minute: the stored ones first,
// then (with gl) at most 25 requests per project, pausing while GitLab
// rate-limits.
func settleRebuilds(ctx context.Context, st *store.Store, gl web.Changer, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
		ps, err := st.ElsewhereProjects(ctx, time.Now().Add(-31*24*time.Hour))
		if err != nil {
			log.Warn("settling rebuilds", "err", err)
			continue
		}
		ask := gl // nil for the rest of the pass once GitLab rate-limits
		for _, p := range ps {
			api := ask
			if k, ok := api.(interface{ Knows(string) bool }); ok && !k.Knows(p.Instance) {
				api = nil // a GitLab the console has no token for: stored comparisons only
			}
			n, err := web.SettleProject(ctx, st, api, p, time.Now(), 0, 25)
			if n > 0 {
				log.Info("settled rebuilds with GitLab's comparisons", "project", p.String(), "builds", n)
			}
			var rl *gitlab.RateLimited
			if errors.As(err, &rl) {
				// The other projects are still settled from what is stored.
				ask = nil
				continue
			}
			if err != nil {
				log.Warn("settling rebuilds", "project", p.String(), "err", err)
			}
		}
	}
}

// endSignInSessions removes the sessions kept from a sign-in that is off
// now. Their GitLab grants stay valid at GitLab until they expire or each
// person revokes the application there.
func endSignInSessions(ctx context.Context, st *store.Store, log *slog.Logger) error {
	n, err := st.DeleteSessions(ctx)
	if err != nil {
		return fmt.Errorf("ending the sessions of an earlier sign-in: %w", err)
	}
	if n > 0 {
		log.Info("sign-in is off: the sessions of an earlier sign-in ended; their GitLab grants stay valid until revoked in GitLab", "sessions", n)
	}
	return nil
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

// errOneTokenSeveralGitLabs refuses the installation's token next to
// several GitLab URLs: every read of another GitLab's jobs would send it
// there.
var errOneTokenSeveralGitLabs = errors.New("-gitlab-token-file is a token of one GitLab, but several -gitlab-url are given: it would be sent to each of them; use -sign-in (each person's own token), or one URL")

// urlsRefused says why a start with these GitLab URLs would be refused,
// or nil: the installation's token next to several of them, or a sign-in
// entry the URLs do not fit (as loadSignIn and auth.New check them).
func urlsRefused(c config, urls []string) error {
	if c.gitlabTokenFile != "" && len(urls) > 1 {
		return errOneTokenSeveralGitLabs
	}
	if c.signIn == "" {
		return nil
	}
	b, err := os.ReadFile(c.signIn)
	if err != nil {
		return nil // not the URLs' doing
	}
	var f signInFile
	if json.Unmarshal(b, &f) != nil {
		return nil
	}
	known := map[string]bool{}
	for _, u := range urls {
		known[history.InstancePrefix(u)] = true
	}
	for _, g := range f.GitLab {
		switch {
		case g.URL == "" && (len(urls) != 1 || len(f.GitLab) != 1):
			return errors.New("sign-in: name each GitLab's url: the console serves several")
		case g.URL != "" && !known[history.InstancePrefix(g.URL)]:
			return fmt.Errorf("sign-in: %s is not one of the console's GitLab URLs (they must match exactly)", g.URL)
		}
	}
	return nil
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

// overLinkAuthorizer gives the hub's requests their role. They come over
// the member's own link, proven by the pinned key (over): they may read
// what the web may, but not the store's writes or this console's sign-in,
// and a session they carry is not taken (control.RoleHub). Every other
// request is authorize's.
func overLinkAuthorizer(over func(context.Context) bool, authorize func(*http.Request) (control.Role, error)) func(*http.Request) (control.Role, error) {
	return func(r *http.Request) (control.Role, error) {
		if over(r.Context()) {
			return control.RoleHub, nil
		}
		return authorize(r)
	}
}
