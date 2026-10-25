# 0043. The daemons' priority, default requests and disk probe

Date: 2026-10-25

## Context

On 2026-10-25 a full disk on a single-node test box (the node's root disk
held the cache and a benchmark's registry) made the kubelet evict pods,
and the daemon went among the first: it had no resource requests
(BestEffort) and no priority. The kubelet ranks pods for eviction under
memory and disk pressure by whether they use more than they requested,
then by priority, then by how much more (Kubernetes docs, node-pressure
eviction); without ephemeral-storage requests every pod is in the first
group, so priority decides. BuildKit's API reports how much the cache
holds, not how much room is left on its disk, so the console could not see
it coming.

## Decision

- The buildkit chart creates its own PriorityClass, on by default:
  cluster-scoped and named `<namespace>-<fullname>`, value 1000000 (above
  CI jobs at 0, below Kubernetes' system classes), `preemptionPolicy:
  Never` (the daemon never evicts running jobs to get scheduled).
  `priorityClassName` uses an existing class instead; `priorityClass.create`
  switches it off (no permission for cluster-scoped objects).
- Default requests 500m CPU and 512Mi, no limits: the point is having
  requests, not their size. Helm cannot know the node (a `lookup` breaks
  GitOps), so values list sizes, NOTES says when the chart's minimum is
  still in place, and installers that know the node (quickstart, the v0.4
  installer) size them.
- A disk probe next to each daemon: the console binary's `disk-probe` mode
  (one image to publish and pin), the cache's volume read-only, unprivileged,
  answering `GET /disk` with statfs numbers over plain HTTP on its port.
  Off in the standalone chart (no console to read it), on in the box; the
  box refuses a probe image or port that differs from the console's.
- The console reads the probe at every inspection of a daemon (once a
  minute) and the Nodes page shows the disk against BuildKit's keep-free
  floor and the kubelet's default eviction thresholds (10 % / 15 %
  available); a nearly full disk lights the Nodes item.

## Consequences

- One daemon rollout on upgrade (pod template), with an upgrade note.
- Installing needs permission to create a PriorityClass (or the switch).
- Builds share the daemon pod's network and can read the probe's numbers;
  they are numbers about the disk the builds already write to.
- Without `--reset-then-reuse-values` an upgraded box keeps its old values
  and the probe stays off.
