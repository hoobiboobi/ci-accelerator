# buildkit

![Version: 0.3.1](https://img.shields.io/badge/Version-0.3.1-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.34.1](https://img.shields.io/badge/AppVersion-v0.34.1-informational?style=flat-square)

Persistent BuildKit daemons as a shared build service for CI, with probe and garbage-collection defaults derived from a production setup.

Part of [ci-accelerator](https://github.com/hoobiboobi/ci-accelerator); this chart is the daemons alone, for a cluster that already has a runner. It runs BuildKit v0.34.1 (the official `moby/buildkit` image, pinned by digest) as a privileged DaemonSet on the build nodes, with the probe, termination and garbage-collection defaults of a production setup: the garbage-collection policies are rendered in full by the chart (see `config.gc`). mTLS between jobs and daemons is mandatory: cert-manager issues the daemons' certificate and a client certificate (Secret `<fullname>-client`) into each of `tls.client.namespaces`, or you provide them (`tls.source=existingSecret`, made by `scripts/buildkit-certs.sh`).

```sh
helm install buildkit oci://ghcr.io/hoobiboobi/charts/buildkit --version 0.3.1 \
  -n ci-accelerator --set 'tls.client.namespaces={ci-jobs}'
```

CI jobs reach the daemons through the Service `<fullname>` on port 1234. By default each job talks to the daemon on its own node (`service.routing: PreferSameNode`), so a project's cache stays warm on the nodes its jobs run on; the headless Service `<fullname>-headless` lists every daemon for other components.

The box ([charts/ci-accelerator](https://github.com/hoobiboobi/ci-accelerator/tree/main/charts/ci-accelerator)) installs this chart with a gitlab-runner and the control plane ([charts/control-plane](https://github.com/hoobiboobi/ci-accelerator/tree/main/charts/control-plane)), whose build router sends each job to the node where its project built before. With `global.router.enabled` and `global.router.service` (the box sets both), the daemons' certificate names the router's Service and their NetworkPolicy admits the router's pods instead of the clients' namespaces; the jobs keep their client certificates, which the daemons check through the router. The box also turns on `diskProbe` (a sidecar reporting the cache disk's free space to the console) and `networkPolicy`.

How to use the daemons from a job, where the cache should live and what to watch: the [documentation](https://ci-accelerator.io/docs/), [architecture](https://ci-accelerator.io/docs/architecture/) and [troubleshooting](https://ci-accelerator.io/docs/troubleshooting/). This chart upgrades in place from the previous release; the upgrade notes in the [changelog](https://ci-accelerator.io/changelog/) say when an upgrade restarts the daemons (an `emptyDir` cache then starts empty).

## Requirements

Kubernetes: `>=1.34.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` |  |
| commonLabels | object | `{}` | Labels added to every resource the chart creates (including the pods: changing them restarts the daemons). They cannot override the chart's own labels. |
| config.cdi | object | `{"enabled":false}` | Container Device Interface (GPUs and other devices for RUN steps). Off by default: CI builds need no devices. Requires buildkit >= v0.33.1: older versions panic when a build requests a device while CDI is disabled (GHSA-r456-g3gm-cvxf). |
| config.extraConfig | string | `""` | Raw TOML inserted after the top-level keys and before the chart's tables: use it for top-level keys (e.g. insecure-entitlements) and new tables (e.g. [registry."docker.io"]). Never repeat [grpc], [worker.oci], [worker.containerd], [history], [system] or [cdi]; the chart validates the result and fails the render otherwise. Do not reach into those tables with dotted keys either (worker.oci.max-parallelism = 5): the chart's check accepts them, buildkitd's parser does not, and the daemon fails to start. Use the typed values or config.workerOCI. |
| config.gc.cacheMounts.keepDuration | string | `"336h"` |  |
| config.gc.cacheMounts.maxUsedSpace | string | `""` | Expire cache mounts (whatever their age) only while the WHOLE cache is above this size: buildkit compares policy caps with the total usage, not with the filtered records. Empty: expire by age only. |
| config.gc.maxUsedSpace | string | `"75%"` | Reclaim when the cache grows above this. |
| config.gc.minFreeSpace | string | `"30%"` | Reclaim when free space on the state-dir filesystem drops below this. It is compared with free blocks including the filesystem's root reserve (ext4: 5%), while kubelet evicts on available blocks at 10-15%. With the cache on the node's root disk (emptyDir, or hostPath not on its own disk) keep it at 30% or more; on a dedicated disk use an absolute size (production: 400GB on a 3 TB NVMe). |
| config.gc.reservedSpace | string | `"10GB"` | The capped policies never reclaim below this much cache. Age-based policies (cache mounts, local context) still expire by age. |
| config.gcPolicy | list | `[]` | Replace the chart's GC policies entirely. Each item: filters, all, keepDuration, reservedSpace, maxUsedSpace, minFreeSpace. Filters use buildkit's prune syntax, e.g. "type==exec.cachemount". |
| config.history | object | `{"maxAge":"48h","maxEntries":1000}` | Build history, which the controller reads (it copies each record when it completes, and reads the history again after a restart). Keys: maxAge (duration) and maxEntries (int). Set both, or neither: a key that is not set becomes 0, not the buildkit default (48h / 50); maxEntries 0 stops recording new history, and maxAge alone drops the 50-entry floor. 1000 records cover about an hour of a busy farm; buildkit's own 50 last minutes there. |
| config.log.format | string | `""` | json or text; empty: text. |
| config.log.level | string | `""` | error, warn, info or debug; empty: info. |
| config.maxParallelism | int | `0` | Max concurrently running build operations per daemon: RUN, COPY/ADD and source operations (image pulls, git, local context). 0 = unlimited, which is buildkit's default (not the CPU count). Production ran 28 on a 48-CPU daemon. |
| config.maxRegistryConcurrency | int | `0` | Max concurrent connections per registry; 0 keeps the buildkit default (4). Pushes that get stuck hold their slot and can hang every build on the daemon (moby/buildkit#7275); the probes do not detect that. |
| config.workerOCI | object | `{}` | Extra keys for the [worker.oci] table, e.g. {snapshotter: native}. Values: strings, numbers, booleans or lists. The keys the chart sets (enabled, gc, max-parallelism, gcpolicy) are rejected. |
| diskProbe.enabled | bool | `false` | Run the probe. Off here (no controller to read it); the box turns it on. |
| diskProbe.image.digest | string | `""` | When set, the image is pulled by digest (tag is kept for readability). |
| diskProbe.image.pullPolicy | string | `"IfNotPresent"` |  |
| diskProbe.image.repository | string | `"ghcr.io/hoobiboobi/ci-accelerator"` | The ci-accelerator image: mirror it together with the control plane's. |
| diskProbe.image.tag | string | `""` | Default: v0.3.0, the release that last changed the probe program or its image; pinned, so a chart release alone does not restart every daemon. |
| diskProbe.logLevel | string | `""` | Its log level (debug adds a line per reading); empty: info. Set, it restarts every daemon once. |
| diskProbe.port | int | `9180` | Its port (GET /disk, /healthz); the daemons tell it to the control plane as a worker label. |
| diskProbe.resources | object | `{"limits":{"memory":"32Mi"},"requests":{"cpu":"5m","memory":"16Mi"}}` | The probe's resources (it reads one number a minute). |
| extraEnv | list | `[]` | Extra environment variables for the daemon container. |
| fullnameOverride | string | `""` | Override the full resource name (<release>-<chart> by default). Install-time only, like nameOverride. |
| global.buildNodes.enabled | bool | `true` | false: ignore selector/tolerations below and run on any Linux node. (A switch, because Helm merges `selector: {}` with the default instead of clearing it.) |
| global.buildNodes.selector."ci-accelerator.io/node-role" | string | `"build"` |  |
| global.buildNodes.tolerations[0].effect | string | `"NoSchedule"` |  |
| global.buildNodes.tolerations[0].key | string | `"ci-accelerator.io/node-role"` |  |
| global.buildNodes.tolerations[0].operator | string | `"Equal"` |  |
| global.buildNodes.tolerations[0].value | string | `"build"` |  |
| global.router.enabled | bool | `false` |  |
| global.router.service | string | `""` | The router's Service in this namespace. Rendered with tpl. |
| global.tls.issuerRef | object | `{"group":"cert-manager.io","kind":"ClusterIssuer","name":""}` | cert-manager issuer for the daemons' and the clients' certificates (tls.source=certManager). Empty name: the chart creates a CA dedicated to this release (see tls.certManager). Your own issuer must be dedicated to these daemons too (any certificate it signs gets in) and must put ca.crt into the Secrets it fills (CA or Vault issuers do, ACME does not). Client certificates in other namespaces need a ClusterIssuer. |
| image.digest | string | `"sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c"` | Image index digest. The runtime pulls by digest when it is set, so change or clear it together with tag (another tag with this default digest is refused). Default: the digest of the appVersion tag (multi-arch index). |
| image.pullPolicy | string | `"IfNotPresent"` |  |
| image.repository | string | `"moby/buildkit"` |  |
| image.tag | string | `""` | Defaults to the chart appVersion. |
| imagePullSecrets | list | `[]` | Image pull secrets for private mirrors of the buildkit image. |
| minReadySeconds | int | `10` | A new daemon must stay ready this long before the rollout moves on to the next node. |
| nameOverride | string | `""` | Override the chart name used in resource names. Install-time only: changing it later renames every object and conflicts with the immutable DaemonSet selector. |
| networkPolicy.egress.blockedCIDRs | list | `["169.254.169.254/32","fd00:ec2::254/128"]` | Never reachable: cloud instance metadata (node credentials) for IPv4 and IPv6. |
| networkPolicy.egress.clusterCIDRs | list | `[]` | The cluster's pod and Service CIDRs. Listed here, the daemons and every build lose access to in-cluster services (allow what they need in extraRules, e.g. an in-cluster registry or the OTLP backends). The API offers no way to discover them. Traffic to the node itself is always allowed by Kubernetes, and whether the API server can be blocked depends on the CNI. |
| networkPolicy.egress.dns | list | `[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"kube-system"}},"podSelector":{"matchLabels":{"k8s-app":"kube-dns"}}}]` | Peers that serve DNS on port 53. |
| networkPolicy.egress.extraRules | list | `[]` | More NetworkPolicyEgressRule entries (rendered with tpl). |
| networkPolicy.enabled | bool | `false` | Render a NetworkPolicy for the daemon pods. |
| networkPolicy.ingress.extraFrom | list | `[]` | More NetworkPolicyPeer entries for port 1234 (e.g. an ipBlock for NodePort clients outside the cluster). |
| networkPolicy.ingress.namespaces | list | `[]` | Namespaces whose pods may connect to port 1234, in addition to tls.client.namespaces. |
| nodeSelector | object | `{}` | Merged over global.buildNodes.selector (and kubernetes.io/os: linux). |
| otel.metricsEndpoint | string | `""` | Full OTLP URL for metrics (with the path, e.g. http://otel-collector:4318/v1/metrics). Empty: no metrics are pushed. Both endpoints are rendered with tpl, so a parent chart can build them from the release name. |
| otel.metricsIntervalSeconds | int | `60` | Seconds between metric pushes. Counters are deltas per interval: sum them over time (or convert them to cumulative in a collector). |
| otel.protocol | string | `"http/protobuf"` | http/protobuf (what most OTLP receivers accept over HTTP) or grpc. |
| otel.resourceAttributes | object | `{}` | More resource attributes for every metric and span. The chart always adds k8s.namespace.name, k8s.pod.name and k8s.node.name, so each series and span names its daemon. |
| otel.tracesEndpoint | string | `""` | Full OTLP URL for traces (e.g. http://otel-collector:4318/v1/traces). Empty: no traces are exported. CI jobs join a build to their own trace by setting TRACEPARENT for buildctl. |
| podAnnotations | object | `{}` |  |
| podLabels | object | `{}` | Extra pod labels. They cannot override the selector or component labels. |
| preStopSleepSeconds | int | `5` | On deletion, keep serving for this long before buildkitd gets SIGTERM, so kube-proxy withdraws the endpoint first and no new connection is refused. Counts against terminationGracePeriodSeconds. 0 disables. |
| preflight.buildNodes | bool | `true` | Fail if no node carries every label of the build-node selector (the DaemonSet would want 0 pods while helm reports success). Needs cluster-wide list access to Nodes (an installer that is only a namespace admin gets Forbidden); set false then, or for node pools scaled to zero. |
| preflight.podSecurity | bool | `true` | Fail if the namespace's pod-security.kubernetes.io/enforce label is set to anything but privileged (no daemon pod could start). Needs read access to the Namespace object, which namespace admins have. A namespace without the label is not checked (the cluster default applies); NOTES warns. |
| preflight.tlsSecret | bool | `true` | With tls.source=existingSecret: fail if the Secret exists but lacks tls.crt, tls.key or ca.crt (`kubectl create secret tls` writes no ca.crt). A Secret that does not exist yet is not an error: the daemons wait for it. Needs read access to Secrets in the release namespace. |
| priorityClass.create | bool | `true` | Create the PriorityClass. false: no class (or priorityClassName's). |
| priorityClass.preemptionPolicy | string | `"Never"` | Never: the daemon does not evict running CI jobs to get scheduled; PreemptLowerPriority lets it. |
| priorityClass.value | int | `1000000` | Above CI jobs (0), below Kubernetes' own system classes (2000000000). |
| priorityClassName | string | `""` | An existing PriorityClass for the daemons. Empty: the one this chart creates (priorityClass), or none if that is switched off. |
| probes.liveness.failureThreshold | int | `6` |  |
| probes.liveness.initialDelaySeconds | int | `0` |  |
| probes.liveness.periodSeconds | int | `30` |  |
| probes.liveness.terminationGracePeriodSeconds | int | `30` | Grace for a restart caused by this probe. A wedged daemon is killed after this instead of the pod's terminationGracePeriodSeconds. |
| probes.liveness.timeoutSeconds | int | `15` |  |
| probes.readiness.failureThreshold | int | `3` |  |
| probes.readiness.initialDelaySeconds | int | `0` | Keep 0: kubelet runs readiness right after startup succeeds, but skips that run while still inside initialDelaySeconds; the pod then waits a full period. |
| probes.readiness.periodSeconds | int | `10` | A daemon that stops answering leaves the endpoints after periodSeconds x failureThreshold (30 s); until then its node's jobs still go to it. |
| probes.readiness.timeoutSeconds | int | `10` |  |
| probes.startup | object | `{"failureThreshold":150,"initialDelaySeconds":2,"periodSeconds":2,"terminationGracePeriodSeconds":30,"timeoutSeconds":3}` | Polls often until the daemon answers, then hands over to readiness (kubelet runs it right away) and liveness, which is off meanwhile, so a slow start on a large cache is not killed. Budget: failureThreshold x max(periodSeconds, timeoutSeconds) = 5 to 7.5 min. |
| resources | object | `{"requests":{"cpu":"500m","memory":"512Mi"}}` | Daemon resources. The small default requests keep the daemon out of BestEffort, which the kubelet evicts first under memory pressure; size them to the build load and leave kubelet and containerd headroom (an unlimited daemon starved a 192-core node in production). A hard CPU limit throttles builds. A memory limit applies to buildkitd and all RUN steps together, and an OOM kill takes the whole daemon with every running build; leave headroom above max-parallelism x the largest step. Sizes:   a small node (4-8 CPU, 8-16 GiB):  requests {cpu: "1", memory: 2Gi},  limits {memory: 6Gi}   16-32 CPU, 64 GiB:                 requests {cpu: "8", memory: 16Gi}, limits {memory: 48Gi}   production (48+ CPU, 192+ GiB):    requests {cpu: "48", memory: 128Gi}, limits {memory: 128Gi} |
| service.annotations | object | `{}` |  |
| service.externalTrafficPolicy | string | `"Cluster"` | externalTrafficPolicy for NodePort (Cluster or Local). |
| service.nodePort | string | `""` | Fixed node port when type is NodePort; must lie in the cluster's service-node-port-range (default 30000-32767). Empty lets Kubernetes pick. |
| service.port | int | `1234` |  |
| service.routing | string | `"PreferSameNode"` | How in-cluster clients reach a daemon. The cache is per daemon, so staying on the client's node keeps it warm.   PreferSameNode: own node's daemon; falls back to another node when it                   is not ready (cold cache, build still succeeds).                   Needs kube-proxy >= 1.34 on every node (or a proxy that                   implements node hints); older ones silently balance                   cluster-wide.   Local:          own node's daemon only; no ready daemon there means no                   endpoints and the build fails.   Cluster:        any daemon. |
| service.type | string | `"ClusterIP"` | ClusterIP for CI jobs in the cluster; NodePort to reach the daemons from outside (every node IP; the server certificate must then also cover the address clients use, or clients pass --tlsservername). |
| storage.emptyDir.sizeLimit | string | `""` | Kubernetes quantity, e.g. "50Gi"; empty means no limit. Exceeding it gets the pod evicted with ~1 s grace (running builds fail, cache lost), so the chart requires an absolute config.gc.maxUsedSpace of at most 80% of it whenever it is set. |
| storage.hostPath.path | string | `""` | Required with storage.type=hostPath. A directory created in advance on the dedicated cache disk, as a subdirectory of the mount (e.g. /var/mnt/nvme/buildkit, not /var/mnt/nvme): if the disk is not mounted, the pod then fails to start instead of filling the OS disk. One release per path per node (buildkitd locks the directory). The data stays after `helm uninstall`. |
| storage.hostPath.type | string | `"Directory"` | Directory: the path must exist (recommended). DirectoryOrCreate: kubelet creates it on whatever filesystem is there (test clusters). |
| storage.type | string | `"emptyDir"` | emptyDir: the cache lives as long as the pod, on the node's root disk (quick start, CI). Every rollout starts with an empty cache. hostPath: the cache survives restarts and rollouts (production). |
| terminationGracePeriodSeconds | int | `300` | Seconds a stopping daemon gets to finish running builds (it stops accepting new ones at once). Builds still running after this are killed. With maxUnavailable 1 a rollout takes up to nodes x this, so pass a helm `--timeout` to match. Production builds took 7-21 min: consider 1200-1800 there. Pod evictions and node shutdowns do not honour it. |
| tls.certManager.caDuration | string | `"87600h"` | Lifetime of the CA the chart creates. It is renewed with the same key, so certificates it signed stay valid. |
| tls.certManager.clusterDomain | string | `"cluster.local"` | Cluster DNS domain, for the fully qualified Service names. |
| tls.certManager.clusterResourceNamespace | string | `"cert-manager"` | Namespace where cert-manager's ClusterIssuers read their Secrets (its --cluster-resource-namespace; by default its own namespace). With an empty global.tls.issuerRef the chart puts its CA there: a self-signed Issuer, the CA Certificate and its Secret. That Secret (the CA key) stays after `helm uninstall`. |
| tls.certManager.duration | string | `"8760h"` | Lifetime of the daemon and client certificates. cert-manager renews them after two thirds; the daemons pick up a renewed certificate only when they restart (see reloader), within the remaining third. |
| tls.certManager.extraDnsNames | list | `[]` | More names and addresses for the daemon certificate (e.g. a node address for NodePort clients, or a TCP proxy's Service such as the ci-accelerator build router). Names are rendered with tpl. |
| tls.certManager.ipAddresses | list | `[]` |  |
| tls.client.namespaces | list | `[]` | Namespaces that get a client certificate in Secret <fullname>-client (tls.crt, tls.key, ca.crt), e.g. the namespace of the CI job pods. They must exist. With source=certManager only. Entries are rendered with tpl, so a parent chart may write "{{ .Values.global.<key> }}". |
| tls.existingSecret | string | `""` | With source=existingSecret: name of a Secret in the release namespace with the daemon's certificate (tls.crt, tls.key) and the CA that signs client certificates (ca.crt). The server certificate must cover the Service names clients use (<fullname>.<namespace>.svc). |
| tls.reloader | bool | `true` | Annotate the DaemonSet for Stakater Reloader. buildkitd reads its certificates only at start, so a renewed Secret takes effect after a restart; with Reloader in the cluster that is a rolling restart (per updateStrategy; nothing happens with OnDelete) as soon as the Secret changes. Under Argo CD or Flux run Reloader with the flag `--reload-strategy=annotations`. Without Reloader, restart the daemons yourself before the old certificate expires. |
| tls.source | string | `"certManager"` | certManager: cert-manager (installed beforehand) issues the daemon certificate (Secret <fullname>-tls) and client certificates. existingSecret: you provide the daemon Secret and the client certificates (scripts/buildkit-certs.sh makes both). |
| tolerations | list | `[]` | Appended to global.buildNodes.tolerations. |
| updateStrategy.rollingUpdate.maxSurge | int | `0` | Must stay 0 with storage.type=hostPath (buildkitd locks the state dir, so a surge pod never starts). |
| updateStrategy.rollingUpdate.maxUnavailable | int | `1` |  |
| updateStrategy.type | string | `"RollingUpdate"` | RollingUpdate or OnDelete. With OnDelete, Helm 4's `--wait` (kstatus) waits for pods that are never replaced: use --wait=legacy or no --wait. |

