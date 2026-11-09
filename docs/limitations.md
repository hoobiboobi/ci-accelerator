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
- **Builds share the daemon's network.** RUN steps run in the daemon pod's
  network namespace, so they see its localhost and the connections of
  concurrent builds. Their egress is whatever the daemon's NetworkPolicy
  allows. List your cluster CIDRs in
  `buildkit.networkPolicy.egress.clusterCIDRs` to keep builds out of
  in-cluster services. Blocking the API server reliably depends on the CNI:
  kube-proxy sends Service traffic for `kubernetes.default` to the control
  plane's node addresses, and a node's own address is always reachable from
  its pods, so list the control-plane node addresses too.
- **The disk probe's numbers are readable by builds.** The box runs a small
  container next to each daemon that reports, on port 9180 for the
  controller, the cache disk's size and free space and the last hour of the
  node's CPU, memory and pressure and of the daemon pod's CPU and memory
  (every 10 s). Builds share the pod's network and can read those numbers
  too, so a build can see when the node was busy, including with other
  projects' builds. The probe reads the host's cgroup filesystem through a
  read-only mount; the daemon's own share needs cgroup v2 nodes.
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
  cannot open another project's builds this way. It can still add builds
  to another project's lists, and a job that names a pipeline before that
  pipeline's own jobs are seen hides that pipeline's builds from everyone
  but admins until one of its project's members opens one of them (the
  console then asks GitLab, with that person's token, whose pipeline it
  is). A build whose job's pod the console never saw (it ran while the
  controller was away, or in a namespace it does not watch) is shown to
  admins only until GitLab has said whose its pipeline is (asked when
  the build's page opens); in a pipeline claimed this way that stays so.
- **Client certificates:** any certificate from the installation's CA is
  accepted. By default cert-manager lets namespace editors request
  certificates from any ClusterIssuer; on a shared cluster restrict that,
  e.g. with cert-manager approver-policy.
- **The router places builds; it does not authorize them.** It passes the
  bytes through and never reads a build, so the daemons' mutual TLS is
  still what lets a job in, and any job may build anything on the daemon
  it is sent to. Per-project cache mount namespaces or authorization
  would need the router to read the build requests
  ([ADR-0046](adr/0046-cache-reuse.md)).
- **Job pods need Pod Security `baseline`.** The runner's job pods do not
  meet `restricted`.

**Cache and routing.**
- **The cache is per daemon.** The router sends a project's jobs to the
  node where it built in the last `control-plane.router.warmDays` (7) days,
  so each project warms one node. When that daemon is not ready, the job
  goes to another one: it succeeds, with a cold cache there. A project
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
  it last told them.
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
  `gitlab-runner:alpine-v19.4.2` have HIGH and CRITICAL findings fixed
  upstream but in no release of theirs yet. They are listed, with the date
  each is looked at again, in `.trivyignore-third-party.yaml`; a weekly
  scan (`make third-party-scan`) fails on a new one. Their exposure is not
  analysed.
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
  effort basis: known token formats, passwords in URLs, bearer tokens and
  the values of build arguments with secret-like names. Dockerfiles are
  kept too, masked the same way. The API returns stored build arguments,
  with secret-named ones masked.
- **Step output is clipped.** A step keeps the first 8 KiB and the last
  56 KiB of what it printed. The controller reads a build holding at most
  64 MiB of its output: in a build where hundreds of steps print that
  much, the later ones show only how many bytes were cut. A build whose
  provenance or trace is larger than 64 MiB is not stored (the controller
  logs `record not stored`).
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
