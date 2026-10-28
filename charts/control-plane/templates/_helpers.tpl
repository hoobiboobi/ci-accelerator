{{/*
App name (app.kubernetes.io/name), overridable via nameOverride.
*/}}
{{- define "control-plane.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Prefix of every object's name: fullnameOverride, else the release name.
Short enough for "-controller-tokenreview" and the namespace in front of it
to stay a valid name.
*/}}
{{- define "control-plane.prefix" -}}
{{- default .Release.Name .Values.fullnameOverride | trunc 40 | trimSuffix "-" }}
{{- end }}

{{/* The controller: Deployment, ServiceAccount, the API's Service. */}}
{{- define "control-plane.controllerName" -}}
{{- printf "%s-controller" (include "control-plane.prefix" .) }}
{{- end }}

{{/* The web: Deployment, ServiceAccount, the console's Service. */}}
{{- define "control-plane.webName" -}}
{{- printf "%s-web" (include "control-plane.prefix" .) }}
{{- end }}

{{/* The build router: DaemonSet, ServiceAccount, Service. */}}
{{- define "control-plane.routerName" -}}
{{- printf "%s-router" (include "control-plane.prefix" .) }}
{{- end }}

{{/* The router runs: router.enabled, else global.router.enabled. */}}
{{- define "control-plane.router" -}}
{{- $on := dig "router" "enabled" nil .Values.AsMap }}
{{- if kindIs "invalid" $on }}{{ $on = dig "router" "enabled" false (.Values.global | default dict) }}{{ end }}
{{- if eq (toString $on) "true" }}true{{ end }}
{{- end }}

{{/*
The daemons' headless Service from daemons.discover (host:port): its name,
namespace (default: the release's) and port. As JSON.
*/}}
{{- define "control-plane.daemonsService" -}}
{{- $d := include "control-plane.discover" . }}
{{- $host := splitList ":" $d | first }}
{{- $labels := splitList "." $host }}
{{- $ns := .Release.Namespace }}
{{- if gt (len $labels) 1 }}{{ $ns = index $labels 1 }}{{ end }}
{{- toJson (dict "name" (first $labels) "namespace" $ns "port" (splitList ":" $d | last)) }}
{{- end }}

{{/* Namespaces whose pods may reach the router. As JSON. */}}
{{- define "control-plane.routerClients" -}}
{{- $out := list }}
{{- range (dig "router" "clientNamespaces" (list) .Values.AsMap) }}{{ $out = append $out (tpl . $) }}{{ end }}
{{- if not $out }}{{ with include "control-plane.jobsNamespace" . }}{{ $out = list . }}{{ end }}{{ end }}
{{- toJson $out }}
{{- end }}

{{/* Placement on the build nodes (global.buildNodes) merged with router's. */}}
{{- define "control-plane.routerNodeSelector" -}}
{{- $bn := default (dict) .Values.global.buildNodes }}
{{- $sel := dict "kubernetes.io/os" "linux" }}
{{- if ne (toString $bn.enabled) "false" }}{{ $sel = merge (deepCopy (default (dict) $bn.selector)) $sel }}{{ end }}
{{- toYaml (merge (deepCopy (default (dict) .Values.router.nodeSelector)) $sel) }}
{{- end }}

{{- define "control-plane.routerTolerations" -}}
{{- $bn := default (dict) .Values.global.buildNodes }}
{{- $t := list }}
{{- if ne (toString $bn.enabled) "false" }}{{ $t = default (list) $bn.tolerations }}{{ end }}
{{- with concat $t (default (list) .Values.router.tolerations) }}{{ toYaml . }}{{ end }}
{{- end }}

{{- define "control-plane.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Selector labels of a role (.role: controller or web): never add anything
that can change between upgrades. The role keeps either Deployment and
Service from selecting the other's pods.
*/}}
{{- define "control-plane.selectorLabels" -}}
app.kubernetes.io/name: {{ include "control-plane.name" .root }}
app.kubernetes.io/instance: {{ .root.Release.Name | quote }}
app.kubernetes.io/component: {{ .role }}
{{- end }}

{{/*
ci-accelerator.io/component lets other charts' NetworkPolicies select the
role: the daemons admit the controller, never the web.
*/}}
{{- define "control-plane.componentLabels" -}}
app.kubernetes.io/part-of: ci-accelerator
ci-accelerator.io/component: {{ .role }}
{{- end }}

{{- define "control-plane.labels" -}}
{{- $own := merge
  (include "control-plane.selectorLabels" . | fromYaml)
  (include "control-plane.componentLabels" . | fromYaml)
  (dict
    "helm.sh/chart" (include "control-plane.chart" .root)
    "app.kubernetes.io/version" .root.Chart.AppVersion
    "app.kubernetes.io/managed-by" .root.Release.Service) }}
{{- toYaml (merge $own (default (dict) .root.Values.commonLabels)) }}
{{- end }}

{{/*
Pod labels: without the chart version, so a chart release that changes
nothing else does not restart the pods.
*/}}
{{- define "control-plane.podLabels" -}}
{{- $own := merge
  (include "control-plane.selectorLabels" . | fromYaml)
  (include "control-plane.componentLabels" . | fromYaml) }}
{{- $pod := dig .role "podLabels" (dict) .root.Values.AsMap }}
{{- toYaml (merge $own $pod (default (dict) .root.Values.commonLabels)) }}
{{- end }}

{{/*
The released ci-accelerator images (ghcr.io/hoobiboobi/ci-accelerator) by tag,
with their index digests from the release notes. A digest pinned for one
release would run that release under another tag (the runtime pulls by
digest), so a pin of a listed digest under a different tag is refused.
Every release adds its own line afterwards (test/released-images.sh keeps
the list the same in charts/buildkit and charts/control-plane and checks the
last published release is in it).
*/}}
{{- define "control-plane.releasedImages" -}}
v0.3.0: sha256:785091522e89806379d527acff74cbadf6dc6f8371bb31adc408cace2af842a2
v0.3.1: sha256:e7252ed4cd63943a63b7876410005080050d51fe52f4f3c6f843663a03fb18f0
v0.3.2: sha256:1494c38392a0dd8ee418e14de4b78851f739d4c21475a3b23fbbaf65aa1265f9
{{- end }}

{{/*
The image: repository:tag@digest when a digest is set. The tag follows the
chart (v<appVersion>) unless set; a digest pinned for an earlier release and
kept by helm upgrade --reset-then-reuse-values would go on running that
release under the new tag, so it is refused with the way forward.
*/}}
{{- define "control-plane.image" -}}
{{- $tag := default (printf "v%s" .Chart.AppVersion) .Values.image.tag }}
{{- $digest := toString (default "" .Values.image.digest) }}
{{- if and $digest (not .Values.image.tag) }}
{{- range $released, $d := include "control-plane.releasedImages" . | fromYaml }}
{{- if and (eq (toString $d) $digest) (ne $released $tag) }}
{{- fail (printf "image.digest (control-plane.image.digest in the box) is the digest of the %s image, but this chart runs %s: set the digest of %s from its release notes, or remove the digest" $released $tag $tag) }}
{{- end }}
{{- end }}
{{- end }}
{{- if $digest }}
{{- printf "%s:%s@%s" .Values.image.repository $tag $digest }}
{{- else }}
{{- printf "%s:%s" .Values.image.repository $tag }}
{{- end }}
{{- end }}

{{- define "control-plane.discover" -}}
{{- tpl .Values.daemons.discover . }}
{{- end }}

{{- define "control-plane.jobsNamespace" -}}
{{- tpl .Values.jobs.namespace . }}
{{- end }}

{{- define "control-plane.urlsConfigMap" -}}
{{- tpl .Values.gitlab.urlsFrom.configMap . }}
{{- end }}

{{/*
Secret with the controller's client certificate: the user's, or the one
cert-manager fills.
*/}}
{{- define "control-plane.clientSecretName" -}}
{{- default (printf "%s-client" (include "control-plane.controllerName" .)) .Values.tls.existingSecret }}
{{- end }}

{{/* Secret with the API's certificate (tls.crt, tls.key, ca.crt). */}}
{{- define "control-plane.apiSecretName" -}}
{{- default (printf "%s-tls" (include "control-plane.controllerName" .)) .Values.controller.api.tls.existingSecret }}
{{- end }}

{{/*
Issuer of the certificates: tls.certManager.issuerRef, else
global.tls.issuerRef. The name is rendered with tpl. As YAML.
*/}}
{{- define "control-plane.issuerRef" -}}
{{- $ref := .Values.tls.certManager.issuerRef }}
{{- if not $ref.name }}
{{- $ref = .Values.global.tls.issuerRef }}
{{- end }}
{{- toYaml (dict "name" (tpl (default "" $ref.name) .) "kind" (default "ClusterIssuer" $ref.kind) "group" (default "cert-manager.io" $ref.group)) }}
{{- end }}

{{- define "control-plane.pvcName" -}}
{{- default (printf "%s-data" (include "control-plane.controllerName" .)) .Values.controller.persistence.existingClaim }}
{{- end }}

{{- define "control-plane.signIn" -}}
{{- if .Values.signIn.enabled }}true{{ end }}
{{- end }}

{{/*
Where browsers reach the console: signIn.url, else global.console.url.
*/}}
{{- define "control-plane.signInURL" -}}
{{- $u := tpl (default "" .Values.signIn.url) . }}
{{- if not $u }}{{ $u = dig "console" "url" "" (default (dict) .Values.global) }}{{ end }}
{{- trimSuffix "/" $u }}
{{- end }}

{{/*
The controller's sign-in file: names of the mounted Secrets' files, no
secret.
*/}}
{{- define "control-plane.signInConfig" -}}
{{- $gl := list }}
{{- range $i, $g := .Values.signIn.gitlab }}
{{- $gl = append $gl (dict "url" (default "" $g.url) "clientIdFile" (printf "/sign-in/gitlab-%d/client-id" $i) "secretFile" (printf "/sign-in/gitlab-%d/client-secret" $i) "admins" (default (list) $g.admins)) }}
{{- end }}
{{- toJson (dict "url" (include "control-plane.signInURL" .) "sessionKeyFile" "/sign-in/key/key" "gitlab" $gl) }}
{{- end }}

{{/*
The cache folders listed in caches.stores, as the controller reads them.
*/}}
{{- define "control-plane.cachesJson" -}}
{{- $out := list }}
{{- range .Values.caches.stores }}
{{- $s := dict "server" .server "bucket" .bucket "region" (default "" .region) "path" (trimAll "/" (default "" .path)) "instance" (sha256sum (toString .gitlabUrl) | trunc 6) "secret" .existingSecret }}
{{- if .insecure }}{{ $_ := set $s "insecure" true }}{{ end }}
{{- if hasKey . "pathStyle" }}{{ $_ := set $s "pathStyle" .pathStyle }}{{ end }}
{{- $out = append $out $s }}
{{- end }}
{{- toJson $out }}
{{- end }}

{{- define "control-plane.cachesConfigMap" -}}
{{- if .Values.caches.stores }}{{ include "control-plane.controllerName" . }}-caches{{ else }}{{ tpl (default "" .Values.caches.fromConfigMap) . }}{{ end }}
{{- end }}

{{/*
Checks that fail the render with a message naming the value to set.
*/}}
{{- define "control-plane.validate" -}}
{{- include "control-plane.refuseOld" . }}
{{- if and (not .Values.gitlab.urls) (not (include "control-plane.urlsConfigMap" .)) }}
{{- fail "control-plane: set gitlab.urls (the runners' gitlabUrl, exactly) or gitlab.urlsFrom.configMap" }}
{{- end }}
{{- $d := include "control-plane.discover" . }}
{{- if not $d }}
{{- fail "control-plane: set daemons.discover to the BuildKit chart's headless Service, host:port (e.g. buildkit-headless.ci-accelerator.svc:1234)" }}
{{- end }}
{{- if not (regexMatch "^[a-z0-9.-]+:[0-9]+$" $d) }}
{{- fail (printf "control-plane: daemons.discover must be host:port, got %q" $d) }}
{{- end }}
{{- if and (not .Values.tls.existingSecret) (not (include "control-plane.issuerRef" . | fromYaml).name) }}
{{- fail "control-plane: the controller's client certificate needs tls.existingSecret, or an issuer in tls.certManager.issuerRef or global.tls.issuerRef (the daemons' CA)" }}
{{- end }}
{{- range $i, $c := .Values.caches.stores }}
{{- range $k := list "server" "bucket" "region" "gitlabUrl" "existingSecret" }}
{{- if not (get $c $k) }}
{{- fail (printf "control-plane: caches.stores[%d].%s is needed" $i $k) }}
{{- end }}
{{- end }}
{{- if regexMatch "^[a-z]+://|/" (toString $c.server) }}
{{- fail (printf "control-plane: caches.stores[%d].server is a host (and port), without https:// or a path" $i) }}
{{- end }}
{{- end }}
{{- if include "control-plane.signIn" . }}
{{- $u := include "control-plane.signInURL" . }}
{{- if not $u }}
{{- fail "control-plane: sign-in needs signIn.url (or global.console.url): where browsers reach the console" }}
{{- end }}
{{- if not (regexMatch "^(https://[^/?#\\s]+|http://(localhost|127\\.0\\.0\\.1|\\[::1\\])(:[0-9]+)?)(/[^?#\\s]*)?$" $u) }}
{{- fail (printf "control-plane: signIn.url must be https (plain http only on localhost, for a port-forward), got %q" $u) }}
{{- end }}
{{- if not .Values.signIn.sessionKey.existingSecret }}
{{- fail "control-plane: sign-in needs signIn.sessionKey.existingSecret: a Secret with 32 random bytes in base64 (openssl rand -base64 32)" }}
{{- end }}
{{- $gl := .Values.signIn.gitlab }}
{{- if not $gl }}
{{- fail "control-plane: sign-in needs signIn.gitlab: the OAuth application of each GitLab (url, existingSecret)" }}
{{- end }}
{{- $seen := dict }}
{{- range $gl }}
{{- if not .existingSecret }}
{{- fail (printf "control-plane: signIn.gitlab %q needs existingSecret: a Secret with the OAuth application's client-id and client-secret" (default "" .url)) }}
{{- end }}
{{- if and (not .url) (gt (len $gl) 1) }}
{{- fail "control-plane: with several signIn.gitlab entries each needs its url, exactly as the runners have it" }}
{{- end }}
{{- if hasKey $seen (default "" .url) }}
{{- fail (printf "control-plane: signIn.gitlab names %q twice" (default "" .url)) }}
{{- end }}
{{- $_ := set $seen (default "" .url) true }}
{{- if and .url $.Values.gitlab.urls (not (has .url $.Values.gitlab.urls)) }}
{{- fail (printf "control-plane: signIn.gitlab url %q is not one of gitlab.urls (they must match exactly)" .url) }}
{{- end }}
{{- end }}
{{- if .Values.gitlab.token.existingSecret }}
{{- fail "control-plane: gitlab.token and sign-in exclude each other: with sign-in GitLab is read with each person's own token" }}
{{- end }}
{{- end }}
{{- if and .Values.tls.existingSecret (not .Values.controller.api.tls.existingSecret) }}
{{- fail (printf "control-plane: with tls.existingSecret (no cert-manager) the controller's API needs controller.api.tls.existingSecret too: a Secret with tls.crt, tls.key and ca.crt for the name %s" (include "control-plane.controllerName" .)) }}
{{- end }}
{{- if include "control-plane.router" . }}
{{- if not (include "control-plane.jobsNamespace" .) }}
{{- fail "control-plane: the router knows each CI job by its pod: set jobs.namespace" }}
{{- end }}
{{- if eq 1234 (int .Values.router.healthPort) }}
{{- fail "control-plane: router.healthPort must not be 1234, the port the jobs connect to" }}
{{- end }}
{{- if gt (int .Values.router.warmDays) (int .Values.retention.days) }}
{{- fail (printf "control-plane: router.warmDays (%d) cannot exceed retention.days (%d): older builds are deleted" (int .Values.router.warmDays) (int .Values.retention.days)) }}
{{- end }}
{{- with dig "router" "service" "" (.Values.global | default dict) }}
{{- if ne (tpl . $) (include "control-plane.routerName" $) }}
{{- fail (printf "control-plane: global.router.service (%s) names another Service than the router's, %s: the daemons' certificate would not name the router" (tpl . $) (include "control-plane.routerName" $)) }}
{{- end }}
{{- end }}
{{- end }}
{{- if eq (int .Values.controller.api.port) (int .Values.web.port) }}
{{- fail "control-plane: controller.api.port and web.port must differ" }}
{{- end }}
{{- if or (lt (int .Values.retention.days) 1) (lt (int .Values.retention.outputDays) 1) (gt (int .Values.retention.outputDays) (int .Values.retention.days)) }}
{{- fail "control-plane: retention.days and retention.outputDays must be at least 1, and output kept no longer than builds" }}
{{- end }}
{{- end }}

{{/*
0.3.0 renamed every object of the console (0.2.x) and does not take over a
release of it: the old controller would keep running next to the new one,
on another volume. Refuse when this release still has the old console's
Deployment. lookup is empty under helm template and GitOps renders.
*/}}
{{- define "control-plane.refuseOld" -}}
{{- /* Its Deployment: <release>-console, or <release> when the release name
holds "console" (or fullnameOverride). Read by name: no list. */}}
{{- range (uniq (compact (list (printf "%s-console" .Release.Name | trunc 63 | trimSuffix "-") .Release.Name (default "" .Values.fullnameOverride)))) }}
{{- $d := lookup "apps/v1" "Deployment" $.Release.Namespace . }}
{{- $l := dig "metadata" "labels" (dict) $d }}
{{- if and (eq (get $l "app.kubernetes.io/instance") $.Release.Name) (eq (get $l "ci-accelerator.io/component") "console") }}
{{- fail (printf "release %s has the console of ci-accelerator 0.2.x (Deployment %s), which 0.3.0 cannot upgrade in place: its objects have new names. Remove the release (helm uninstall %s -n %s), wait until its pods are gone, and install again; the old console's history stays on the PersistentVolumeClaim %s-data until you delete it." $.Release.Name . $.Release.Name $.Release.Namespace .) }}
{{- end }}
{{- end }}
{{- end }}

{{/* Placement and pod settings of a role: .root, .role. As YAML. */}}
{{- define "control-plane.roleValue" -}}
{{- with (get (get .root.Values .role) .key) }}{{ toYaml . }}{{ end }}
{{- end }}

{{/* What the web reads from the controller at start, as a checksum. */}}
{{- define "control-plane.setupChecksum" -}}
{{- $signIn := include "control-plane.signIn" . }}
{{- $setup := dict "signIn" $signIn "token" (ne (toString .Values.gitlab.token.existingSecret) "") "urls" (default (list) .Values.gitlab.urls) "urlsFrom" (include "control-plane.urlsConfigMap" .) }}
{{- if $signIn }}{{ $_ := set $setup "signInConfig" (include "control-plane.signInConfig" .) }}{{ end }}
{{- toJson $setup | sha256sum | trunc 16 }}
{{- end }}

{{/* The log flags of a role: dict "root" . "role" <the role's values>. */}}
{{- define "control-plane.logArgs" -}}
- -log-level={{ .role.logLevel | default .root.Values.log.level }}
- -log-format={{ .root.Values.log.format }}
{{- end }}
