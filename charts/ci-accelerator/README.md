# ci-accelerator

![Version: 0.3.2](https://img.shields.io/badge/Version-0.3.2-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.3.2](https://img.shields.io/badge/AppVersion-0.3.2-informational?style=flat-square)

A self-hosted fast CI runner for GitLab: persistent BuildKit daemons with a shared cache on dedicated build nodes, and a gitlab-runner that sends CI jobs to them.

The box: one release that installs persistent BuildKit daemons on your build nodes, a gitlab-runner whose jobs use them, the build router that sends each job to the node where its project's cache is, and the control plane whose console explains every build. Set the GitLab URL and the runner token's Secret; everything else has working defaults.

What the release runs (with the release name `ci-accelerator`, objects are `ci-accelerator-...`):

- **The daemons** (`<release>-buildkit`, a DaemonSet on the build nodes, BuildKit pinned by tag and digest), each with a small disk probe next to it for the console's Nodes page.
- **The runner** (the official `gitlab-runner` chart). Its CI jobs run in their own namespace (`ci-accelerator-jobs`, created and deleted with the release, Pod Security `baseline`) on the build nodes, with a client certificate mounted, and connect to `tcp://<release>-router.<namespace>.svc:1234`.
- **The build router** (`<release>-router`, a DaemonSet on the build nodes). It knows each job by its pod's address and sends it to the daemon on the node where its project built in the last week (`control-plane.router.warmDays`); a project with no such node goes to its node by a hash every router agrees on, so a pipeline's parallel jobs share one daemon's steps. It passes the jobs' TLS through and holds no key; the daemons admit only the routers and the controller. `global.router.enabled=false` lets the jobs connect to the daemons' Service instead (their own node's daemon).
- **The control plane**: the controller (`<release>-controller`, one pod, its store on a volume; it reads every daemon's build history and the job pods and holds every GitLab token) and the web (`<release>-web`, two pods, nothing kept), which serves the console on a ClusterIP Service. `control-plane.enabled=false` leaves both out; the router is part of the control plane, so it needs `global.router.enabled=false` too (the chart refuses it otherwise).

Before you install: Kubernetes 1.34 or newer, cert-manager (the daemons, the jobs and the controller authenticate each other with certificates it issues), build nodes labelled and tainted `ci-accelerator.io/node-role=build`, a release namespace that allows privileged pods (the daemons need them), a default StorageClass for the controller's volume, and a CNI that enforces NetworkPolicy for the policies the box ships.

```sh
helm install ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.3.2 \
  -n ci-accelerator \
  --set gitlab-runner.gitlabUrl=https://gitlab.example.com/ \
  --set gitlab-runner.runners.secret=gitlab-runner-token \
  --wait --timeout 10m
```

For production, say where the cache lives (`buildkit.storage`: a hostPath on a dedicated disk; the default emptyDir is emptied by every rollout, and every project starts cold again) and pin the control plane's image by digest (`control-plane.image.digest`). The [documentation](https://ci-accelerator.io/docs/) has the quickstart, the requirements, the console's levels and the limitations; [GitOps](https://ci-accelerator.io/docs/gitops/) installs the same release with Flux or Argo CD. The subcharts' values are under `buildkit`, `control-plane` and `gitlab-runner`; the table below lists the box's own keys.

**Upgrading.** From 0.3.0 on, `helm upgrade --reset-then-reuse-values` upgrades in place. A 0.2.x release does not upgrade to 0.3.0: the console became the control plane and its objects and values have new names, so the chart refuses and says how to move (uninstall, delete the daemons' certificate Secret `<release>-buildkit-tls`, move the values under `console:` to `control-plane:`, install again; a hostPath cache stays). Step by step: [Upgrading from 0.2.x to 0.3.0](https://ci-accelerator.io/docs/troubleshooting/#upgrading-from-02x-to-030).

## Requirements

Kubernetes: `>=1.34.0-0`

| Repository | Name | Version |
|------------|------|---------|
| file://../buildkit | buildkit | 0.3.2 |
| file://../control-plane | control-plane | 0.3.2 |
| https://charts.gitlab.io | gitlab-runner | 0.93.0 |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| buildkit.diskProbe.enabled | bool | `true` | The probe next to each daemon (charts/buildkit diskProbe). |
| buildkit.networkPolicy.enabled | bool | `true` |  |
| buildkit.networkPolicy.ingress.extraFrom[0].podSelector.matchLabels."ci-accelerator.io/component" | string | `"controller"` |  |
| buildkit.tls.client.namespaces[0] | string | `"{{ .Values.global.jobs.namespace }}"` |  |
| control-plane.caches.fromConfigMap | string | `"{{ .Release.Name }}-runner-caches"` |  |
| control-plane.daemons.discover | string | `"{{ include \"ci-accelerator.buildkitHeadless\" . }}.{{ .Release.Namespace }}.svc:1234"` |  |
| control-plane.enabled | bool | `true` |  |
| control-plane.gitlab.urlsFrom.configMap | string | `"{{ .Release.Name }}-runner-gitlab"` |  |
| control-plane.jobs.namespace | string | `"{{ .Values.global.jobs.namespace }}"` |  |
| control-plane.networkPolicy.enabled | bool | `true` | Keep the box's CI jobs and builds away from the console: the jobs namespace and the daemons (RUN steps share their network) may not connect; Ingress controllers in other namespaces and port-forward still do. Needs a CNI that enforces NetworkPolicy; see charts/control-plane. |
| control-plane.router.priorityClassName | string | `"{{ .Release.Namespace }}-{{ include \"ci-accelerator.buildkitName\" . }}"` |  |
| control-plane.signIn.enabled | bool | `false` |  |
| control-plane.tls.certManager.issuerRef.name | string | `"{{ include \"ci-accelerator.issuerName\" . }}"` |  |
| control-plane.tls.existingSecret | string | `""` |  |
| gitlab-runner.enabled | bool | `true` |  |
| gitlab-runner.gitlabUrl | string | `""` | Required: your GitLab, e.g. https://gitlab.example.com/ |
| gitlab-runner.image.tag | string | `"alpine-v19.4.2"` |  |
| gitlab-runner.rbac.create | bool | `true` |  |
| gitlab-runner.rbac.rules[0].resources[0] | string | `"pods"` |  |
| gitlab-runner.rbac.rules[0].verbs[0] | string | `"create"` |  |
| gitlab-runner.rbac.rules[0].verbs[1] | string | `"delete"` |  |
| gitlab-runner.rbac.rules[0].verbs[2] | string | `"get"` |  |
| gitlab-runner.rbac.rules[0].verbs[3] | string | `"list"` |  |
| gitlab-runner.rbac.rules[0].verbs[4] | string | `"watch"` |  |
| gitlab-runner.rbac.rules[1].resources[0] | string | `"pods/attach"` |  |
| gitlab-runner.rbac.rules[1].resources[1] | string | `"pods/exec"` |  |
| gitlab-runner.rbac.rules[1].verbs[0] | string | `"create"` |  |
| gitlab-runner.rbac.rules[1].verbs[1] | string | `"delete"` |  |
| gitlab-runner.rbac.rules[1].verbs[2] | string | `"get"` |  |
| gitlab-runner.rbac.rules[1].verbs[3] | string | `"patch"` |  |
| gitlab-runner.rbac.rules[2].resources[0] | string | `"pods/log"` |  |
| gitlab-runner.rbac.rules[2].verbs[0] | string | `"get"` |  |
| gitlab-runner.rbac.rules[2].verbs[1] | string | `"list"` |  |
| gitlab-runner.rbac.rules[3].resources[0] | string | `"secrets"` |  |
| gitlab-runner.rbac.rules[3].verbs[0] | string | `"create"` |  |
| gitlab-runner.rbac.rules[3].verbs[1] | string | `"delete"` |  |
| gitlab-runner.rbac.rules[3].verbs[2] | string | `"get"` |  |
| gitlab-runner.rbac.rules[3].verbs[3] | string | `"update"` |  |
| gitlab-runner.rbac.rules[4].resources[0] | string | `"serviceaccounts"` |  |
| gitlab-runner.rbac.rules[4].verbs[0] | string | `"get"` |  |
| gitlab-runner.rbac.rules[5].resources[0] | string | `"services"` |  |
| gitlab-runner.rbac.rules[5].verbs[0] | string | `"create"` |  |
| gitlab-runner.rbac.rules[5].verbs[1] | string | `"get"` |  |
| gitlab-runner.rbac.rules[6].resources[0] | string | `"events"` |  |
| gitlab-runner.rbac.rules[6].verbs[0] | string | `"list"` |  |
| gitlab-runner.runners.cache | object | `{}` |  |
| gitlab-runner.runners.config | string | `"{{/* namespace = global.jobs.namespace, set in the TOML */}}{{ include \"ci-accelerator.runnerToml\" (dict \"root\" . \"gitlabUrl\" .Values.gitlabUrl \"build\" true \"cache\" (default (dict) .Values.runners.cache)) }}"` |  |
| gitlab-runner.runners.jobNamespace | string | `"ci-accelerator-jobs"` | Keep equal to global.jobs.namespace. |
| gitlab-runner.runners.secret | string | `""` | Required: a Secret in this release's namespace with the runner authentication token (runner-token: glrt-...) and an empty runner-registration-token key. |
| global.buildNodes | object | `{"enabled":true,"selector":{"ci-accelerator.io/node-role":"build"},"tolerations":[{"effect":"NoSchedule","key":"ci-accelerator.io/node-role","operator":"Equal","value":"build"}]}` | Dedicated build nodes: the daemons and the CI job pods run there, so a job reaches the daemon (and the cache) on its own node. Label and taint them before install. |
| global.console.url | string | `""` | The console's address as users reach it (https://...). When set, every job prints the link to its page in its log, and sign-in through GitLab (control-plane.signIn) uses it. Empty: no link. |
| global.jobs.architecture | string | `"amd64"` | CPU architecture of the build nodes (kubernetes.io/arch). The runner picks its helper image by it. |
| global.jobs.namespace | string | `"ci-accelerator-jobs"` | Namespace of the CI job pods. They run untrusted code, so they get their own namespace, apart from the runner and the daemons. Keep gitlab-runner.runners.jobNamespace equal to it. |
| global.router.enabled | bool | `true` | CI jobs connect to the build router (part of the control plane), which sends each job to the daemon where its project's cache is; the daemons then admit only the router and name it in their certificate. false: jobs connect to the daemons' Service (their own node's daemon). |
| global.router.service | string | `"{{ .Release.Name | trunc 40 | trimSuffix \"-\" }}-router"` |  |
| global.tls.issuerRef | object | `{"group":"cert-manager.io","kind":"ClusterIssuer","name":""}` | cert-manager issuer for daemon and client certificates. Empty name: a CA dedicated to this release is created (see buildkit.tls). |
| jobsNamespace.create | bool | `true` | false: the namespace exists already (or GitOps creates it); it needs the client certificate (issued here) and Pod Security "baseline". |
| jobsNamespace.podSecurity | string | `"baseline"` | Pod Security level enforced for the job pods. |
| preflight.runnerSecret | bool | `true` | Fail if gitlab-runner.runners.secret exists but lacks a key the runner chart mounts (runner-token, and runner-registration-token, which may be empty), or if runner-token does not look like a token (e.g. a copied placeholder). A Secret that does not exist yet is not an error. Needs read access to Secrets in the release namespace. |
| runnerTiers | object | `{}` |  |

