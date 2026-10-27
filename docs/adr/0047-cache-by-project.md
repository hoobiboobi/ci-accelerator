# 0047. Whose the cache is, and removing a project's part

Date: 2026-10-27

## Context

An operator sees how full a build node's disk is but not who fills it.
BuildKit's disk usage lists records (id, parents, description, type,
size, creation time) and no build, session or project; its history lists
steps, not cache records.

## Decision

- The controller keeps each node's listing (taken every minute with the
  daemon's other figures) and attributes a layer to the build that made
  it: created while a build on the same node executed a step with the
  record's description (COPY, ADD, WORKDIR by the step's name, RUN by its
  command). Several projects at that moment: shared; none: not
  attributed. Base images, build contexts and cache mounts are pools. The
  owner is kept with the record, past its build's retention. Checked on a
  real daemon's listing: every layer attributed.
- The Nodes page shows each node's cache by project, the pools, and how
  much of it is attributed.
- Removing a project's part from a node: BuildKit's prune for exactly the
  records it made alone and nothing holds (parents still used by other
  records stay), asked for by an admin (anyone without sign-in) from the
  console's own pages, recorded with who asked.

## Consequences

- Attribution depends on the steps' times: two projects running an
  identical step on one node at the same moment share it, never a guess.
- Prune takes BuildKit's cache lock while it runs (moby/buildkit#7175):
  builds on that node may wait; the page says so.
