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
Names of the VictoriaMetrics and VictoriaTraces subcharts (their common
chart's rule: <release>-<name>, or the release name when it contains the
name; name overrides are refused below). The Services add "-server".
*/}}
{{- define "ci-accelerator.vmName" -}}
{{- if contains "victoria-metrics-single" .Release.Name }}{{ .Release.Name | trunc 63 | trimSuffix "-" }}{{ else }}{{ printf "%s-victoria-metrics-single" .Release.Name | trunc 63 | trimSuffix "-" }}{{ end }}
{{- end }}

{{- define "ci-accelerator.vtName" -}}
{{- if contains "vt-single" .Release.Name }}{{ .Release.Name | trunc 63 | trimSuffix "-" }}{{ else }}{{ printf "%s-vt-single" .Release.Name | trunc 63 | trimSuffix "-" }}{{ end }}
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
{{- end }}
{{- if or .Values.buildkit.fullnameOverride .Values.buildkit.nameOverride }}
{{- fail "buildkit.fullnameOverride and buildkit.nameOverride are not supported in this chart: the runner configuration derives the daemons' address from the release name." }}
{{- end }}
{{- $vm := index .Values "victoria-metrics-single" }}
{{- $vt := index .Values "victoria-traces-single" }}
{{- if or $vm.fullnameOverride $vm.nameOverride $vt.fullnameOverride (ne (toString (default "vt-single" $vt.nameOverride)) "vt-single") }}
{{- fail "fullnameOverride and nameOverride of victoria-metrics-single and victoria-traces-single are not supported in this chart: the daemons' OTLP endpoints are derived from the release name." }}
{{- end }}
{{- /* The default endpoints name this release's storage. */}}
{{- if and (not $vm.enabled) (contains "ci-accelerator.vmName" (toString .Values.buildkit.otel.metricsEndpoint)) }}
{{- fail "victoria-metrics-single.enabled is false, but buildkit.otel.metricsEndpoint still points at it: set it to your OTLP metrics endpoint (e.g. http://collector:4318/v1/metrics) or to \"\"." }}
{{- end }}
{{- if and (not $vt.enabled) (contains "ci-accelerator.vtName" (toString .Values.buildkit.otel.tracesEndpoint)) }}
{{- fail "victoria-traces-single.enabled is false, but buildkit.otel.tracesEndpoint still points at it: set it to your OTLP traces endpoint (e.g. http://collector:4318/v1/traces) or to \"\"." }}
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
{{- $token := b64dec (default "" (get $data "runner-token")) }}
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
