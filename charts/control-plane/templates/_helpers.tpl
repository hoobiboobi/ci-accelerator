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
nothing else does not restart the pods. .from: the values block whose
podLabels apply, when it is not the role's own (a hub: controller).
*/}}
{{- define "control-plane.podLabels" -}}
{{- $own := merge
  (include "control-plane.selectorLabels" . | fromYaml)
  (include "control-plane.componentLabels" . | fromYaml) }}
{{- $pod := dig (default .role .from) "podLabels" (dict) .root.Values.AsMap }}
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
v0.3.3: sha256:8103f17d6512ed305f445fc436dfe19cf9669d57745ae95f2cc98756f39b1204
v0.3.4: sha256:3518d5093f0da345fc783c0d83536e4d5aed56dd576c19e4f1f5aae48bf34aa8
v0.3.5: sha256:740794c205bd081c2e858b1024d3f1765079b7272677f8fc04ec0d9a9fa8310e
v0.3.6: sha256:23d03de7f1a7b75de9453de44ce2c5d6281cf4e4dc06d16e037a4a703ecbe30f
v0.3.7: sha256:0844cb04ea0f90f4f1ede299c5afc72942d37db2ed7da5ee211ab1601fd0f7e8
v0.3.8: sha256:d71c6bda6c9c0d52e2a67d0f85b5827abfa36af45c4e4c0a8d698e2686a1f927
v0.3.9: sha256:32fe2e553556eabfbf877d5e7e78d2fb887a0adb6f3b70475cda16d9766f7f75
v0.3.10: sha256:0bf21ef3f30bf49a3f9220091aea4b2e037ec486a0a236a78ae77d4538e48711
v0.3.11: sha256:1abe877553716bbec50cb0a261603a4ef05ab1d7158721348760ceb810f7dfe7
v0.4.0: sha256:5641f71e255c36f46db112df9b29cf9e470ead0d10f46676c3a4ad8947c97333
v0.4.1: sha256:08e0b22c5eba8544d6a104d42c9ce7e5f57d8901d21dffa7f289842f570ae256
v0.5.0: sha256:a3041d81a259b8b598e953ef05c103c9df7844fb7ef8f0ddfb82afaf87bbe6e4
v0.5.1: sha256:b5637db74780e029519403f6a4cff1ff0332f54a221dd9266ee6a9d79aef8516
v0.5.2: sha256:5071488b282537162b5e5025e8b6e4748e8273e91188afd88e0a67bf8a18239b
v0.6.0: sha256:59f96add6a35ad0583a4e2f72c39e93efcba9f67d12aaa25be264a1f5c2d3cdd
v1.0.0-alpha.1: sha256:ddd38f801817d5b6498a936d41dc03142ecc23f162522ce216ce5a3ce91657c2
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

{{/*
host:port of the Service of every daemon pod, ready or not (the BuildKit
chart's <fullname>-all): daemons.discoverAll, else derived from discover
when its host names a -headless Service, else empty.
*/}}
{{- define "control-plane.discoverAll" -}}
{{- with dig "discoverAll" "" .Values.daemons }}
{{- tpl . $ }}
{{- else }}
{{- $d := include "control-plane.discover" . }}
{{- $parts := splitList ":" $d }}
{{- if eq (len $parts) 2 }}
{{- $labels := splitList "." (first $parts) }}
{{- $name := first $labels }}
{{- if and (hasSuffix "-headless" $name) (gt (len $name) 9) }}
{{- $labels = prepend (rest $labels) (printf "%s-all" (trimSuffix "-headless" $name)) }}
{{- printf "%s:%s" (join "." $labels) (last $parts) }}
{{- end }}
{{- end }}
{{- end }}
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
global.tls.issuerRef. The name is rendered with tpl; a name that is the
global issuer's (as the box passes it) takes the global kind and group
too. As YAML.
*/}}
{{- define "control-plane.issuerRef" -}}
{{- $ref := .Values.tls.certManager.issuerRef }}
{{- if not $ref.name }}
{{- $ref = .Values.global.tls.issuerRef }}
{{- end }}
{{- $name := tpl (default "" $ref.name) . }}
{{- $global := dig "tls" "issuerRef" (dict) (.Values.global | default dict) }}
{{- if and $global.name (eq $name (toString $global.name)) }}
{{- $ref = $global }}
{{- end }}
{{- toYaml (dict "name" $name "kind" (default "ClusterIssuer" $ref.kind) "group" (default "cert-manager.io" $ref.group)) }}
{{- end }}

{{- define "control-plane.pvcName" -}}
{{- default (printf "%s-data" (include "control-plane.controllerName" .)) .Values.controller.persistence.existingClaim }}
{{- end }}

{{/*
Sign-in to this release's pages. A fleet member has no pages of its own
(people sign in at the hub), so its signIn values are ignored: values
of the box it was before may stay in place.
*/}}
{{- define "control-plane.signIn" -}}
{{- if and .Values.signIn.enabled (ne (include "control-plane.fleetRole" .) "member") }}true{{ end }}
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
{{- $admins := default (list) $g.admins }}
{{- if eq (include "control-plane.fleetRole" $) "hub" }}{{ $admins = default (list) $g.owners }}{{ end }}
{{- $gl = append $gl (dict "url" (default "" $g.url) "clientIdFile" (printf "/sign-in/gitlab-%d/client-id" $i) "secretFile" (printf "/sign-in/gitlab-%d/client-secret" $i) "admins" $admins) }}
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
{{- include "control-plane.validateFleet" . }}
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
{{- /* The runners keep archives under project/<id>/: one folder of two
GitLabs would mix their projects of the same id (the controller refuses it). */}}
{{- range $j, $e := $.Values.caches.stores }}
{{- if and (lt $j $i) (eq (toString $e.server) (toString $c.server)) (eq (toString $e.bucket) (toString $c.bucket)) (eq (trimAll "/" (default "" $e.path)) (trimAll "/" (default "" $c.path))) (ne (toString $e.gitlabUrl) (toString $c.gitlabUrl)) }}
{{- fail (printf "control-plane: caches.stores[%d] and caches.stores[%d] list the same folder (server/bucket/path) for two GitLabs: their projects' archives would mix; give each GitLab its own path" $j $i) }}
{{- end }}
{{- end }}
{{- end }}
{{- include "control-plane.validateSignIn" . }}
{{- if and .Values.gitlab.token.existingSecret (gt (len (default (list) .Values.gitlab.urls)) 1) }}
{{- fail "control-plane: gitlab.token is a token of one GitLab, but gitlab.urls names several: the console would send it to each of them. Use sign-in (each person's own token), or one URL" }}
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

{{/*
The box's part in a fleet: "" (a box on its own), "hub" or "member".
Read with dig: values of releases before the fleet have no fleet key.
*/}}
{{- define "control-plane.fleetRole" -}}
{{- dig "fleet" "role" "" .Values.AsMap }}
{{- end }}

{{- define "control-plane.hubName" -}}
{{- include "control-plane.webName" . }}
{{- end }}

{{/*
The Secret a fleet member's key is in: the user's, or the one the
controller makes when it joins with a join token.
*/}}
{{- define "control-plane.fleetKeySecret" -}}
{{- $f := dig "fleet" (dict) .Values.AsMap }}
{{- default (printf "%s-fleet-key" (include "control-plane.controllerName" .)) (dig "key" "existingSecret" "" $f) }}
{{- end }}

{{/*
The Secrets a fleet member's controller writes, as a YAML list: those its
hub delivers (the names the hub's generated values use) and, joining with
a token, the one it keeps its key in.
*/}}
{{- define "control-plane.fleetSecretNames" -}}
{{- $names := list "fleet-runner-token" "fleet-gitlab-token" "fleet-s3-cache" }}
{{- if dig "fleet" "join" "existingSecret" "" .Values.AsMap }}
{{- $names = append $names (include "control-plane.fleetKeySecret" .) }}
{{- end }}
{{- toYaml $names }}
{{- end }}

{{/*
Checks of a fleet hub or member: fail the render with the value to set.
Also refuses turning one release from a box into a hub or back: the hub's
Deployment has the web's name with another (immutable) selector, and it
would open the box's store. lookup is empty under helm template.
*/}}
{{- define "control-plane.validateFleet" -}}
{{- $role := include "control-plane.fleetRole" . }}
{{- $f := dig "fleet" (dict) .Values.AsMap }}
{{- if not (has $role (list "" "hub" "member")) }}
{{- fail (printf "control-plane: fleet.role is \"\" (a box), \"hub\" or \"member\", got %q" $role) }}
{{- end }}
{{- $web := lookup "apps/v1" "Deployment" .Release.Namespace (include "control-plane.webName" .) }}
{{- $webLabels := dig "metadata" "labels" (dict) $web }}
{{- $webOurs := eq (get $webLabels "app.kubernetes.io/instance") .Release.Name }}
{{- if eq $role "hub" }}
{{- $ctrl := lookup "apps/v1" "Deployment" .Release.Namespace (include "control-plane.controllerName" .) }}
{{- if or (and $webOurs (eq (get $webLabels "app.kubernetes.io/component") "web")) (eq (dig "metadata" "labels" "app.kubernetes.io/instance" "" $ctrl) .Release.Name) }}
{{- fail (printf "control-plane: release %s is a box (it runs a controller or a web); a fleet hub is a release of its own, in its own namespace: install it with another release name, and keep this one as it is" .Release.Name) }}
{{- end }}
{{- else if and $webOurs (eq (get $webLabels "app.kubernetes.io/component") "hub") }}
{{- fail (printf "control-plane: release %s is a fleet hub; it cannot become a box: keep fleet.role=hub, and install a box as a release of its own" .Release.Name) }}
{{- end }}
{{- if eq $role "hub" }}
{{- if not (dig "key" "existingSecret" "" $f) }}
{{- fail "control-plane: a fleet hub needs fleet.key.existingSecret: a Secret with its private key (PEM) under fleet.key.key; ci-accelerator fleet-keygen or openssl genpkey -algorithm ed25519 make one" }}
{{- end }}
{{- if or (dig "join" "existingSecret" "" $f) (dig "name" "" $f) (dig "hub" "url" "" $f) (dig "hub" "publicKeys" "" $f) }}
{{- fail "control-plane: fleet.join, fleet.name, fleet.hub.url and fleet.hub.publicKeys are a member's values; a fleet hub takes fleet.key, fleet.apiToken, fleet.secretsKey, fleet.hub.caCert and fleet.keepRemovedDays" }}
{{- end }}
{{- $u := include "control-plane.signInURL" . }}
{{- if not (regexMatch "^https?://[^/?#@\\s]+$" $u) }}
{{- fail (printf "control-plane: a fleet hub needs its console address, at the root of its host: global.console.url (or signIn.url), e.g. https://ci.example.com; the hub writes it into the values of its clusters, got %q" $u) }}
{{- end }}
{{- if include "control-plane.signIn" . }}
{{- range .Values.signIn.gitlab }}
{{- if and (not .url) (not $.Values.gitlab.urls) }}
{{- fail "control-plane: a fleet hub with sign-in needs the GitLab people sign in with: signIn.gitlab[].url (or gitlab.urls); a hub has no runners to take it from" }}
{{- end }}
{{- end }}
{{- else if not (dig "apiToken" "existingSecret" "" $f) }}
{{- fail "control-plane: a fleet hub needs a way in for its owners: sign-in (signIn.enabled, with signIn.gitlab[].owners) or a bootstrap token for its API (fleet.apiToken.existingSecret)" }}
{{- end }}
{{- range .Values.signIn.gitlab }}
{{- if .admins }}
{{- fail "control-plane: on a fleet hub signIn.gitlab[].admins is replaced by owners: the people who see and manage the whole fleet" }}
{{- end }}
{{- end }}
{{- end }}
{{- if eq $role "member" }}
{{- if and (not (dig "key" "existingSecret" "" $f)) (not (dig "join" "existingSecret" "" $f)) }}
{{- fail "control-plane: a fleet member needs fleet.join.existingSecret (a Secret with a join token issued by the hub) or fleet.key.existingSecret (a Secret with its private key, PEM, registered at the hub)" }}
{{- end }}
{{- if and (dig "key" "existingSecret" "" $f) (dig "join" "existingSecret" "" $f) }}
{{- fail "control-plane: a fleet member takes fleet.key.existingSecret (a key registered at the hub) or fleet.join.existingSecret (a join token), not both" }}
{{- end }}
{{- if or (dig "apiToken" "existingSecret" "" $f) (dig "secretsKey" "existingSecret" "" $f) }}
{{- fail "control-plane: fleet.apiToken and fleet.secretsKey are a hub's values; a fleet member does not use them" }}
{{- end }}
{{- $u := dig "hub" "url" "" $f }}
{{- if not (regexMatch "^https?://[^/?#@\\s]+/?$" $u) }}
{{- fail (printf "control-plane: a fleet member needs fleet.hub.url, the hub's console address (https://...), at the root of its host and without user or password, got %q" $u) }}
{{- end }}
{{- /* The binary's rule (hubAddress): http only to localhost, a loopback IP or a Service (*.svc, *.svc.*). */ -}}
{{- if and (hasPrefix "http://" (lower $u)) (dig "join" "existingSecret" "" $f) (not (regexMatch "^(?i)http://(localhost|127\\.[0-9]+\\.[0-9]+\\.[0-9]+|\\[::1\\]|[^/:?#@\\s]+\\.svc(\\.[^/:?#@\\s]+)*)(:[0-9]+)?/?$" $u)) }}
{{- fail (printf "control-plane: a fleet member with a join token needs an https fleet.hub.url (http only to localhost or a Service of the cluster), got %q" $u) }}
{{- end }}
{{- if not (contains "BEGIN PUBLIC KEY" (dig "hub" "publicKeys" "" $f)) }}
{{- fail "control-plane: a fleet member needs fleet.hub.publicKeys: the hub's public key(s), PEM (-----BEGIN PUBLIC KEY-----)" }}
{{- end }}
{{- if not (regexMatch "^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$" (dig "name" "" $f)) }}
{{- fail (printf "control-plane: a fleet member needs fleet.name: lower-case letters, digits and dashes, at most 40, got %q" (dig "name" "" $f)) }}
{{- end }}
{{- end }}
{{- /* A member's or a hub's values without the role would install a plain
box that never dials a hub, without a word. */}}
{{- if eq $role "" }}
{{- $set := list }}
{{- if dig "name" "" $f }}{{ $set = append $set "name" }}{{ end }}
{{- range $k := list "hub.url" "hub.publicKeys" "join.existingSecret" "key.existingSecret" "apiToken.existingSecret" "secretsKey.existingSecret" }}
{{- $parts := splitList "." $k }}
{{- if dig (first $parts) (last $parts) "" $f }}{{ $set = append $set $k }}{{ end }}
{{- end }}
{{- with $set }}
{{- fail (printf "control-plane: fleet.%s is set but fleet.role is empty: set fleet.role to member (a box that joins a hub) or hub, or remove the fleet values" (first .)) }}
{{- end }}
{{- end }}
{{- if and (eq $role "") .Values.signIn.enabled }}
{{- range .Values.signIn.gitlab }}
{{- if .owners }}
{{- fail "control-plane: signIn.gitlab[].owners is for a fleet hub (fleet.role hub); a box names its admins in signIn.gitlab[].admins" }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{- define "control-plane.validateSignIn" -}}
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
{{- /* The controller takes a url-less entry only for a single GitLab. */}}
{{- if and (not .url) (gt (len (default (list) $.Values.gitlab.urls)) 1) }}
{{- fail "control-plane: gitlab.urls names several GitLabs: each signIn.gitlab entry needs its url, exactly as in gitlab.urls" }}
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
{{- end }}
