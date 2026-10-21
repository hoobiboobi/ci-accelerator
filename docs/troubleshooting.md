# Troubleshooting

Symptoms first, then runbooks. Commands assume the release `ci-accelerator`
in namespace `ci-accelerator` with jobs in `ci-accelerator-jobs`; adjust
the names. How the parts fit together is in
[architecture.md](architecture.md).

Talking to a daemon from inside its pod needs an empty environment, as the
probes do, or tracing variables can make `buildctl` hang on exit:

```sh
kubectl -n ci-accelerator exec <daemon pod> -- env -i OTEL_TRACES_EXPORTER=none \
  /usr/bin/buildctl --addr unix:///run/buildkit/buildkitd.sock <command>
```

## The install fails

The chart stops on purpose when the install could not work. The message
names the fix; the common ones:

| Message starts with | Fix |
|---|---|
| `namespace ... enforces Pod Security level` | Label the box namespace `pod-security.kubernetes.io/enforce=privileged` |
| `no node matches the build-node selector` | Label and taint the build nodes, or `global.buildNodes.enabled=false` |
| `tls.source=certManager, but the cluster has no cert-manager` | Install cert-manager (command in the message); for `helm template`, pass `--api-versions cert-manager.io/v1` |
| `gitlab-runner.gitlabUrl is required` / `gitlab-runner.runners.secret is required` | Set them |
| `Secret .../gitlab-runner-token ... has no runner-registration-token key` | See [The runner](#the-runner) |
| `runner-token does not look like a token` | The Secret holds a placeholder or a pasted string with spaces; re-create it |
| `Secret ... (tls.existingSecret) has no ca.crt` | `kubectl create secret tls` writes no `ca.crt`; use `create secret generic` with all three files |
| `Namespace ... (global.jobs.namespace) exists and was not created by this release` | Helm cannot adopt a namespace it did not create: `jobsNamespace.create=false` to use yours (label it `baseline`), or delete it |
| `Namespace ... (global.jobs.namespace) is still being deleted` | A reinstall right after uninstall: wait for the namespace to go |
| `gitlab-runner.runners.config does not render to valid TOML` | Your own `runners.config` (or a value it uses) breaks the TOML; the message names the line |
| `global.buildNodes.selector has kubernetes.io/arch=...` | Set `global.jobs.architecture` to the same architecture |
| `image.tag is ..., but image.digest is still the digest of ...` | Set `image.digest` to the new tag's digest, or `""` |

Checks that read the live cluster (Pod Security, build nodes, Secrets)
need read access to Namespaces, Nodes and Secrets. Each one has a
`preflight.*` switch for installers without it.

## The daemons do not start

```sh
kubectl -n ci-accelerator get ds ci-accelerator-buildkit
kubectl -n ci-accelerator get pods -l app.kubernetes.io/name=buildkit -o wide
kubectl -n ci-accelerator describe pod <daemon pod> | tail -20
```

| Symptom | Cause |
|---|---|
| `DESIRED 0` | No node matches the selector (build nodes not labelled) |
| `Pending` | Taint without the matching toleration, or no resources left on the node |
| `ContainerCreating`, event about the TLS Secret | The certificate is not issued yet: `kubectl -n ci-accelerator get certificate`, then `kubectl describe certificate` |
| `CrashLoopBackOff`, log `failed to load config` | A configuration buildkitd rejects (often `config.extraConfig`). The rollout stops on the first node; `helm rollback` |
| `CrashLoopBackOff` with hostPath, lock error | Another daemon holds the state directory: a second release on the same path, or a surge pod (the chart refuses `maxSurge` with hostPath) |
| No pods at all after a successful `helm install` | The namespace enforces `baseline`/`restricted` without a label the chart could see (some distributions default to `baseline`); see the DaemonSet's events |

## The runner

The runner chart mounts **both** keys of its token Secret:

```sh
kubectl -n ci-accelerator create secret generic gitlab-runner-token \
  --from-literal=runner-token=glrt-... --from-literal=runner-registration-token=
kubectl -n ci-accelerator get secret gitlab-runner-token \
  -o go-template='{{range $k, $v := .data}}{{$k}} len={{len $v}}{{"\n"}}{{end}}'
```

| Symptom | Cause | Fix |
|---|---|---|
| Runner pod in `ContainerCreating`, event `FailedMount ... non-existent secret key: runner-registration-token` | The empty key is missing | `kubectl -n ci-accelerator patch secret gitlab-runner-token --type merge -p '{"data":{"runner-registration-token":""}}'` |
| Log `Verifying runner... is not valid` | Wrong or placeholder token, or the runner was deleted in GitLab | Fix the Secret, then `kubectl -n ci-accelerator rollout restart deploy/ci-accelerator-gitlab-runner` (the token is read at start) |
| The runner is online, jobs stay pending or run elsewhere | Tags: a runner with tags that does not run untagged jobs only takes jobs with matching `tags:`. On gitlab.com untagged jobs go to the shared runners, which have no `BUILDKIT_*` variables | Add `tags:` to the job, or allow untagged jobs on the runner |
| Runner pod `Pending` (`Unschedulable`), `helm install --wait` times out on the runner Deployment; the same for the VictoriaMetrics/VictoriaTraces pods | Every node is a tainted build node: the runner manager and the storage do not tolerate the build-node taint | Keep one node without the taint, or add tolerations: `gitlab-runner.tolerations`, `victoria-metrics-single.server.tolerations`, `victoria-traces-single.server.tolerations` |
| Job pod `Pending` | It cannot go to a build node: missing toleration or node selector in a hand-made runner configuration | Use the configuration from the umbrella or `integrations/gitlab-runner` |
| Job fails pulling `gitlab-runner-helper:x86_64-...` with `no match for platform` | arm64 build nodes, but the runner assumed amd64 | `global.jobs.architecture=arm64` (or `kubernetes.io/arch` in the fragment's node selector) |
| Jobs rejected by Pod Security | The jobs namespace enforces `restricted` | Use `baseline` |

## A job cannot reach the daemons

Run the connection check from the job (or from a debug pod in the jobs
namespace with the client Secret mounted):

```sh
buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" debug workers -v
```

| Error | Cause |
|---|---|
| `x509: certificate signed by unknown authority` | The client's `ca.crt` is from another CA than the daemons' certificate (a Secret from another installation, or a CA that was replaced) |
| `certificate is valid for ..., not <host>` | The address is not among the certificate's names (custom DNS names, IPs, NodePort clients): `buildkit.tls.certManager.extraDnsNames` / `ipAddresses`, or `--tlsservername` |
| `tls: certificate required`, or `broken pipe` / `connection reset by peer` right after connecting | No client certificate, or one from another CA: the client Secret is not mounted or comes from the wrong namespace. TLS 1.3 often reports a missing certificate only as a reset |
| `certificate has expired` | The daemons still serve an old certificate after a renewal: restart them (`kubectl -n ci-accelerator rollout restart ds/ci-accelerator-buildkit`) |
| `i/o timeout`, `context deadline exceeded` | Network, not TLS: the NetworkPolicy does not admit the job's namespace (`tls.client.namespaces` / `networkPolicy.ingress.namespaces`), or routing is `Local` and the node has no ready daemon, or the job pod is not on a build node |
| `Unavailable ... connection refused` | No daemon is listening: all daemons down, or a wrong address |

## Builds are slow or not cached

1. **Which daemon built it?** The job's `debug workers -v` prints
   `worker.hostname`; metrics and traces carry `k8s.pod.name`. The cache is
   per daemon: a job on another node, or a fallback while its own daemon
   was not ready, builds cold.
2. **Was the daemon restarted?** With `emptyDir`, every restart and every
   rollout empties the cache:
   ```sh
   kubectl -n ci-accelerator get pods -l app.kubernetes.io/name=buildkit
   ```
3. **Does the build context change every time?** `.git` in the context is
   the usual culprit, since every job has a fresh clone. So are generated
   files. See [dockerfile-guide.md](dockerfile-guide.md).
4. **Do the build arguments change every time?** An `ARG` fed with
   `CI_JOB_ID` or a timestamp invalidates every step after it.
5. **Did GC remove it?**
   - Cache mounts expire after `config.gc.cacheMounts.keepDuration` (14
     days) unused.
   - Everything else goes when the cache is above `config.gc.maxUsedSpace`
     or free disk is below `minFreeSpace`.
   - Check the usage:
     ```sh
     kubectl -n ci-accelerator exec <daemon pod> -- env -i OTEL_TRACES_EXPORTER=none \
       /usr/bin/buildctl --addr unix:///run/buildkit/buildkitd.sock du | tail -3
     ```

## Builds hang

- **Every build on one daemon hangs, but the daemon still answers its
  probes.** A registry push that got stuck can hold BuildKit's per-registry
  slots and block every build on that daemon (moby/buildkit#7275). Find the
  daemon (see above) and delete its pod; the DaemonSet replaces it.
- **A job waits for a long time after a build node died.** `buildctl` has
  no keepalive: when a node disappears without closing its connections
  (power loss, network partition), the job waits for TCP to give up, which
  can take from 15 minutes to two hours. Bound the build in the job:
  ```sh
  timeout -s TERM -k 30 3000 buildctl ...
  ```
  `buildctl --timeout` only bounds connecting.
- **A build fails with `failed to get state for index`.** Another build
  that shared its steps was cancelled (moby/buildkit#7268). Retry the job.
- **Every build on one node fails at once and its daemon restarts after a
  job was cancelled.** With concurrent builds sharing a step, cancelling one
  at the moment the step finishes can crash buildkitd (`panic` in
  `flightcontrol` in the previous container's log:
  `kubectl -n <ns> logs <daemon pod> --previous`), moby/buildkit#7253.
  Retry the jobs. Fixed in BuildKit v0.34.0.

## Disk, inodes and processes

**Sizing a dedicated disk.** BuildKit sizes are binary. On a 3 TB NVMe
(about 2.7 TiB):

```yaml
storage:
  type: hostPath
  hostPath:
    path: /var/mnt/nvme/buildkit   # a directory on the mounted disk, created in advance
config:
  gc:
    maxUsedSpace: "2TB"            # 2 TiB
    reservedSpace: "100GB"
    minFreeSpace: "400GB"
```

Keep headroom beyond `maxUsedSpace`. Two reasons:
- GC runs only after builds, while running builds and BuildKit's databases
  grow outside the GC budget. One of those databases grows without bound
  in v0.33 (moby/buildkit#7263).
- On a node's root disk, `minFreeSpace` must stay above kubelet's eviction
  thresholds (10-15%); the chart's default is 30%. An evicted daemon gets
  about a second of grace and loses its running builds.

**Inodes.** Package caches (`node_modules`) create millions of small
files. On ext4, check `df -i` on the cache disk; XFS allocates inodes
dynamically.

**Processes.** RUN steps run inside the daemon's pod, so their processes
count against the node's pod PID limit (kubelet `podPidsLimit`). With high
`config.maxParallelism` and builds that start many processes, raise the
limit on build nodes.

## Cluster integration

- **Cluster Autoscaler.** It does not count DaemonSet pods when it decides
  a node is idle, so it may remove a build node and its cache. Annotate
  build nodes with `cluster-autoscaler.kubernetes.io/scale-down-disabled=true`,
  or keep build pools out of autoscaling.
- **Resource limits imposed by the namespace:**
  - a ResourceQuota that requires requests rejects daemon pods without
    `resources`;
  - a LimitRange can add a default memory limit, and an OOM kill takes the
    daemon down with every running build.

  Set `buildkit.resources` explicitly. Production started from requests of
  48 CPU / 128 GiB and a memory limit of 128 GiB.
- **Priority.** Give the daemons a PriorityClass above the CI job pods
  (`buildkit.priorityClassName`), so jobs never preempt them.
- **kube-proxy replacements.** Same-node routing needs kube-proxy 1.34+ or
  a replacement that implements `PreferSameNode`. Others balance across
  nodes silently: caches stay cold and builds still work.

## The console (preview)

- **No pipelines on the pages.** The console lists builds of CI jobs it can
  attribute: the job pods must carry the runner's annotations (the box's
  runner configuration or `integrations/gitlab-runner/values.yaml`), and
  `console.jobs.namespace` must be the jobs namespace. Builds started
  outside CI are stored but not listed.
- **A build node shows as unreachable.** The console reads each daemon's
  history over mutual TLS: check its client certificate (`<release>-console-client`)
  and that the daemons' NetworkPolicy admits the console pod. Daemons that
  are gone after a rollout disappear from the page by themselves.
- **No runner or checkout time, no jobs of other runners.** Those come from
  GitLab's API. Give the console a token that can read the projects' jobs
  (`console.gitlab.token.existingSecret`). The console calls GitLab only
  while a page shows a pipeline, and waits while GitLab rate-limits. The
  build-nodes page shows the last API error.
- **"Files or node — unclear".** The step copies files, and the job ran on
  a node that had not built the target last. The records cannot tell which
  it was; another job of the same pipeline on the target's previous node
  settles it. Fewer, larger build nodes, or pinning a project to one node,
  make it rare.
- **Jobs do not print the console's link.** Set `global.console.url`, or
  with your own runner add `CI_ACCELERATOR_CONSOLE_URL` to the runner's
  `environment` and the hook lines from
  `integrations/gitlab-runner/values.yaml`.

## Helm

- **Upgrades:** use `helm upgrade --reset-then-reuse-values`. With
  `--reuse-values`, keys added by a newer chart are missing from the old
  values.
- **`helm template`:**
  - pass `--api-versions cert-manager.io/v1`;
  - on Helm 3 older than 3.19, also `--kube-version 1.34.0` or newer: the
    charts require Kubernetes 1.34.
- **`updateStrategy.type=OnDelete`:** Helm 4's `--wait` waits for pods that
  are never replaced; use `--wait=legacy` or no `--wait`.

## Runbooks

### Empty one daemon's cache

Online, without a restart:

```sh
kubectl -n ci-accelerator exec <daemon pod> -- env -i OTEL_TRACES_EXPORTER=none \
  /usr/bin/buildctl --addr unix:///run/buildkit/buildkitd.sock prune --all
```

### Recover a corrupted state directory

After a crash (OOM kill, node failure) buildkitd repairs some of its state
by itself.

- **Repaired automatically.** If `cache.db` or `history.db` cannot be
  opened, the daemon logs `resetting to empty`, keeps a `*.bak` copy and
  starts with that part empty.
- **Needs a wipe.** If it fails to open `metadata_v2.db` or
  `containerdmeta.db`, it crash-loops. Wipe the node's state directory:

1. Take the daemon off the node: remove the build label, and the
   DaemonSet stops its pod gracefully.
   ```sh
   kubectl label node <node> ci-accelerator.io/node-role-
   ```
2. Wipe the directory. The debug pod needs a privileged namespace, so run
   it in the box namespace:
   ```sh
   kubectl -n ci-accelerator debug node/<node> -it --image=busybox:1.37 \
     -- sh -c 'rm -rf /host/var/mnt/nvme/buildkit/*'
   ```
3. Put the label back, and a fresh daemon starts with an empty cache:
   ```sh
   kubectl label node <node> ci-accelerator.io/node-role=build
   ```

### Move from emptyDir to hostPath

1. Create the directory on every build node, on the dedicated disk.
2. Set `buildkit.storage.type=hostPath` and `buildkit.storage.hostPath.path`.
3. Upgrade. Every daemon restarts with an empty cache, one node at a time.

### Uninstall completely

`helm uninstall` removes the release, including the jobs namespace and
every job pod in it. It leaves:

- **with hostPath, the cache on each build node:** remove it as in step 2
  above;
- **the metrics and traces volumes:**
  ```sh
  kubectl -n ci-accelerator delete pvc --all
  ```
  Only do this if nothing else in that namespace uses PVCs.
- **the certificate Secrets** (cert-manager keeps them when their
  Certificates are deleted):
  - the CA key in cert-manager's namespace:
    ```sh
    kubectl -n cert-manager delete secret -l app.kubernetes.io/instance=ci-accelerator
    ```
  - the daemons' certificate in the box namespace:
    ```sh
    kubectl -n ci-accelerator delete secret ci-accelerator-buildkit-tls
    ```
- **the token Secret and the box namespace you created.**

Before reinstalling on the same hostPath, wait until the old daemon pods
are gone: a new daemon cannot take the locked state directory.

## Local cluster (kind)

- **Every worker in the example is a tainted build node.** Pods that are
  not build pods need a toleration for the control-plane node: the runner,
  VictoriaMetrics, VictoriaTraces and cert-manager.
- **A sleeping laptop pauses the Docker VM.** Long "hangs" in tests and a
  cert-manager restart with `clockHealth failed ... out of sync` follow a
  host sleep. Check `pmset -g log | grep -E ' Sleep | Wake '` before
  debugging software.
