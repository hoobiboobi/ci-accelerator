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
Name of the headless discovery Service: <fullname>-headless, within the
63-character limit of a Service name. Other components resolve the daemons
through it, so keep this the only place the name is built.
*/}}
{{- define "buildkit.headlessName" -}}
{{- printf "%s-headless" (include "buildkit.fullname" . | trunc 54 | trimSuffix "-") }}
{{- end }}

{{/*
Value for the helm.sh/chart label.
*/}}
{{- define "buildkit.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels. Immutable on DaemonSets — never add anything here that can
change between upgrades.
*/}}
{{- define "buildkit.selectorLabels" -}}
app.kubernetes.io/name: {{ include "buildkit.name" . }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{/*
Labels every chart-built object carries in addition to the selector labels.
ci-accelerator.io/component lets resources from other ci-accelerator charts
(NetworkPolicy, scrape configs) select the daemons.
*/}}
{{- define "buildkit.componentLabels" -}}
app.kubernetes.io/part-of: ci-accelerator
ci-accelerator.io/component: buildkit
{{- end }}

{{/*
Labels for top-level objects. commonLabels are added, but cannot override
the chart's own labels (sprig merge keeps the first dict's keys).
*/}}
{{- define "buildkit.labels" -}}
{{- $own := merge
  (include "buildkit.selectorLabels" . | fromYaml)
  (include "buildkit.componentLabels" . | fromYaml)
  (dict
    "helm.sh/chart" (include "buildkit.chart" .)
    "app.kubernetes.io/version" .Chart.AppVersion
    "app.kubernetes.io/managed-by" .Release.Service) }}
{{- toYaml (merge $own (default (dict) .Values.commonLabels)) }}
{{- end }}

{{/*
Labels for the pod template. Deliberately without helm.sh/chart and
app.kubernetes.io/version: any change of the pod template restarts every
daemon (and empties an emptyDir cache), so a chart release that changes
nothing else must not touch it. podLabels and commonLabels cannot override
the selector or component labels.
*/}}
{{- define "buildkit.podLabels" -}}
{{- $own := merge
  (include "buildkit.selectorLabels" . | fromYaml)
  (include "buildkit.componentLabels" . | fromYaml) }}
{{- toYaml (merge $own (default (dict) .Values.podLabels) (default (dict) .Values.commonLabels)) }}
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
Node selector: kubernetes.io/os=linux, then global.buildNodes.selector, then
nodeSelector; later sources win on conflicting keys.
*/}}
{{- define "buildkit.nodeSelector" -}}
{{- $bn := include "buildkit.buildNodes" . | fromJson }}
{{- $sel := merge (deepCopy (default (dict) .Values.nodeSelector)) (default (dict) $bn.selector) (dict "kubernetes.io/os" "linux") }}
{{- toYaml $sel }}
{{- end }}

{{/*
Tolerations: global.buildNodes.tolerations followed by tolerations.
*/}}
{{- define "buildkit.tolerations" -}}
{{- $bn := include "buildkit.buildNodes" . | fromJson }}
{{- $global := default (list) $bn.tolerations }}
{{- with concat $global (default (list) .Values.tolerations) }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{/*
Container image reference: repository:tag@digest when a digest is set (the
runtime pulls by digest; the tag stays readable), else repository:tag. The
tag defaults to the chart appVersion.
*/}}
{{- define "buildkit.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag }}
{{- if .Values.image.digest }}
{{- printf "%s:%s@%s" .Values.image.repository $tag .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}
{{- end }}

{{/*
Probe command. Exec probes inherit the container environment, and buildctl
turns standard tracing variables (OTEL_TRACES_EXPORTER, OTEL_EXPORTER_OTLP_*,
JAEGER_*) into an exporter that flushes on exit: with a slow or dead trace
backend every probe then takes 10-30 s and probes kill healthy daemons.
`env -i` starts buildctl with an empty environment; OTEL_TRACES_EXPORTER=none
is buildkit's own off switch. OTEL_SDK_DISABLED is not read by buildkit.
*/}}
{{- define "buildkit.probeExec" -}}
exec:
  command:
    - env
    - -i
    - OTEL_TRACES_EXPORTER=none
    - /usr/bin/buildctl
    - --addr
    - unix:///run/buildkit/buildkitd.sock
    - debug
    - workers
{{- end }}

{{/*
Path of the rendered buildkitd.toml inside the container. Kept out of
/etc/buildkit so the image's own files there (cdi, cni.json, provenance.d)
stay visible to buildkitd.
*/}}
{{- define "buildkit.configDir" -}}
/etc/buildkitd
{{- end }}

{{/*
Where the TLS Secret is mounted. Not under the config directory: that is a
read-only ConfigMap volume.
*/}}
{{- define "buildkit.tlsDir" -}}
/etc/buildkitd-tls
{{- end }}
