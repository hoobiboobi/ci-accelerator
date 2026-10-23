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

{{- define "console.signIn" -}}
{{- if dig "enabled" false (default (dict) .Values.signIn) }}true{{ end }}
{{- end }}

{{/*
Where browsers reach the console: signIn.url, else global.console.url.
*/}}
{{- define "console.signInURL" -}}
{{- $u := tpl (dig "url" "" (default (dict) .Values.signIn)) . }}
{{- if not $u }}{{ $u = dig "console" "url" "" (default (dict) .Values.global) }}{{ end }}
{{- trimSuffix "/" $u }}
{{- end }}

{{/*
The console's sign-in file: names of the mounted Secrets' files, no secret.
*/}}
{{- define "console.signInConfig" -}}
{{- $gl := list }}
{{- range $i, $g := dig "gitlab" (list) (default (dict) .Values.signIn) }}
{{- $gl = append $gl (dict "url" (default "" $g.url) "clientIdFile" (printf "/sign-in/gitlab-%d/client-id" $i) "secretFile" (printf "/sign-in/gitlab-%d/client-secret" $i) "admins" (default (list) $g.admins)) }}
{{- end }}
{{- toJson (dict "url" (include "console.signInURL" .) "sessionKeyFile" "/sign-in/key/key" "gitlab" $gl) }}
{{- end }}

{{/*
The cache folders listed in caches.stores, as the console reads them.
*/}}
{{- define "console.cachesJson" -}}
{{- $out := list }}
{{- range (dig "stores" (list) (default (dict) .Values.caches)) }}
{{- $s := dict "server" .server "bucket" .bucket "region" (default "" .region) "path" (trimAll "/" (default "" .path)) "instance" (sha256sum (toString .gitlabUrl) | trunc 6) "secret" .existingSecret }}
{{- if .insecure }}{{ $_ := set $s "insecure" true }}{{ end }}
{{- if hasKey . "pathStyle" }}{{ $_ := set $s "pathStyle" .pathStyle }}{{ end }}
{{- $out = append $out $s }}
{{- end }}
{{- toJson $out }}
{{- end }}

{{- define "console.cachesConfigMap" -}}
{{- $c := default (dict) .Values.caches }}
{{- if $c.stores }}{{ include "console.fullname" . }}-caches{{ else }}{{ tpl (default "" $c.fromConfigMap) . }}{{ end }}
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
{{- range $i, $c := (dig "stores" (list) (default (dict) .Values.caches)) }}
{{- range $k := list "server" "bucket" "region" "gitlabUrl" "existingSecret" }}
{{- if not (get $c $k) }}
{{- fail (printf "console: caches.stores[%d].%s is needed" $i $k) }}
{{- end }}
{{- end }}
{{- if regexMatch "^[a-z]+://|/" (toString $c.server) }}
{{- fail (printf "console: caches.stores[%d].server is a host (and port), without https:// or a path" $i) }}
{{- end }}
{{- end }}
{{- if include "console.signIn" . }}
{{- $u := include "console.signInURL" . }}
{{- if not $u }}
{{- fail "console: sign-in needs signIn.url (or global.console.url): where browsers reach the console" }}
{{- end }}
{{- if not (regexMatch "^(https://[^/?#\\s]+|http://(localhost|127\\.0\\.0\\.1|\\[::1\\])(:[0-9]+)?)(/[^?#\\s]*)?$" $u) }}
{{- fail (printf "console: signIn.url must be https (plain http only on localhost, for a port-forward), got %q" $u) }}
{{- end }}
{{- if not (dig "sessionKey" "existingSecret" "" (default (dict) .Values.signIn)) }}
{{- fail "console: sign-in needs signIn.sessionKey.existingSecret: a Secret with 32 random bytes in base64 (openssl rand -base64 32)" }}
{{- end }}
{{- $gl := dig "gitlab" (list) (default (dict) .Values.signIn) }}
{{- if not $gl }}
{{- fail "console: sign-in needs signIn.gitlab: the OAuth application of each GitLab (url, existingSecret)" }}
{{- end }}
{{- $seen := dict }}
{{- range $gl }}
{{- if not .existingSecret }}
{{- fail (printf "console: signIn.gitlab %q needs existingSecret: a Secret with the OAuth application's client-id and client-secret" (default "" .url)) }}
{{- end }}
{{- if and (not .url) (gt (len $gl) 1) }}
{{- fail "console: with several signIn.gitlab entries each needs its url, exactly as the runners have it" }}
{{- end }}
{{- if hasKey $seen (default "" .url) }}
{{- fail (printf "console: signIn.gitlab names %q twice" (default "" .url)) }}
{{- end }}
{{- $_ := set $seen (default "" .url) true }}
{{- if and .url $.Values.gitlab.urls (not (has .url $.Values.gitlab.urls)) }}
{{- fail (printf "console: signIn.gitlab url %q is not one of gitlab.urls (they must match exactly)" .url) }}
{{- end }}
{{- end }}
{{- if dig "token" "existingSecret" "" .Values.gitlab }}
{{- fail "console: gitlab.token and sign-in exclude each other: with sign-in GitLab is read with each person's own token" }}
{{- end }}
{{- end }}
{{- if or (lt (int .Values.retention.days) 1) (lt (int .Values.retention.outputDays) 1) (gt (int .Values.retention.outputDays) (int .Values.retention.days)) }}
{{- fail "console: retention.days and retention.outputDays must be at least 1, and output kept no longer than builds" }}
{{- end }}
{{- end }}
