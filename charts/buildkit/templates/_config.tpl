{{/*
Shared GC caps as TOML lines; empty values are skipped.
*/}}
{{- define "buildkit.gcCaps" -}}
{{- with .reservedSpace }}
  reservedSpace = {{ . | quote }}
{{- end }}
{{- with .maxUsedSpace }}
  maxUsedSpace = {{ . | quote }}
{{- end }}
{{- with .minFreeSpace }}
  minFreeSpace = {{ . | quote }}
{{- end }}
{{- end }}

{{/*
One [[worker.oci.gcpolicy]] table from a policy dict.
*/}}
{{- define "buildkit.gcPolicyTable" -}}
[[worker.oci.gcpolicy]]
{{- if .all }}
  all = true
{{- end }}
{{- with .filters }}
  filters = {{ toJson . }}
{{- end }}
{{- with .keepDuration }}
  keepDuration = {{ . | quote }}
{{- end }}
{{- include "buildkit.gcCaps" . }}
{{- end }}

{{/*
The GC policy list. buildkit applies worker-level caps only when no explicit
policy exists, so the chart renders upstream's default policy shape itself,
with cache mounts split out and the shared caps injected where upstream puts
them. config.gcPolicy replaces the whole set.
*/}}
{{- define "buildkit.gcPolicyList" -}}
{{- $gc := .Values.config.gc }}
{{- $caps := dict "reservedSpace" $gc.reservedSpace "maxUsedSpace" $gc.maxUsedSpace "minFreeSpace" $gc.minFreeSpace }}
{{- $policies := .Values.config.gcPolicy }}
{{- if not $policies }}
{{- $policies = list
  (dict "filters" (list "type==source.local" "type==source.git.checkout") "keepDuration" "48h" "maxUsedSpace" "512MB")
  (dict "filters" (list "type==exec.cachemount") "keepDuration" $gc.cacheMounts.keepDuration "maxUsedSpace" $gc.cacheMounts.maxUsedSpace)
  (merge (dict "keepDuration" "1440h") $caps)
  $caps
  (merge (dict "all" true) $caps)
}}
{{- end }}
{{- toJson $policies }}
{{- end }}

{{- define "buildkit.gcPolicies" -}}
{{- range (include "buildkit.gcPolicyList" . | fromJsonArray) }}

{{ include "buildkit.gcPolicyTable" . }}
{{- end }}
{{- end }}

{{/*
A TOML value for config.workerOCI: integers without a float exponent,
everything else as JSON (valid TOML for strings, booleans and arrays).
*/}}
{{- define "buildkit.tomlValue" -}}
{{- if kindIs "float64" . }}
{{- if eq (float64 (int64 .)) . }}{{ int64 . }}{{ else }}{{ . }}{{ end }}
{{- else }}
{{- toJson . }}
{{- end }}
{{- end }}

{{/*
Full buildkitd.toml.
*/}}
{{- define "buildkit.config" -}}
# Rendered by the ci-accelerator buildkit chart. Do not edit in the cluster.
root = "/var/lib/buildkit"
{{- with .Values.config.extraConfig }}

# --- config.extraConfig ---
{{ trim . }}
# --- end of config.extraConfig ---
{{- end }}

{{- $log := dig "log" dict .Values.config }}
{{- if or $log.level $log.format }}

[log]
{{- with $log.level }}
  level = {{ . | quote }}
{{- end }}
{{- with $log.format }}
  format = {{ . | quote }}
{{- end }}
{{- end }}

[grpc]
  # unix socket: used by the probes, never TLS. tcp: used by CI jobs, mutual
  # TLS: with a CA set, buildkitd requires a client certificate signed by it.
  address = ["unix:///run/buildkit/buildkitd.sock", "tcp://0.0.0.0:1234"]

[grpc.tls]
  cert = "{{ include "buildkit.tlsDir" . }}/tls.crt"
  key = "{{ include "buildkit.tlsDir" . }}/tls.key"
  ca = "{{ include "buildkit.tlsDir" . }}/ca.crt"
{{- with .Values.config.history }}
{{- if or .maxAge (hasKey . "maxEntries") }}

[history]
{{- with .maxAge }}
  maxAge = {{ . | quote }}
{{- end }}
{{- if hasKey . "maxEntries" }}
  maxEntries = {{ int64 .maxEntries }}
{{- end }}
{{- end }}
{{- end }}
{{- if not .Values.config.cdi.enabled }}

[cdi]
  disabled = true
{{- end }}
{{- with .Values.config.maxRegistryConcurrency }}

[system]
  maxRegistryConcurrency = {{ int64 . }}
{{- end }}

[worker.oci]
  enabled = true
  gc = true
{{- with .Values.config.maxParallelism }}
  max-parallelism = {{ int64 . }}
{{- end }}
{{- range $k, $v := .Values.config.workerOCI }}
  {{ $k }} = {{ include "buildkit.tomlValue" $v }}
{{- end }}
{{- include "buildkit.gcPolicies" . }}

[worker.containerd]
  enabled = false
{{- end }}
