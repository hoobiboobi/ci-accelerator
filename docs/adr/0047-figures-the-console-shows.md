# 0047. Figures the console shows

Date: 2026-10-28

## Context

The console explains why an image build was slow or not cached. A figure
on a page is read as a fact: if it is estimated, mixed from different
kinds of builds or taken from too few of them, people draw wrong
conclusions about their code or the build farm. A night of real
pipelines checked against the jobs' own GitLab logs found several such
figures: medians defined two ways on two pages, a "slowest 5 %" from five
builds, a usual time of two builds (one of them the build itself),
parallel steps summed into a wall-clock bar, a "not caused by the code"
share that counted failed and unclear builds, a guessed "time saved".

## Decision

- Every median and p95 is a nearest-rank percentile: always one of the
  measured times. A "usual" time needs five builds of the same kind (from
  the cache, or rebuilt); a p95 needs twenty rebuilds. With fewer the page
  shows a dash and says how many there are.
- Times on a time axis are wall time: the union of the steps' intervals
  (parallel stages count once). "Step time" is shown apart, as such.
- A page says what a figure is when it is not obvious: what a median is of,
  that Kubernetes records whole seconds, that a cgroup counts only block
  I/O, that a year's figure is a median of daily medians.
- Shares of rebuild time name what they count: the build farm's (its
  reasons only) and, apart, unclear.
- A text never claims more than the records prove: a failed step is "new"
  only against a previous build that reported every step; an exit code
  137 is what a SIGKILL returns, not proof of one; "the cache went with the
  daemon" only when the daemon's cache did not persist.
- Each figure that can be recounted from the data is covered by a test
  that recounts it; test/verify/joblogs.py compares the console's steps
  with the jobs' own logs on a live installation.

## Consequences

- Some figures appear later (after five builds, twenty rebuilds) than a
  dashboard that shows any number would show them.
- New figures must follow these rules; review checks for them.
