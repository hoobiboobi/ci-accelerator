# Installing with Flux or Argo CD

The box is one Helm release from an OCI registry, so a GitOps tool installs
it like any other chart. Two things are yours to create before it: the box
namespace with its Pod Security label (the daemons are privileged pods) and
the Secrets the values name - the chart never generates credentials, so
nothing drifts between syncs. cert-manager is a prerequisite installed on
its own; the box release depends on it.

## Flux

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata: { name: ci-accelerator, namespace: flux-system }
spec:
  type: oci
  url: oci://ghcr.io/hoobiboobi/charts
  interval: 1h
---
apiVersion: v1
kind: Namespace
metadata:
  name: ci-accelerator
  labels:
    pod-security.kubernetes.io/enforce: privileged
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: { name: ci-accelerator, namespace: ci-accelerator }
spec:
  interval: 10m
  dependsOn:
    - { name: cert-manager, namespace: cert-manager }   # your cert-manager HelmRelease
  chart:
    spec:
      chart: ci-accelerator
      version: 0.2.5
      sourceRef: { kind: HelmRepository, name: ci-accelerator, namespace: flux-system }
  values:
    gitlab-runner:
      gitlabUrl: https://gitlab.example.com/
      runners:
        secret: gitlab-runner-token          # a Secret you manage (SOPS, External Secrets, ...)
    global:
      jobs: { architecture: amd64 }
      console: { url: https://ci-console.example.com }
    buildkit:
      storage:
        type: hostPath
        hostPath: { path: /var/mnt/nvme/buildkit }
      config:
        gc: { maxUsedSpace: "2TB", reservedSpace: "100GB", minFreeSpace: "400GB" }
    control-plane:
      image: { digest: "sha256:..." }        # from the release notes
      gitlab:
        token: { existingSecret: gitlab-api-token }
```

The Secrets (`gitlab-runner-token` with the keys `runner-token` and an empty
`runner-registration-token`; `gitlab-api-token` with `token`) live in
the box namespace; the jobs namespace is created by the release.

## Argo CD

The same values in an `Application` with the OCI repository as its source
(`repoURL: ghcr.io/hoobiboobi/charts`, `chart: ci-accelerator`,
`targetRevision: 0.2.5`). Give cert-manager an earlier sync wave
(`argocd.argoproj.io/sync-wave`) and let the box create the jobs namespace
itself (no `CreateNamespace` needed for it; the box namespace you create
with its label, or with `CreateNamespace=true` plus a `managedNamespaceMetadata`
label). Argo CD renders the chart with the cluster's API versions, so the
chart sees cert-manager's API; the checks that need the live cluster are
skipped during rendering and run at install.

## What the chart does for GitOps

- Every credential is an `existingSecret`; no `genCA`/`randAlphaNum` in the
  templates ([ADR-0002](adr/0002-helm-and-gitops.md)).
- The jobs namespace is part of the release (`jobsNamespace.create`); set
  it to `false` when your tool creates it, keeping its `baseline` Pod
  Security label and the client certificate the box issues into it.
- An upgrade that changes the daemons' pod template rolls the DaemonSet one
  node at a time; `emptyDir` caches are lost then (README, "Where the
  cache lives").
- Cluster-scoped objects: the daemons' PriorityClass
  (`<namespace>-<release>-buildkit`), the controller's ClusterRole and
  ClusterRoleBinding for TokenReviews
  (`<namespace>-<release>-controller-tokenreview`) and, with a CA the chart
  bootstraps, a ClusterIssuer. The tool's service account needs to create
  them (for Argo CD, an AppProject whose `clusterResourceWhitelist` allows
  `scheduling.k8s.io/PriorityClass` and the two RBAC kinds), or set
  `buildkit.priorityClass.create=false` and name an existing class in
  `buildkit.priorityClassName`. The names stay the same across syncs.
- The disk probe runs the ci-accelerator image at its own pinned tag
  (it changes only when the probe does): a mirrored registry needs
  `buildkit.diskProbe.image` set as well as `control-plane.image`.
- 0.3.0 does not upgrade a 0.2.x release in place (the console became the
  control plane, with new names): delete the release (the Argo CD
  `Application` or Flux `HelmRelease`), wait for its pods, and create it
  again with the values moved from `console:` to `control-plane:`
  ([troubleshooting](troubleshooting.md#upgrading-from-02x-to-030)).
