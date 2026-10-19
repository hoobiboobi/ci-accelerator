# 0026. The runner sets every job's trace context

Date: 2026-10-18 (trace-id layout superseded by [ADR-0028](0028-one-trace-per-job.md): one trace per job)

## Context

`buildctl` joins a trace when `TRACEPARENT` is set, and the daemons export their spans into it. Asking users to add trace setup to every `.gitlab-ci.yml` means most pipelines would never be traced. gitlab-runner runs a configurable `pre_build_script` in the same shell as each job's script, right before it.

## Decision

- The runner configuration (the umbrella's and `integrations/gitlab-runner`) exports, in every job:
  - `TRACEPARENT`: version `00`; a trace-id made of 16 hex characters derived from the GitLab URL and the pipeline id in hex; the job id in hex as parent span-id; flags `01`;
  - `OTEL_RESOURCE_ATTRIBUTES`: the pipeline id, job id and project path, added to whatever the job already sets.

  A `TRACEPARENT` the job sets itself is kept. The script uses only `printf` and `export`, so it works in any image with a POSIX shell.
- Job pods carry the annotation `ci-accelerator.io/pipeline-id`, next to the job and project ids the runner adds.

## Consequences

- Every build of a pipeline lands in one trace, with no change to the user's pipeline. The trace id follows from the pipeline id, so a pipeline's trace can be opened without a lookup table.
- The pipeline and its jobs are not spans yet; builds hang under a missing parent. A collector can emit those spans later from the job pods (no GitLab credentials needed), and the derived ids make them fit.
- Other OpenTelemetry-aware tools in a job also join the pipeline's trace.
