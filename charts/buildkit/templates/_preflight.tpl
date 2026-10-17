{{/*
Fail early when the release namespace enforces a Pod Security level that
rejects the privileged daemon. Without this, `helm install` reports
"deployed" while the DaemonSet creates no pods (the reason only shows in its
events). lookup returns nothing under `helm template` and in GitOps tools
that render with it, so the check is skipped there. lookup fails the
install on Forbidden: disable preflight.podSecurity if the installer cannot
read Namespace objects.
*/}}
{{- define "buildkit.preflight" -}}
{{- if .Values.preflight.podSecurity }}
{{- $ns := lookup "v1" "Namespace" "" .Release.Namespace }}
{{- $level := dig "metadata" "labels" "pod-security.kubernetes.io/enforce" "" $ns }}
{{- if and $level (ne $level "privileged") }}
{{- fail (printf "namespace %q enforces Pod Security level %q, but buildkitd runs as a privileged pod and would never start. Label the namespace: kubectl label namespace %s pod-security.kubernetes.io/enforce=privileged --overwrite (or set preflight.podSecurity=false to skip this check)" .Release.Namespace $level .Release.Namespace) }}
{{- end }}
{{- end }}
{{- end }}
