{{/*
Render-time checks for value combinations that pass the schema but would
crash buildkitd, stall a rollout, or silently delete the cache. Each fails
the install/upgrade with the fix in the message.
*/}}

{{/* "true" when a buildkit disk size is empty or zero ("", "0", "0GB", "0%"). */}}
{{- define "buildkit.isZeroSize" -}}
{{- if regexMatch "^(0*(\\.0*)? ?([kKmMgGtTpP]([iI]?[bB])?|[bB])?|0+%)$" (toString (default "" .)) }}true{{ end }}
{{- end }}

{{/* "true" when a duration is empty or zero ("", "0", "0s", "0h0m"). */}}
{{- define "buildkit.isZeroDuration" -}}
{{- if regexMatch "^([0.]*(ns|us|µs|ms|s|m|h)?)*$" (toString (default "" .)) }}true{{ end }}
{{- end }}

{{/* A buildkit size (binary units: "10GB" = 10 GiB) in MiB, as a float. */}}
{{- define "buildkit.sizeMiB" -}}
{{- $s := toString . | trim }}
{{- $num := float64 (regexFind "^[0-9]+(\\.[0-9]+)?" $s) }}
{{- $unit := regexReplaceAll "^[0-9]+(\\.[0-9]+)? ?" $s "" | lower | trimSuffix "b" | trimSuffix "i" }}
{{- $exp := index (dict "" 0 "k" 1 "m" 2 "g" 3 "t" 4 "p" 5) $unit }}
{{- $v := $num }}
{{- range until (int $exp) }}{{ $v = mulf $v 1024 }}{{ end }}
{{- divf $v 1048576 }}
{{- end }}

{{/* A Kubernetes quantity ("50Gi", "500M") in MiB, as a float. */}}
{{- define "buildkit.quantityMiB" -}}
{{- $s := toString . | trim }}
{{- $num := float64 (regexFind "^[0-9]+(\\.[0-9]+)?" $s) }}
{{- $unit := regexReplaceAll "^[0-9]+(\\.[0-9]+)?" $s "" }}
{{- $factor := index (dict
  "" 1.0 "Ki" 1024.0 "Mi" 1048576.0 "Gi" 1073741824.0 "Ti" 1099511627776.0 "Pi" 1125899906842624.0 "Ei" 1152921504606846976.0
  "k" 1e3 "M" 1e6 "G" 1e9 "T" 1e12 "P" 1e15 "E" 1e18) $unit }}
{{- divf (mulf $num $factor) 1048576 }}
{{- end }}

{{- define "buildkit.validate" -}}
{{- $v := .Values }}
{{- $gc := $v.config.gc }}

{{- /* TLS is not optional: without client certificates anything that reaches
       the port, including RUN steps on the daemon itself, controls it. */}}
{{- if not $v.tls.existingSecret }}
{{- fail (printf "tls.existingSecret is required: the name of a Secret in namespace %s with tls.crt, tls.key (the daemon's certificate) and ca.crt (the CA that signs client certificates). scripts/buildkit-certs.sh from %s creates a CA, this certificate and client certificates: scripts/buildkit-certs.sh --name %s --namespace %s" .Release.Namespace .Chart.Home (include "buildkit.fullname" .) .Release.Namespace) }}
{{- end }}

{{- /* GC: a capless policy without keepDuration deletes every unused record
       after each build. */}}
{{- if not $v.config.gcPolicy }}
{{- if and (include "buildkit.isZeroSize" $gc.maxUsedSpace) (include "buildkit.isZeroSize" $gc.minFreeSpace) }}
{{- fail "config.gc: set maxUsedSpace or minFreeSpace to a non-zero size. Without either, the unfiltered GC policies have no cap and buildkit deletes the whole cache after every build." }}
{{- end }}
{{- if and (include "buildkit.isZeroDuration" $gc.cacheMounts.keepDuration) (include "buildkit.isZeroSize" $gc.cacheMounts.maxUsedSpace) }}
{{- fail "config.gc.cacheMounts.keepDuration: must be a non-zero duration (e.g. \"336h\"). Empty or zero, without cacheMounts.maxUsedSpace, makes buildkit delete every cache mount after each build." }}
{{- end }}
{{- else }}
{{- range $i, $p := $v.config.gcPolicy }}
{{- if and (not $p.filters) (include "buildkit.isZeroDuration" $p.keepDuration) (include "buildkit.isZeroSize" $p.maxUsedSpace) (include "buildkit.isZeroSize" $p.minFreeSpace) (include "buildkit.isZeroSize" $p.reservedSpace) }}
{{- fail (printf "config.gcPolicy[%d]: has no filters, no keepDuration and no caps, so buildkit deletes every unused record after each build. Add maxUsedSpace or minFreeSpace (or a keepDuration)." $i) }}
{{- end }}
{{- end }}
{{- end }}

{{- /* emptyDir with sizeLimit: percentage caps refer to the node disk, not to
       the emptyDir, and exceeding sizeLimit gets the pod evicted with ~1 s
       grace (running builds fail, the cache is deleted). */}}
{{- if and (eq $v.storage.type "emptyDir") $v.storage.emptyDir.sizeLimit }}
{{- $limit := include "buildkit.quantityMiB" $v.storage.emptyDir.sizeLimit | float64 }}
{{- $budget := mulf $limit 0.8 }}
{{- $caps := list }}
{{- if $v.config.gcPolicy }}
{{- range $i, $p := $v.config.gcPolicy }}
{{- $caps = append $caps (dict "name" (printf "config.gcPolicy[%d].maxUsedSpace" $i) "value" $p.maxUsedSpace) }}
{{- $caps = append $caps (dict "name" (printf "config.gcPolicy[%d].reservedSpace" $i) "value" $p.reservedSpace) }}
{{- end }}
{{- else }}
{{- $caps = list (dict "name" "config.gc.maxUsedSpace" "value" $gc.maxUsedSpace) (dict "name" "config.gc.reservedSpace" "value" $gc.reservedSpace) (dict "name" "config.gc.cacheMounts.maxUsedSpace" "value" $gc.cacheMounts.maxUsedSpace) }}
{{- if include "buildkit.isZeroSize" $gc.maxUsedSpace }}
{{- fail (printf "storage.emptyDir.sizeLimit is %s: set config.gc.maxUsedSpace to an absolute size of at most 80%% of it (e.g. \"%dGB\"); otherwise the cache outgrows the limit and the pod is evicted." $v.storage.emptyDir.sizeLimit (int64 (divf $budget 1024))) }}
{{- end }}
{{- end }}
{{- range $caps }}
{{- if not (include "buildkit.isZeroSize" .value) }}
{{- if hasSuffix "%" (toString .value) }}
{{- fail (printf "%s is %s, but storage.emptyDir.sizeLimit is set: percentages refer to the node disk, not to the emptyDir. Use an absolute size of at most 80%% of %s." .name .value $v.storage.emptyDir.sizeLimit) }}
{{- end }}
{{- if gt (include "buildkit.sizeMiB" .value | float64) $budget }}
{{- fail (printf "%s (%s) exceeds 80%% of storage.emptyDir.sizeLimit (%s). Databases, the content store and running builds are outside the GC budget and GC runs only after builds, so leave headroom or the pod is evicted. Note: buildkit units are binary (\"10GB\" = 10 GiB)." .name .value $v.storage.emptyDir.sizeLimit) }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{- /* hostPath: an explicit, pre-created directory; one daemon per state dir. */}}
{{- if eq $v.storage.type "hostPath" }}
{{- if not $v.storage.hostPath.path }}
{{- fail "storage.hostPath.path is required with storage.type=hostPath: point it at a directory created in advance on the dedicated cache disk, as a subdirectory of the mount (e.g. /var/mnt/nvme/buildkit), so that a missing mount fails the pod instead of filling the OS disk." }}
{{- end }}
{{- $strategy := default (dict) $v.updateStrategy }}
{{- $surge := toString (dig "rollingUpdate" "maxSurge" "0" $strategy) }}
{{- if not (regexMatch "^0%?$" $surge) }}
{{- fail (printf "updateStrategy.rollingUpdate.maxSurge is %s, which cannot work with storage.type=hostPath: buildkitd locks its state dir, so a surge pod on the same node never starts and the rollout never completes. Use maxSurge 0." $surge) }}
{{- end }}
{{- end }}

{{- /* workerOCI must not shadow the keys the chart renders in [worker.oci]. */}}
{{- range $k, $_ := $v.config.workerOCI }}
{{- if not (regexMatch "^[A-Za-z0-9_-]+$" $k) }}
{{- fail (printf "config.workerOCI: %q is not a bare TOML key (letters, digits, '_' and '-')." $k) }}
{{- end }}
{{- if has $k (list "enabled" "gc" "max-parallelism" "gcpolicy") }}
{{- fail (printf "config.workerOCI.%s: the chart sets this [worker.oci] key itself%s." $k (ternary " (use config.maxParallelism)" (ternary " (use config.gcPolicy)" "" (eq $k "gcpolicy")) (eq $k "max-parallelism"))) }}
{{- end }}
{{- end }}

{{- /* The rendered buildkitd.toml must parse, and extraConfig must not add GC
       policies behind config.gcPolicy's back. */}}
{{- $toml := include "buildkit.config" . | fromToml }}
{{- if hasKey $toml "Error" }}
{{- fail (printf "config.extraConfig: the rendered buildkitd.toml is invalid (%s). extraConfig is inserted before the chart's tables: use it for top-level keys and new tables only. Never repeat [grpc], [worker.oci], [worker.containerd], [history], [system] or [cdi]; [worker.oci] keys go to config.workerOCI." (get $toml "Error")) }}
{{- end }}
{{- $rendered := len (include "buildkit.gcPolicyList" . | fromJsonArray) }}
{{- $parsed := len (dig "worker" "oci" "gcpolicy" (list) $toml) }}
{{- if ne $rendered $parsed }}
{{- fail "config.extraConfig: defines [[worker.oci.gcpolicy]] tables; set GC policies with config.gcPolicy instead." }}
{{- end }}
{{- end }}
