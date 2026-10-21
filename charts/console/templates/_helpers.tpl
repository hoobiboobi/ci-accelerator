{{/*
Chart name, overridable via nameOverride.
*/}}
{{- define "console.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified name: <release>-<chart>, or just <release> when the release
name already contains the chart name. Truncated to 63 chars (DNS label).
*/}}
{{- define "console.fullname" -}}
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

{{- define "console.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels: never add anything that can change between upgrades.
*/}}
{{- define "console.selectorLabels" -}}
app.kubernetes.io/name: {{ include "console.name" . }}
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end }}

{{/*
ci-accelerator.io/component lets the daemons' NetworkPolicy admit the
console.
*/}}
{{- define "console.componentLabels" -}}
app.kubernetes.io/part-of: ci-accelerator
ci-accelerator.io/component: console
{{- end }}

{{- define "console.labels" -}}
{{- $own := merge
  (include "console.selectorLabels" . | fromYaml)
  (include "console.componentLabels" . | fromYaml)
  (dict
    "helm.sh/chart" (include "console.chart" .)
    "app.kubernetes.io/version" .Chart.AppVersion
    "app.kubernetes.io/managed-by" .Release.Service) }}
{{- toYaml (merge $own (default (dict) .Values.commonLabels)) }}
{{- end }}

{{/*
Pod labels: without the chart version, so a chart release that changes
nothing else does not restart the console.
*/}}
{{- define "console.podLabels" -}}
{{- $own := merge
  (include "console.selectorLabels" . | fromYaml)
  (include "console.componentLabels" . | fromYaml) }}
{{- toYaml (merge $own (default (dict) .Values.podLabels) (default (dict) .Values.commonLabels)) }}
{{- end }}

{{- define "console.image" -}}
{{- $tag := default (printf "v%s" .Chart.AppVersion) .Values.image.tag }}
{{- if .Values.image.digest }}
{{- printf "%s:%s@%s" .Values.image.repository $tag .Values.image.digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}
{{- end }}

{{- define "console.discover" -}}
{{- tpl .Values.daemons.discover . }}
{{- end }}

{{- define "console.jobsNamespace" -}}
{{- tpl .Values.jobs.namespace . }}
{{- end }}

{{- define "console.urlsConfigMap" -}}
{{- tpl .Values.gitlab.urlsFrom.configMap . }}
{{- end }}

{{/*
Secret with the client certificate: the user's, or the one cert-manager
fills.
*/}}
{{- define "console.tlsSecretName" -}}
{{- default (printf "%s-client" (include "console.fullname" .)) .Values.tls.existingSecret }}
{{- end }}

{{/*
Issuer of the client certificate: tls.certManager.issuerRef, else
global.tls.issuerRef. The name is rendered with tpl. As YAML.
*/}}
{{- define "console.issuerRef" -}}
{{- $ref := .Values.tls.certManager.issuerRef }}
{{- if not $ref.name }}
{{- $ref = .Values.global.tls.issuerRef }}
{{- end }}
{{- toYaml (dict "name" (tpl (default "" $ref.name) .) "kind" (default "ClusterIssuer" $ref.kind) "group" (default "cert-manager.io" $ref.group)) }}
{{- end }}

{{- define "console.pvcName" -}}
{{- default (printf "%s-data" (include "console.fullname" .)) .Values.persistence.existingClaim }}
{{- end }}

{{/*
Checks that fail the render with a message naming the value to set.
*/}}
{{- define "console.validate" -}}
{{- if and (not .Values.gitlab.urls) (not (include "console.urlsConfigMap" .)) }}
{{- fail "console: set gitlab.urls (the runners' gitlabUrl, exactly) or gitlab.urlsFrom.configMap" }}
{{- end }}
{{- $d := include "console.discover" . }}
{{- if not $d }}
{{- fail "console: set daemons.discover to the BuildKit chart's headless Service, host:port (e.g. buildkit-headless.ci-accelerator.svc:1234)" }}
{{- end }}
{{- if not (regexMatch "^[a-z0-9.-]+:[0-9]+$" $d) }}
{{- fail (printf "console: daemons.discover must be host:port, got %q" $d) }}
{{- end }}
{{- if and (not .Values.tls.existingSecret) (not (include "console.issuerRef" . | fromYaml).name) }}
{{- fail "console: the client certificate needs tls.existingSecret, or an issuer in tls.certManager.issuerRef or global.tls.issuerRef (the daemons' CA)" }}
{{- end }}
{{- if or (lt (int .Values.retention.days) 1) (lt (int .Values.retention.outputDays) 1) (gt (int .Values.retention.outputDays) (int .Values.retention.days)) }}
{{- fail "console: retention.days and retention.outputDays must be at least 1, and output kept no longer than builds" }}
{{- end }}
{{- end }}
