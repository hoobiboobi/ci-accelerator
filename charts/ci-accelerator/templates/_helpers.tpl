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
{{- $runner := index .Values "gitlab-runner" }}
{{- $jobs := .Values.global.jobs.namespace }}
{{- if eq $jobs .Release.Namespace }}
{{- fail (printf "global.jobs.namespace is the release namespace (%s). CI jobs run untrusted code: give them their own namespace, apart from the runner and the daemons." $jobs) }}
{{- end }}
{{- if $runner.enabled }}
{{- if not $runner.gitlabUrl }}
{{- fail "gitlab-runner.gitlabUrl is required: the URL of your GitLab, e.g. https://gitlab.example.com/ (or set gitlab-runner.enabled=false to use your own runner with integrations/gitlab-runner/values.yaml)." }}
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
{{- include "ci-accelerator.checkTiers" . }}
{{- include "ci-accelerator.checkDiskProbe" . }}
{{- include "ci-accelerator.preflightJobsNamespace" . }}
{{- if and (dig "console" "enabled" false .Values.AsMap) (eq (toString .Values.buildkit.tls.source) "existingSecret") (not (dig "console" "tls" "existingSecret" "" .Values.AsMap)) }}
{{- fail "console.enabled with buildkit.tls.source=existingSecret: give the console its own client certificate signed by the daemons' CA, console.tls.existingSecret (a Secret with ca.crt, tls.crt and tls.key in the release namespace)." }}
{{- end }}
{{- if or .Values.buildkit.fullnameOverride .Values.buildkit.nameOverride }}
{{- fail "buildkit.fullnameOverride and buildkit.nameOverride are not supported in this chart: the runner configuration derives the daemons' address from the release name." }}
{{- end }}
{{- end }}

{{/*
The jobs namespace, when this release creates it: Helm refuses to adopt a
namespace it did not create ("cannot be imported into the current
release"), and one that is still being deleted (a reinstall right after
uninstall) fails the install half-way. lookup is empty offline.
*/}}
{{- define "ci-accelerator.preflightJobsNamespace" -}}
{{- if .Values.jobsNamespace.create }}
{{- $name := .Values.global.jobs.namespace }}
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
The disk probe runs the console's image next to each daemon, and the
console reads it on its port: both pairs must agree.
*/}}
{{- define "ci-accelerator.checkDiskProbe" -}}
{{- $probe := .Values.buildkit.diskProbe | default dict }}
{{- $console := .Values.console | default dict }}
{{- $port := int (dig "daemons" "diskProbePort" 0 $console) }}
{{- if $console.enabled }}
{{- if and $probe.enabled (ne $port (int (default 9180 $probe.port))) }}
{{- fail (printf "console.daemons.diskProbePort (%d) must equal buildkit.diskProbe.port (%d)." $port (int (default 9180 $probe.port))) }}
{{- end }}
{{- if and (not $probe.enabled) (gt $port 0) }}
{{- fail "console.daemons.diskProbePort is set but buildkit.diskProbe.enabled is false: enable the probe, or set the port to 0." }}
{{- end }}
{{- if $probe.enabled }}
{{- $pi := $probe.image | default dict }}
{{- $ci := $console.image | default dict }}
{{- if or (ne (toString $pi.repository) (toString $ci.repository)) (ne (toString $pi.tag) (toString $ci.tag)) (ne (toString $pi.digest) (toString $ci.digest)) }}
{{- fail (printf "buildkit.diskProbe.image (%s:%s %s) must equal console.image (%s:%s %s): the probe is the console's program. Set both to the same repository, tag and digest." (toString $pi.repository) (toString $pi.tag) (toString $pi.digest) (toString $ci.repository) (toString $ci.tag) (toString $ci.digest)) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
