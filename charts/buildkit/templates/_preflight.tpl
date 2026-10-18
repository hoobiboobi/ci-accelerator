{{/*
Install-time checks against the live cluster. Without them `helm install`
reports "deployed" (even with --wait) while the DaemonSet creates no pods.
lookup returns nothing under `helm template` and in GitOps tools that render
with it, so the checks are skipped there. lookup fails the install when the
installer may not read the objects (Forbidden): switch the check off then.
*/}}
{{- define "buildkit.preflight" -}}

{{- /* Pod Security: the namespace must not enforce a level that rejects the
       privileged daemon. A missing label means the cluster default applies,
       which a chart cannot read (NOTES warns about it). */}}
{{- if .Values.preflight.podSecurity }}
{{- $ns := lookup "v1" "Namespace" "" .Release.Namespace }}
{{- $level := dig "metadata" "labels" "pod-security.kubernetes.io/enforce" "" $ns }}
{{- if and $level (ne $level "privileged") }}
{{- fail (printf "namespace %q enforces Pod Security level %q, but buildkitd runs as a privileged pod and would never start. Label the namespace: kubectl label namespace %s pod-security.kubernetes.io/enforce=privileged --overwrite (or set preflight.podSecurity=false to skip this check)" .Release.Namespace $level .Release.Namespace) }}
{{- end }}
{{- end }}

{{- /* Build nodes: at least one node must carry every label of the rendered
       node selector, or the DaemonSet wants 0 pods. */}}
{{- if and .Values.preflight.buildNodes (ne (toString .Values.global.buildNodes.enabled) "false") }}
{{- $nodes := lookup "v1" "Node" "" "" }}
{{- if $nodes }}
{{- $selector := include "buildkit.nodeSelector" . | fromYaml }}
{{- $matching := 0 }}
{{- range $nodes.items }}
{{- $labels := default (dict) .metadata.labels }}
{{- $ok := true }}
{{- range $k, $want := $selector }}
{{- if ne (toString (get $labels $k)) (toString $want) }}{{ $ok = false }}{{ end }}
{{- end }}
{{- if $ok }}{{ $matching = add1 $matching }}{{ end }}
{{- end }}
{{- if eq $matching 0 }}
{{- fail (printf "no node matches the build-node selector %s, so no daemon would run. Label and taint the build nodes (kubectl label node <node> ci-accelerator.io/node-role=build; kubectl taint node <node> ci-accelerator.io/node-role=build:NoSchedule), or set global.buildNodes.enabled=false to run on any node, or preflight.buildNodes=false to skip this check." (toJson $selector)) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
