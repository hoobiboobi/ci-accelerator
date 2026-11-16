# Limitations

These are known, deliberate or not yet solved. Read them before you share
one installation between teams.

**Security model: one installation is one trust domain.** (Reporting a
vulnerability: [SECURITY.md](../SECURITY.md).)
- The daemons run as **privileged** pods. Their namespace must allow
  privileged pods.
- **Builds share the cache:** RUN steps can read and write cache mounts
  that other projects' builds left in the same daemon.
- **A fetched commit is readable without a token.** With the CI
  component's `context-source: git`, a daemon keeps the commits it fetched
  and serves a commit's files to any build that names the commit id,
  whether or not it has a token for that repository. A build job does not
  even have to know the id: any client of a daemon can list its recent
  builds, and a git context's address (repository and commit) is part of
  each. Treat every project that builds on one installation as able to
  read the code the others built there.
- **A job reads its daemon's build records.** Its client certificate opens
  BuildKit's whole control API, as the console's collector uses it: any
  job can read every recent build record of the daemon it reaches (build
  options with their build-arg values, the Dockerfile, step output, the
  trace) and prune its cache or delete its history. Sign-in in the
  console separates only what the console shows.
- **Builds share the daemon's network.** RUN steps run in the daemon pod's
  network namespace, so they see its localhost and the connections of
  concurrent builds. Their egress is whatever the daemon's NetworkPolicy
  allows. List your cluster CIDRs in
  `buildkit.networkPolicy.egress.clusterCIDRs` to keep builds out of
  in-cluster services. Blocking the API server reliably depends on the CNI:
  kube-proxy sends Service traffic for `kubernetes.default` to the control
  plane's node addresses, and a node's own address is always reachable from
  its pods, so list the control-plane node addresses too.
  `clusterCIDRs` keeps RUN steps out of in-cluster services, not CI job
  scripts: the jobs namespace's policy keeps every in-cluster destination
  open (it must reach kube-dns and the router). To limit job pods, set
  `jobsNamespace.networkPolicy.enabled=false` and write your own policy.
- **The disk probe's numbers are readable by builds.** The box runs a small
  container next to each daemon that reports, on port 9180 for the
  controller, the cache disk's size and free space and the last hour of the
  node's CPU, memory and pressure and of the daemon pod's CPU and memory
  (every 10 s). Builds share the pod's network and can read those numbers
  too, so a build can see when the node was busy, including with other
  projects' builds. The probe reads the host's cgroup filesystem through a
  read-only mount; the daemon's own share needs cgroup v2 nodes. A RUN
  step can open many connections to the probe on `localhost:9180` (it
  serves 32 at a time; more wait); if the probe's container is
  OOM-killed, the daemon pod is not Ready until it restarts.
- **Trace data is untrusted.** Any build (a RUN step) can write spans
  through BuildKit, so traces in your backend are input from builds. The
  buildctl command line, build-arg values included, is recorded in traces:
  **never pass secrets as build args**; use `--secret`.
- **A job can claim another job's builds in the console.** The console
  ties a build to its GitLab job and pipeline by the trace id buildctl
  sends, and the job's pipeline id by a CI variable; a job can set both.
  With sign-in on, a build whose trace names a job and a pipeline of
  different projects is shown to admins only, and a pipeline keeps the
  project it was first seen in until GitLab says whose it is, so a job
  cannot open another project's builds this way. A build that reuses the
  id of a stored build does not replace it, and once a pipeline's project
  is known only that project's builds settle each other's reasons. With
  another project's pipeline and job ids (both are sequential and shown in
  job logs), a job can still add builds to that project's lists and job
  page (the link in that job's log may open the added build), make one of
  them the previous build the project's next real build is compared with
  (so that build's reason can be wrong), make its own node count as warm
  for the project (the router may then send the project's jobs there), and
  enter the project's hints, usual times and Insights. A job that names a
  pipeline before that pipeline's own jobs are seen hides that pipeline's
  builds from everyone but admins until one of its project's members opens
  one of them (the console then asks GitLab, with that person's token,
  whose pipeline it is); without sign-in and without a GitLab token nothing
  asks, and such a claim stays. A build whose job's pod the console never
  saw (it ran while the controller was away, or in a namespace it does not
  watch) is shown to admins only until GitLab has said whose its pipeline
  is (asked when the build's page opens); in a pipeline claimed this way
  that stays so.
- **The runner must not let jobs overwrite pod annotations.** The router
  and the console take a job's id, project and URL from the annotations
  the runner puts on its pod. With the runner's
  `pod_annotations_overwrite_allowed` matching `job.runner.gitlab.com/`,
  `project.runner.gitlab.com/` or `ci-accelerator.io/` keys, a job can
  pass for another project: leave it unset, or not matching them
  ([integrations/gitlab-runner](../integrations/gitlab-runner/README.md)).
- **Client certificates:** any certificate from the installation's CA is
  accepted. By default cert-manager lets namespace editors request
  certificates from any ClusterIssuer; on a shared cluster restrict that,
  e.g. with cert-manager approver-policy. A job can copy the client
  certificate it mounts; the daemons accept any certificate of their CA
  until it expires, and there is no revocation. The box issues the jobs'
  client certificate for 72 hours (`buildkit.tls.client.duration`). To cut
  off copies sooner, rotate the CA.
- **The router places builds; it does not authorize them.** It passes the
  bytes through and never reads a build, so the daemons' mutual TLS is
  still what lets a job in, and any job may build anything on the daemon
  it is sent to. Per-project cache mount namespaces or authorization
  would need the router to read the build requests
  ([ADR-0046](adr/0046-cache-reuse.md)).
- **Job pods need Pod Security `baseline`.** The runner's job pods do not
  meet `restricted`. Job containers keep the container runtime's default
  capabilities (`NET_RAW` included) and may escalate privileges, as
  `baseline` allows; add `cap_drop` and `allow_privilege_escalation =
  false` to the runner configuration when jobs need neither (`sudo` or
  `ping` then stop working).
- **Jobs share the images pulled to a node.** Job pods use the pull policy
  `if-not-present`: a job can start a private image that another project
  already pulled to the same node, without that image's registry
  credentials. Where projects must not share images, enable the
  `AlwaysPullImages` admission plugin or set `pull_policy = ["always",
  "if-not-present"]` in the runner configuration.

**Cache and routing.**
- **The cache is per daemon.** The router sends a project's jobs to the
  node where it built in the last `control-plane.router.warmDays` (7) days,
  so each project warms one node. When that daemon is not ready, the job
  goes to another one: it succeeds, with a cold cache there. The project
  then stays on that node: the router sends a project's jobs to the node
  it used most recently, so after a rollout or a failed connection the
  project warms its new node once and keeps to it. A project
  that has not built in the zone yet goes to a node by hash. The job's
  build context travels to the chosen node, which may not be its own.
- **Zones:** with `control-plane.router.zones` (the default) a job stays
  in its zone (`topology.kubernetes.io/zone` of the daemons' nodes), so a
  project warms up once per zone; when its zone has no ready daemon,
  `crossZone` (on by default) sends it to another zone, or refuses it.
  Clusters that put one zone label on every node, or zones close enough
  that crossing them costs nothing, set `zones=false`.
- **The router knows a job by its pod's address.** With a CNI that hides
  it (SNAT inside the cluster) every job goes to its own node's daemon, as
  without the router, after the router waited up to 3 s for a pod with
  that address to appear (a job's pod can connect before the router has
  seen it). While the controller is away, the routers keep routing on what
  it last told them, and age that warmth themselves.
- **emptyDir mode:** the cache lives only as long as the pod, so every
  rollout empties it, and the router no longer counts builds of a replaced
  daemon as warmth. Use `hostPath` on a dedicated disk in production.
- **What uninstall leaves behind:**
  - with hostPath: the cache on the nodes;
  - the controller's volume `<release>-controller-data` (the console's history), kept on purpose;
  - the certificate Secrets: the CA key in cert-manager's namespace
    (`<namespace>-<release>-buildkit-ca`; delete it by that name, not by
    label: another box of the same release name shares the labels), and
    the daemons' and the controller's certificates in the box namespace
    (`<release>-buildkit-tls` among them).

**Operations.**
- **Certificate renewal needs a restart.** buildkitd reads its certificate
  only at start: after a renewal (once a year by default) the daemons must
  restart. With Stakater Reloader in the cluster that happens by itself.
- **Probes miss some hangs.** They check that the daemon answers, not that
  builds make progress. Stuck registry pushes can hang all builds on one
  daemon without failing a probe (moby/buildkit#7275).
- **Upstream BuildKit issues that matter here:**
  - a cancelled build can fail another build that shares its steps
    (moby/buildkit#7268);
  - heap retained after builds (moby/buildkit#7181);
  - metadata that grows outside the GC budget (moby/buildkit#7263).
- **Known vulnerabilities in the default third-party images.** The
  daemons' `moby/buildkit:v0.34.2` (its CNI plugins are built with Go
  1.25.7; brotli, curl and go-archive) and the runner's
  `gitlab-runner:alpine-v19.5.1` have HIGH and CRITICAL findings fixed
  upstream but in no release of theirs yet. They are listed, with the date
  each is looked at again, in `.trivyignore-third-party.yaml`; a weekly
  scan (`make third-party-scan`) fails on a new one. Their exposure is not
  analysed.
- **Cluster-scoped names join the namespace and the release.** The CA
  and its ClusterIssuer, the PriorityClass and the controller's
  ClusterRole are named `<namespace>-<release>-...`: two boxes whose
  namespace and release names join to the same string (namespace `a-b`
  with release `c`, namespace `a` with release `b-c`) collide. Choose
  names that do not.
- **Kernel risk on Talos 6.18:** a very large cache with high parallelism
  has led to node hangs (dying cgroups). This is under investigation, not
  solved.

**The console.**
- **Without sign-in it is open.** Every page, `/search-index`, `/events`
  and `/api/v1` answer whoever reaches the Service `<release>-web`. With
  `global.console.url` set, it answers only requests for that host,
  `localhost`, IP addresses and in-cluster Service names (`*.svc`), so a
  web page elsewhere cannot point a name of its own at it (DNS rebinding);
  without the address, every host. The
  Service is ClusterIP only: use `kubectl port-forward`, or an Ingress with its own
  authentication. "Inside the cluster" is not a trust boundary here: the
  box's own CI jobs and builds run untrusted code in it. The box therefore
  ships a NetworkPolicy (`control-plane.networkPolicy`, on by default)
  that keeps the jobs namespace and the daemons (whose network every RUN
  step shares) away from the web, and admits only the web and the routers
  to the controller; other namespaces, an Ingress controller among them,
  still reach the web. It needs a CNI that enforces NetworkPolicy.
- **A configured token is shared.** Without sign-in, anyone who reaches
  the console can read the logs of the jobs it shows (masked, a part at a
  time) through the installation's GitLab token.
- **It stores build data.** Step output is kept 7 days, masked on a best
  effort basis: GitLab, GitHub, Slack, npm, PyPI, Stripe, Google API and
  Vault tokens, JWTs, private keys, AWS key ids, passwords in URLs and
  token-only user information in URLs, Authorization headers of any
  scheme, the values of assignments, options, headers and query
  parameters named like a secret, registry JSON credentials, and the
  values of build arguments with secret-like names (also URL-encoded,
  base64 and JSON-escaped). A password in a URL that contains `/` is not
  recognised. Dockerfiles are kept too, masked the same way. At rest, the
  store keeps build operations with secret build-argument values and the
  formats above replaced by keyed hashes; the key is in the same
  database. The API returns stored build arguments: secret-named ones
  masked, and known token formats and passwords in URLs masked in the
  others.
- **Step output is clipped.** A step keeps the first 8 KiB and the last
  56 KiB of what it printed. The controller reads a build holding at most
  64 MiB of its output: in a build where hundreds of steps print that
  much, the later ones show only how many bytes were cut. A build whose
  provenance is larger than 64 MiB is not stored (the controller logs
  `record not stored`); one whose trace is larger is stored without its
  job. A build record beyond the controller's bounds (2000 steps, a 1 MiB
  Dockerfile, long step names and errors) is stored cut and reads as
  truncated.
- **Job pods carry the commit author's name and email** as annotations,
  and the console stores them.
- **One controller.** SQLite on a ReadWriteOnce volume (never NFS), with
  the `Recreate` strategy: while the controller restarts (an upgrade that
  changes it), the pages answer that it is not reachable (502) and the
  routers route on what they last knew. There is no built-in backup.
- **No in-place upgrade from 0.2.x.** 0.3.0 is installed again, and its
  console starts with an empty history
  ([upgrading.md](upgrading.md#from-02x-to-030-or-later-install-again)).
- **Without sign-in and without a token** the console shows no runner wait
  times and cannot compare commits, so "files or node" stays "unclear".
- **"Build farm" only when it is proven.** BuildKit does not record which
  files a step read, so when a step that reads the build context is
  rebuilt with the same definition, either its files changed or the cache
  was elsewhere. The console says "build farm" when another job of the same
  pipeline found the step in the cache on its node and both builds read the
  same context folder (the CI component names it; without it, only builds
  of the same image count), or when nothing below the step reads files at
  all. GitLab's comparison of the commits proves a change ("your change",
  naming the files), but no change in the repository is not no change in
  the context: CI artifacts and files generated in the job are not in it.
  Such a rebuild stays unclear, saying what the comparison showed;
  `control-plane.reasons.trustGitComparison=true` counts it as the build
  farm's when your build contexts hold repository files alone. A build
  whose context the build node fetched from git (the CI component's
  `context-source: git`) is the repository's folder at its commit, so
  there the comparison decides either way, counting only the changed files
  inside that folder, and a rebuild of the same commit is the farm's. A step that
  fetches from the network without a checksum (`ADD https://...`, a git
  branch) is unclear too, and a build with `--no-cache` counts as the
  job's own choice.
- **What GitLab's comparison cannot see.**
  - A COPY source that is a symbolic link (or lies under a linked folder)
    is compared by the link's own path: when only the link's target
    changed, GitLab's comparison shows no change in what the step copies,
    and a build from a git context (`context-source: git`) is counted as
    the build farm's.
  - With `reasons.trustGitComparison=true`, the comparison looks at the
    context folder only: a step that copies from a named local build
    context in another folder of the repository
    (`--build-context name=../shared`) can be counted as the build farm's
    when only that folder changed.
  - A comparison GitLab answers with 404 (a commit force-pushed away or a
    deleted branch, or a project the installation's token can no longer
    see) leaves the rebuild unclear for good: it is not asked again.
- **Images are told apart by Dockerfile target.** Builds without a target
  (the CI component's default) are named by the repository they push to,
  and each is compared with the previous build that pushed the same
  repository and has that repository's own usual time and slow mark; but
  the Images page and the daily figures put all of a project's builds
  without a target in one row. Builds with one target that push different
  repositories (one Dockerfile for several services) are compared each with
  its own repository's previous build, but share the target's row there
  too. Give each image its own target, or one project per image, to see
  them apart there.
- **A failed build's Dockerfile** comes from BuildKit's stored error: only
  the failed step's lines are marked (BuildKit records no provenance for a
  failed build), and a canceled build has none.

**Scope.**
- **GitLab only.** GitHub Actions support (through Actions Runner
  Controller) is planned.
- **The CI component is for GitLab only**; GitHub Actions is planned with
  the GitHub support above.

**A fleet hub** (experimental, [fleet.md](fleet.md)):
- The hub keeps no copy of a cluster's builds: while a cluster is away the
  hub shows only the pages each person opened before (dated) and says the
  cluster is not connected for the rest.
- GitLab is read by each cluster with its own token; a person's token at
  the hub decides only which projects that person sees.
- Lists, Insights and comparisons stay per cluster; a pipeline whose jobs
  ran in several clusters is shown grouped by cluster.
- The hub is one process with SQLite: when it restarts, the console is
  away for the restart and every member reconnects (seconds in the lab).
- Its values, management API (`/api/v1/fleet/`) and protocol may change
  between releases without the usual deprecation cycle.
- "Add a cluster" offers a short list of settings; the rest is set in the
  box's values by hand. Roles go to people one by one (no GitLab groups).
- The hub keeps the runner tokens, GitLab tokens and S3 keys entered in
  it: whoever holds its secrets key and a backup holds those credentials.
  Its secrets key cannot be changed in place: kept secrets then must be
  entered again.
- A member trusts its hub like its own web: the hub reads that cluster's
  builds and asks GitLab with the member's token for any project the
  token reads, and writes its runner, GitLab and S3 Secrets
  ([fleet.md](fleet.md)).
- A fleet member runs in Kubernetes (it writes the Secrets its hub
  delivers there); a box run as a plain process cannot join.
- A box in a fleet does not serve its read-only JSON API
  (`/api/v1/...`): the hub shows its pages only.
- The pages the hub shows for an absent cluster live in the hub's memory
  (at most a day old, 2000 pages, 64 MiB) and are gone after a hub
  restart.
- The hub's image has no shell or `tar`: backups leave and enter its pod
  through a debug container ([fleet.md](fleet.md), section 6).
- Keys are replaced by hand: the hub's in three steps over every member,
  a member's by removing and adding it again ([fleet.md](fleet.md),
  section 7).
