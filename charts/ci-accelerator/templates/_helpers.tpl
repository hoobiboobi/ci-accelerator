{{/*
Labels for the objects this chart creates itself.
*/}}
{{- define "ci-accelerator.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: ci-accelerator
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Full name of the buildkit subchart: <release>-buildkit, or the release name
when it already contains "buildkit" (the subchart's own rule; name overrides
are refused below).
*/}}
{{- define "ci-accelerator.buildkitName" -}}
{{- if contains "buildkit" .Release.Name }}{{ .Release.Name | trunc 63 | trimSuffix "-" }}{{ else }}{{ printf "%s-buildkit" .Release.Name | trunc 63 | trimSuffix "-" }}{{ end }}
{{- end }}

{{/*
The address CI jobs use: the build router's Service (global.router.service,
the control-plane subchart's name; its name overrides are refused), or the
daemons' Service without it.
*/}}
{{- define "ci-accelerator.routerName" -}}
{{- tpl (toString (dig "router" "service" "" .Values.global)) . }}
{{- end }}

{{- define "ci-accelerator.daemonsAddress" -}}
{{- if eq (toString (dig "router" "enabled" false (.Values.global | default dict))) "true" -}}
tcp://{{ include "ci-accelerator.routerName" . }}.{{ .Release.Namespace }}.svc:1234
{{- else -}}
tcp://{{ include "ci-accelerator.buildkitName" . }}.{{ .Release.Namespace }}.svc:1234
{{- end }}
{{- end }}

{{/*
The issuer of the daemons' certificates, as the buildkit subchart picks it:
global.tls.issuerRef, or the CA it creates (<namespace>-<full name>-ca).
*/}}
{{- define "ci-accelerator.issuerName" -}}
{{- default (printf "%s-%s-ca" .Release.Namespace (include "ci-accelerator.buildkitName" .) | trunc 240 | trimSuffix "-") .Values.global.tls.issuerRef.name }}
{{- end }}

{{/*
The daemons' headless Service, by the buildkit subchart's rule.
*/}}
{{- define "ci-accelerator.buildkitHeadless" -}}
{{- printf "%s-headless" (include "ci-accelerator.buildkitName" . | trunc 54 | trimSuffix "-") }}
{{- end }}

{{/*
Service names of the VictoriaMetrics and VictoriaTraces subcharts the box
had until 0.1.0. The box no longer has them; these stay because the OTLP
endpoints of 0.1.0's values name them, and `helm upgrade --reuse-values`
renders those values (NOTES then says the endpoints lead nowhere).
*/}}
{{- define "ci-accelerator.vmService" -}}
{{- $full := ternary .Release.Name (printf "%s-victoria-metrics-single" .Release.Name) (contains "victoria-metrics-single" .Release.Name) | trunc 63 | trimSuffix "-" }}
{{- printf "%s-server" $full | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "ci-accelerator.vtService" -}}
{{- $full := ternary .Release.Name (printf "%s-vt-single" .Release.Name) (contains "vt-single" .Release.Name) | trunc 63 | trimSuffix "-" }}
{{- printf "%s-server" $full | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The job pods' node selector and tolerations in the runner's TOML, called
from runners.config (rendered by the runner chart, so .Values are the
runner's values, with the globals). TOML refuses a key defined twice, so
both are built as dicts: kubernetes.io/arch appears once (the executor
picks the helper image's architecture from it), and tolerations that
render to the same runner key are merged. The runner's format is a map of
"key=value" (Equal) or "key" (Exists) to one effect; a key listed with
different effects becomes "" (all effects). tolerationSeconds has no place
in that format and is dropped.
*/}}
{{- define "ci-accelerator.runnerNodeSelector" -}}
{{- $selector := dict }}
{{- if .Values.global.buildNodes.enabled }}
{{- $selector = deepCopy (default (dict) .Values.global.buildNodes.selector) }}
{{- end }}
{{- $_ := set $selector "kubernetes.io/arch" .Values.global.jobs.architecture }}
{{- range $k, $v := $selector }}
{{ $k | quote }} = {{ toString $v | quote }}
{{- end }}
{{- end }}

{{- define "ci-accelerator.runnerTolerations" -}}
{{- $tolerations := dict }}
{{- if .Values.global.buildNodes.enabled }}
{{- range .Values.global.buildNodes.tolerations }}
{{- $key := toString (default "" .key) }}
{{- if ne (toString (default "Equal" .operator)) "Exists" }}
{{- $key = printf "%s=%s" $key (toString (default "" .value)) }}
{{- end }}
{{- $effect := toString (default "" .effect) }}
{{- if and (hasKey $tolerations $key) (ne (get $tolerations $key) $effect) }}
{{- $effect = "" }}
{{- end }}
{{- $_ := set $tolerations $key $effect }}
{{- end }}
{{- end }}
{{- range $k, $v := $tolerations }}
{{ $k | quote }} = {{ $v | quote }}
{{- end }}
{{- end }}

{{/*
Render-time checks for the box: required values and the names the
subcharts must agree on.
*/}}
{{- define "ci-accelerator.validate" -}}
{{- include "ci-accelerator.refuseOld" . }}
{{- $runner := index .Values "gitlab-runner" }}
{{- $jobs := .Values.global.jobs.namespace }}
{{- if eq $jobs .Release.Namespace }}
{{- fail (printf "global.jobs.namespace is the release namespace (%s). CI jobs run untrusted code: give them their own namespace, apart from the runner and the daemons." $jobs) }}
{{- end }}
{{- if $runner.enabled }}
{{- if not $runner.gitlabUrl }}
{{- fail "gitlab-runner.gitlabUrl is required: the URL of your GitLab, e.g. https://gitlab.example.com/ (to use your own runner with integrations/gitlab-runner/values.yaml, set gitlab-runner.enabled=false and this to the URL it uses)." }}
{{- end }}
{{- if not $runner.runners.secret }}
{{- fail (printf "gitlab-runner.runners.secret is required: a Secret in namespace %s with the runner authentication token, e.g. kubectl -n %s create secret generic gitlab-runner-token --from-literal=runner-token=glrt-... --from-literal=runner-registration-token=" .Release.Namespace .Release.Namespace) }}
{{- end }}
{{- include "ci-accelerator.preflightRunnerSecret" . }}
{{- if ne (toString $runner.runners.jobNamespace) $jobs }}
{{- fail (printf "gitlab-runner.runners.jobNamespace (%s) must equal global.jobs.namespace (%s): the runner's permissions are created there." (toString $runner.runners.jobNamespace) $jobs) }}
{{- end }}
{{- $arch := index (default (dict) .Values.global.buildNodes.selector) "kubernetes.io/arch" }}
{{- if and .Values.global.buildNodes.enabled $arch (ne (toString $arch) (toString .Values.global.jobs.architecture)) }}
{{- fail (printf "global.buildNodes.selector has kubernetes.io/arch=%s but global.jobs.architecture is %s: set both to the build nodes' architecture (the runner picks its helper image from it)." (toString $arch) (toString .Values.global.jobs.architecture)) }}
{{- end }}
{{- /* The runner chart renders runners.config the same way; a broken
TOML only shows up in the runner's log otherwise (the runner keeps
running with no [[runners]] and takes no jobs). */}}
{{- $config := tpl (toString $runner.runners.config) (dict "Values" $runner "Release" .Release "Chart" .Chart "Capabilities" .Capabilities "Template" .Template) }}
{{- $parsed := fromToml $config }}
{{- if hasKey $parsed "Error" }}
{{- fail (printf "gitlab-runner.runners.config does not render to valid TOML: %s" (get $parsed "Error")) }}
{{- end }}
{{- end }}
{{- /* The controller and the routers tell jobs by a hash of the runner's
URL (the box hands it to them in <release>-runner-gitlab) and do not
start without one. URLs given directly or in a ConfigMap of the user's own
do not need it. */}}
{{- $urlsFrom := tpl (toString (dig "control-plane" "gitlab" "urlsFrom" "configMap" "" .Values.AsMap)) . }}
{{- if and (dig "control-plane" "enabled" false .Values.AsMap) (not $runner.gitlabUrl) (not (dig "control-plane" "gitlab" "urls" nil .Values.AsMap)) (eq $urlsFrom (printf "%s-runner-gitlab" .Release.Name)) }}
{{- fail "gitlab-runner.gitlabUrl is required while the control plane is on, also with gitlab-runner.enabled=false: the URL of the GitLab your runner uses, exactly as the runner has it (the console and the router tell jobs by a hash of it). Or set control-plane.gitlab.urls, or control-plane.gitlab.urlsFrom.configMap to a ConfigMap of your own." }}
{{- end }}
{{- include "ci-accelerator.checkTiers" . }}
{{- include "ci-accelerator.preflightJobsNamespace" . }}
{{- if and (dig "control-plane" "enabled" false .Values.AsMap) (eq (toString .Values.buildkit.tls.source) "existingSecret") (not (dig "control-plane" "tls" "existingSecret" "" .Values.AsMap)) }}
{{- fail "control-plane.enabled with buildkit.tls.source=existingSecret: give the controller its own client certificate signed by the daemons' CA, control-plane.tls.existingSecret (a Secret with ca.crt, tls.crt and tls.key in the release namespace), and its API a server certificate, control-plane.controller.api.tls.existingSecret." }}
{{- end }}
{{- $cp := index .Values "control-plane" | default dict }}
{{- $router := eq (toString (dig "router" "enabled" false .Values.global)) "true" }}
{{- if and $router (not $cp.enabled) }}
{{- fail "the build router is part of the control plane: with control-plane.enabled=false set global.router.enabled=false too (the CI jobs then connect to the daemons' Service)." }}
{{- end }}
{{- /* The control plane reads its own router.enabled first; the runners'
address and the daemons' NetworkPolicy and certificate follow only the
global switch. */}}
{{- $cpRouter := dig "router" "enabled" nil $cp }}
{{- if and (not (kindIs "invalid" $cpRouter)) (ne (eq (toString $cpRouter) "true") $router) }}
{{- fail "control-plane.router.enabled must be left unset or equal global.router.enabled: global.router.enabled switches the router, the runners' address and the daemons' NetworkPolicy and certificate together." }}
{{- end }}
{{- if and $router (or $cp.fullnameOverride $cp.nameOverride) }}
{{- fail "control-plane.fullnameOverride and nameOverride are not supported with the router: the runner configuration derives the router's address from the release name." }}
{{- end }}
{{- /* The router gets the daemons' PriorityClass by name: it must exist. */}}
{{- $pc := dig "router" "priorityClassName" "" $cp }}
{{- if and $router (contains "ci-accelerator.buildkitName" $pc) (or .Values.buildkit.priorityClassName (not (dig "create" true (.Values.buildkit.priorityClass | default dict)))) }}
{{- fail "the router runs with the daemons' PriorityClass, which buildkit.priorityClass.create=false or buildkit.priorityClassName leaves out: set control-plane.router.priorityClassName to an existing class (the daemons' own), or \"\" for none." }}
{{- end }}
{{- if or .Values.buildkit.fullnameOverride .Values.buildkit.nameOverride }}
{{- fail "buildkit.fullnameOverride and buildkit.nameOverride are not supported in this chart: the runner configuration derives the daemons' address from the release name." }}
{{- end }}
{{- end }}

{{/*
The jobs namespace, when this release creates it: Helm refuses to adopt a
namespace it did not create ("cannot be imported into the current
release"), and one that is still being deleted (a reinstall right after
uninstall) fails the install half-way. When it no longer creates it, but
did: Helm deletes the namespace, with every job pod, the client
certificate and the runners' roles in it, unless the live object says
helm.sh/resource-policy: keep. lookup is empty offline.
*/}}
{{- define "ci-accelerator.preflightJobsNamespace" -}}
{{- $name := .Values.global.jobs.namespace }}
{{- if not .Values.jobsNamespace.create }}
{{- $ns := lookup "v1" "Namespace" "" $name }}
{{- $annotations := default (dict) (dig "metadata" "annotations" (dict) $ns) }}
{{- if and $ns (eq (toString (index $annotations "meta.helm.sh/release-name")) .Release.Name) (eq (toString (index $annotations "meta.helm.sh/release-namespace")) .Release.Namespace) (ne (toString (index $annotations "helm.sh/resource-policy")) "keep") }}
{{- fail (printf "jobsNamespace.create=false would make Helm delete Namespace %s (global.jobs.namespace), which this release created, together with the running CI jobs and everything else in it. Keep jobsNamespace.create=true, or first tell Helm to keep the namespace: kubectl annotate namespace %s helm.sh/resource-policy=keep (it then also stays after helm uninstall)." $name $name) }}
{{- end }}
{{- else }}
{{- $ns := lookup "v1" "Namespace" "" $name }}
{{- if $ns }}
{{- $owner := index (default (dict) $ns.metadata.annotations) "meta.helm.sh/release-name" }}
{{- $ownerNs := index (default (dict) $ns.metadata.annotations) "meta.helm.sh/release-namespace" }}
{{- if $ns.metadata.deletionTimestamp }}
{{- fail (printf "Namespace %s (global.jobs.namespace) is still being deleted, probably by a previous uninstall of this chart. Wait until it is gone (kubectl wait --for=delete namespace/%s --timeout=5m) and install again." $name $name) }}
{{- end }}
{{- if not (and (eq (toString $owner) .Release.Name) (eq (toString $ownerNs) .Release.Namespace)) }}
{{- fail (printf "Namespace %s (global.jobs.namespace) exists and was not created by this release. Either set jobsNamespace.create=false to use it as it is (label it pod-security.kubernetes.io/enforce=baseline), or delete it, or choose another global.jobs.namespace." $name) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The runner token Secret, when it already exists: the runner chart mounts
both keys (a missing one leaves the runner in ContainerCreating), and a
token that is not one only shows up as "is not valid" in the runner's log.
lookup is empty offline, so the check is skipped there.
*/}}
{{- define "ci-accelerator.preflightRunnerSecret" -}}
{{- $runner := index .Values "gitlab-runner" }}
{{- if and .Values.preflight.runnerSecret $runner.runners.secret }}
{{- $name := $runner.runners.secret }}
{{- $ns := .Release.Namespace }}
{{- $secret := lookup "v1" "Secret" $ns $name }}
{{- if $secret }}
{{- $data := default (dict) $secret.data }}
{{- if not (hasKey $data "runner-registration-token") }}
{{- fail (printf "Secret %s/%s (gitlab-runner.runners.secret) has no runner-registration-token key. The runner chart mounts it even when empty; add it: kubectl -n %s patch secret %s --type merge -p '{\"data\":{\"runner-registration-token\":\"\"}}' (or set preflight.runnerSecret=false to skip this check)" $ns $name $ns $name) }}
{{- end }}
{{- /* The runner reads it with $(cat ...), which drops trailing newlines
(a Secret made with --from-file has one). */}}
{{- $token := b64dec (default "" (get $data "runner-token")) | trimSuffix "\n" | trimSuffix "\r" }}
{{- $registration := b64dec (default "" (get $data "runner-registration-token")) }}
{{- if and (not $token) (not $registration) }}
{{- fail (printf "Secret %s/%s (gitlab-runner.runners.secret) has no token: put the runner authentication token from GitLab (glrt-...) into its runner-token key." $ns $name) }}
{{- end }}
{{- if and $token (not (regexMatch "^[A-Za-z0-9._-]+$" $token)) }}
{{- fail (printf "Secret %s/%s (gitlab-runner.runners.secret): runner-token does not look like a token (it contains spaces or characters outside A-Z, a-z, 0-9, '.', '_' and '-'; a copied placeholder?). Re-create it with the token from GitLab." $ns $name) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
ci-accelerator 0.3.0 split the console (0.2.x) into the control plane's
controller and web, with new names for every object and value. It does
not take over a 0.2.x release: refuse values under console: (kept by
--reuse-values, or an old values file) and a release that still runs the
old console, with the way forward. lookup is empty under helm template and
GitOps renders.
*/}}
{{- define "ci-accelerator.refuseOld" -}}
{{- $how := printf "ci-accelerator 0.3.0 cannot upgrade a 0.2.x release in place: the console became the control plane (a controller and a web) and its objects have new names. Remove the release (helm uninstall %s -n %s), wait until its pods are gone, delete the daemons' certificate Secret (kubectl -n %s delete secret %s-tls: the new certificate names the build router; without cert-manager make it again with scripts/buildkit-certs.sh --router), and install 0.3.0; the daemons' cache on hostPath stays, and the old console's history stays on the PersistentVolumeClaim of its Deployment (<name>-data) until you delete it." .Release.Name .Release.Namespace .Release.Namespace (include "ci-accelerator.buildkitName" .) }}
{{- if hasKey .Values "console" }}
{{- fail (printf "values under console: are the 0.2.x console's: since 0.3.0 they are under control-plane: (the controller's pod settings and persistence under control-plane.controller, the web's under control-plane.web; see the upgrade notes). %s" $how) }}
{{- end }}
{{- /* The 0.2.x console's Deployment: <release>-console, or <release> when
the release name holds "console". Read by name: no list of Deployments. */}}
{{- range (uniq (list (printf "%s-console" .Release.Name | trunc 63 | trimSuffix "-") .Release.Name)) }}
{{- $d := lookup "apps/v1" "Deployment" $.Release.Namespace . }}
{{- $l := dig "metadata" "labels" (dict) $d }}
{{- if and (eq (get $l "app.kubernetes.io/instance") $.Release.Name) (eq (get $l "ci-accelerator.io/component") "console") }}
{{- fail (printf "release %s has the console of ci-accelerator 0.2.x (Deployment %s). %s" $.Release.Name . $how) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
The include line of the GitLab CI component of this release (ci/gitlab/build.yml
at the tag v<chart version>): its integrity is the sha256 of that file, kept
equal to it by the component's tests and the release check.
*/}}
{{- define "ci-accelerator.componentIntegrity" -}}
sha256-utebAOE9RqUh3Hh/aF8rOX0x1N2LESl7XKyQqQ/pZkU=
{{- end }}
