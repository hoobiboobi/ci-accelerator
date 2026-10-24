# ci-accelerator

![Version: 0.2.0](https://img.shields.io/badge/Version-0.2.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.2.0](https://img.shields.io/badge/AppVersion-0.2.0-informational?style=flat-square)

A self-hosted fast CI runner for GitLab: persistent BuildKit daemons with a shared cache on dedicated build nodes, and a gitlab-runner that sends CI jobs to them.

The box: one release that installs persistent BuildKit daemons on your build nodes, a gitlab-runner whose jobs use them, and the console that explains every build. Set the GitLab URL and the runner token's Secret; everything else has working defaults. cert-manager is a prerequisite (the daemons and jobs authenticate each other with certificates it issues).

```sh
helm install ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version <version> \
  -n ci-accelerator \
  --set gitlab-runner.gitlabUrl=https://gitlab.example.com/ \
  --set gitlab-runner.runners.secret=gitlab-runner-token \
  --wait --timeout 10m
```

For production, say where the cache lives (`buildkit.storage`: a hostPath on a dedicated disk) and pin the console image by digest. The repository's [README](https://github.com/hoobiboobi/ci-accelerator#readme) has the quickstart, the requirements, the console's levels and the limitations; [docs/gitops.md](https://github.com/hoobiboobi/ci-accelerator/blob/main/docs/gitops.md) installs the same release with Flux or Argo CD. The subcharts' values are under `buildkit`, `console` and `gitlab-runner`; the table below lists the box's own keys.

## Requirements

Kubernetes: `>=1.34.0-0`

| Repository | Name | Version |
|------------|------|---------|
| file://../buildkit | buildkit | 0.2.0 |
| file://../console | console | 0.2.0 |
| https://charts.gitlab.io | gitlab-runner | 0.93.0 |

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| buildkit.networkPolicy.enabled | bool | `true` |  |
| buildkit.networkPolicy.ingress.extraFrom[0].podSelector.matchLabels."ci-accelerator.io/component" | string | `"console"` |  |
| buildkit.tls.client.namespaces[0] | string | `"{{ .Values.global.jobs.namespace }}"` |  |
| console.caches.fromConfigMap | string | `"{{ .Release.Name }}-console-caches"` |  |
| console.daemons.discover | string | `"{{ include \"ci-accelerator.buildkitHeadless\" . }}.{{ .Release.Namespace }}.svc:1234"` |  |
| console.enabled | bool | `true` |  |
| console.gitlab.urlsFrom.configMap | string | `"{{ .Release.Name }}-console-gitlab"` |  |
| console.jobs.namespace | string | `"{{ .Values.global.jobs.namespace }}"` |  |
| console.signIn.enabled | bool | `false` |  |
| console.tls.certManager.issuerRef.name | string | `"{{ include \"ci-accelerator.issuerName\" . }}"` |  |
| console.tls.existingSecret | string | `""` |  |
| gitlab-runner.enabled | bool | `true` |  |
| gitlab-runner.gitlabUrl | string | `""` | Required: your GitLab, e.g. https://gitlab.example.com/ |
| gitlab-runner.image.tag | string | `"alpine-v19.4.1"` |  |
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
| global.console.url | string | `""` | The console's address as users reach it (https://...). When set, every job prints the link to its page in its log, and sign-in through GitLab (console.signIn) uses it. Empty: no link. |
| global.jobs.architecture | string | `"amd64"` | CPU architecture of the build nodes (kubernetes.io/arch). The runner picks its helper image by it. |
| global.jobs.namespace | string | `"ci-accelerator-jobs"` | Namespace of the CI job pods. They run untrusted code, so they get their own namespace, apart from the runner and the daemons. Keep gitlab-runner.runners.jobNamespace equal to it. |
| global.tls.issuerRef | object | `{"group":"cert-manager.io","kind":"ClusterIssuer","name":""}` | cert-manager issuer for daemon and client certificates. Empty name: a CA dedicated to this release is created (see buildkit.tls). |
| jobsNamespace.create | bool | `true` | false: the namespace exists already (or GitOps creates it); it needs the client certificate (issued here) and Pod Security "baseline". |
| jobsNamespace.podSecurity | string | `"baseline"` | Pod Security level enforced for the job pods. |
| preflight.runnerSecret | bool | `true` | Fail if gitlab-runner.runners.secret exists but lacks a key the runner chart mounts (runner-token, and runner-registration-token, which may be empty), or if runner-token does not look like a token (e.g. a copied placeholder). A Secret that does not exist yet is not an error. Needs read access to Secrets in the release namespace. |
| runnerTiers | object | `{}` |  |

