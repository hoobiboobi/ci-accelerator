# 0028. One trace per job

Date: 2026-10-19 (supersedes the trace-id layout of [ADR-0026](0026-job-trace-context.md))

## Context

ADR-0026 gave all jobs of a pipeline one trace id. Reading BuildKit's build history showed two problems with that:

- **Every record gets every job's spans.** BuildKit records the spans of a build by trace id, so each build record of a pipeline carries the spans of every concurrent job of that pipeline on the same daemon. Two parallel jobs already doubled each record's trace (38 → 76 spans); a hundred would grow it a hundredfold, in the daemon's memory, in its history and in anything that reads the history.
- **Telling records apart needs guessing.** A record does not say which client started it. With a shared trace id, the only way to tell concurrent jobs' records apart is timing.

Separately, 8 bytes of pipeline id and 8 bytes of job id do not fit next to an instance prefix in a 16-byte trace id. Job ids on gitlab.com are already above 2³².

## Decision

The runner's `pre_build_script` gives every job its own trace:

- **Trace id:**
  - 1 byte scheme version, `01`;
  - 3 bytes from the SHA-256 of the GitLab URL;
  - the pipeline id in 6 bytes;
  - the job id in 6 bytes.
- **Parent span id:** the job id.
- **Out of range:** ids outside 1 to 2⁴⁸−1, or ids that are not numbers, set no `TRACEPARENT`; the job runs as before.
- **Unchanged:** a job's own `TRACEPARENT` is kept, and `OTEL_RESOURCE_ATTRIBUTES` still carries `ci.pipeline.id`, `ci.job.id` and `ci.project.path`.

## Consequences

- **Each build record holds only its own job's spans.** The job, pipeline and GitLab come from the record's trace id by arithmetic, not by timing. Several builds in one job share the job's trace and belong to it.
- **Only the trace id can be relied on.** BuildKit waits at most 3 seconds after a build for the client's spans unless the client finalizes the record, which `buildctl` does not do. So a record may hold only the daemon's spans, without the `ci.*` attributes. The trace id is always there.
- **A pipeline is no longer one trace** in the trace UI. Its jobs are found by the `ci.pipeline.id` resource attribute in the trace backend. A later collector can add pipeline spans that link the job traces.
- **The scheme version** lets a later layout change without breaking readers.
- **Traces recorded by 0.1.0** keep the old layout.
- **A job that sets one `TRACEPARENT` for a whole pipeline** brings the mixing back. Readers detect it (several `ci.job.id` values in one record) and report the record as ambiguous instead of guessing.
