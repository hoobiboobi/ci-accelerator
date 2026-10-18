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
{{- if ne (toString $runner.runners.jobNamespace) $jobs }}
{{- fail (printf "gitlab-runner.runners.jobNamespace (%s) must equal global.jobs.namespace (%s): the runner's permissions are created there." (toString $runner.runners.jobNamespace) $jobs) }}
{{- end }}
{{- end }}
{{- if or .Values.buildkit.fullnameOverride .Values.buildkit.nameOverride }}
{{- fail "buildkit.fullnameOverride and buildkit.nameOverride are not supported in this chart: the runner configuration derives the daemons' address from the release name." }}
{{- end }}
{{- end }}
