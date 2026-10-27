# control-plane

![Version: 0.2.5](https://img.shields.io/badge/Version-0.2.5-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.2.5](https://img.shields.io/badge/AppVersion-0.2.5-informational?style=flat-square)

The ci-accelerator control plane: a controller that collects the build history of BuildKit daemons and the CI jobs around it, and a web that serves the console, which explains every build: what came from cache, what came from another job, and why a step was rebuilt.

Part of [ci-accelerator](https://github.com/hoobiboobi/ci-accelerator). The control plane runs as two Deployments from one image: the controller (`<release>-controller`, one pod) reads every daemon's build history and the CI job pods into SQLite on its own volume, holds the sign-in sessions and every GitLab token; the web (`<release>-web`, two pods, nothing kept) serves the console from the controller's API. The console answers one question: why an image build was slow or not cached - the change or the build farm. It works in three levels: by itself (builds, steps, cache verdicts, the pipeline timeline), with a GitLab token (job results, runner waits, "files or node" by comparing commits), or with sign-in through GitLab (each person sees the projects GitLab lets them read). The box ([charts/ci-accelerator](https://github.com/hoobiboobi/ci-accelerator/tree/main/charts/ci-accelerator)) installs it on by default; on its own it needs the daemons' CA for the controller's certificates (`tls`) and their headless Service (`daemons.discover`).

[The console](https://ci-accelerator.io/docs/#the-console) in the documentation explains the levels and the sign-in setup. Up to 0.2.x this chart was `console`; 0.3.0 does not upgrade a release of it in place (the upgrade notes in the changelog say how to move).

## Requirements

Kubernetes: `>=1.34.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| caches | object | `{"every":"10m","fromConfigMap":"","key":"caches.json","stores":[]}` | The runners' S3 cache (GitLab's cache: keyword): the controller lists its folders now and then, and the console shows each project's cache size and key count on the Nodes page, with a warning when the bucket has no lifecycle rule (the runners never delete a cache). Either the folders here (a Secret with the keys accesskey and secretkey per folder; keys that may only read are enough), or a ConfigMap with them as JSON (the box writes one). The controller may read only the Secrets named here. |
| caches.every | string | `"10m"` | How often the folders are listed. |
| caches.fromConfigMap | string | `""` | Or the name of a ConfigMap whose key holds the list (fields server, bucket, region, path, instance, secret, insecure, pathStyle); the Role to read its Secrets is then the writer's to make. Rendered with tpl. |
| caches.key | string | `"caches.json"` | The ConfigMap's key with the list. |
| commonLabels | object | `{}` | Labels for every object; they cannot override the chart's own labels. |
| controller.affinity | object | `{}` |  |
| controller.api.clusterDomain | string | `"cluster.local"` | Cluster domain in the API certificate's names. |
| controller.api.port | int | `8443` | The API's port. |
| controller.api.tls.existingSecret | string | `""` | Secret with the API's server certificate (tls.crt, tls.key, and ca.crt that it chains to) for the name <release>-controller; empty: issued by cert-manager from the daemons' CA (server auth only). Needed with tls.existingSecret. |
| controller.nodeSelector | object | `{}` |  |
| controller.persistence | object | `{"existingClaim":"","size":"10Gi","storageClass":""}` | The store. SQLite needs block storage (ReadWriteOnce): never NFS or another shared file system. The claim <release>-controller-data is kept when the release is removed. |
| controller.podAnnotations | object | `{}` |  |
| controller.podLabels | object | `{}` |  |
| controller.resources.limits.memory | string | `"1Gi"` |  |
| controller.resources.requests.cpu | string | `"50m"` |  |
| controller.resources.requests.memory | string | `"128Mi"` |  |
| controller.tolerations | list | `[]` |  |
| daemons.discover | string | `""` | host:port of the BuildKit chart's headless Service; every address it resolves to is a daemon. Rendered with tpl. |
| daemons.diskProbePort | int | `0` | Port of the disk probe next to each daemon (the buildkit chart's diskProbe), for the free space on the Nodes page. 0: not read. |
| daemons.serverName | string | `""` | Name in the daemons' certificate. Default: the host of discover. |
| enabled | bool | `false` | The switch the box uses (control-plane.enabled). Installed on its own, this chart ignores it. |
| fullnameOverride | string | `""` | Prefix of every object's name. Default: the release name, so objects are <release>-controller and <release>-web. |
| gitlab.token | object | `{"existingSecret":"","key":"token"}` | A token that can read the projects' jobs (read_api): a Secret's key. The controller reads with it the pipelines' jobs (how long each waited for a runner and ran, jobs of other runners) while the console's pages are in use, once per finished pipeline; GitLab's comparison of two commits, which settles "files or node"; and the end of a job's log on request. Without it, job times come from the job pods and "files or node" stays unclear. Not with sign-in: GitLab is then read with each person's own token. |
| gitlab.urls | list | `[]` | URLs of the GitLab instances whose jobs the runners take, exactly as the runners have them (gitlabUrl, slash included): every build's trace id carries a hash of that string. |
| gitlab.urlsFrom | object | `{"configMap":"","key":"urls"}` | Or read them from a ConfigMap key (comma-separated). Rendered with tpl. |
| global.buildNodes.enabled | bool | `true` | false: ignore selector/tolerations below and run on any Linux node. |
| global.buildNodes.selector."ci-accelerator.io/node-role" | string | `"build"` |  |
| global.buildNodes.tolerations[0].effect | string | `"NoSchedule"` |  |
| global.buildNodes.tolerations[0].key | string | `"ci-accelerator.io/node-role"` |  |
| global.buildNodes.tolerations[0].operator | string | `"Equal"` |  |
| global.buildNodes.tolerations[0].value | string | `"build"` |  |
| global.console.url | string | `""` | Where browsers reach the console; the default of signIn.url. |
| global.tls.issuerRef.group | string | `"cert-manager.io"` |  |
| global.tls.issuerRef.kind | string | `"ClusterIssuer"` |  |
| global.tls.issuerRef.name | string | `""` |  |
| image.digest | string | `""` | When set, the image is pulled by digest (tag is kept for readability). |
| image.pullPolicy | string | `"IfNotPresent"` |  |
| image.repository | string | `"ghcr.io/hoobiboobi/ci-accelerator"` |  |
| image.tag | string | `""` | Default: v<appVersion>. |
| imagePullSecrets | list | `[]` |  |
| jobs.namespace | string | `""` | Namespace of the CI job pods, read for job names, pipelines and commits. Empty: builds are kept without them. Rendered with tpl. |
| jobs.rbac | object | `{"create":true}` | A Role and RoleBinding there that let the controller get, list and watch pods. Without them, give that access some other way. |
| nameOverride | string | `""` | Name of the app (app.kubernetes.io/name). Default: control-plane. |
| networkPolicy.deniedComponents | list | `["buildkit"]` | Pods of this namespace whose ci-accelerator.io/component label is one of these may not reach the web: the daemons, whose network every build shares. |
| networkPolicy.deniedNamespaces | list | `[]` | More namespaces whose pods may not reach the web (the jobs namespace is always one when jobs.namespace is set). Rendered with tpl. |
| networkPolicy.enabled | bool | `false` | Render the NetworkPolicies: the web admits connections from every namespace except this one, the jobs namespace and deniedNamespaces, and from pods of this namespace except deniedComponents; the controller admits only the web and the routers; the routers admit only their clients' namespaces. Only ingress is limited. |
| networkPolicy.extraFrom | list | `[]` | More NetworkPolicyPeer entries that may reach the web, e.g. an ipBlock with the node addresses when the Ingress controller runs on the host network. |
| retention.days | int | `30` | Days builds are kept in full (steps, reasons). Each image's daily figures (builds by outcome, build times) are kept a year regardless. At 150 pipelines a day with 15 image builds each, a project takes about 0.25 GB per 30 days. |
| retention.outputDays | int | `7` | Days the steps' output is kept. |
| router.clientNamespaces | list | `[]` | Namespaces whose pods may connect to the router; default: the jobs namespace. Rendered with tpl. |
| router.crossZone | bool | `true` | When no daemon of the job's zone is ready: true sends the job to another zone's daemon, false refuses the connection. |
| router.drainSeconds | int | `300` | Seconds open connections (running builds) may finish when a router stops; the pod's grace period is this plus preStopSleepSeconds and 10. |
| router.enabled | bool | `false` | Run the router (the box does). The runners must then use tcp://<release>-router.<namespace>.svc:1234. |
| router.healthPort | int | `8080` | The port of /livez and /readyz. |
| router.nodeSelector | object | `{}` | Merged over global.buildNodes.selector. |
| router.overloadBuilds | int | `0` | Skip a daemon running this many builds when another one is free; 0: never (a busy warm daemon is usually still faster than a cold one). |
| router.podAnnotations | object | `{}` |  |
| router.podLabels | object | `{}` |  |
| router.port | int | `1234` | The port the jobs connect to. |
| router.preStopSleepSeconds | int | `5` | Seconds a stopping router keeps accepting while the Service drops it. |
| router.priorityClassName | string | `""` | PriorityClass of the router pods; the box gives them the daemons'. |
| router.resources.limits.memory | string | `"256Mi"` |  |
| router.resources.requests.cpu | string | `"20m"` |  |
| router.resources.requests.memory | string | `"32Mi"` |  |
| router.routing | string | `"PreferSameNode"` | How the Service picks a router: PreferSameNode (the job's node, any other when it has none ready), Local (the job's node only) or Cluster. |
| router.tolerations | list | `[]` | Appended to global.buildNodes.tolerations. |
| router.warmDays | int | `7` | A project's layers unused for this many days no longer make their node warm for it. |
| router.zones | bool | `true` | Keep a job's builds in its zone (topology.kubernetes.io/zone of the daemons' nodes): a project then warms up once per zone. false: every daemon is one zone (zones close enough that crossing them costs nothing, or one zone label per node). |
| signIn | object | `{"enabled":false,"gitlab":[],"sessionKey":{"existingSecret":"","key":"key"},"url":""}` | Sign-in through GitLab. Each person signs in with a GitLab the runners serve and sees the projects whose jobs GitLab lets them read (Reporter or more, or public pipelines); GitLab is read with their own token. GitLab never calls the console: the browser carries the redirects. |
| signIn.gitlab | list | `[]` | One OAuth application per GitLab (User or Group Settings > Applications, or Admin > Applications on your own GitLab): confidential, scopes openid and read_api, redirect URI <url>/auth/callback/<id> (the NOTES and the controller's log print it).   url: exactly as the runners have it; may be left empty when the        console serves one GitLab   existingSecret: a Secret with the keys client-id and client-secret   admins: GitLab usernames who see everything (builds of no known        project, the JSON API) |
| signIn.sessionKey | object | `{"existingSecret":"","key":"key"}` | A Secret holding 32 random bytes in base64 (openssl rand -base64 32); it seals each session's GitLab token. Changing it signs everyone out. |
| signIn.url | string | `""` | Where browsers reach the console: https (an Ingress or a gateway you add), or http only on localhost (a port-forward). Default: global.console.url. Rendered with tpl. |
| tls.certManager.duration | string | `"8760h"` |  |
| tls.certManager.issuerRef | object | `{"group":"cert-manager.io","kind":"ClusterIssuer","name":""}` | Default: global.tls.issuerRef. Rendered with tpl. |
| tls.certManager.renewBeforePercentage | int | `33` |  |
| tls.existingSecret | string | `""` | Secret with the controller's client certificate (ca.crt, tls.crt, tls.key) signed by the daemons' CA; empty: issued by cert-manager. |
| web.affinity | object | `{}` |  |
| web.nodeSelector | object | `{}` |  |
| web.podAnnotations | object | `{}` |  |
| web.podLabels | object | `{}` |  |
| web.port | int | `8080` | The port of the pages and the JSON API. |
| web.replicas | int | `2` | Pods serving the pages. |
| web.resources.limits.memory | string | `"256Mi"` |  |
| web.resources.requests.cpu | string | `"20m"` |  |
| web.resources.requests.memory | string | `"48Mi"` |  |
| web.tolerations | list | `[]` |  |

