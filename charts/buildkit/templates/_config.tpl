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
GC policies. buildkit applies worker-level caps only when no explicit policy
exists (D18), so the chart renders upstream's default policy shape itself,
with cache mounts split out and the shared caps injected where upstream puts
them. config.gcPolicy replaces the whole set.
*/}}
{{- define "buildkit.gcPolicies" -}}
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
{{- range $policies }}

{{ include "buildkit.gcPolicyTable" . }}
{{- end }}
{{- end }}

{{/*
Full buildkitd.toml.
*/}}
{{- define "buildkit.config" -}}
# Rendered by the ci-accelerator buildkit chart. Do not edit in the cluster.
root = "/var/lib/buildkit"

[grpc]
  # unix socket: used by the probes (no TLS needed); tcp: used by CI jobs.
  address = ["unix:///run/buildkit/buildkitd.sock", "tcp://0.0.0.0:1234"]
{{- with .Values.config.history }}

[history]
{{- with .maxAge }}
  maxAge = {{ . | quote }}
{{- end }}
{{- if hasKey . "maxEntries" }}
  maxEntries = {{ .maxEntries }}
{{- end }}
{{- end }}
{{- with .Values.config.maxRegistryConcurrency }}

[system]
  maxRegistryConcurrency = {{ . }}
{{- end }}

[worker.oci]
  enabled = true
  gc = true
{{- with .Values.config.maxParallelism }}
  max-parallelism = {{ . }}
{{- end }}
{{- include "buildkit.gcPolicies" . }}

[worker.containerd]
  enabled = false
{{- with .Values.config.extraConfig }}

# --- extraConfig ---
{{ . }}
{{- end }}
{{- end }}
