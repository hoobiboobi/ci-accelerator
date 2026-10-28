# Changelog

Each release upgrades in place from the previous one; see
[docs/adr/0027-upgrades-and-branches.md](docs/adr/0027-upgrades-and-branches.md).
Every version has **Upgrade notes**, even if they say "None". The release
workflow copies them into the GitHub release.

## Unreleased

### Upgrade notes

- From 0.3.1 or 0.3.0, `helm upgrade` in place; the daemons stay as they
  are. A control plane pinned by digest
  (`control-plane.image.digest`, as the README told production installs)
  needs the new release's digest from its notes: the chart now refuses the
  digest of an earlier release under its own tag, which would run that
  release under the new name.
- On its first start the controller removes the builds 0.3.0 and 0.3.1
  stored twice after a daemon pod was replaced on a hostPath, and sums up
  the images' daily figures again from the first such day. The schema
  version stays: `helm rollback` to 0.3.1 keeps working (that release then
  logs "record not stored" for the records a replaced pod sends again,
  instead of storing them twice).

### Fixed

- The GitLab CI component of 0.3.1 is refused by GitLab ("header:spec
  config description is too long (maximum is 256 characters)"): every
  pipeline that included it failed without jobs. Since 0.2.3 it also did
  not work on GitLab 17.9 to 18.9, which know no `spec:description` at
  all. The component has no `spec:description` any more and works from
  GitLab 17.9 on, as its README says; its tests now run it through real
  GitLab CE (17.9 and 19.4) whenever it changes, from its tag on GitHub
  before a release, and in an end-to-end pipeline on the box.
- The console stored every build a second time when a daemon pod was
  replaced on a hostPath (a drain, an eviction, a rollout): the new pod
  sends the old pod's history again, which is on that disk. Counts,
  medians and the job pages doubled; a build record is now stored once.
- Pinning by digest covered only the control plane: the disk probe next
  to each daemon still ran ci-accelerator:v0.3.0 by tag. The README and
  the release notes name its digest (`buildkit.diskProbe.image.digest`),
  and a pinned digest of another release is refused there too.
- The command for the router's decisions (README "Logs", install notes)
  read one router pod; it reads every router now, and the log commands
  with `-l` print every line (`--tail=-1`), not the last 10 of each pod.
- The install notes' example job had no runner tags, so a runner that
  takes only tagged jobs never ran it; the notes print the component's
  include with its integrity instead. They point to the README's
  "Setting up the console".
- The README's sign-in step creates the OAuth Secret with
  `--from-literal`, as every other Secret in the instructions.
- A running pipeline's page in the console was empty until the pipeline
  ended ("No job of this pipeline built an image", and grey bars on a
  timeline that was not shown). It now shows the timeline with the running
  jobs, how many run and wait, and asks for itself again every 15 seconds
  while the pipeline runs; the list's card says "running for" and counts
  waiting jobs instead of "did not run". A job counts as running on
  GitLab's word or a pod start in the last 24 hours, so a pod that went
  away while the controller was down no longer keeps a job running. The
  job page that a job's log links to is live and turns into the build's
  page once the build is stored.
- Builds without a Dockerfile target (the CI component's default) were
  called "the default target", their image page listed every build of the
  project, and the image filter and compare picks led nowhere. They are
  named by the repository they push to (several tags of one push are split),
  have their own image page and filter, and each build is compared with the
  previous build that pushed the same repository, not with another
  Dockerfile's.
- A failed build had no Dockerfile view: BuildKit records no provenance for
  it. The console now reads the Dockerfile and the failed step's lines from
  BuildKit's stored error; a parse error marks its line, a canceled build
  says it has none, and stages a failed build never reached are no longer
  dimmed. Failed builds still in BuildKit's history get theirs when 0.3.2
  starts; older ones stay without.
- The session key's error says what the text decoded to.

### Added

- docs/gitops.md: how to make the console's session key with Flux or Argo
  CD (by hand, SOPS, Sealed Secrets, or External Secrets' Password
  generator, whose defaults the console refuses).

## 0.3.1 - 2026-10-28

### Upgrade notes

- From 0.3.0, `helm upgrade` in place. The control plane's image moves to
  v0.3.1 (the same program), so the controller, web and router pods
  restart once; the daemons and their disk probe stay as they are.
- From 0.2.x, install again as the 0.3.0 notes below describe.

### Changed

- The documentation describes 0.3.0 throughout: the build router and the
  control plane (controller and web) in the README, the architecture
  (with a new components diagram), troubleshooting (the router, the
  logs, reinstalling over 0.2.x), GitOps, the runner cache, the values
  fragment for an existing runner, the CI component, the charts' READMEs
  and SECURITY.md.
- The CI component's description and the comments on the gitlab-runner
  image tag name what 0.3.0 does (the router; runner 19.4.2).

### Removed

- docs/design/cache-per-project.md: it described the per-project cache
  accounting that 0.3.0 dropped.

### Fixed

- The release notes said `--set control-plane.enabled=false` leaves the
  console out; the router is part of it, so `global.router.enabled=false`
  is needed too, or the install fails.

### Known issues

- The GitLab CI component of this release does not work: GitLab refuses
  its header ("header:spec config description is too long (maximum is 256
  characters)") and the pipeline fails without jobs. Fixed in 0.3.2; until
  then include the component of v0.3.0 (the same job) on GitLab 18.10 or
  newer. The components of 0.2.3 to 0.3.1 do not work on GitLab 17.9 to
  18.9 at all (they use `spec:description`, which those versions refuse).

## 0.3.0 - 2026-10-28

### Upgrade notes

- **0.2.x does not upgrade in place: install again.** The console became
  the control plane, and every object and value of it has a new name, so
  Helm refuses the upgrade and says how to move:
  1. `helm uninstall <release> -n <namespace> --wait`, then wait until its
     pods are gone (the jobs namespace goes with the release), and delete
     the daemons' certificate Secret `<release>-buildkit-tls`: it outlives
     the release, and the new one names the build router (without
     cert-manager, make it again with `scripts/buildkit-certs.sh --router`).
  2. Move your values from `console:` to `control-plane:`. The
     controller's pod settings and volume go under
     `control-plane.controller` (`resources`, `nodeSelector`,
     `tolerations`, `affinity`, `podLabels`, `podAnnotations`,
     `persistence`), the web's under `control-plane.web` (its port was
     `service.port`; the web no longer follows the controller's
     placement). A runner cache's `consoleSecretName` is now
     `controllerSecretName`.
  3. Install this release. The daemons' cache on a hostPath stays (an
     emptyDir cache starts empty, as on every rollout); the new console
     starts with an empty history, and the old one stays on the volume
     `<release>-console-data` until you delete it.
  4. Point port-forwards, Ingresses and the address behind
     `global.console.url` at the Service `<release>-web` (it was
     `<release>-console`).

  On the one-machine install, `quickstart.sh` stops with the same steps.
  GitOps: delete the Application or HelmRelease and create it again with
  the new values. Step by step: troubleshooting, "Upgrading from 0.2.x to
  0.3.0".
- New names: chart `control-plane` (was `console`,
  `oci://ghcr.io/hoobiboobi/charts/control-plane`), image
  `ghcr.io/hoobiboobi/ci-accelerator` (was `ci-accelerator-console`; it
  also runs the daemons' disk probe, so the daemons restart once),
  Deployments `<release>-controller` and `<release>-web`, Services
  `<release>-web` (the console) and `<release>-controller` (its API),
  volume `<release>-controller-data`, labels `controller` and `web`
  (`app.kubernetes.io/component` and `ci-accelerator.io/component`).
- CI jobs reach the daemons through the build router
  (`tcp://<release>-router.<namespace>.svc:1234`; the runners of the box
  are configured for it, an own runner with
  `integrations/gitlab-runner/values.yaml` must change
  `BUILDKIT_REMOTE_ADDR`). The daemons' certificate names the router: with
  `buildkit.tls.source=existingSecret`, make it again with
  `scripts/buildkit-certs.sh --router <release>`. The daemons admit only
  the routers and the controller. To keep jobs talking to the daemons
  directly, set `global.router.enabled=false`.
- `charts/buildkit` on its own (upgraded in place): the daemons restart
  once, as on any rollout (an `emptyDir` cache starts empty). Their
  BuildKit worker now says whether its cache persists (`hostPath`) and,
  with the disk probe, the probe's port, so the control plane needs no
  values for either.
- The controller checks the web's ServiceAccount tokens with a
  TokenReview: the chart adds a ClusterRole that allows only that, and its
  binding. Whoever installs the chart needs the right to create them.
- Without cert-manager (`control-plane.tls.existingSecret`), the
  controller's API needs a server certificate too:
  `control-plane.controller.api.tls.existingSecret`, a Secret with
  `tls.crt`, `tls.key` and `ca.crt`. `scripts/buildkit-certs.sh
  --controller <release>` makes one from the daemons' CA. With cert-manager
  it is issued like the controller's client certificate.

### Added

- The build router: each CI job's build goes to the node where its project
  built before (else the project's node by a hash every router agrees on,
  so a new project's parallel jobs share one daemon's steps), zone-aware.
  It passes the jobs' TLS through and holds no key; the build page says
  why its node was chosen.
- `scripts/buildkit-certs.sh --router <prefix>` adds the router's names to
  the daemons' certificate.
- Compare any two builds of an image (`/builds/<id>/compare`): time,
  commit, node, build arguments, the Dockerfiles' differing lines and
  every step's state and time side by side, with the page's own build
  marked "this build" and its column tinted. A Compare button on the build
  page (key `x`), "Compare two builds" on the builds list (pick the first
  build, the list narrows to its image, pick the second), "compare two" on
  an image's page, and both in Ctrl+K.
- A page per build daemon (`/nodes/<daemon>`, linked from the Nodes table,
  the build page and Ctrl+K): its builds, rebuilds, running builds, cache
  by kind and largest cache mounts, disk, the build router's decisions by
  reason, its garbage collection, and the projects it built for (25, the
  rest folded; the reader's project first; projects the reader may not
  see as a count, never by name). A daemon replaced by a rollout answers
  with its node's current daemon.
- Logs: every role writes one JSON object per line with its role, a
  `starting` line with the settings it runs with, `ready`, `stopping` and
  `stopped`, and a line per stored build, job start and end, change of the
  daemons, sign-in and sign-out and routed connection; at `debug` also
  every request, GitLab call, controller API call and closed router
  connection. client-go's and the HTTP servers' own messages go through the
  same log. Tokens, cookies, query strings, build arguments and step output
  are never logged, and anything shaped like a GitLab token is masked.
  Values: `control-plane.log.level`/`format` and each role's `logLevel`;
  `buildkit.config.log.level`/`format` and `buildkit.diskProbe.logLevel`
  (empty by default: the daemons do not change). Every role takes
  `-log-level` and `-log-format`.
- The build page shows BuildKit's Dockerfile build checks that failed
  (rule, line, link), what each executed step's processes used (CPU time,
  time held back by a CPU limit, disk read and written, from BuildKit's
  cgroup totals), and how much step time ran in parallel.
- A failed build says whether the step that failed is new or changed in
  it (with the old and new line); exit codes 137 and 143 are read as the
  signals they are.
- The controller settles "files or node" for every project in the
  background, so the Overview and the lists are right without anyone
  opening a page first: with the installation's GitLab token by asking
  GitLab (at most 25 requests a minute per project), and in any case from
  GitLab's comparisons of commits asked for before, which are stored
  (they never change) and also settle a build again after the controller
  restarts.
- The Nodes page is one table of the daemons, the ones that need
  attention first (not reachable, a disk warning, a cache that could not be
  read; a filter shows only those), with disk, cache, builds, rebuilds for
  want of cache, running builds, projects and the build router's
  decisions; one note per problem naming its nodes; "where <project>
  built" for the project the pages show; base images held by more than one
  node as a table.
- The time from a job's start to its build is split by the job pod's own
  timestamps: the runner preparing the pod, the pod starting (scheduled,
  images pulled), the runner attaching, checkout and script. On the
  timeline and the build page.
- A rebuild on the same node under a replaced daemon says so (and whether
  its cache persisted), instead of "another node".
- A build argument that reaches a step through ENV (the step's text
  carries its value) is reported as that argument's change, not as a
  Dockerfile change.

### Changed

- The console is served by the control plane: a controller (one pod: the
  store, reading the daemons and the job pods, the sessions, every GitLab
  token) and a web (the pages, any number of pods, nothing kept), from one
  image. The web reads the controller's API over HTTPS with a
  server-auth-only certificate from the daemons' CA and proves who it is
  with a projected ServiceAccount token; the controller's NetworkPolicy
  admits the web's pods only. A page whose controller does not answer says
  so (502) instead of showing an empty console; the web rolls without a
  pause.
- One binary, `ci-accelerator`, with roles: `controller`, `web`,
  `disk-probe`, and `all` (the default: controller and web in one process,
  for development).
- Every median and p95 the console shows is a nearest-rank percentile (one
  of the measured times); the build page's "usual" time took the upper
  middle value of an even count before, the daily figures the lower one.
  "Usual" says how many builds it is the median of, and needs five
  builds of its kind; a p95 needs twenty rebuilds (a dash otherwise). The
  images table compares rebuilds with rebuilds.
- The Overview gives the build farm's share of rebuild time and, apart,
  the unclear share (it counted failed and unclear rebuilds as "not caused
  by the code").
- "Time the cache saved, about" became the measured fact: how long the
  steps not run took when they last ran.
- Byte sizes use binary units with their names (KiB, MiB, GiB).
- The pipeline timeline draws a build's parts where its steps ran (the
  union of their intervals), not the sum of parallel steps' times.

### Fixed

- A rebuild after a build whose last steps came from the cache was called
  "new step"; it is now compared with the step in that build's records.
- "Read new lines" at the end of a finished job's log showed the whole log
  again (gitlab.com leaves Content-Range out of a 416 answer).
- A live page stopped hearing of a project for good after one failed
  permission check at GitLab.
- With sign-in, `/livez` answered with a redirect to the sign-in page.
- A settled "files or node" went back to unclear whenever the controller
  restarted (it reads the daemons' history again) and stayed so until
  someone opened the project; it is settled again within a minute now,
  from the stored comparison.
- A pipeline's duration differed between its card and its timeline.
- "Wrote 0 B" for steps that wrote files (a cgroup counts block I/O only);
  1.5 MiB was shown as 2 MiB.

### Security

- The daemons run BuildKit v0.34.1 (was v0.33.1, which has no newer
  patch): built with Go 1.26.10, it is free of the Go runtime's
  advisories govulncheck finds reachable in v0.33.1 (net/http, TLS,
  x509: the revoked eMudhra intermediates; image pulls from a registry
  whose chain uses them now fail verification). v0.34.0 also fixes a
  daemon panic when one of several builds sharing a step is cancelled
  (moby/buildkit#7253). The CI component's client image and the
  ci-accelerator image's build use the same BuildKit. An earlier chart's
  default digest kept by `helm upgrade --reuse-values` now counts as this
  chart's: before, the daemons went on running the old BuildKit under the
  new name (seen on a test install). To pin an earlier BuildKit, set its
  tag as well as its digest.
- The box's runner image is gitlab-runner alpine-v19.4.2 (was 19.4.1,
  built with Go 1.26.5): the 19.4 line's rebuild on Go 1.26.10, taken
  the day after its release under the security-patch rule (ADR-0041).
  The values fragment for an existing runner says the same.
- quickstart.sh installs Helm v4.3.1 (was 4.3.0), whose Go runtime and
  golang.org/x/net carry the fixes 4.3.0 lacks; CI checks with v4.3.1
  and v3.22.1 (security-patch rule; both are two weeks old on
  2026-10-29).

## 0.2.5 - 2026-10-26

### Upgrade notes

- None to do. Upgrading from 0.2.4 restarts only the console, never the
  daemons (their pod template is unchanged).
- The console sets a cookie named `tz` (the browser's time zone) to show
  times in it; with cookies blocked it shows UTC and says so.

### Added

- The pipeline timeline's names column can be widened: drag its border,
  double-click it to fit the longest name (again to go back), or use the
  arrow keys; the width is remembered in the browser. A cut-off name shows
  whole on hover.
- A job's log reads in parts: the last 500 lines first, then "500 earlier
  lines" as often as needed back to the first line, and "Read new lines"
  for what was added since. Only the parts are read from GitLab (byte
  ranges), not the whole log every time.

### Changed

- The console shows every time in the reader's time zone (the browser's),
  with the moment in UTC on hover; the pipelines filter's dates are the
  reader's days. The image page's year chart stays per UTC day and says
  so. Without cookies the pages show UTC and say so.
- docs/architecture.md: the components and namespaces diagram is an image
  (docs/images/components-and-namespaces.svg) instead of ASCII art, and
  shows which certificates the release's CA signs.

### Fixed

- Opening a pipeline no longer shows every step for a moment before folding
  them (and the build page's cached stages likewise): the server sends the
  folded rows hidden.
- The mouse wheel over the pipeline timeline goes on to the page once the
  timeline has scrolled to its end (it was trapped inside it).
- Bar labels near the timeline's right edge are moved before their bars
  instead of being cut off.

## 0.2.4 - 2026-10-25

### Upgrade notes

- The console's rebuild reasons got stricter: a "build farm" verdict is
  now only given when nothing below the rebuilt step reads the build
  context; a rebuild the records cannot explain is "files or node —
  unclear" until a job of the same pipeline or GitLab's commit comparison
  settles it. Builds stored before the upgrade keep their old verdicts
  until the image builds again (the reason is recomputed) or they age out
  of the 30-day retention. Without a GitLab credential the console now
  says "unclear" where it used to guess; with one, the comparison names
  the actual changed files.
- The Nodes page's "Rebuilt for want of cache here" now counts evicted
  caches too (the same definition the Overview's "build farm" uses), so
  the number can rise after the upgrade.
- The disk probe's image tag now defaults to a pinned `v0.2.3` instead of
  the chart version, and the box no longer requires
  `buildkit.diskProbe.image` to equal `console.image`. Upgrading from
  0.2.3 the rendered daemon pods are unchanged: NO daemon rollout. If you
  mirror images, mirror the probe's pinned tag too.

### Fixed

- A chart release alone no longer restarts every daemon: the disk probe's
  default image tag was `v<chart version>`, so any upgrade after 0.2.3
  changed the pod template although the probe program had not changed.

- A rebuild caused by a commit is no longer blamed on the build farm when
  the file-reading step was deduplicated into a concurrent job, cached, or
  sits in another stage (`COPY --from`): the engine follows the step's
  whole ancestor cone and GitLab's comparison checks every file the
  context feeds into it.
- A sibling job's cache hit no longer counts as proof that the files did
  not change when that cache was written by the same pipeline seconds
  earlier.
- A parallel job or retry of the same pipeline is never "the previous
  build": comparing against one made the commit's own work look like the
  farm's fault ("same commit") on its own pipeline page.
- Pages no longer claim "it is on another node" for a build whose previous
  build ran on the same daemon, and no longer name a file as *changed*
  unless GitLab's comparison verified it — a pipeline witness proves that
  files changed, not which, and the pages now say so.
- The Overview's "build farm" and the Nodes page's per-node counter use
  one definition of the farm's fault and can no longer disagree.

## 0.2.3 - 2026-10-25

### Upgrade notes

- The daemons restart once: their pods get a PriorityClass, small resource
  requests and (in the box) a disk probe container. A rollout replaces one
  node's daemon at a time; with `storage.type=emptyDir` each daemon starts
  with an empty cache, with `hostPath` it keeps it.
- The buildkit chart creates a PriorityClass (cluster-scoped, named
  `<namespace>-<release>-buildkit`), which needs permission to create
  PriorityClasses. Without it, or to use your own class, set
  `buildkit.priorityClass.create=false` or `buildkit.priorityClassName`.
- The daemons now request 500m CPU and 512Mi of memory by default
  (`buildkit.resources`); size them to your build nodes (the chart's
  values list sizes).
- With `--reuse-values` the box keeps its old values: the disk probe stays
  off until `--reset-then-reuse-values` (as docs/releasing.md recommends).
- The disk probe runs the console's image on the build nodes. If you
  mirror `console.image`, set `buildkit.diskProbe.image` to the same
  repository, tag and digest; the box refuses values where they differ.
- A one-node install from `quickstart.sh` keeps its
  `/var/lib/ci-accelerator/values.yaml`: it gets the default requests, not
  ones sized to the machine. Add `buildkit.resources` there (a fresh
  install writes half the CPUs and a quarter of the memory requested,
  three quarters of the memory as the limit).

### Added

- A GitLab CI/CD component, `ci/gitlab/build.yml`: one job that builds an
  image on the build farm and pushes it with the job token. Inputs for the
  context, Dockerfile, target, platforms, build arguments, secrets (by CI/CD
  variable name), images, provenance, a timeout and more; the digest goes to
  later jobs as a dotenv report. Included by URL from the release's tag
  with an integrity hash (any GitLab 17.9 or newer, also self-managed), or
  copied into a project. The release notes print the include block.
- Console: the pipelines page searches by pipeline number (or its first
  digits), commit SHA, commit title, branch, author and job name, and
  narrows to the last 24 hours, 7 or 30 days or to dates (UTC). Ctrl+K
  offers to search all pipelines for what was typed.
- The daemons get their own PriorityClass (above CI jobs, never preempting
  them): under memory or disk pressure the kubelet evicts CI jobs before
  them. On a test box whose disk filled up, the daemon went first.
- The daemons have small default resource requests instead of none, so
  they are no longer BestEffort; `quickstart.sh` sizes them from the
  machine on a first install.
- Console: the Nodes page shows each daemon's disk (cache, other files,
  free space, inodes) from a disk probe next to the daemon, and warns
  under BuildKit's keep-free floor or near the kubelet's eviction
  thresholds; a nearly full disk lights the Nodes item.

### Fixed

- Console: the "Live" mark in the sidebar no longer flashes, and the
  buttons below it no longer move, when going from page to page; a dropped
  connection shows only after 3 seconds. The time of the last update is in
  UTC like every other time.
- Console: the Nodes page updates itself when builds start and end; it
  never subscribed to the daemons' state.
- Console: a daemon replaced by a rollout disappeared from the Nodes page
  only if its address left the Service while its stream was still open;
  when the stream had broken first (the pod stopping), it stayed as
  "Unreachable" until the console restarted.
- Console: an image's page opened while another project is chosen (a
  shared link, an old tab) switches to the project that builds the image
  instead of answering "Not found".

## 0.2.2 - 2026-10-25

### Upgrade notes

- The box now creates a NetworkPolicy for the console
  (`console.networkPolicy.enabled`, on by default). Pods of other
  namespaces and `kubectl port-forward` keep reaching it; the box's jobs
  namespace and its daemons do not. An Ingress controller on the host
  network reaches pods from a node address: allow those addresses in
  `console.networkPolicy.extraFrom`, or switch the policy off. The console
  is not restarted.

### Security

- The console was reachable from the box's own CI jobs and from every
  build (RUN steps share the daemons' network), and without sign-in it
  shows every project's builds, Dockerfiles and step output. A
  NetworkPolicy now keeps both out (found by an internal review). The
  standalone console chart has the same policy, off by default
  (`networkPolicy.enabled`).

### Fixed

- The box's install notes showed the build job image as
  `moby/buildkit:<box version>` (for example `moby/buildkit:0.2.1`, which
  does not exist) since 0.2.0; they show the BuildKit release the daemons
  run again.
- Chart READMEs: three value descriptions were cut short (`tls.reloader`,
  `terminationGracePeriodSeconds`, `caches.*`), and `caches.fromConfigMap`
  showed a stray example.
- Documentation: the runner cache's `consoleSecretName` is documented,
  troubleshooting names the real `preflight` keys, the ADR index lists
  0040 and 0041, small link and wording fixes.

### Changed

- The charts' homepage and Artifact Hub links point to the website,
  https://ci-accelerator.io, which renders the documentation of the
  released version.
- A final release's "Full changelog" link compares with the previous
  final release, so it covers its release candidates.
- The release check also covers `docs/gitops.md`, the README's one-node
  upgrade command and the Artifact Hub change list.
- The browser checks give Chrome 60 s to start and stop it when it does
  not.
- The release workflow signs the charts right after pushing them, before
  the console image is built. Artifact Hub checks a version's signature
  only when it first indexes the version; 0.2.1 was indexed in the three
  minutes between push and signature and is listed there as unsigned,
  although `cosign verify` passes for every artifact of the release.

## 0.2.1 - 2026-10-24

### Upgrade notes

None: no pod template changes; the daemons and the runner keep running.

### Fixed

- The pipeline timeline's "Critical path" toggle did nothing: the style it
  relied on was missing. Rows off the path now fade and the path's job
  names go bold.
- `quickstart.sh` rewrote `/var/lib/ci-accelerator/values.yaml` on every
  run, losing sign-in and other edits. The file is written on the first
  install only; a later run keeps it, applies a newly given GitLab token
  with `--set` and says how to keep it.

### Added

- Each chart carries a README (generated from its values comments) and
  Artifact Hub metadata: icon, images, links and the release's changes;
  the release pushes each chart's repository metadata (`artifacthub/`)
  next to it. The charts are listed on Artifact Hub.

## 0.2.0 - 2026-10-24

### Upgrade notes

- **VictoriaMetrics and VictoriaTraces are no longer part of the box**
  (ADR-0039):
  - The upgrade removes their StatefulSets. Their volumes stay; delete
    them by hand once you no longer need the data
    (`kubectl -n <namespace> get pvc | grep -e victoria-metrics-single -e vt-single`).
  - To keep metrics and traces, run your own OTLP backend and set
    `buildkit.otel.metricsEndpoint` and `tracesEndpoint`. With
    `--reuse-values` the old endpoints point at the removed Services:
    upgrade with `--reset-then-reuse-values`, or set them to your backend
    or `""`. The install notes warn while old values remain.
  - The daemons restart when their endpoints change.
- **Runner configuration.** The box's runner TOML is now one template for
  every runner. Without a cache it renders exactly as before (the runner
  does not restart). With `--reuse-values` the main runner keeps the TOML
  of the release it was installed with, so `gitlab-runner.runners.cache`
  takes effect only with `--reset-then-reuse-values`.

- **Trace ids.** Every CI job now gets its own trace instead of sharing one
  per pipeline. Trace ids have a new layout:
  - `01` + 3 bytes from the GitLab URL;
  - the pipeline id and the job id, 6 bytes each.

  Traces stored by 0.1.0 keep the old layout. To find a pipeline's jobs
  in your trace backend, query the `ci.pipeline.id` resource attribute.
  Nothing to change in values.
- **Job pod annotations.** The runner configuration gains annotations, so
  the runner pod restarts once on upgrade; the build daemons do not. If you
  use `integrations/gitlab-runner/values.yaml` with your own runner, copy
  its new `pod_annotations` block. Job pods now carry the commit author's
  name and e-mail address, readable by whoever can read pods in the jobs
  namespace.
- **The console is on by default.** The box gains a `console` subchart
  (`console.enabled`, true): one Deployment and a 10 GiB volume from the
  default StorageClass (`console.persistence.*`), a client certificate from
  the daemons' CA, a Role that reads the pods of the jobs namespace, and
  one ingress rule on the daemons' NetworkPolicy. A cluster without a
  default StorageClass needs `console.persistence.storageClass` or
  `console.enabled=false`, or the install waits for the volume. With
  `--reuse-values` an upgrade keeps it off (the old values have no such
  key); `--reset-then-reuse-values` turns it on.
- **The daemons restart once.** Every daemon now carries its node as a
  worker label (`ci-accelerator.io/node`), and keeps 1000 build records
  (48 h) for the console instead of buildkit's 50 (`config.history`, in
  both charts). Both change the daemons' pod template: the upgrade rolls
  the DaemonSet; plan it between busy hours. With an `emptyDir` cache the
  daemons also start with an empty cache, so the next build of every image
  rebuilds every step. With either cache the console has no earlier build
  to compare with (0.1.0's records carry the old trace ids and their job
  pods are gone), so each image's next build that rebuilds a step is
  reported as its first.
- **The runner hook** gains a line that prints the console's link when
  `global.console.url` is set (nothing otherwise). With your own runner,
  copy the new `pre_build_script` lines from
  `integrations/gitlab-runner/values.yaml`.

### Changed

- Pinned components: the console image is built with Go 1.27.3 on the
  current distroless base, the browser checks run on Node 24.22.1 and the
  one-machine install (and the e2e) pin cert-manager v1.21.3 - security
  patches adopted under ADR-0041 before they were two weeks old.
- The box installs no metrics and traces storage (ADR-0039). The daemons
  push over OTLP only to an endpoint you set (`buildkit.otel.*`), none by
  default. Builds can no longer reach the box's observability data,
  because there is none.
- One trace per CI job (ADR-0028). With one trace per pipeline, BuildKit
  attached the spans of every concurrent job of a pipeline to each of its
  build records. Job ids beyond 2^32 (gitlab.com) now fit, and ids that
  are not numbers no longer produce a broken `TRACEPARENT`.

### Added

- The console shows its version at the bottom of the sidebar and with
  `console -version`: the image tag and the commit it was built from.
- The pipeline timeline's critical path follows the stages: the job that
  ended last, the job of the previous stage it waited for, and so on; the
  header sums what the chain waited for runners and ran.
- Vulnerability checks in CI: `govulncheck` over the console's code for both
  release targets and `trivy` over the built image on every pull request
  (`make vulncheck`, `make image-scan`), plus a weekly run against `main`
  and the latest published image. Grace periods live in
  `.govulncheck-allow` and `.trivyignore.yaml` with an expiry and a reason.
  `SECURITY.md` says how to report a vulnerability.
- `quickstart.sh` (attached to every release): try the box on one clean
  Linux machine. It installs k3s, Helm, cert-manager and the box with its
  console; k3s, Helm, cert-manager and the chart are pinned and checked
  against their sha256 (the box's images come by tag, the daemons' by
  digest). `--uninstall` removes it all, the build cache included. Checked
  on every pull request on a clean Ubuntu runner: it installs,
  builds twice through the real runner (the second build cached), shows
  the build in the console, and uninstalls.
- `charts/console` and the box's `console.enabled` (on by default; false
  leaves it out): one pod that reads every daemon's build history and the CI job
  pods into SQLite and serves read-only pages and a JSON API on a
  ClusterIP Service. It answers one question: why an image build was slow
  or not cached, and whether the change or the build farm caused it
  (ADR-0037):
  - Overview: rebuild reasons grouped and ranked by the build time they
    cost;
  - Pipelines: each pipeline with its jobs by stage on a time axis, how
    long each waited for a runner and ran (from GitLab: read for the
    pipelines on the page, once when finished), the usual and longest
    wait; a pipeline opens into every job, each image build into its
    stages and steps;
  - Builds: every image build, filtered by branch, outcome, image or your
    own, optionally grouped by pipeline;
  - Images: cache rate, usual and slowest build time, the steps that
    rebuild most and why, over 24 hours, 7 or 30 days, and builds per
    week by outcome over the last year;
  - Nodes: each daemon's builds, running builds, cache by kind, cache
    policies, the runner cache bucket per project;
  - a build: one sentence, what changed since the previous build of the
    image, its steps on a list, a timeline and its Dockerfile; a step:
    source lines, output and history; a pipeline: why its images were
    rebuilt and a timeline of every job (from GitLab, or from the job pods
    without a token), each image build opening into its stages and
    steps.

  Five outcomes everywhere: from cache, your change, build farm, unclear,
  failed. A rebuild whose cache was on another node is settled with
  GitLab's comparison of the two builds' commits (ADR-0038): the files
  its step copies changed (your change) or did not (build farm); it stays
  unclear only without a GitLab token or sign-in. Search (Ctrl+K), keyboard shortcuts, light and dark themes, live
  updates; the pages work without JavaScript. Builds are kept 30 days
  (`console.retention.days`); each image's daily figures (builds by
  outcome, build times) are kept a year. With `global.console.url`, every job prints
  the link to its build. Without sign-in, keep it inside the cluster. Its
  image is published with every release
  (`ghcr.io/hoobiboobi/ci-accelerator-console:v<version>`, amd64 and
  arm64, signed); `make console-image` builds it for kind.
- S3 cache for GitLab's `cache:` keyword (`gitlab-runner.runners.cache`;
  ADR-0035, docs/runner-cache.md): one folder per release and GitLab in
  any S3-compatible bucket, shared by every runner of the box; job logs
  show each cache's size.
- Runner tiers (`runnerTiers`): more runners next to the main one, any
  number, each its own pod with its own GitLab runner (tags), job size,
  placement (build nodes or any node) and S3 cache. The install notes say
  what to create in GitLab.
- Sign-in through GitLab for the console (`console.signIn`, off by
  default; ADR-0034): each person signs in with a GitLab the runners serve
  and sees the projects whose jobs GitLab lets them read; GitLab is read
  with their own token. It needs the console's address
  (`global.console.url`), a Secret with a session key and, per GitLab, an
  OAuth application (confidential, scopes `openid read_api`; the install
  notes print its redirect URI). Admins (GitLab usernames) also see builds
  of no known project and the JSON API. Sessions end after 7 days unused
  and 30 days after sign-in at the latest; sign-out revokes the session's
  token at GitLab.
- Job pods carry the job's data from GitLab's CI variables as annotations
  under `ci-accelerator.io/`: pipeline source and creation time, project
  path, job stage and start time, commit title and author, merge request.
  A commit title reaches the pod as text, never expanded.
- How the console explains a build (`internal/history`): each step is
  cached, built or received from another job running it at the same time,
  and the first rebuilt step gets a reason. A rebuild is blamed on the
  build node when the step reads no files, or when another job of the same
  pipeline proves it. Each executed step's output is kept masked (secret
  build arguments and known token formats, also in step names) and clipped
  to 64 KiB. Tested on recorded builds (`test/history/generate.sh`) by
  `make go-test`.
- Checks for contributors: `make go-test` (Go) and `make ui-test`, the
  console's pages in headless Chrome (`test/ui`), without and with
  sign-in (then also the sign-in page and signing out), both in the checks
  workflow; the e2e box scenario runs the browser checks against the box's
  console.

- The e2e scenario `upgrade`: the last published release, upgraded in place
  to the tree under test and rolled back.
- `CONTRIBUTING.md`, `docs/releasing.md` and ADR-0027: how changes reach
  `main`, when and how releases are cut, what an upgrade must keep.
- `docs/design/console.md`: design of the v0.2 console, and ADR-0029:
  sign-in and permissions through GitLab, job data from pod annotations,
  the Dockerfile and step output kept per build.

## 0.1.0 - 2026-10-19

First release: the `ci-accelerator` box and the standalone `buildkit` chart.

### Upgrade notes

First release. Pre-releases 0.1.0-rc.1 and 0.1.0-rc.2 upgrade to it in place
with `--reuse-values`.
