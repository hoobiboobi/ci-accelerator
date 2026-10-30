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

{{- /* TLS Secret: when it exists it must hold all three keys, or the pods
       hang in ContainerCreating. `kubectl create secret tls` writes no
       ca.crt. A missing Secret is fine: it may be created after the chart. */}}
{{- if and .Values.preflight.tlsSecret (eq .Values.tls.source "existingSecret") }}
{{- $secret := lookup "v1" "Secret" .Release.Namespace .Values.tls.existingSecret }}
{{- if $secret }}
{{- $missing := list }}
{{- range list "tls.crt" "tls.key" "ca.crt" }}
{{- if not (index (default (dict) $secret.data) .) }}{{ $missing = append $missing . }}{{ end }}
{{- end }}
{{- with $missing }}
{{- fail (printf "Secret %s/%s (tls.existingSecret) has no %s. It needs tls.crt and tls.key (the daemon's certificate) and ca.crt (the CA that signs client certificates); `kubectl create secret tls` writes no ca.crt, use: kubectl create secret generic %s -n %s --from-file=tls.crt --from-file=tls.key --from-file=ca.crt (or set preflight.tlsSecret=false to skip this check)" $.Release.Namespace $.Values.tls.existingSecret (join ", " .) $.Values.tls.existingSecret $.Release.Namespace) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Kubernetes refuses to change either field of an existing PriorityClass, and
its error does not say the way out. Only on upgrade: the class belongs to the
release and goes with it on uninstall, so an install never finds one, and a
namespace-scoped installer (who may not read cluster-scoped objects, so the
lookup fails on Forbidden) still gets the preflights' messages. Included
after buildkit.preflight for the same reason. lookup is empty offline.
*/}}
{{- define "buildkit.priorityClassGuard" -}}
{{- if and .Release.IsUpgrade (not .Values.priorityClassName) (dig "create" true (.Values.priorityClass | default dict)) }}
{{- $name := include "buildkit.priorityClassName" . }}
{{- $value := int64 (dig "value" 1000000 (.Values.priorityClass | default dict)) }}
{{- $policy := dig "preemptionPolicy" "Never" (.Values.priorityClass | default dict) }}
{{- with lookup "scheduling.k8s.io/v1" "PriorityClass" "" $name }}
{{- if or (ne (int64 .value) $value) (ne (toString (default "PreemptLowerPriority" .preemptionPolicy)) (toString $policy)) }}
{{- fail (printf "priorityClass.value and priorityClass.preemptionPolicy cannot change on the existing PriorityClass %s (value %d, %s): Kubernetes refuses it. Keep the old settings, or delete the class and upgrade again: kubectl delete priorityclass %s (running daemon pods keep their priority until they restart)." $name (int64 .value) (toString (default "PreemptLowerPriority" .preemptionPolicy)) $name) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
