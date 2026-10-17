{{/*
Chart name, overridable via nameOverride.
*/}}
{{- define "buildkit.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified name: <release>-<chart>, or just <release> when the release
name already contains the chart name. Truncated to 63 chars (DNS label).
*/}}
{{- define "buildkit.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Value for the helm.sh/chart label.
*/}}
{{- define "buildkit.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels. Immutable on DaemonSets/StatefulSets — never add anything
here that can change between upgrades.
*/}}
{{- define "buildkit.selectorLabels" -}}
app.kubernetes.io/name: {{ include "buildkit.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Common labels. ci-accelerator.io/component lets resources from other
ci-accelerator charts (NetworkPolicy, scrape configs) select the daemons.
*/}}
{{- define "buildkit.labels" -}}
helm.sh/chart: {{ include "buildkit.chart" . }}
{{ include "buildkit.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: ci-accelerator
ci-accelerator.io/component: buildkit
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Build-node placement from global.buildNodes, or nothing when it is disabled.
*/}}
{{- define "buildkit.buildNodes" -}}
{{- $bn := default (dict) .Values.global.buildNodes }}
{{- if ne (toString $bn.enabled) "false" }}
{{- toJson $bn }}
{{- else }}
{{- toJson (dict) }}
{{- end }}
{{- end }}

{{/*
Node selector: global.buildNodes.selector with nodeSelector merged over it.
*/}}
{{- define "buildkit.nodeSelector" -}}
{{- $bn := include "buildkit.buildNodes" . | fromJson }}
{{- $global := default (dict) $bn.selector }}
{{- $sel := merge (deepCopy .Values.nodeSelector) $global }}
{{- with $sel }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{/*
Tolerations: global.buildNodes.tolerations followed by tolerations.
*/}}
{{- define "buildkit.tolerations" -}}
{{- $bn := include "buildkit.buildNodes" . | fromJson }}
{{- $global := default (list) $bn.tolerations }}
{{- with concat $global .Values.tolerations }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{/*
Container image reference. Digest wins over tag; tag defaults to appVersion.
*/}}
{{- define "buildkit.image" -}}
{{- if .Values.image.digest }}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}
{{- end }}

{{/*
Probe command. Exec probes inherit the container env, so OTEL is switched
off and the OTLP endpoints are removed explicitly: with tracing configured,
a dead trace backend hangs the probe and liveness kills every daemon at once.
*/}}
{{- define "buildkit.probeExec" -}}
exec:
  command:
    - env
    - -u
    - OTEL_EXPORTER_OTLP_ENDPOINT
    - -u
    - OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
    - OTEL_SDK_DISABLED=true
    - buildctl
    - --addr
    - unix:///run/buildkit/buildkitd.sock
    - debug
    - workers
{{- end }}
