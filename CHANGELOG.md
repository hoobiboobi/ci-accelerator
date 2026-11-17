# Changelog

Each release upgrades in place from the previous one; see
[docs/adr/0027-upgrades-and-branches.md](docs/adr/0027-upgrades-and-branches.md).
Every version has **Upgrade notes**, even if they say "None". The release
workflow copies them into the GitHub release.

## Unreleased

### Upgrade notes

- The console's store gains a column (`builds.order_ms`) on its first
  start; a rollback to 0.6.0 keeps working on the same store. A box that
  does not set `fleet.role` renders as before.
- A box's GitLab token is now read from its Secret at every use; a Secret
  emptied while the box runs stops its GitLab reads at once. An empty
  token file at start still starts the box with a warning, as in 0.6.0.
- A box that joins a fleet as a member stops serving its own web and
  read-only JSON API (`/api/v1/builds`); the hub shows its
  pages under `/m/<name>/` but not the JSON API, and the sessions of its
  own sign-in end at its first start as a member. Upgrade a fleet's hub
  before its members: a member newer than its hub is refused until the
  hub is upgraded.
- The controller, the web and the routers restart once for the new
  image, and the main runner's pod once for its new pod security context
  (RuntimeDefault seccomp); the daemons and their disk probe stay, and so
  do the store and the cache. A router restart lets its open builds finish
  for `control-plane.router.drainSeconds` (300 s) and then cuts them.
- The controller's API now refuses a TokenReview that does not confirm
  its audience: a cluster whose token authenticator does not report
  audiences refuses the web and the routers after the upgrade.
  Kubernetes' own ServiceAccount authenticator reports them.
- The store records the last day its daily figures were summed (a row in
  its meta table) and when the runners' S3 cache was last listed (a
  column); a rollback to 0.6.0 ignores both. A step whose stored operation
  holds a secret may once show a different reason after the upgrade.
- A controller started without sign-in (and not in a fleet) deletes the
  sessions and sealed GitLab tokens an earlier sign-in left. The grants
  stay valid at GitLab until they expire: revoke them there.
- If you scrape the main runner's metrics, list the scraper in
  `gitlab-runner.metrics.from`: the runner's pod now has a NetworkPolicy
  that admits nothing else.
- `GET /api/v1/builds` lists one image name per `images` item. A client
  that split the items on commas keeps working; one that matched an item
  against the joined string must match single names now.
- GitLab CI component: a later job that read `IMAGE` or `IMAGE_DIGEST`
  from a `push: false` job got empty values; it now gets none.
  `timeout-minutes: 0` now fails the job instead of cancelling the build
  at once. The include's integrity changes (the release notes print the
  new one).
- quickstart.sh: `--runner-token-file` now wins over `RUNNER_TOKEN` when
  both are given (as its help always said); a `--build-dir` that is not
  an absolute path without spaces is refused.
- integrations/gitlab-runner: the fragment now runs gitlab-runner
  alpine-v19.5.1; install it with chart 0.94.1.
- control-plane: the new value `retention.storeShare` (default 70) bounds
  the controller's database to that percent of
  `controller.persistence.size`. A store already above it loses its
  oldest builds at the first hourly retention after the upgrade (the
  images' daily figures stay); set `retention.storeShare: 0` to keep the
  old behaviour.

### Security

- With sign-in and several GitLabs, a person's GitLab token is only ever
  sent to the GitLab that issued it. Before, opening a build or a job of
  another GitLab (including a link printed in a job log) could send it
  there, and an admin's pages of another GitLab did too; those pages now
  show the stored data, and no token is renewed because of the other
  GitLab's refusal.
- The controller's API checks that Kubernetes confirmed its audience in
  each TokenReview, and bounds the reviews of unknown tokens per caller
  address and overall (a 503 asking to try again past the bound); refused
  calls are logged once a minute per route.
- A job the console has no record of redirects to its build only for
  someone who may see that build.
- The sign-in check judges `/static/` and `/auth/` addresses as sent, so
  an encoded slash no longer reaches the not-found page without sign-in.
  A sign-in return address with a dot segment ending under `/auth/` is
  refused.
- Sign-in: the login cookie answers one callback only; a replayed
  callback is refused without asking GitLab's token endpoint.
- With sign-in at an https, non-loopback address, the console sends
  `Strict-Transport-Security` (one year).
- With sign-in, the Nodes pages show running targets, cache-mount names
  and base-image names to admins only.
- Secret masking covers more formats: Authorization headers of any
  scheme, `PRIVATE-TOKEN`/`JOB-TOKEN` headers, options and assignments
  named like a secret, secret query parameters, JWTs, registry JSON
  credentials, npm, PyPI, Stripe, Google and Vault tokens, runner
  registration tokens and Slack webhooks; secret build arguments also
  URL-encoded, base64 and JSON-escaped. A long log or line cut where
  masking shortened it no longer shows the remainder of a secret.
- Build-argument values are masked for known token formats and passwords
  in URLs whatever the argument's name, when a build is stored and when it
  is read. A token in a value, or a secret argument's value repeated in
  another argument, no longer stops the console from naming that argument
  as the change between two builds.
- Stored build operations and the build context option no longer keep
  secret values in clear (keyed hashes, as for secret-named variables);
  "fetched from the network" paths hide secret build arguments.
- Order hint: the fragment and the "the step also runs" text are built
  from the step name, which is masked, not from the raw command (a token
  in an installer's RUN line appeared in clear there and in
  `/api/v1/builds` `order[].fragment` and `order[].others`).
- The console's database and step-output files are created readable by
  their owner only (0600), and existing ones are tightened on start.
- The build router refuses connections above 4096 open in all or 256
  from one address (`-max-conns`, `-max-conns-per-source`) and closes a
  connection that sends nothing within 10 s before dialing a daemon: a CI
  job can no longer exhaust the router's memory.
- The daemons' NetworkPolicy and the jobs namespace's also block
  `168.63.129.16` (Azure WireServer), `100.100.100.200` (Alibaba) and
  `fd20:ce::254` (GCP IPv6) by default
  (`buildkit.networkPolicy.egress.blockedCIDRs`).
- The console's NetworkPolicy also keeps out `router.clientNamespaces`,
  the daemons' namespace and pods labelled
  `ci-accelerator.io/component=ci-job`
  (`control-plane.networkPolicy.deniedComponents`, default
  `[buildkit, ci-job]`).
- The box issues the jobs' client certificate for 72 hours
  (`buildkit.tls.client.duration`, new); the daemons' certificate keeps
  8760h. A copied job certificate stops working sooner.
- The main runner's pod gets a NetworkPolicy (nothing reaches it;
  `gitlab-runner.metrics.from` lets a scraper reach :9252: its listener
  also serves the URL of every job it runs) and RuntimeDefault seccomp.
- golang.org/x/net v0.62.0 (CVE-2026-78661 in x/net/html; govulncheck
  found no call path).

### Added

- A fleet (experimental: its values, API and protocol may change between
  releases): several boxes under one console. A hub is a release of the
  control-plane chart with `fleet.role: hub`; a box joins it with
  `fleet.role: member`, its controller keeping one outgoing connection to
  the hub's console address (no inbound address, works behind NAT), keys
  pinned both ways. The hub shows each cluster's console under
  `/m/<name>/` with a cluster switcher, lists the clusters on Members,
  opens a pipeline whose jobs ran in several clusters grouped by cluster,
  and sends the job links printed in job logs to the cluster that ran the
  job. While a cluster is away the hub shows the pages each person opened
  before, dated (for at most a day). Clusters join by name and public key
  or with a join token, through the console or the hub's management API
  (`/api/v1/fleet/`). The hub never sends a person's session to a member,
  bounds a member's answers to 64 MiB, and writes nothing into a member's
  history; the controller API states its version (`Config.API`) and its
  setup digest. docs/fleet.md, ADR-0063.
- Fleet administration (experimental): roles viewer / operator / owner kept
  in the hub (the values' `signIn.gitlab[].owners` are locked owners), API
  tokens with an expiry chosen at issue, the Access and Audit pages; "Add a
  cluster" in the console with a short list of settings, generated values
  and install commands and a one-time join token (`fleet.join`); the
  runner's token and S3 keys sealed at the hub (`fleet.secretsKey`) and
  delivered to the cluster over its link; `ci-accelerator hub backup`,
  `hub restore` (voids every API and join token it holds) and `hub grant`
  in the hub's pod. docs/fleet.md, ADR-0064.
- A fleet member trades its join token again until the hub answers (no
  restart needed), sends it only over https (http to localhost or a
  cluster Service), follows no redirect with it, and starts without
  waiting for the hub; `fleet-keygen` refuses to write over an existing
  file; a delivery sets only the keys the hub keeps.
- The image's `/licenses/README` lists the linked Go modules with their
  versions and where their source is; `github.com/hashicorp/yamux`
  (MPL-2.0, unmodified) is new with the fleet link, and the image's
  license label reads `Apache-2.0 AND MPL-2.0`.

- The console says when a rebuild installed dependencies again only
  because a `COPY` of the whole build context comes before the install,
  and none of the files the install reads changed (GitLab's comparison of
  the commits proves which did): in the verdict with the share of the
  build it took, in the steps list, and as an "order" speed-up with a
  Dockerfile fragment that copies those files first (pip, uv, Poetry, npm,
  Yarn, pnpm, Bun, Go modules, Composer, Mix; Bundler and Cargo in words).
  A later rebuild whose dependencies came from the cache says the order
  was fixed, with the time before and after. Insights lists their time;
  `GET /api/v1/builds` returns them as `order`.
- The controller also reads daemons that are not ready (a stopping
  daemon) through a third Service of the buildkit chart,
  `<fullname>-all` (headless, every daemon pod, ready or not): a
  controller that restarts while a daemon drains still stores the builds
  that daemon finishes. The control-plane chart's new value
  `daemons.discoverAll` names it (derived from `daemons.discover` when
  that names a `-headless` Service) and passes it as
  `CI_ACCELERATOR_DISCOVER_ALL` (`-discover-all`). Without that Service,
  the ready daemons are read as before; no daemon restarts for it.
- buildkit chart: `networkPolicy.ingress.controller` admits the
  controller's pods; the box sets it, so a `networkPolicy.ingress.extraFrom`
  of your own no longer removes the controller.
- GitLab CI component: new input `output-prefix` (e.g. `API_` gives
  `API_IMAGE_REF`) for pipelines with several component jobs.
- The controller warns once when a CI job pod's URL starts with none of
  the configured GitLab URLs (its builds then get no job), and logs
  `reading record` with the record's ref before it reads a build's history
  blobs (Info for a build new to the console), so a record whose reading
  ends the process can be identified from the log.

### Changed

- The upgrade test starts from 0.6.0. Nothing changes for a user.
- Controller: new value `retention.storeShare` (default 70 % of
  `controller.persistence.size`, passed as `CI_ACCELERATOR_MAX_STORE_MIB`
  or `-max-store-mib`): above it the oldest builds are deleted before
  their retention ends, oldest first, and a `retention` warning is
  logged; the images' daily figures are kept. A size given other than in
  whole Mi, Gi or Ti, or an existing claim, sets no bound.
- Charts: `caches.stores` that list one folder (server, bucket, path) for
  two GitLabs fail the render, as the controller refuses them at start.
- The controller reads the installation's GitLab token from its file at
  every use: a changed token Secret counts without a restart, and an
  emptied or unreadable one stops GitLab reads (job times, logs, commit
  comparisons) until it is fixed.
- Console: with sign-in every page is sent with `Cache-Control: no-store`
  (Back after signing out no longer shows a page from the browser's
  cache). A time axis over more than a day has at most 13 ticks.
- Routing: a project that the router sent elsewhere once (a daemon
  restarting, a failed connection, an overloaded node) no longer stays
  split across nodes: its jobs go back to the node it used most recently,
  so parallel jobs meet on one daemon again. Connections of jobs the
  router does not know honour `crossZone: false` and `overloadBuilds`, and
  spread by node instead of all going to one daemon.
- The routers age warmth themselves (`CI_ACCELERATOR_WARM_FOR`, from
  `router.warmDays`): projects stop being warm after `warmDays` even while
  the controller is away, their own decisions are forgotten after that,
  and running-build counts older than a minute no longer make a router
  pass a daemon over. A stopping router sends all the routing decisions it
  still holds, not only the first 1000, and warns once (not every 2 s)
  while the controller is away.
- Console: one signed-in person can hold at most 16 live-update streams.
  Each event must reach the browser within a minute, or the stream ends.
  A page whose live connection ended reconnects (after 5 s, doubling to a
  minute) and catches up; a failed update says "Not updating"; a big page
  refreshes at most every 5 s. At most 16 GitLab pipeline reads run at
  once.
- Console: at most four build pages, step pages and comparisons are
  prepared at once per process; the others wait.
- Console: the Resources tab says that its cores over time add up each
  step's average.
- Console: the project switcher works beside a narrow or collapsed
  sidebar and from a phone's header (which also offers signing out);
  projects and Sign out are in search (Ctrl K). Keyboard: a build's tabs
  move with the arrow keys, Home and End; live updates keep the keyboard
  focus and the scroll of the timeline and tables; Esc on a step opened
  from a pipeline's timeline returns to its row; a filter changed with the
  keyboard applies on Enter or when leaving it, not at every arrow key;
  the search and shortcuts dialogs keep focus and keys to themselves;
  single-key shortcuts can be turned off from search. Screen readers: a
  main landmark and a skip link, timeline rows with their level, the live
  clock no longer announced every update, named icons and copy buttons, an
  h1 on a step's page. A node's Load rows are one line again; quiet rows
  keep full contrast; the bottom bar fits 340 px screens; a print layout.
  Wording: the project banner says something changed (not "new builds"),
  the timeline filter "Received" is "From another job", an image's year
  view no longer suggests a longer window or claims a 30-day retention.
- The console API (`GET /api/v1/builds`): `images` lists one image name
  per item (a build the CI component pushed with a commit tag and
  `latest` used to answer both names joined with a comma in one item);
  required lists (`order[].files`, `order[].managers`,
  `hints[].managers`, `managers[].problems`) answer `[]`, never `null`; a
  new `orderNote` says when the order hints could not be read or cover
  only the newest 100 builds, and `hintsNote` speaks of the cache-mount
  hints only; answers carry `Cache-Control: no-store`, and a store failure
  answers a fixed text, the cause going to the console's JSON log. The
  published contract no longer refuses unknown properties (new fields may
  appear within v1) and lists 401 and 403. The sign-in gate's 403 for the
  API is no longer cacheable.
- `make api-check` fails when no release tag is visible instead of
  passing without comparing (fetch the tags, or set
  `API_CHECK_BASE=<ref>`); it also refuses an operationId removed or a
  parameter default changed.
- The controller refuses a caches list that names one folder (server,
  bucket, path) for two GitLabs. A changed GitLab URL file that the
  controller would refuse at start (a second URL next to `gitlab.token`,
  or a sign-in entry that does not fit the URLs) no longer ends the
  running controller: it keeps the old URLs and logs a warning.
- The console, controller API, web, router health and fleet hub servers
  close kept-alive connections idle for 2 minutes. The disk probe serves
  at most 32 connections at once; more wait.
- A panic while reading a step for hints is logged (with its stack, at
  most 10 per process) instead of only dropping the hint.
- Chart render refuses: a system namespace (`kube-system`, `kube-public`,
  `kube-node-lease`, `default`, cert-manager's) as `global.jobs.namespace`;
  fleet values without `fleet.role`; a `signIn.gitlab` entry without `url`
  next to several `gitlab.urls`; `extraConfig` that sets buildkitd's debug
  address; `openFilesLimit` above 1073741816. The daemons' NetworkPolicy
  admits in-cluster pods by a namespace selector while
  `buildkit.networkPolicy.egress.clusterCIDRs` is empty (Cilium did not
  apply its ipBlock to pod addresses, cutting builds off from in-cluster
  registries, GitLab, S3 and OTLP).
- integrations/gitlab-runner: chart 0.94.1 and runner 19.5.1, the
  versions the box runs (was 0.93.0 and 19.4.2). The docs say to leave
  the runner's `pod_annotations_overwrite_allowed` unset, or not matching
  the job, project and `ci-accelerator.io` annotations: an allowed
  overwrite lets a job pass for another project.
- quickstart.sh: a k3s install that failed after the binary was placed
  is retried on the next run, and `--uninstall` removes it; k3s that does
  not come up stops with a pointer to `journalctl -u k3s`. The closing
  note and the printed `helm upgrade` keep a GitLab token added on a
  second run; the install notes printed at the end name kubectl as `sudo
  k3s kubectl`. `--build-dir` is checked (absolute, no spaces) before
  anything is installed. An option without a value, an empty or
  unreadable token file and a token with spaces inside are named; a byte
  order mark and blanks around a token are dropped. `--version` accepts
  `v0.7.0`. The uninstall command printed at the end works as printed
  (`sudo bash <path> --uninstall`). A stop at the checks leaves no
  `/var/lib/ci-accelerator` behind.
- GitLab CI component: a job with `push: false` no longer writes empty
  `IMAGE` and `IMAGE_DIGEST` into its dotenv report. The build stops
  before the job's own timeout when that comes first, and a buildctl
  killed early (out of memory, a cancelled job) is no longer reported as
  a timeout. `timeout-minutes` 0 is refused, and 010 means ten minutes.
  Blanks around `buildctl-args` lines are dropped. The README says what a
  provenance attestation records and that CI/CD variables named `CIA_*`
  outrank the inputs.
- scripts/buildkit-certs.sh stops instead of creating a new CA when
  `ca.crt` is there and `ca.key` is not; for `--client controller` it
  prints the box namespace and `control-plane.tls.existingSecret`;
  printed paths are quoted.
- The image's `/licenses` carries the licence files of forked or vendored
  code inside modules, and Go's own licence.
- Documentation: the release candidate procedure (a pull request that
  sets `<v>-rc.1`), the release workflow's guards, the supported versions
  in SECURITY.md (0.6.x), the image pinning there and for the runner, the
  network policies of the jobs namespace and the runners, how a daemon
  stops (ADR-0061), taking a build node out for maintenance
  (docs/production.md), the controller's memory, what a job can read from
  its daemon, and the limitations the fixes above leave.

### Fixed

- Sign-in: an open live page checks the session without renewing it, so
  the next page you open renews the session cookie again (before, a
  console tab left visible was signed out seven days after sign-in, and
  its session never ran out of idle time). A session that ends because it
  was idle or too old has its GitLab token revoked, as a sign-out does.
- The web reads large answers of its own controller again (`GET
  /api/v1/builds` with a high limit); the 64 MiB bound applies to fleet
  members only. After a controller restart the web reconnects its change
  stream within a second, however often the controller restarted before.
  The controller logs when it cannot read its renewed TLS certificate (it
  keeps serving the old one).
- Collector: one daemon that stops answering in the middle of a record no
  longer stops the console from storing every other daemon's builds:
  blobs are read outside the collector's lock, and a read that gets
  nothing for 2 minutes is given up. Callers with a deadline (job
  comparison) stop waiting when it passes.
- A build that could not be stored (a short store error, a daemon read
  failure) is tried again every 30 seconds while its daemon's stream
  lasts, up to 5 times; a failed save of another build no longer leaves
  the arriving one skipped. A build whose trace is larger than 64 MiB is
  stored without its job instead of not at all. A record that makes the
  collector panic is logged and skipped instead of restarting the
  controller.
- The controller no longer crashes on a build-check warning whose line
  range has no start or end, and it bounds what it keeps of a build's
  record (at most 2000 steps, step names 4 KiB, errors 16 KiB, the
  Dockerfile 1 MiB, the progress stream's vertices and warnings); a build
  beyond a bound reads as truncated.
- A daemon that was unready for a moment is no longer disconnected,
  forgotten and read again 35 minutes later. A build whose start event
  comes after its end no longer shows as running. A daemon with a very
  large cache (over about 80,000 records) is inspected again; when its
  cache cannot be listed, its disk and load are still read, so the disk
  warnings keep working. At most 8 running builds naming one CI job are
  followed live at once, so builds naming one job cannot take every live
  view of the box.
- A job pod's URL now matches its GitLab whatever the case of the host
  name and with or without the default port; of two GitLab URLs on one
  host, the longest that matches wins.
- Verdicts: a change to a file that a glob COPY source reads (`COPY
  package*.json`, without a COPY of the whole context) now counts as your
  change. A rebuild of the same commit whose step also downloads something
  without a checksum stays unclear, also with
  `reasons.trustGitComparison`. An edited `.dockerignore` of the context
  keeps a rebuild unclear instead of "no change in what it copies". A
  change found in a comparison GitLab cut short is marked "cut short".
- Verdicts: one GitLab comparison that keeps failing (a diff timeout, a
  403) no longer holds back the project's other rebuilds; a 403 to the
  installation's token is asked again later instead of being stored as
  unresolved.
- Verdicts: a step rebuilt because its source changed (a context, a git
  repository or an image below an unchanged Dockerfile line) names the
  source instead of "this step changed" with the same text. A "files
  changed" verdict made from a sibling job is reconsidered when that job
  is decided otherwise. A cache hit on a daemon where another pipeline had
  just built the step no longer proves "other node": the "cache
  elsewhere" settling also looks at the project's other pipelines that
  ran at the same time on the same daemons (a merge request's pipeline
  and its branch's, of one commit).
- The build page no longer says that no cache held the project's layers
  when the router passed over a warm node that was not ready or busy.
- A pipeline read only from GitLab's API (its job pods never seen) now
  gets its commit, so its rebuilds can be settled.
- Order hint: the build page's "order fixed" verdict appears only when
  an install of the project's dependency files by the same package
  manager came from the cache; a tool installed before the copy (`pip
  install poetry`) neither counts as a fix nor hides the order hint of the
  install after the copy. `poetry install --only-root` no longer counts as
  an install of the dependencies.
- Order hint: an editable install (`pip install -e .`, `uv pip install -e
  .`) gets an early install without `-e`, and `-rrequirements.txt` is
  rewritten too. A change to a requirement file that another one includes
  (`requirements/base.txt`), a gemspec, `patches/` or `.yarn/` counts as a
  dependency change. The "Replace lines" fragment is not offered when
  ENV, ARG, USER or LABEL lines lie between the COPY and the install. The
  hint is not shown for a rebuild that GitLab's comparison cut short.
- Order and cache-mount hint figures ("installed again N times", "ran N
  times", median) count only the image's own builds: several images of a
  project built without `--target` are no longer mixed. A build whose
  reason changed after GitLab's comparison no longer counts in the order
  figures and Insights. The API's hints answer shares one time window for
  all builds.
- Cache-mount and order hints: reading a RUN step's script is bounded (a
  few hundred bytes of `A=$A$A` lines could take gigabytes of memory in
  the console and the controller).
- Cache-mount hint: no fragment for a here-document RUN (it showed
  BuildKit's one-line summary as the command); commands that only empty
  the cache (`apt-get clean`, `rm -rf /var/lib/apt/lists/*`, `dnf clean
  all`) are left out of the fragment and named in a caveat; dnf's
  `keepcache` option goes to every dnf command; a cache-off flag is
  dropped only from its manager's command (uv's `-n` no longer turns
  `head -n 5` into `head 5`); a cache mount with `sharing=private` is no
  longer reported as not locked; the Next.js fragment has no fixed
  `id=next-cache`, and its caveat says what an id does.
- Store: retention on a full controller volume deletes expired
  step-output files first, deletes builds of days already summed even
  when the daily figures cannot be written, cuts the write-ahead log back
  to 64 MiB after checkpoints, and starts on a full volume; each hourly
  retention run logs a `store volume` warning when the volume is nearly
  full. Retention
  deletes in pauses, so the collector is not held off for seconds after a
  long outage; it keeps a job while a build of it is kept, and forgets
  projects of which nothing is kept (they leave the project menu).
- Store: a record that reuses a stored build's id no longer replaces that
  build, and once a pipeline's project is known a build of another
  project or of no known job no longer settles its builds' reasons. Daily
  figures no longer skip days when a rebuild was settled before the
  hourly rollup after days of downtime. Reading a step's output at the
  moment its day file expires says "no output kept" instead of failing.
  Ids no GitLab issues (2^63 and above) read as nothing instead of failing
  a cache scan or a page.
- Console: the build list's Older pages no longer skip builds that
  started in the same millisecond, and the pipelines list offers "Older"
  only when older pipelines exist. Pipeline search ignores case in any
  script, not only ASCII; non-ASCII project initials and searches are cut
  by whole letters.
- Console: a pipeline page and the project's change banner no longer
  refresh on GitLab re-reads that change nothing. The runner cache card's
  time is the last successful listing.
- Console: while the controller does not answer, the Nodes pages say so
  instead of stating that no daemon is reached or that a daemon was
  replaced; a node's page answers 503 and updates when the controller is
  back. The Nodes page is dated by its oldest daemon reading, and a daemon
  whose cache cannot be listed shows when its cache figures are from. It
  says when GC limits differ per node; a full cache at the GC floor no
  longer counts as "needs attention"; disk shares are rounded down.
- Console: failed and canceled steps count as "steps run" everywhere,
  and canceled builds are said as canceled in Insights and pipeline
  sentences. Durations are rounded once ("1 min 0 s", never "60 s"), and
  negative ones show as 0. Chart dots and bars stay inside their box with
  skewed clocks; the live page's label of an early, long step sits at the
  right edge. Wording: "1 step was built", "N more reasons"; the memory
  line names the step by its command; a preview of a windowed page keeps
  the window.
- Console: static files are kept for a year only under their current
  version (during an upgrade a browser could keep an old script), and
  `Vary` keeps `Accept-Encoding` with sign-in. The web pod's per-project
  figures are bounded (stale ones dropped, at most 100 000 builds kept).
- Runner caches: a store address that answers with something other than
  an S3 listing is an error, not an empty cache; a lifecycle answer
  without rules is "unknown"; redirects are not followed.
- Disk probe: node CPU no longer jumps to absurd values when a
  `/proc/stat` counter goes back, and guest time is no longer counted
  twice (takes effect with the next disk-probe image).
- Role `all` stops at once with a live page open and no longer exits
  with an error; the web role ends live pages at shutdown instead of
  waiting 5 s.
- Install notes print the right OAuth redirect URI without the runner's
  `gitlabUrl` and the right runner Deployment name for long release
  names; the control-plane notes warn about a GitLab nobody can sign in
  with.

## 0.6.0 - 2026-11-13

### Upgrade notes

- The daemons restart once, for their new pod template (the open-files
  limit, `buildkit.openFilesLimit`, and the drain before SIGTERM). A
  running build ends with them; start the upgrade when no build runs, as
  for any daemon change. A rollback to 0.5.2 works; the console's store
  is unchanged.
- Pipelines that include the CI component with `integrity:` need the new
  value from the release notes (the component's job script changed).

### Added

- The CI component notes in the job's log when the checkout's `.git` is
  in the build context (no `.dockerignore` line keeps it out): every
  job's checkout writes its own, so a step that copies the whole context
  runs again in every pipeline. In a lab run of popular open-source
  projects, a quarter lost their warm builds this way. The component's
  integrity changes: take the new one from the release notes.

### Fixed

- A `RUN` with yum on CentOS 7 (ventoy in the lab run) hung for the job's
  hour on the box while it took four minutes with `docker build`: the
  daemons and every RUN step inherited the node's open-files limit, 2^31
  on a node whose containerd runs with `LimitNOFILE=infinity`, and the
  tool closes every possible descriptor. The daemons now cap it at
  `openFilesLimit` (default 1048576, `0` keeps the node's).
- A build whose provenance maps other digests than its progress reports
  (seen with older frontend images: `# syntax=docker/dockerfile:1.4`,
  `1.7-labs`) said "Nothing was rebuilt: every step came from a build
  running at the same time" although it built them all (grafana and pake
  in the lab run, in every build). Its steps are now its own; their Dockerfile lines and usage are
  not shown for such a build.
- A Dockerfile with ten steps or more without stage names lost its steps
  1 to 9 on every page: BuildKit names them "[ 1/10]" to "[ 9/10]" (the
  number padded to the count's width), which the console did not read as
  steps (rustdesk's build in the lab run showed none of its 9 steps).
  Stage names no longer keep the padding either ("dev-base " in the
  Resources table).
- A rebuild of a step that copies the whole build context, when the
  context holds the job's `.git` (no `.dockerignore` entry for it), was
  "unclear" for the same commit and "Dockerfile changed" (or another
  file) after a commit even when `.dockerignore` keeps that file out. The
  console now keeps what the context held (its `.dockerignore` patterns
  and the paths the frontend asked for, from the build's provenance),
  never counts a file the context did not hold, and names the job's
  `.git` with what to do about it. Builds stored before the upgrade keep
  their verdicts.
- A build of another pipeline that ran at the same time (a merge
  request's pipeline next to its branch's, same commit) counted as the
  image's previous build: the rebuild was compared with a build that left
  it no cache, and its reason became "same commit, unclear". The previous
  build is now one that ended before this one started.
- A project's runner cache on the Nodes page warned about every S3
  folder (no lifecycle rule, a failed listing), also those of other
  runner tiers that hold none of its caches; it now shows the state of
  the folders that hold them (all folders when none does).

### Changed

- The ci-accelerator image is built with Go 1.27.4, which fixes
  GO-2026-6859 (an HTTP/1 server crash in `net/http`, reached by the
  console's web server). The patch is two days old: a security fix of
  the pinned minor line is taken at once
  ([ADR-0041](docs/adr/0041-security-patch-exception.md)).
- A stopping daemon now finishes its running builds before it receives
  SIGTERM (its `preStop` waits while `buildctl debug histories` lists a
  running build, then 15 s more), and the console's controller keeps
  reading a daemon whose address left the Service until it closes the
  stream (up to 35 minutes). Before, a daemon deleted in the middle of a
  build let the build finish, but with the default `emptyDir` storage
  the console never stored it: every rollout lost the builds that were
  still running ([ADR-0061](docs/adr/0061-drain-before-sigterm.md)).
- The upgrade test starts from 0.5.2, and the release notes point to
  docs/production.md for pinning the images by digest. Nothing changes
  for a user.
- Documentation: the install is one procedure per starting point, each
  step with its check and what it prints: docs/install.md (a cluster),
  docs/install-existing-cluster.md (a cluster that already runs
  cert-manager, a runner or admission policies), docs/quickstart.md (one
  machine), docs/first-build.md (the first CI job), docs/production.md
  (the cache disk, daemon sizes, digests, zones, OTLP, mirrors);
  docs/upgrading.md, docs/console.md and docs/runner-cache.md are
  procedures too. The README keeps the short form. Every procedure was
  run as written on fresh clusters and machines.

## 0.5.2 - 2026-11-10

### Upgrade notes

- From 0.5.1, `helm upgrade` in place. The controller, the web and the
  routers restart once for the new image, and the main runner's pod once
  for gitlab-runner 19.5.1 (the official chart 0.94.1); the daemons, the
  control plane's store and the cache stay as they are.
- The fixed step count (see Fixed) applies to builds stored from now on,
  and to those of the last two hours or so, which the controller reads
  again from the daemons when it restarts. Older builds keep the count
  they were stored with: one whose only executed steps were git sources
  fetched again still reads "Unclear: 1 step ran".

### Fixed

- Rebuild reasons, found on moby/moby's own Dockerfile:
  - a build whose only executed steps were git or HTTP sources resolved
    again (BuildKit does that in every build) counted them as steps run
    and read "Unclear: 1 step ran; nothing to compare with". It built
    nothing: the lists, the build page and the reasons now leave such
    sources out, as they leave out base images;
  - a COPY --link line that did not change read "Dockerfile changed: LINK
    COPY ... -> COPY ...": BuildKit records it as two operations under one
    step, and a build lists one of them, not always the same one. The
    previous build's step is now the operation of the same kind.

### Added

- The console API has a contract: [docs/api/openapi.yaml](docs/api/openapi.yaml)
  (OpenAPI 3.0.3) describes `GET /api/v1/builds`. Within v1 no field is
  removed or renamed and no type changes; new fields and values may
  appear. The project's checks hold the responses to it and compare it
  with the last release's ([ADR-0059](docs/adr/0059-api-contract.md)).
- The ci-accelerator image carries an SPDX SBOM for each platform, next
  to its provenance; the release checks it before signing. Read it with
  `docker buildx imagetools inspect ghcr.io/hoobiboobi/ci-accelerator:<tag> --format '{{json .SBOM}}'`
  ([ADR-0060](docs/adr/0060-sbom.md)).

### Changed

- The README is short: what it is, how to try and install it, the first
  limitations and a map of the documentation. Its details moved, word for
  word, to [docs/install.md](docs/install.md) (what gets installed,
  requirements, options, where the cache lives),
  [docs/console.md](docs/console.md) (the console, its levels, sign-in,
  logs, OTLP), [docs/limitations.md](docs/limitations.md) and
  [docs/testing.md](docs/testing.md); the repository map is in
  CONTRIBUTING.md. Links to the README's old sections point there.
- Checks: golangci-lint (govet, staticcheck, unused, ineffassign) over the
  Go code, actionlint and zizmor over the workflows, the API contract, and
  one e2e run per kind cluster at a time. zizmor found that the release
  job restored a cache that pull requests write: the release no longer
  uses one. Nothing changes for a user.

- The ci-accelerator image is built on a newer digest of its distroless base
  (static-debian13:nonroot). Nothing changes for a user.
- CI: actions/upload-artifact v7.0.2 (retries when the artifact service
  rate-limits). Nothing changes for a user.
- The box's runner is gitlab-runner 19.5.1 with the official chart 0.94.1
  (was 19.4.2 with 0.93.0): 19.5.3 fixes nothing the box runs into and is
  taken once it is two weeks old.
- Dependabot no longer scans `test/hints/cases`: those are test fixtures
  with versions pinned on purpose, never shipped. It still raises security
  updates for the rest; version updates stay with Renovate. Nothing changes
  for a user.

### Security

- The control plane is built with github.com/klauspost/compress v1.20.2
  (was v1.19.2): it fixes a panic on crafted zstd frames (GO-2026-6865,
  GHSA-fpmh-frq8-7rj8), which govulncheck reports reachable through the
  store of step output. The store only decodes what it compressed itself.
- CVE-2026-78661 in golang.org/x/net/html v0.61.0 (in the image; govulncheck
  finds no call path) is accepted until 2026-11-24: v0.62.0 with the fix
  is taken once it is two weeks old.
- The default BuildKit v0.34.2 and gitlab-runner alpine-v19.5.1 images
  carry CVE-2026-123158 (openssh), CVE-2026-113351 and CVE-2026-115506
  (libexpat) and CVE-2026-78661 (golang.org/x/net/html), with no release of
  either that fixes them yet (gitlab-runner alpine-v19.5.3 has the same):
  accepted until 2026-12-01 and looked at again then.

## 0.5.1 - 2026-11-04

### Upgrade notes

- From 0.5.0, `helm upgrade` in place. The controller, the web and the
  routers restart once for the new image; the daemons, their disk probe
  and the main runner stay as they are. Builds stored before the upgrade
  keep, as a rule, the reasons they were given; new builds get the
  corrected ones.

### Fixed

- Rebuild reasons, found on moby/moby's own Dockerfile:
  - a git or HTTP source (`ADD https://…git`) is fetched again in every
    build; the console took it for the first rebuilt step and said
    "Dockerfile changed: … this step changed" with the same text before
    and after. It now passes over such a source, as it does over a base
    image, and explains the step that used it;
  - a step that builds on the build context and on a source fetched
    from the network was "fed by a remote source" (unclear) even when
    GitLab's comparison of the commits showed a changed file it copies.
    The context now comes first, so the comparison can prove the change;
    with no change in the repository it stays unclear (never the build
    farm's, whatever `trustGitComparison` says);
  - a RUN step after a changed `ENV` line read "this step changed" between
    two equal texts. The page now says its environment changed and shows
    the two `ENV` lines.
- `quickstart.sh` no longer ends by saying the console cannot read
  GitLab when a GitLab token was given (with `--gitlab-token-file`, or
  kept from an earlier run).

## 0.5.0 - 2026-11-04

### Upgrade notes

- From 0.4.1, `helm upgrade` in place. The controller, the web and the
  routers restart once for the new image; the daemons, their disk probe
  and the main runner stay as they are. Nothing changes in the store:
  the hints are worked out from what it already keeps, also for builds
  stored before the upgrade.

### Added

- Cache-mount hints in the console ([ADR-0058](docs/adr/0058-cache-mount-hints.md)):
  a build step that runs a package manager without a cache mount, or
  with one that cannot work, gets a ready fragment to put in the
  Dockerfile. The page shows how often the step ran in the last 30 days,
  how long that took in all, and the median. Covers about thirty
  managers, among them apt, apk, dnf, zypper, pip, uv, Poetry, conda,
  npm, Yarn, pnpm, Bun, Go, Cargo, Maven, Gradle, NuGet, Composer,
  Bundler, pub, Hex and Deno.
  The hints are shown:
  - in a Speed-ups card on the build page;
  - as a marker on the step;
  - on the Dockerfile tab;
  - on a Speed-up tab in the step's panel;
  - as `hints` in `GET /api/v1/builds`.
- [docs/dockerfile-guide.md](docs/dockerfile-guide.md): cache mounts for
  non-root steps (`uid`/`mode`), on every later step that uses the
  packages, empty under `--no-cache`, and the common mounts that do
  nothing.

## 0.4.1 - 2026-11-03

### Upgrade notes

- From 0.4.0, `helm upgrade` in place. The controller, the web and the
  routers restart once for the new image; the daemons, their disk probe
  and the main runner stay as they are.
- One-machine installs: run 0.4.1's `quickstart.sh` the same way as the
  first time ([docs/upgrading.md](docs/upgrading.md#one-machine-installs-quickstartsh)).
  A GitLab token that a second run of an earlier script added is kept
  from now on, as long as the running release still uses it; if a later
  run already dropped it, pass `--gitlab-token-file` once more.

### Changed

- `quickstart.sh`, run again to upgrade: it stops on a 0.2.x install
  before it changes anything on the machine (it upgraded cert-manager and
  rewrote the runner's Secret first); it keeps using a GitLab token that
  the running release uses, also when `values.yaml` does not name it (a
  later run dropped it), and leaves out one the release no longer uses; it says that k3s stays as installed instead of
  naming the version it would install; the command it prints for applying
  `values.yaml` waits up to 15 minutes.
- Upgrading has a page of its own, [docs/upgrading.md](docs/upgrading.md):
  Helm, the one-machine install (what a second run of the new release's
  `quickstart.sh` changes and what it leaves alone), GitOps, checks,
  rollbacks and the moves from 0.2.x and 0.1.0.
- Documentation brought up to 0.4.0: git contexts in the architecture,
  troubleshooting and the network policies, the e2e against GitLab CE,
  the disk probe's load figures in the buildkit chart's README.

## 0.4.0 - 2026-11-03

### Upgrade notes

- From 0.3.11, `helm upgrade` in place. The controller, the web and the
  routers restart once for the new image; the main runner stays as it is.
  The store gets two more columns, which 0.3.11 ignores: a rollback keeps
  working on the same store.
- **The daemons restart once** for BuildKit v0.34.2 (a security release,
  see below), one node at a time: the builds running on a node finish
  first (up to `terminationGracePeriodSeconds`). With the default
  `emptyDir` storage the restarted daemons start with an empty cache; with
  `hostPath` they keep it.
- The component's `include` changes with the release (its integrity hash
  is in the release notes and the box's NOTES); jobs that keep the old
  include keep working.
- The component's job now `extends` a template of its own
  (`.cia-checkout-local` or `.cia-checkout-git`). With the default
  `local` it sets nothing: an `extends:` you add to the job replaces it
  without changing anything.

### Added

- The CI component builds from a git context with
  `context-source: git`: the build job checks nothing out, and the build
  node fetches the pipeline's commit itself (only that commit, into a copy
  of the repository it keeps), with the job token as a BuildKit secret.
  Other jobs of the pipeline keep their checkout. Meant for repositories
  with a large history; see the component's README for what a git context
  leaves out (`.dockerignore`, CI artifacts, Git LFS, private submodules)
  and [ADR-0057](docs/adr/0057-builds-from-a-git-context.md).
- The console reads a git context's commit as the build context: a new
  commit is "your change" or not by comparing the commits (which holds in
  full for a git context), and a rebuild of the same commit is the build
  farm's.

### Security

- BuildKit v0.34.2 for the daemons, the CI component's client image and
  the image build, at once under the security-patch rule
  ([ADR-0041](docs/adr/0041-security-patch-exception.md)): it fixes a
  daemon panic when a build resolves the same git source twice
  (GHSA-2fqp-xg6f-v7gj, which git contexts make reachable) and a client of
  the control API setting extended attributes on files outside the state
  directory (GHSA-w26x-xg27-7qjc).

### Changed

- GitLab's comparison of two commits now tells a submodule's new commit
  from a file: it counts for everything the build copies from inside the
  submodule, and for a context folder inside it. Comparisons stored by
  earlier releases are asked again when a git context's rebuild needs
  them.
- The CI component no longer fails a job after a good build when BuildKit
  prints a step twice (its summary used `head` under `pipefail`), and
  refuses a `timeout-minutes` that is not a whole number before the build.
- The console's project switch builds its redirect from the checked
  parts of the address it returns to (a CodeQL finding; it could not lead
  elsewhere before either).
- Renovate has no schedule of its own; the workflow's weekly run decides
  (a run GitHub started late fell outside the old window).

## 0.3.11 - 2026-11-02

### Upgrade notes

- From 0.3.10, `helm upgrade` in place. The controller, the web and the
  routers restart once for the new image; the daemons, their disk probe
  and the main runner stay as they are. A router's restart, like every
  release's, cuts the builds it still carries after
  `control-plane.router.drainSeconds` (300 s by default; production with
  long builds: 1200-1800 s, with a longer `--timeout`).
- **Rebuilds the console called "build farm" because GitLab showed no
  change in the repository become "unclear"** when the controller starts:
  the comparison cannot see CI artifacts or generated files in a build
  context. Set `control-plane.reasons.trustGitComparison=true` to keep the
  old reading if your build contexts hold repository files alone (it can
  be switched either way later; GitLab is not asked again). Insights'
  figures change accordingly for the last 30 days.
- **The jobs namespace gets a NetworkPolicy** (`jobsNamespace.networkPolicy`,
  on): nothing connects into a job pod, and job pods reach everything but
  cloud instance metadata (`buildkit.networkPolicy.egress.blockedCIDRs`).
  It is created also when the namespace is not the release's
  (`jobsNamespace.create=false`). Turn it off with
  `jobsNamespace.networkPolicy.enabled=false`.
- **Runner tier pods get a NetworkPolicy** without ingress: their
  `:9252` (health, metrics and an unauthenticated job list and pprof) is
  reachable only by the kubelet's probes. A scraper of their metrics needs
  `runnerTiers.<tier>.manager.metricsFrom`.
- **The console answers only its own host names when it knows its address**
  (`global.console.url`, without sign-in): `localhost`, IP addresses and
  `*.svc` names still work. If people reach it under another name, set
  `global.console.url` to that name.
- **The CI component** no longer marks its job `interruptible` by default
  (cancelling a build whose steps another build shares can fail that build,
  moby/buildkit#7268); `interruptible: true` brings it back. It passes the
  context folder to BuildKit (`--opt ci-accelerator.io/context`, ignored by
  the Dockerfile frontend, no cache key changes), so its integrity hash
  changes: update an `include` with `integrity`.

### Changed

- **"Build farm" only when it is proven**
  ([ADR-0056](docs/adr/0056-build-farm-only-when-proven.md)). No change in
  the repository is no longer taken for no change in the build context: such
  a rebuild is unclear, and its page says what GitLab's comparison showed
  and what it cannot see. A witness from the same pipeline counts only when
  it read the same context folder (or is a build of the same image), and a
  verdict the settling made itself is never a witness. Builds with
  `--no-cache` count as the job's own choice, and a step behind a download
  without a checksum (`ADD https://...`, a git branch) as unclear, naming the
  source without its credentials.
- **A change in a stage shared with another job** is named: a build that
  received the changed stage from a parallel job said "this step changed"
  with the same text on both sides; it now names the build argument or
  the line that changed.
- **One Dockerfile for several images with one target** (`--target app`
  for each service): each build is compared with its own repository's
  previous build, not with the other service's.

### Fixed

- A pipeline search or the home page held the controller's database for
  seconds at a few hundred thousand jobs: the store now keeps the query
  planner's statistics, and the pipeline queries name their index.
- A build's Dockerfile line ranges are kept within the file: ranges from a
  client's source map could make one page view walk billions of lines.
- Ids beyond GitLab's range and times in the future from job variables
  (`CI_PIPELINE_ID`, `CI_PIPELINE_CREATED_AT`, `CI_JOB_STARTED_AT`) are not
  stored (a pipeline id of 2^63 made the project's Pipelines page fail),
  and a project's name comes from the job's URL, which the job cannot set.
- A job's second build no longer shows the first build's time as
  "checkout and the job's script"; the waterfall shows the runner's wait
  once per job.
- The build page found the router's decision for a job only among its
  first 50 connections.
- After one failed connection to the only daemon of a node or zone, the
  router turned jobs away for 30 s; it now tries that daemon again.
- The web and the routers notice a controller that stopped answering
  within about 20 s (HTTP/2 pings), not at each call's timeout.
- After a top cache hit, a build restored from the store read every
  changed step as new.
- The CI component no longer exports the digest of an earlier job's
  `build-metadata.json` when `push: false` or the build fails, and names
  `IMAGE` only for an image it pushed.
- `quickstart.sh --uninstall` removes only what the script installed: it
  refuses without its install's marker, and keeps a `--build-dir` that
  existed before the install.

### Added

- The published image carries the licences and notices of every module
  linked into it (`/licenses`) and the `org.opencontainers.image.licenses`
  label.
- A release is built only from a commit on `main` or a release branch, and
  its `SHA256SUMS` is signed (`SHA256SUMS.sigstore.json`, `cosign
  verify-blob`).
- A weekly scan of the third-party images the charts run by default
  (BuildKit, gitlab-runner), with dated grants for findings that have no
  release to move to (`make third-party-scan`).
- **Renovate keeps the pins current** (docs/dependencies.md): a weekly
  run from the repository's own workflow, as a GitHub App, opens one pull
  request per subject (the Go toolchain, BuildKit, the gitlab-runner chart
  with its image, cert-manager, k3s, Helm, kind and kubectl, the test
  tools, Node, the Go modules, the GitHub Actions) for versions at least
  two weeks old, with the checksums next to the versions recomputed from
  where each project publishes them (`scripts/update-pins.sh`, also for
  bumps by hand). `make renovate-check` shows every pin is found, `make
  pins-check` compares every sum with upstream's. The CI component's
  `build.yml` gained one comment line, so its integrity hash changes with
  the next release.

## 0.3.10 - 2026-10-31

### Upgrade notes

- From 0.3.9, `helm upgrade` in place. The controller, the web and the
  routers restart once for the new image; the daemons, their disk probe
  and the runner stay as they are.

### Changed

- **The console compresses its responses** (gzip or zstd, whichever the
  browser accepts) for pages, JSON, text, the search index and its
  stylesheet and script: a pipeline's page goes as 8.7 KB instead of
  87 KB, the home page as 7 KB instead of 46 KB. That counts wherever the
  console is reached through a tunnel, a VPN or a slow proxy; the live
  updates' event stream, images and responses under 1 KB go as they are.

## 0.3.9 - 2026-10-31

### Upgrade notes

- From 0.3.8, `helm upgrade` in place. The controller, the web and the
  routers restart once for the new image; the routers' DaemonSet now rolls with
  `maxSurge: 1, maxUnavailable: 0` (no pod restarts for that alone). The
  controller adds an index of executed steps at its first start (no
  schema change: a rollback keeps working).
- Refused now, with a message saying what to change: the standalone
  control-plane chart with `gitlab.token` and more than one `gitlab.urls`
  (the token was sent to each GitLab: use sign-in or one URL); in the box,
  runner tiers named `gitlab` or `caches`, and another
  `buildkit.service.port` without the router.
- The CI component's integrity hash changes (see the release notes).

### Fixed

- Live tail: a progress bar that redraws its line no longer freezes at
  an early percentage, and a cut drawing can no longer show the first part
  of a secret; secret build arguments of several lines and armored PGP
  private keys are masked (PGP keys also in stored output).
- Router: a daemon still listed but not reachable (a lost node) no
  longer fails the jobs routed to it: the router tries the next choice and
  passes that daemon over for 30 seconds. A rollout starts each node's new
  router before the old one drains.
- Sign-in: two tabs signing in at once both completed nothing; the
  second completes now. Behind an ended session the pages' scripts no
  longer start sign-ins at GitLab every few seconds: the page stops
  updating and says so.
- Control plane: the installation's GitLab token is refused for more
  than one GitLab; in the box, the controller's certificates use the
  global issuer's kind and group, not only its name.
- Console: pipelines created in the same second at a page's edge are
  no longer skipped; a job that built twice is drawn by its newest build;
  steps received from another job carry no CPU or memory of their own and
  wait for their own stage only; pick mode hears of the pinned project;
  projects of several GitLabs are told apart; huge numbers in an address
  are 404, not 500; with sign-in a reader's refused commit comparison is
  not stored for everyone.
- Store: saving a build no longer walks every cached run of each step
  under the write lock (1.4 s per save at 60,000 builds); a read cut short
  reports its error.
- Box and component: clashing tier names and another daemon port are
  refused instead of rendering broken objects; the component no longer
  fails a job after a successful build whose log has no context-transfer
  line, and trims indented build-args and secrets lines;
  `scripts/buildkit-certs.sh` names a missing option value and keeps
  wildcard SANs as written.

## 0.3.8 - 2026-10-31

### Upgrade notes

- From 0.3.7, `helm upgrade` in place. The controller, the web and the
  routers restart once for the new image; the web also gets
  `CI_ACCELERATOR_PUBLIC_URL` (from `signIn.url` or `global.console.url`).
  The daemons, their disk probe and the runner stay as they are.

### Added

- **Link previews:** a console link shared in Telegram, Slack or another
  chat shows a card with the page's title, its project, the sentence the
  page leads with (a build's verdict, a pipeline's summary, the insights'
  figures) and the console's image. With sign-in no card shows a project:
  chat apps get the sign-in page's card. Set `global.console.url` (or
  `signIn.url`) so the cards' addresses are where people reach the
  console; without it the request's host is used.

### Fixed

- A step's panel closed when a stage was folded on a build page, or a row
  of a pipeline's timeline (or its depth and filter buttons) was used
  (0.3.7). Only a click on the page's free space closes it now.

## 0.3.7 - 2026-10-30

### Upgrade notes

- From 0.3.6, `helm upgrade` in place. The controller, the web and the
  routers restart once for the new image; the daemons, their disk probe and
  the runner stay as they are.
- The console's home page (`/`) is now Pipelines; the page that was
  Overview is Insights, at `/insights` (keys `g s`). Links to `/pipelines`
  keep working.

### Changed

- The console opens on Pipelines, the page most visits start from. The
  former Overview (why images were rebuilt, by the build time it cost) is
  called Insights and comes second in the menu.
- The console opens in its dark theme, as the project's site does, until
  the viewer picks the light one (`t` or the sidebar); it no longer follows
  the system's colour scheme. A light theme picked earlier is kept.
- A step's panel closes on a click outside it (a click on another step
  moves it there), and its left edge widens it; the width is kept in the
  browser, a double-click on the edge restores the default.

### Fixed

- Signed in without a readable project, the home page says why it is
  empty, as the former Overview did.

## 0.3.6 - 2026-10-30

### Upgrade notes

- From 0.3.5, `helm upgrade` in place. Nothing in the box changed: the
  controller, the web and the routers restart once for the new image's
  version; the daemons, their disk probe and the runner stay as they are.

### Added

- **A live demo:** [demo.ci-accelerator.io](https://demo.ci-accelerator.io)
  is the console of a ci-accelerator box that builds this repository on
  every push, through its read-only GitLab mirror
  [gitlab.com/ci-accelerator/ci-accelerator](https://gitlab.com/ci-accelerator/ci-accelerator).
  The console runs without sign-in and reads GitLab with a fine-grained
  token that may only read the mirror; it is reached through a Cloudflare
  Tunnel (no open port on the box).
- The project builds itself on its own build farm: every branch pushed to
  GitHub goes to the mirror, whose pipeline builds the image (pushed to
  the mirror's registry with the job's token, `:edge` for main) and runs
  `go test` and `go vet` as builds of their own
  (`ci/showcase/test.Dockerfile`, Go caches in cache mounts). The three
  jobs start together and share their first steps on the daemon. The
  commit on GitHub gets the status "ci-accelerator / own build farm".

## 0.3.5 - 2026-10-30

### Upgrade notes

- From 0.3.4, `helm upgrade` in place. The controller, the web and the
  routers restart once (a new image); the daemons and their disk probe
  stay as they are.
- To see builds live, jobs need the CI component of this release (or
  `--opt "ci-accelerator.io/traceparent=$TRACEPARENT"` on a `buildctl
  build` of their own); its `include` integrity changes. Builds without it
  show once they complete, as before.

### Added

- **A running build shows live** on its job's page: what runs now, how
  long the rest likely takes against the last build of the image that ran
  the same step, the node's CPU and memory, how many builds the daemon
  runs and how long tasks waited for a core, and every step on a timeline
  that grows as the build goes. A step opens to its last 8 lines (secrets
  masked); the running step opens by itself and folds when it is done,
  while steps the reader opened stay open, and a step's lines can be
  paused. The page follows the build every 2 seconds and becomes the
  build's page when BuildKit completes it. On the pipeline's timeline a
  running job opens into its stages and steps so far, and folds once it
  is done. ([ADR-0053](docs/adr/0053-running-builds-live.md))
- The CI component passes the job's `TRACEPARENT` as the build option
  `ci-accelerator.io/traceparent`, which names the build's job while it
  runs. The Dockerfile frontend ignores it; cache keys do not change.

## 0.3.4 - 2026-10-30

### Upgrade notes

- From 0.3.3, `helm upgrade` in place. The controller and the routers
  restart once (a new image, and their pod templates read the GitLab URLs
  from a file now); the daemons and their disk probe stay as they are
  (the probe keeps the v0.3.3 image).
- Images pinned by digest: set `control-plane.image.digest` to the v0.3.4
  digest from the release notes; `buildkit.diskProbe.image.digest` keeps
  the v0.3.3 one.
- The charts now refuse a few settings that never worked, at render time
  instead of failing later. Each message says what to change:
  - `control-plane.router.enabled` different from `global.router.enabled`
    in the box (use `global.router.enabled` only);
  - `gitlab-runner.enabled=false` with the control plane on and no GitLab
    URL for it: neither `gitlab-runner.gitlabUrl`, nor
    `control-plane.gitlab.urls`, nor your own
    `control-plane.gitlab.urlsFrom.configMap` (the controller and routers
    crash-looped);
  - a runner tier on `nodes: any` while the jobs' Service routes `Local`
    (its jobs could not reach a router);
  - `buildkit.storage.emptyDir.sizeLimit` with a custom `config.gcPolicy`
    that has no unfiltered absolute `maxUsedSpace`;
  - GC sizes under `buildkit.config.workerOCI` (`reservedSpace`,
    `maxUsedSpace`, `minFreeSpace`, `gckeepstorage`): buildkitd ignored
    them; set `config.gc` instead;
  - changing `buildkit.priorityClass.value` or `preemptionPolicy` after
    install (the message gives the `kubectl delete priorityclass` way);
  - switching `jobsNamespace.create` to false on an installed release
    before the namespace carries `helm.sh/resource-policy=keep` (Helm
    deleted the namespace and the running jobs in it).
- `s3.pathStyle: false` for the runner cache now takes effect; installs
  that set it see one runner configuration change.
- Multi-platform images: the first rebuild after the upgrade can show as
  "unclear: nothing on record to compare with" when its previous build was
  stored by 0.3.3, the first rebuilt step is of a later platform and its
  text did not change (0.3.3
  recorded only the first platform's step definitions); from the next
  build on they are compared normally.
- **Console admins are named by GitLab user id only**: write
  `signIn.gitlab[].admins` as `["id:<n>"]` (the number on the person's
  GitLab profile page). Usernames are refused by the chart and the
  controller: GitLab gives a renamed or deleted account's username to the
  next account that takes it, which would then be an admin.

### Security

- Console with sign-in: a CI job chooses its own trace id (and can set
  `CI_PIPELINE_ID`), so a member of one project could open another
  project's build pages (the previous build's Dockerfile steps, build
  argument values, commit) by naming their own pipeline and the other
  project's job, or claim a pipeline id before that pipeline existed
  (pipeline ids are sequential). A build now belongs to its job's project
  only when the job is of the pipeline the trace names and no other
  project owns that pipeline; when the projects disagree, GitLab's answer
  decides whose the pipeline is (asked when a build page opens; each
  project's read is its own), and until then only admins see the build.
  A build whose job's pod the console never saw is its pipeline's
  project's only once GitLab has confirmed the pipeline. A build page
  lists only its job's builds of its own pipeline, and the link in a job's
  log opens no build when the builds naming an unknown job disagree on the
  pipeline. README Limitations says what a job can still do with its trace
  id.
- A build whose job's pod was never seen kept no project, and its step
  history listed every project's runs of the same step; it now takes its
  project from GitLab's job list and its history stays within it.
- A build with more than 4096 steps could show another build's step
  output (possibly another project's) for its later steps.
- Dockerfiles are masked like step output before they are stored and
  shown (the Dockerfile tab printed tokens written into a Dockerfile);
  token formats right after a letter (a literal `\n` in a `printf`) are
  masked too; a private key cut by the edge of a job log part is masked;
  the job log endpoint no longer takes `?lines=`.
- Sign-out ends the session even when the browser leaves or GitLab is
  slow, and no longer says the GitLab access was revoked when GitLab did
  not confirm it.

### Fixed

- The controller ran out of memory on a build whose steps printed
  hundreds of MiB (and again on every start until the record left the
  daemon's history); logs are now read as a stream within bounded memory.
- The controller's memory grew by about 11 MB per project and day of
  kept step output (OOM within a week at the default 1Gi limit).
- A restarted controller (or a broken history stream) stored builds
  older than two hours again without the steps they received from other
  builds: received steps turned into built ones and reasons and figures
  changed.
- A daemon whose address briefly left the Service could freeze the
  collection of builds and the Nodes page; replaced daemon pods no longer
  leave their last two hours of builds in memory.
- Reading a job's log no longer spins forever on a `?before=` past its
  end, nor floods GitLab when an empty log answers every range with 416.
- Multi-platform builds: the steps of the second and later platforms were
  shown as received from no build, their usage dropped and their
  rebuilds ignored.
- Images without a Dockerfile target: the previous build is found however
  old, "files or node" is settled against that image's own previous
  commit, and the usual time and slow mark are each image's own.
- A build after a failed or canceled build of the same image was
  reported as "Dockerfile changed: this step is new"; the previous build
  is the last one that did not fail, and failed builds after a build stored
  late are compared again with it.
- A build whose job became known only from GitLab's job list (the
  controller was away while its pod ran) was never compared, nor was the
  next build of its image with it; such builds are now compared again until
  it succeeds (the store gains a column and an index on start, which 0.3.3
  ignores after a rollback).
- When a build printed so much that a step's log was cut at the console's
  limit, the shown start of that log could contain part of a secret.
- A GitLab token renewal cut off by a closed page lost the rotated token
  and signed the person out; renewals queued behind a failed one share its
  failure instead of hitting GitLab one after another; a rate-limited
  permission check waits as GitLab asks, and the home page says GitLab did
  not answer instead of "no project you can read".
- A brief Kubernetes API outage turned every console page into a 500; the
  controller keeps recently confirmed tokens and answers 503 otherwise.
- Changing `gitlab-runner.gitlabUrl` now reaches the controller and the
  routers without a manual restart.
- The router took a job's zone only from a daemon on the job's node: while
  that node's daemon was between pods (a rollout, an eviction) the job
  lost its zone and `crossZone: false` was ignored. A node now keeps its
  zone; a node that never had a daemon still counts every daemon as its
  zone, as before.
- `buildkit.extraEnv` can override variables the chart sets (duplicate
  names were rejected by server-side apply).
- The runner cache page: a lifecycle rule that does not expire the
  runners' folder no longer hides the "never deleted" warning, and
  `Shared=false` runners' copies of a key are summed.
- GitLab CI component: builds with `push: false` on a GitLab without a
  container registry; base images from the Dependency Proxy get the job's
  credentials; a File-type variable as a secret mounts the file's
  content; a timeout is reported as one and a buildctl that ignores
  SIGTERM is killed.
- `quickstart.sh`: a second run keeps the build cache of `values.yaml`
  (and refuses another `--build-dir`), `--uninstall` removes the cache
  directory actually in use, and a second run on a disk the cache filled
  warns instead of stopping.
- Docs: "Uninstall completely" deleted the CA Secret of any other box
  with the same release name; GC policies with both an age and a cap are
  described correctly; the runner integration installed box 0.3.0.
- CI: changes to `.dockerignore`, `LICENSE` and `scripts/vulncheck.sh`
  run the checks that depend on them, a moved file runs the checks of the
  place it left, the release gate scripts run on every chart change, and
  the weekly scan covers the disk probe's image while it lags behind.

## 0.3.3 - 2026-10-29

### Upgrade notes

- From 0.3.2, 0.3.1 or 0.3.0, `helm upgrade` in place. **Every daemon
  restarts once**: the disk probe next to each daemon moves to the v0.3.3
  image (it now reports load), and its pod template gains a read-only
  mount of the host's `/sys/fs/cgroup` and two environment variables. A
  `hostPath` cache is kept; the default `emptyDir` cache starts empty, as
  on every daemon restart. The rollout goes one node at a time; builds
  running on a node finish first (up to `terminationGracePeriodSeconds`).
- **Images pinned by digest** (as the README tells production installs):
  set both `control-plane.image.digest` and `buildkit.diskProbe.image.digest`
  to the v0.3.3 digest from the release notes (one image, two roles). The
  charts refuse a kept v0.3.2 control-plane digest or a kept v0.3.0 probe
  digest and say which version they run.
- **Mirrors:** mirror `ghcr.io/hoobiboobi/ci-accelerator:v0.3.3`; the probe
  no longer runs `v0.3.0`.
- A probe kept on an earlier image on purpose (`buildkit.diskProbe.image.tag`
  set) keeps working and keeps reporting the disk; the Nodes pages then say
  "no answer" for load.
- The daemon's own share of its node (CPU, memory) is measured on cgroup v2
  nodes; on cgroup v1 the pages show the node's figures and say the
  daemon's share is not measured.
- The store gains a column (`steps.mem_bytes`) without a new schema
  version: `helm rollback` to 0.3.2 keeps working on the same store (0.3.2
  writes 0 there).

### Added

- A build's page has a **Resources** tab: the executed steps by CPU time
  with the cores each used on average over its run, its run time, the
  most memory its cgroup held in one of BuildKit's samples and the bytes
  it wrote; a chart of the cores the running steps used together over the
  build, against the node's cores when the probe reports them, saying for
  how long the steps wanted more cores than the node has; and the build's
  totals. The figures under the verdict add the cores busy on average.
  Memory is recorded for builds from this release on; the step drawer
  names it too.
- The Nodes page draws each daemon's node over the last hour (CPU and
  memory) next to the latest reading, with a "waiting for CPU" chip when
  the node's pressure stall information says some task waited for a core
  10 % or more of the last minute. A daemon's page has a Load section: the
  node's cores and memory split into the daemon's pod, the rest of the
  node and what is free, the pressure (or that the kernel reports none),
  the builds running, and the last hour drawn with axes for cores, memory
  and time. The node's memory is total minus available, the daemon's its
  working set (the kubelet's measure), so the two add up.
- The disk probe serves `GET /load` (the node's and the daemon pod's CPU
  and memory, read from `/proc` and the pod's cgroup every 10 s, an hour
  kept) next to `GET /disk`.
- The Builds list's image column can be widened: drag its border,
  double-click it to fit the longest name (and again to go back), or use
  the arrow keys; the width is remembered.

### Fixed

- A pipeline that built one image said "All 1 image were rebuilt"; it says
  "The image was rebuilt because of the change" (or came from the cache,
  or failed).

## 0.3.2 - 2026-10-29

### Upgrade notes

- From 0.3.1 or 0.3.0, `helm upgrade` in place. The control plane's image
  moves to v0.3.2, so the controller, web and router pods restart once;
  the daemons and their disk probe stay as they are. A control plane pinned
  by digest
  (`control-plane.image.digest`, as the README told production installs)
  needs the new release's digest from its notes: the chart now refuses the
  digest of an earlier release under its own tag, which would run that
  release under the new name.
- On its first start the controller removes the builds 0.3.0 and 0.3.1
  stored twice after a daemon pod was replaced on a hostPath, and sums up
  the images' daily figures again from the first such day. Failed builds
  still in BuildKit's history get their Dockerfile then; older ones stay
  without.
- Pipelines that include the GitLab CI component of 0.3.1: include the
  0.3.2 component (the release notes print its include with its
  integrity). The schema
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
