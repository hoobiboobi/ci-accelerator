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
{{- $digest := toString .Values.image.digest }}
{{- /* helm upgrade --reuse-values keeps the computed values of the release
before, the earlier chart's default digest among them: under this chart's
tag it would run the earlier BuildKit under the new name (the runtime pulls
by digest). An earlier default counts as this chart's default; pinning an
earlier BuildKit takes its tag too. */}}
{{- if and (eq $tag .Chart.AppVersion) (has $digest (list "sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea")) }}
{{- $digest = "sha256:99368f3e46177c6405b9740e4461d91cee004128daeecca78d4ceac50ec1da5c" }}
{{- end }}
{{- if $digest }}
{{- printf "%s:%s@%s" .Values.image.repository $tag $digest }}
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
OpenTelemetry environment of the daemon. buildkitd reads its exporters from
the environment only. Resource attributes name the daemon on every metric
and span; the probes start buildctl with an empty environment, so none of
this reaches them. extraEnv can replace any of it (buildkit.env).
*/}}
{{- define "buildkit.otelEnv" -}}
{{- $o := .Values.otel }}
{{- $attrs := list (printf "k8s.namespace.name=%s" .Release.Namespace) "k8s.pod.name=$(CI_ACCELERATOR_POD_NAME)" "k8s.node.name=$(CI_ACCELERATOR_NODE_NAME)" }}
{{- range $k, $v := $o.resourceAttributes }}
{{- $attrs = append $attrs (printf "%s=%s" $k $v) }}
{{- end }}
- name: CI_ACCELERATOR_POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: CI_ACCELERATOR_NODE_NAME
  valueFrom:
    fieldRef:
      fieldPath: spec.nodeName
- name: OTEL_RESOURCE_ATTRIBUTES
  value: {{ join "," $attrs | quote }}
{{- with (tpl $o.metricsEndpoint $) }}
- name: OTEL_EXPORTER_OTLP_METRICS_ENDPOINT
  value: {{ . | quote }}
- name: OTEL_EXPORTER_OTLP_METRICS_PROTOCOL
  value: {{ $o.protocol | quote }}
- name: OTEL_METRIC_EXPORT_INTERVAL
  value: {{ mul (int64 $o.metricsIntervalSeconds) 1000 | quote }}
{{- end }}
{{- with (tpl $o.tracesEndpoint $) }}
- name: OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
  value: {{ . | quote }}
- name: OTEL_EXPORTER_OTLP_TRACES_PROTOCOL
  value: {{ $o.protocol | quote }}
{{- end }}
{{- end }}

{{/*
The daemon's environment: the chart's variables, then extraEnv. An extraEnv
entry named like one of the chart's takes its place (and its position, for
the $(VAR) references after it): the API keys env by name, and server-side
apply refuses a name given twice.
*/}}
{{- define "buildkit.env" -}}
{{- $user := dict }}
{{- range .Values.extraEnv }}{{ $_ := set $user (toString .name) . }}{{ end }}
{{- $env := list }}
{{- $replaced := dict }}
{{- range (include "buildkit.otelEnv" . | fromYamlArray) }}
{{- if hasKey $user .name }}
{{- $env = append $env (get $user .name) }}
{{- $_ := set $replaced .name true }}
{{- else }}
{{- $env = append $env . }}
{{- end }}
{{- end }}
{{- range .Values.extraEnv }}
{{- if not (hasKey $replaced (toString .name)) }}{{ $env = append $env . }}{{ end }}
{{- end }}
{{- toYaml $env }}
{{- end }}

{{/*
Namespaces whose pods may reach the daemons: the client certificate
namespaces plus networkPolicy.ingress.namespaces. As a JSON list.
*/}}
{{- define "buildkit.clientNamespaces" -}}
{{- $tls := list }}
{{- if not (include "buildkit.routerService" .) }}
{{- $tls = include "buildkit.tlsClientNamespaces" . | fromJsonArray }}
{{- end }}
{{- toJson (uniq (concat $tls .Values.networkPolicy.ingress.namespaces)) }}
{{- end }}

{{/*
tls.client.namespaces with each entry rendered through tpl, so a parent
chart can pass a name from its own values (e.g. "{{ .Values.global.x }}").
As a JSON list.
*/}}
{{- define "buildkit.tlsClientNamespaces" -}}
{{- $out := list }}
{{- range (default (list) .Values.tls.client.namespaces) }}
{{- $out = append $out (tpl . $) }}
{{- end }}
{{- toJson $out }}
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

{{/*
Secret with the daemon's certificate and the client CA (tls.crt, tls.key,
ca.crt): the user's with tls.source=existingSecret, else the one
cert-manager fills.
*/}}
{{- define "buildkit.tlsSecretName" -}}
{{- if eq .Values.tls.source "existingSecret" }}
{{- .Values.tls.existingSecret }}
{{- else }}
{{- printf "%s-tls" (include "buildkit.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Name of the CA the chart creates with cert-manager: its ClusterIssuer is
cluster-scoped and its objects live in cert-manager's namespace, so the name
carries the release namespace to stay unique across installs.
*/}}
{{- define "buildkit.caName" -}}
{{- printf "%s-%s-ca" .Release.Namespace (include "buildkit.fullname" .) | trunc 240 | trimSuffix "-" }}
{{- end }}

{{/*
Issuer for the daemon and client certificates: global.tls.issuerRef, or the
chart's own CA when its name is empty. As YAML.
*/}}
{{- define "buildkit.issuerRef" -}}
{{- $ref := .Values.global.tls.issuerRef }}
{{- if $ref.name }}
{{- toYaml (dict "name" $ref.name "kind" $ref.kind "group" $ref.group) }}
{{- else }}
{{- toYaml (dict "name" (include "buildkit.caName" .) "kind" "ClusterIssuer" "group" "cert-manager.io") }}
{{- end }}
{{- end }}

{{/*
DNS names clients use for the daemons: both Services, short to fully
qualified. As a JSON list.
*/}}
{{- define "buildkit.serverDnsNames" -}}
{{- $ns := .Release.Namespace }}
{{- $names := list }}
{{- range list (include "buildkit.fullname" .) (include "buildkit.headlessName" .) }}
{{- $names = concat $names (list . (printf "%s.%s" . $ns) (printf "%s.%s.svc" . $ns) (printf "%s.%s.svc.%s" . $ns $.Values.tls.certManager.clusterDomain)) }}
{{- end }}
{{- with include "buildkit.routerService" . }}
{{- $names = concat $names (list . (printf "%s.%s" . $ns) (printf "%s.%s.svc" . $ns) (printf "%s.%s.svc.%s" . $ns $.Values.tls.certManager.clusterDomain)) }}
{{- end }}
{{- range .Values.tls.certManager.extraDnsNames }}
{{- with tpl . $ }}{{ $names = append $names . }}{{ end }}
{{- end }}
{{- toJson $names }}
{{- end }}

{{/*
The ci-accelerator build router in front of the daemons
(global.router.service, rendered with tpl, in the release's namespace),
when global.router.enabled: CI jobs reach the daemons through it, their
TLS passing through, so the daemons' certificate names it and the
NetworkPolicy admits its pods instead of the clients' namespaces. Empty
without it.
*/}}
{{- define "buildkit.routerService" -}}
{{- $r := dig "router" (dict) (.Values.global | default dict) }}
{{- if and (eq (toString (dig "enabled" false $r)) "true") (dig "service" "" $r) }}
{{- tpl $r.service . }}
{{- end }}
{{- end }}

{{/*
The daemons' PriorityClass: the one named in priorityClassName, else the
one this chart creates (cluster-scoped, so the namespace is in its name),
else none. Values added after 0.2 are read with dig: an upgrade with
--reuse-values keeps the old tree.
*/}}
{{- define "buildkit.priorityClassName" -}}
{{- if .Values.priorityClassName }}
{{- .Values.priorityClassName }}
{{- else if dig "create" true (.Values.priorityClass | default dict) }}
{{- printf "%s-%s" .Release.Namespace (include "buildkit.fullname" .) | trunc 253 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
The disk probe's image: ci-accelerator's (the control plane's), pinned to
the release that last changed the probe program or its image unless set.
Pinned on purpose: a default of v<chart version> made every chart release
restart all daemons (the pod template changed), for a probe whose program
had not.
*/}}
{{/*
The released ci-accelerator images (ghcr.io/hoobiboobi/ci-accelerator) by tag,
with their index digests from the release notes. A digest pinned for one
release would run that release under another tag (the runtime pulls by
digest), so a pin of a listed digest under a different tag is refused.
Every release adds its own line afterwards (test/released-images.sh keeps
the list the same in charts/buildkit and charts/control-plane and checks the
last published release is in it).
*/}}
{{- define "buildkit.releasedImages" -}}
v0.3.0: sha256:785091522e89806379d527acff74cbadf6dc6f8371bb31adc408cace2af842a2
v0.3.1: sha256:e7252ed4cd63943a63b7876410005080050d51fe52f4f3c6f843663a03fb18f0
v0.3.2: sha256:1494c38392a0dd8ee418e14de4b78851f739d4c21475a3b23fbbaf65aa1265f9
v0.3.3: sha256:8103f17d6512ed305f445fc436dfe19cf9669d57745ae95f2cc98756f39b1204
v0.3.4: sha256:3518d5093f0da345fc783c0d83536e4d5aed56dd576c19e4f1f5aae48bf34aa8
v0.3.5: sha256:740794c205bd081c2e858b1024d3f1765079b7272677f8fc04ec0d9a9fa8310e
{{- end }}

{{- define "buildkit.diskProbeImage" -}}
{{- $img := dig "image" dict (.Values.diskProbe | default dict) }}
{{- $repo := default "ghcr.io/hoobiboobi/ci-accelerator" $img.repository }}
{{- $tag := default "v0.3.3" $img.tag }}
{{- $digest := toString (default "" $img.digest) }}
{{- /* A digest pinned for another release under the default tag would run
that release as the probe's (the default tag moves when the probe changes). */}}
{{- if and $digest (not $img.tag) }}
{{- $released := include "buildkit.releasedImages" . | fromYaml }}
{{- range $v, $d := $released }}
{{- if and (eq (toString $d) $digest) (ne $v $tag) }}
{{- fail (printf "diskProbe.image.digest (buildkit.diskProbe.image.digest in the box) is the digest of the %s image, but the disk probe runs %s: its digest is %s" $v $tag (default "in its release notes" (get $released $tag))) }}
{{- end }}
{{- end }}
{{- end }}
{{- if $digest }}
{{- printf "%s:%s@%s" $repo $tag $digest }}
{{- else }}
{{- printf "%s:%s" $repo $tag }}
{{- end }}
{{- end }}
