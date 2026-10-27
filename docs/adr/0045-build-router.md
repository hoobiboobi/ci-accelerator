# 0045. The build router: each job to the daemon where its project is warm

Date: 2026-10-27

## Context

Every build node keeps its own cache. With the jobs' Service preferring
the job's own node, a project's builds land wherever its jobs were
scheduled: on the first real pipelines its jobs rebuilt everything on
nodes that had never built it, and on a monorepo 63 % of the rebuilds the
console explained were "the previous build was on another node". The
parallel jobs of a pipeline also only share their common steps (BuildKit
runs an identical step once) when they meet on one daemon.

## Decision

- A router role of the control plane's binary, a DaemonSet on the build
  nodes behind a Service that prefers the job's node. The box's runners
  connect to it; the daemons admit only the routers and the controller.
- It passes the bytes through and never terminates TLS: the job and the
  daemon keep their mutual TLS, the daemons' certificate names the
  router's Service, the router holds no key and reads nothing of a build.
- It knows a job by its pod's address (the job pods are watched), the
  daemons by their headless Service's EndpointSlices (address, node,
  zone, readiness: no access to Nodes).
- The decision is a pure function: the job's node if its cache holds the
  project's layers used in the last week; else the zone's node with the
  project's most recently used layers; else the project's node by rendezvous hash over the zone's
  ready daemons (every router agrees, so a new project's parallel jobs meet
  on one daemon); else another zone, or no daemon (`crossZone`). Warmth is
  kept by node, so a daemon's restart keeps its projects. A connection
  from a pod that is no CI job goes to its own node's daemon.
- The controller serves the routers' view: each project's builds by
  node in the last week (a daemon replaced since counts only when its
  cache survived, hostPath), the routers' decisions of the last hour for
  daemons still running, builds running per node;
  and stores their decisions; the build page says why its node was chosen.
  A router keeps routing on its last view while the controller is away.
- SIGTERM stops taking connections and lets open ones (running builds)
  finish for up to `drainSeconds`.

## Consequences

- The jobs' context upload may cross nodes; the warm cache usually saves
  far more than that costs.
- Identity by pod address needs the source address to reach the router
  unchanged, which is how in-cluster Services work on common CNIs; behind
  SNAT a job is "unknown" and gets its own node's daemon (as before).
- Per-project cache mount namespaces or authorization per project would
  need the router to read the build requests; it does not (see 0046).
