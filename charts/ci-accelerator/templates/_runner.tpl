{{/*
The [[runners]] section of a runner of the box: its main runner (the runner
chart renders it from gitlab-runner.runners.config, with the runner's values
as the root) and each tier (this chart's templates, this chart's values as
the root). One text, so every runner gives its jobs the same daemons,
certificate, trace context, job data and placement rules. A dict:
  root       the template root (.Release and the globals)
  gitlabUrl  the GitLab URL exactly as the runner has it
  head       lines right after [[runners]] (a tier's name, url, token,
             executor, limit); empty for the main runner, whose register
             step adds them
  build      true: jobs run on build nodes (next to a daemon); false: on any
             node of the jobs' architecture
  job        the job container's requests and limits, by runner key
  image      the default job image
  cache      {secretName, s3: {server, bucket, region, insecure, pathStyle},
             path}: the S3 cache; empty: none
*/}}
{{- define "ci-accelerator.runnerToml" -}}
{{- $root := .root }}
{{- $instance := .gitlabUrl | sha256sum | trunc 6 }}
{{- $build := .build }}
{{- $image := default "alpine:3.22" .image }}
{{- $job := default (dict "cpu_request" "50m" "memory_request" "64Mi" "memory_limit" "512Mi") .job }}
{{- $cache := default (dict) .cache }}
{{- $s3 := default (dict) $cache.s3 }}
{{- $s3server := default "" $s3.server -}}
[[runners]]
{{- with .head }}
{{ . | indent 2 }}
{{- end }}
  environment = [
    "BUILDKIT_REMOTE_ADDR={{ include "ci-accelerator.daemonsAddress" $root }}",
    "BUILDKIT_TLS_DIR=/certs",
    {{- with dig "console" "url" "" $root.Values.global }}
    "CI_ACCELERATOR_CONSOLE_URL={{ . }}",
    {{- end }}
    {{- if $s3server }}
    "TRANSFER_METER_FREQUENCY=5s",
    {{- end }}
  ]
  # Every job gets its own trace, without changes to .gitlab-ci.yml:
  # buildctl reads TRACEPARENT. trace-id = scheme 01, 3 bytes for this
  # GitLab (from gitlabUrl), the pipeline id and the job id (6 bytes each);
  # span-id = the job id. A job's trace is found from its ids, a pipeline's
  # traces by the ci.pipeline.id attribute. Ids outside 1..2^48-1 or not
  # numbers set nothing; a job's own TRACEPARENT is kept.
  pre_build_script = '''
  # ci-accelerator: one trace per job (scheme 01: GitLab, pipeline, job)
  if [ -z "${TRACEPARENT:-}" ] && [ "${CI_PIPELINE_ID:-x}" -gt 0 ] 2>/dev/null && [ "${CI_JOB_ID:-x}" -gt 0 ] 2>/dev/null && [ "$CI_PIPELINE_ID" -le 281474976710655 ] && [ "$CI_JOB_ID" -le 281474976710655 ]; then
    export TRACEPARENT="00-01{{ $instance }}$(printf '%012x%012x' "$CI_PIPELINE_ID" "$CI_JOB_ID")-$(printf '%016x' "$CI_JOB_ID")-01"
  fi
  export OTEL_RESOURCE_ATTRIBUTES="${OTEL_RESOURCE_ATTRIBUTES:+$OTEL_RESOURCE_ATTRIBUTES,}ci.pipeline.id=${CI_PIPELINE_ID:-},ci.job.id=${CI_JOB_ID:-},ci.project.path=${CI_PROJECT_PATH:-}"
  # ci-accelerator: this job's page in the console, when its address is set
  if [ -n "${CI_ACCELERATOR_CONSOLE_URL:-}" ] && [ "${CI_JOB_ID:-x}" -gt 0 ] 2>/dev/null; then
    echo "ci-accelerator: why this job's builds take their time: ${CI_ACCELERATOR_CONSOLE_URL%/}/jobs/{{ $instance }}/$CI_JOB_ID"
  fi
  '''
  [runners.kubernetes]
    namespace = "{{ $root.Values.global.jobs.namespace }}"
    image = {{ $image | quote }}
    pull_policy = "if-not-present"
    {{- range $k := list "cpu_request" "cpu_limit" "memory_request" "memory_limit" }}
    {{- with get $job $k }}
    {{ $k }} = {{ toString . | quote }}
    {{- end }}
    {{- end }}
    helper_cpu_request = "10m"
    helper_memory_request = "32Mi"
    helper_memory_limit = "256Mi"
    service_cpu_request = "10m"
    service_memory_request = "32Mi"
    automount_service_account_token = false
    [runners.kubernetes.node_selector]
    {{- with include "ci-accelerator.jobNodeSelector" (dict "root" $root "build" $build) | trim }}{{ . | nindent 6 }}{{ end }}
    [runners.kubernetes.node_tolerations]
    {{- with include "ci-accelerator.jobTolerations" (dict "root" $root "build" $build) | trim }}{{ . | nindent 6 }}{{ end }}
    [runners.kubernetes.pod_labels]
      "ci-accelerator.io/component" = "ci-job"
    # Job data from GitLab's CI variables on the job pod, for tools that
    # show builds by pipeline and job (the runner adds the job id, name,
    # URL, ref, commit and project id itself). An unset variable gives an
    # empty value; values are expanded once, never as templates.
    [runners.kubernetes.pod_annotations]
      "ci-accelerator.io/pipeline-id" = "$CI_PIPELINE_ID"
      "ci-accelerator.io/pipeline-source" = "$CI_PIPELINE_SOURCE"
      "ci-accelerator.io/pipeline-created-at" = "$CI_PIPELINE_CREATED_AT"
      "ci-accelerator.io/project-path" = "$CI_PROJECT_PATH"
      "ci-accelerator.io/job-stage" = "$CI_JOB_STAGE"
      "ci-accelerator.io/job-started-at" = "$CI_JOB_STARTED_AT"
      "ci-accelerator.io/commit-title" = "$CI_COMMIT_TITLE"
      "ci-accelerator.io/commit-author" = "$CI_COMMIT_AUTHOR"
      "ci-accelerator.io/merge-request-iid" = "$CI_MERGE_REQUEST_IID"
    [[runners.kubernetes.volumes.secret]]
      name = "{{ include "ci-accelerator.buildkitName" $root }}-client"
      mount_path = "/certs"
      read_only = true
{{- if $s3server }}
  # S3 cache for `cache:` in .gitlab-ci.yml. The runner signs a URL per
  # job and the job's helper moves the archive itself; nothing deletes old
  # caches (use the bucket's lifecycle rules). One folder per GitLab for
  # every runner of this release: tiers share a project's caches.
  [runners.cache]
    Type = "s3"
    Path = {{ default (printf "%s/%s" $root.Release.Name $instance) $cache.path | quote }}
    Shared = true
    MaxUploadedArchiveSize = {{ int64 (default 4831838208 $cache.maxArchiveBytes) }}
    [runners.cache.s3]
      ServerAddress = {{ $s3server | quote }}
      BucketName = {{ toString $s3.bucket | quote }}
      BucketLocation = {{ toString $s3.region | quote }}
      Insecure = {{ default false $s3.insecure }}
      {{- /* Not default: it takes false for "not set", and an unset
      PathStyle means path style on every server but AWS. */}}
      {{- if not (has (toString $s3.pathStyle) (list "" "<nil>")) }}
      PathStyle = {{ eq (toString $s3.pathStyle) "true" }}
      {{- end }}
{{- end }}
{{ printf "" }}
{{- end }}

{{/*
Node selector of a runner's jobs: the build nodes' labels when build is
true, and always the architecture (the runner picks its helper image by
it). A dict: root, build.
*/}}
{{- define "ci-accelerator.jobNodeSelector" -}}
{{- $root := .root }}
{{- $selector := dict }}
{{- if and .build $root.Values.global.buildNodes.enabled }}
{{- $selector = deepCopy (default (dict) $root.Values.global.buildNodes.selector) }}
{{- end }}
{{- $_ := set $selector "kubernetes.io/arch" $root.Values.global.jobs.architecture }}
{{- range $k, $v := $selector }}
{{ $k | quote }} = {{ toString $v | quote }}
{{- end }}
{{- end }}

{{/*
Tolerations of a runner's jobs: the build nodes' taints when build is
true, none otherwise. A dict: root, build.
*/}}
{{- define "ci-accelerator.jobTolerations" -}}
{{- if .build }}
{{- include "ci-accelerator.runnerTolerations" .root }}
{{- end }}
{{- end }}

{{/*
A tier's objects are named <release>-runner-<tier>. A dict: root, name.
*/}}
{{- define "ci-accelerator.tierName" -}}
{{- printf "%s-runner-%s" .root.Release.Name .name }}
{{- end }}

{{- define "ci-accelerator.tierSelector" -}}
app.kubernetes.io/name: runner
app.kubernetes.io/instance: {{ .root.Release.Name | quote }}
ci-accelerator.io/runner-tier: {{ .name | quote }}
{{- end }}

{{- define "ci-accelerator.tierLabels" -}}
{{ include "ci-accelerator.tierSelector" . }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
app.kubernetes.io/part-of: ci-accelerator
ci-accelerator.io/component: runner
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
An S3 cache block (gitlab-runner.runners.cache or a tier's cache) is either
off (no s3.server) or complete. A dict: cache, where (the value's name).
*/}}
{{- define "ci-accelerator.checkCache" -}}
{{- $cache := default (dict) .cache }}
{{- $s3 := default (dict) $cache.s3 }}
{{- if $s3.server }}
{{- range $k := list "bucket" "region" }}
{{- if not (get $s3 $k) }}
{{- fail (printf "%s.s3.%s is needed with %s.s3.server" $.where $k $.where) }}
{{- end }}
{{- end }}
{{- if not $cache.secretName }}
{{- fail (printf "%s.secretName: the Secret with the S3 keys (keys accesskey and secretkey) is needed with %s.s3.server" .where .where) }}
{{- end }}
{{- if regexMatch "^[a-z]+://|/" (toString $s3.server) }}
{{- fail (printf "%s.s3.server is a host (and port), without https:// or a path: got %q (use s3.insecure for plain http)" .where (toString $s3.server)) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Render-time checks of the tiers.
*/}}
{{- define "ci-accelerator.checkTiers" -}}
{{- $runner := index .Values "gitlab-runner" }}
{{- /* The Service the jobs dial: the routers' or, without them, the
daemons'. Both run only on the build nodes. */}}
{{- $routingKey := "control-plane.router.routing" }}
{{- $routing := dig "control-plane" "router" "routing" "PreferSameNode" .Values.AsMap }}
{{- if ne (toString (dig "router" "enabled" false .Values.global)) "true" }}
{{- $routingKey = "buildkit.service.routing" }}
{{- $routing = dig "buildkit" "service" "routing" "PreferSameNode" .Values.AsMap }}
{{- end }}
{{- range $name, $tier := (default (dict) .Values.runnerTiers) }}
{{- $tier = default (dict) $tier }}
{{- if not (regexMatch "^[a-z]([-a-z0-9]*[a-z0-9])?$" $name) }}
{{- fail (printf "runnerTiers: %q is not a valid tier name (lower-case letters, digits and '-', starting with a letter)" $name) }}
{{- end }}
{{- $full := include "ci-accelerator.tierName" (dict "root" $ "name" $name) }}
{{- if gt (len $full) 63 }}
{{- fail (printf "runnerTiers.%s: the objects' name %q is longer than 63 characters; use a shorter tier name" $name $full) }}
{{- end }}
{{- if not $runner.gitlabUrl }}
{{- fail "runnerTiers need gitlab-runner.gitlabUrl (the GitLab the tiers' runners belong to)" }}
{{- end }}
{{- if not (dig "existingSecret" "" (default (dict) $tier.token)) }}
{{- fail (printf "runnerTiers.%s.token.existingSecret: the Secret with the tier's runner token (glrt-..., key runner-token). Create the runner in GitLab first, with the tags its jobs use." $name) }}
{{- end }}
{{- range $k := list "requests.cpu" "requests.memory" "limits.memory" }}
{{- $parts := splitList "." $k }}
{{- if not (dig (index $parts 0) (index $parts 1) "" (default (dict) $tier.resources)) }}
{{- fail (printf "runnerTiers.%s.resources.%s is needed: the size of each job's container (e.g. resources: {requests: {cpu: 250m, memory: 512Mi}, limits: {memory: 2Gi}})" $name $k) }}
{{- end }}
{{- end }}
{{- if not (has (default "any" $tier.nodes) (list "any" "build")) }}
{{- fail (printf "runnerTiers.%s.nodes must be any (any node of the jobs' architecture) or build (the build nodes, next to a daemon), got %q" $name (toString $tier.nodes)) }}
{{- end }}
{{- if and (ne (default "any" $tier.nodes) "build") $.Values.global.buildNodes.enabled (eq (toString $routing) "Local") }}
{{- fail (printf "runnerTiers.%s.nodes is any, but %s is Local: the tier's jobs run on nodes without a router or daemon, where that Service has no endpoint, so they cannot connect. Use routing PreferSameNode or Cluster, or nodes: build." $name $routingKey) }}
{{- end }}
{{- if and $tier.concurrent (lt (int $tier.concurrent) 1) }}
{{- fail (printf "runnerTiers.%s.concurrent must be at least 1" $name) }}
{{- end }}
{{- include "ci-accelerator.checkCache" (dict "cache" $tier.cache "where" (printf "runnerTiers.%s.cache" $name)) }}
{{- include "ci-accelerator.preflightTierSecret" (dict "root" $ "name" $name "tier" $tier) }}
{{- end }}
{{- include "ci-accelerator.checkCache" (dict "cache" $runner.runners.cache "where" "gitlab-runner.runners.cache") }}
{{- end }}

{{/*
A tier's token Secret, when it already exists, must hold its key (the
pod would wait in CreateContainerConfigError otherwise). lookup is empty
offline.
*/}}
{{- define "ci-accelerator.preflightTierSecret" -}}
{{- if .root.Values.preflight.runnerSecret }}
{{- $token := default (dict) .tier.token }}
{{- $key := default "runner-token" $token.key }}
{{- $secret := lookup "v1" "Secret" .root.Release.Namespace $token.existingSecret }}
{{- if and $secret (not (hasKey (default (dict) $secret.data) $key)) }}
{{- fail (printf "Secret %s/%s (runnerTiers.%s.token) has no %s key: put the runner's token (glrt-...) there" .root.Release.Namespace $token.existingSecret .name $key) }}
{{- end }}
{{- end }}
{{- end }}
