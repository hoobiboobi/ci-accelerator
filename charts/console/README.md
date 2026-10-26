# console

![Version: 0.2.5](https://img.shields.io/badge/Version-0.2.5-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.2.5](https://img.shields.io/badge/AppVersion-0.2.5-informational?style=flat-square)

Collects the build history of BuildKit daemons and the CI jobs around it, and explains every build: what came from cache, what came from another job, and why a step was rebuilt.

Part of [ci-accelerator](https://github.com/hoobiboobi/ci-accelerator). The console reads every daemon's build history and the CI job pods into SQLite on its own volume and answers one question: why an image build was slow or not cached - the change or the build farm. It works in three levels: by itself (builds, steps, cache verdicts, the pipeline timeline), with a GitLab token (job results, runner waits, "files or node" by comparing commits), or with sign-in through GitLab (each person sees the projects GitLab lets them read). The box ([charts/ci-accelerator](https://github.com/hoobiboobi/ci-accelerator/tree/main/charts/ci-accelerator)) installs it on by default; on its own it needs the daemons' CA for its client certificate (`tls`) and their headless Service (`daemons.discover`).

[The console](https://ci-accelerator.io/docs/#the-console) in the documentation explains the levels and the sign-in setup.

## Requirements

Kubernetes: `>=1.34.0-0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` |  |
| caches | object | `{"every":"10m","fromConfigMap":"","key":"caches.json","stores":[]}` | The runners' S3 cache (GitLab's cache: keyword): the console lists its folders now and then and shows each project's cache size and key count on the Nodes page, with a warning when the bucket has no lifecycle rule (the runners never delete a cache). Either the folders here (a Secret with the keys accesskey and secretkey per folder; keys that may only read are enough), or a ConfigMap with them as JSON (the box writes one). The console may read only the Secrets named here. |
| caches.every | string | `"10m"` | How often the folders are listed. |
| caches.fromConfigMap | string | `""` | Or the name of a ConfigMap whose key holds the list (fields server, bucket, region, path, instance, secret, insecure, pathStyle); the Role to read its Secrets is then the writer's to make. Rendered with tpl. |
| caches.key | string | `"caches.json"` | The ConfigMap's key with the list. |
| commonLabels | object | `{}` | Labels for every object; they cannot override the chart's own labels. |
| daemons.discover | string | `""` | host:port of the BuildKit chart's headless Service; every address it resolves to is a daemon. Rendered with tpl. |
| daemons.diskProbePort | int | `0` | Port of the disk probe next to each daemon (the buildkit chart's diskProbe), for the free space on the Nodes page. 0: not read. |
| daemons.serverName | string | `""` | Name in the daemons' certificate. Default: the host of discover. |
| enabled | bool | `false` | The switch the box uses (console.enabled). It has a default here so that `helm upgrade --reuse-values` from a box without the console keeps it off. Installed on its own, this chart ignores it. |
| fullnameOverride | string | `""` |  |
| gitlab.token | object | `{"existingSecret":"","key":"token"}` | A token that can read the projects' jobs (read_api): a Secret's key. The console reads with it the pipelines' jobs (how long each waited for a runner and ran, jobs of other runners) while its pages are in use, once per finished pipeline; GitLab's comparison of two commits, which settles "files or node"; and the end of a job's log on request. Without it, job times come from the job pods and "files or node" stays unclear. Not with sign-in: GitLab is then read with each person's own token. |
| gitlab.urls | list | `[]` | URLs of the GitLab instances whose jobs the runners take, exactly as the runners have them (gitlabUrl, slash included): every build's trace id carries a hash of that string. |
| gitlab.urlsFrom | object | `{"configMap":"","key":"urls"}` | Or read them from a ConfigMap key (comma-separated). Rendered with tpl. |
| global.tls.issuerRef.group | string | `"cert-manager.io"` |  |
| global.tls.issuerRef.kind | string | `"ClusterIssuer"` |  |
| global.tls.issuerRef.name | string | `""` |  |
| image.digest | string | `""` | When set, the image is pulled by digest (tag is kept for readability). |
| image.pullPolicy | string | `"IfNotPresent"` |  |
| image.repository | string | `"ghcr.io/hoobiboobi/ci-accelerator-console"` |  |
| image.tag | string | `""` | Default: v<appVersion>. |
| imagePullSecrets | list | `[]` |  |
| jobs.namespace | string | `""` | Namespace of the CI job pods, read for job names, pipelines and commits. Empty: builds are kept without them. Rendered with tpl. |
| jobs.rbac | object | `{"create":true}` | A Role and RoleBinding there that let the console get, list and watch pods. Without them, give that access some other way. |
| nameOverride | string | `""` |  |
| networkPolicy.deniedComponents | list | `["buildkit"]` | Pods of this namespace whose ci-accelerator.io/component label is one of these may not connect: the daemons, whose network every build shares. |
| networkPolicy.deniedNamespaces | list | `[]` | More namespaces whose pods may not connect (the jobs namespace is always one when jobs.namespace is set). Rendered with tpl. |
| networkPolicy.enabled | bool | `false` | Render a NetworkPolicy that admits connections from every namespace except this one, the jobs namespace and deniedNamespaces, and from pods of this namespace except deniedComponents. Only ingress is limited. |
| networkPolicy.extraFrom | list | `[]` | More NetworkPolicyPeer entries that may connect, e.g. an ipBlock with the node addresses when the Ingress controller runs on the host network. |
| nodeSelector | object | `{}` |  |
| persistence | object | `{"existingClaim":"","size":"10Gi","storageClass":""}` | The store. SQLite needs block storage (ReadWriteOnce): never NFS or another shared file system. |
| podAnnotations | object | `{}` |  |
| podLabels | object | `{}` |  |
| resources.limits.memory | string | `"1Gi"` |  |
| resources.requests.cpu | string | `"50m"` |  |
| resources.requests.memory | string | `"128Mi"` |  |
| retention.days | int | `30` | Days builds are kept in full (steps, reasons). Each image's daily figures (builds by outcome, build times) are kept a year regardless. At 150 pipelines a day with 15 image builds each, a project takes about 0.25 GB per 30 days. |
| retention.outputDays | int | `7` | Days the steps' output is kept. |
| service.port | int | `8080` |  |
| signIn | object | `{"enabled":false,"gitlab":[],"sessionKey":{"existingSecret":"","key":"key"},"url":""}` | Sign-in through GitLab. Each person signs in with a GitLab the runners serve and sees the projects whose jobs GitLab lets them read (Reporter or more, or public pipelines); GitLab is read with their own token. GitLab never calls the console: the browser carries the redirects. |
| signIn.gitlab | list | `[]` | One OAuth application per GitLab (User or Group Settings > Applications, or Admin > Applications on your own GitLab): confidential, scopes openid and read_api, redirect URI <url>/auth/callback/<id> (the NOTES and the console's log print it).   url: exactly as the runners have it; may be left empty when the        console serves one GitLab   existingSecret: a Secret with the keys client-id and client-secret   admins: GitLab usernames who see everything (builds of no known        project, the JSON API) |
| signIn.sessionKey | object | `{"existingSecret":"","key":"key"}` | A Secret holding 32 random bytes in base64 (openssl rand -base64 32); it seals each session's GitLab token. Changing it signs everyone out. |
| signIn.url | string | `""` | Where browsers reach the console: https (an Ingress or a gateway you add), or http only on localhost (a port-forward). Default: global.console.url. Rendered with tpl. |
| tls | object | `{"certManager":{"duration":"8760h","issuerRef":{"group":"cert-manager.io","kind":"ClusterIssuer","name":""},"renewBeforePercentage":33},"existingSecret":""}` | The client certificate the console presents to the daemons. Either a Secret with ca.crt, tls.crt and tls.key, or one cert-manager issues from the daemons' CA. |
| tls.certManager.issuerRef | object | `{"group":"cert-manager.io","kind":"ClusterIssuer","name":""}` | Default: global.tls.issuerRef. Rendered with tpl. |
| tolerations | list | `[]` |  |

