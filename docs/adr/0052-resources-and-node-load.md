# 0052. What a build's steps used and what a node carries

Date: 2026-10-29

## Context

A build page showed a step's CPU time only in the step's drawer; nothing
said what a whole build used or whether its steps competed for the node's
cores, and the Nodes page showed only disk. The author wanted CPU and
memory per build and per node, the node's total and the daemon's own
share, and asked for mock-ups first; the chosen ones were a Resources tab
on the build page and a Load column with an hour's chart plus a Load
section on a daemon's page.

BuildKit samples every executed step's cgroup (CPU, I/O, memory) and
keeps the samples in the record's provenance; the console already stored
CPU and I/O per step. BuildKit's API says nothing about the node. The
disk probe next to each daemon (ADR-0043) already reads the node's
filesystem; `/proc/stat`, `/proc/meminfo` and `/proc/pressure/*` are the
kernel's and not namespaced, and the pod's cgroup (the daemon and the
probe together) is readable from the host's cgroup filesystem.

## Decision

- Per step, the console keeps the most memory the step's cgroup held in
  one of BuildKit's samples (anonymous memory, file cache and kernel
  memory: what a memory limit counts). It is a sample, not a peak, and
  the pages say so. The column is added without a new schema version
  (ADR-0051), so a rollback keeps working.
- The build page's Resources tab lists the executed steps by CPU time
  with the cores each used on average over its run, and draws the sum of
  the running steps' cores over the build: a step function from the
  steps' intervals and CPU times, with the node's cores as a line when
  the probe reports them. Nothing is modelled: the figures are the
  steps' own, and the capacity line is the node's core count now.
- The disk probe samples every 10 s: the node's busy cores and memory in
  use (total minus available) from `/proc`, the pod's CPU and working set
  (memory.current minus inactive file cache: the kubelet's measure, on the
  node's basis) from its cgroup under the host's cgroup filesystem
  (mounted read-only; the pod's UID from the downward API names the
  cgroup, with the systemd or the cgroupfs naming; cgroup v2 only - on v1
  the pod's share is reported as not measured), the node's pressure stall
  information where the kernel has it. It keeps an hour in memory and
  serves `GET /load`. Longer series are the monitoring stack's job (OTLP
  push), not the console's.
- The chart passes the cgroup root and the pod's UID as environment
  variables, never as arguments: a probe image of an earlier release
  (pinned or mirrored) exits on an unknown flag, which would keep every
  daemon pod from getting ready (found by the review before 0.3.3).
- The Nodes table draws the hour and shows the latest reading; a chip
  says "waiting for CPU" when PSI reports some task waited for a core
  10 % or more of the last minute. The chip does not put a daemon under
  "needs attention": a busy build node is doing its job.
- A daemon's page splits the node's cores and memory into the daemon's
  pod, the rest of the node and what is free, names the pressure or that
  the kernel reports none, and draws the hour.

## Consequences

- The daemons restart once on upgrade (the pod template gains a volume
  and an environment variable), and the probe's image must be the new one
  for `/load` to answer; the pages say "no answer" for load otherwise.
- Memory is known for builds recorded from this release on; earlier
  builds show a dash.
- The probe's pod figures are the whole pod's: the daemon plus the probe,
  whose share is a few files read every 10 s.
- Kernels without PSI (kind's) show no pressure; the pages say so instead
  of showing zero.
