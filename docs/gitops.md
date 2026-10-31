# Installing with Flux or Argo CD

The box is one Helm release from an OCI registry, so a GitOps tool installs
it like any other chart. What is yours to provide:

- **cert-manager**, installed on its own before the box, never as part of
  it ([ADR-0017](adr/0017-cert-manager-prerequisite.md)). The install
  command the chart prints when cert-manager is missing:
  `helm install cert-manager oci://quay.io/jetstack/charts/cert-manager
  --version v1.21.3 --namespace cert-manager --create-namespace --set
  crds.enabled=true`.
- **The build nodes**, labelled and tainted
  `ci-accelerator.io/node-role=build` (README, quickstart).
- **The box namespace** with its Pod Security label: the daemons are
  privileged pods.
- **The Secrets** the values name: the chart never generates credentials,
  so nothing drifts between syncs (below).

Kubernetes 1.34 or newer; every chart says so in its `kubeVersion`.

## The Secrets

All in the box namespace (`ci-accelerator` below), created by you or your
secret tool (SOPS, External Secrets, Sealed Secrets, ...):

| Secret (example name) | Keys | Needed for |
|---|---|---|
| `gitlab-runner-token` | `runner-token` (`glrt-...`), `runner-registration-token` (empty) | always: `gitlab-runner.runners.secret` |
| `gitlab-api-token` | `token` (a `read_api` token) | the console with one shared GitLab token: `control-plane.gitlab.token.existingSecret` |
| `console-gitlab-oauth` | `client-id`, `client-secret` | sign-in through GitLab: `control-plane.signIn.gitlab[].existingSecret` |
| `console-session` | `key`: 32 random bytes in base64 ([how](#the-session-key)) | sign-in: `control-plane.signIn.sessionKey.existingSecret`; changing it signs everyone out |
| `runner-cache` | `accesskey`, `secretkey` | the runners' S3 cache ([runner-cache.md](runner-cache.md)) |
| `runner-<tier>` | `runner-token` | each runner tier ([runner-cache.md](runner-cache.md#runner-tiers)) |

A shared GitLab token and sign-in exclude each other: the chart refuses
both at once. The certificates (the daemons', the CI jobs' client
certificate, the controller's client certificate and its API's server
certificate) come from cert-manager by default and need no Secret from
you; without cert-manager see
[Certificates without cert-manager](#certificates-without-cert-manager).

### The session key

Sign-in through GitLab needs a key: 32 random bytes in base64. The
controller decodes it and seals each person's GitLab token in its store
with it (AES-256-GCM). It lives in a Secret on purpose, apart from the
store's volume, so a copy of the volume alone reveals no token. The chart
never makes it: a key made while rendering would change on every sync. A
new key costs nothing but a sign-in: sessions sealed with the old one are
dropped. The controller reads the key when it starts; restart it after the
Secret changes. Until the Secret exists, the controller's pod waits
(`FailedMount`) and starts by itself once it is there.

Pick one way to make it:

- **Once, by hand.** The Secret stays out of Git; every GitOps tool works
  with it:

  ```sh
  kubectl -n ci-accelerator create secret generic console-session \
    --from-literal=key=$(openssl rand -base64 32)
  ```

- **SOPS (Flux).** Make the Secret locally, encrypt it, commit it; the Flux
  `Kustomization` that applies it decrypts it
  ([Flux's SOPS guide](https://fluxcd.io/flux/guides/mozilla-sops/)):

  ```sh
  kubectl -n ci-accelerator create secret generic console-session \
    --from-literal=key=$(openssl rand -base64 32) --dry-run=client -o yaml >console-session.yaml
  sops --encrypt --age=<your age public key> --encrypted-regex '^(data|stringData)$' --in-place console-session.yaml
  ```

  ```yaml
  # in the Kustomization (kustomize.toolkit.fluxcd.io/v1) that applies it
  spec:
    decryption:
      provider: sops
      secretRef: { name: sops-age }
  ```

  Argo CD reads SOPS only through a plugin; its documentation prefers the
  two ways below.

- **Sealed Secrets.** Seal it locally, commit the `SealedSecret`; it is
  bound to this name and namespace:

  ```sh
  kubectl -n ci-accelerator create secret generic console-session \
    --from-literal=key=$(openssl rand -base64 32) --dry-run=client -o yaml \
    | kubeseal --format yaml >console-session.sealed.yaml
  ```

- **External Secrets Operator** (v1.2.0 or newer) generates it in the
  cluster, once:

  ```yaml
  apiVersion: generators.external-secrets.io/v1alpha1
  kind: Password
  metadata: { name: console-session-key, namespace: ci-accelerator }
  spec:
    length: 32          # 32 characters: the 32 bytes the console decodes
    encoding: base64    # the console reads base64
    allowRepeat: true
    secretKeys: [key]
  ---
  apiVersion: external-secrets.io/v1
  kind: ExternalSecret
  metadata: { name: console-session, namespace: ci-accelerator }
  spec:
    refreshPolicy: CreatedOnce   # every refresh would be a new key
    target: { name: console-session }
    dataFrom:
      - sourceRef:
          generatorRef:
            apiVersion: generators.external-secrets.io/v1alpha1
            kind: Password
            name: console-session-key
  ```

  The generator's defaults (24 characters, not base64) are refused by the
  console, and a `refreshInterval` (ESO's examples use 30m) would sign
  everyone out that often. ESO makes a new key when the Secret is deleted
  or edited, and deleting the `ExternalSecret` deletes the Secret: in
  either case everyone signs in again.

## Flux

The charts are signed with cosign by the release workflow; Flux checks the
signature before it uses the chart. The signatures are Sigstore bundles
(cosign 3), which Flux verifies from 2.8 on (source-controller 1.8); with
an older Flux leave out `verify` and check with `cosign verify` as the
release notes show.

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata: { name: ci-accelerator, namespace: flux-system }
spec:
  interval: 1h
  url: oci://ghcr.io/hoobiboobi/charts/ci-accelerator
  ref: { tag: 0.3.10 }
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
  verify:
    provider: cosign
    matchOIDCIdentity:
      - issuer: "^https://token.actions.githubusercontent.com$"
        subject: "^https://github.com/hoobiboobi/ci-accelerator/.github/workflows/release.yml@refs/tags/v.*$"
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
  releaseName: ci-accelerator
  dependsOn:
    - { name: cert-manager, namespace: cert-manager }   # your cert-manager HelmRelease
  chartRef: { kind: OCIRepository, name: ci-accelerator, namespace: flux-system }
  values:
    gitlab-runner:
      gitlabUrl: https://gitlab.example.com/
      runners:
        secret: gitlab-runner-token
    global:
      jobs: { architecture: amd64 }
      console: { url: https://ci-console.example.com }
    buildkit:
      storage:
        type: hostPath
        hostPath: { path: /var/mnt/nvme/buildkit }
      config:
        gc: { maxUsedSpace: "2TB", reservedSpace: "100GB", minFreeSpace: "400GB" }
      diskProbe:
        image: { digest: "sha256:..." }      # ci-accelerator:v0.3.3 (the probe image, unchanged since), from the release notes
    control-plane:
      image: { digest: "sha256:..." }        # ci-accelerator:v0.3.10, from the release notes
      gitlab:
        token: { existingSecret: gitlab-api-token }
```

`releaseName` pins the release name (Flux's default is the HelmRelease's
name, prefixed with `spec.targetNamespace` when that is set); every object
name below follows from it. The jobs namespace (`ci-accelerator-jobs`) is
created by the release. Flux runs Helm's install and upgrade itself,
against the cluster.

### Sign-in instead of the shared token

```yaml
    control-plane:
      signIn:
        enabled: true                         # at global.console.url
        sessionKey: { existingSecret: console-session }
        gitlab:
          - existingSecret: console-gitlab-oauth
            admins: ["id:1234567"]            # GitLab user ids who see everything
```

Drop `control-plane.gitlab.token` then. The console must be served over
https at `global.console.url` (plain http only on `localhost`, through a
port-forward); the GitLab OAuth application's redirect URI is
`<global.console.url>/auth/callback/<id>`, where `<id>` is the first six
hex digits of the sha256 of the GitLab URL exactly as the runner has it.
GitOps tools do not show the install notes that print it; compute it, or
read it from the controller's log:

```sh
printf %s https://gitlab.example.com/ | sha256sum | cut -c1-6    # shasum -a 256 on macOS
kubectl -n ci-accelerator logs deploy/ci-accelerator-controller | grep redirect_uri
```

The console is the Service `ci-accelerator-web` (port 8080, plain HTTP):
put your Ingress or gateway with TLS in front of it. The box's
NetworkPolicy admits connections to the web from every other namespace
except the jobs namespace, and from the box namespace except the daemons'
pods; an Ingress controller on the host network needs its node addresses
in `control-plane.networkPolicy.extraFrom`.

## Argo CD

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: ci-accelerator
  namespace: argocd
  annotations:
    argocd.argoproj.io/sync-wave: "1"        # cert-manager's Application in an earlier wave
spec:
  project: ci-accelerator
  source:
    repoURL: ghcr.io/hoobiboobi/charts       # no oci:// here
    chart: ci-accelerator
    targetRevision: 0.3.10
    helm:
      releaseName: ci-accelerator
      valuesObject:
        gitlab-runner:
          gitlabUrl: https://gitlab.example.com/
          runners:
            secret: gitlab-runner-token
        # ... the same values as in the Flux example
  destination:
    server: https://kubernetes.default.svc
    namespace: ci-accelerator
  syncPolicy:
    automated: { prune: true }
    syncOptions: [CreateNamespace=true]
    managedNamespaceMetadata:
      labels:
        pod-security.kubernetes.io/enforce: privileged
```

Sync waves order Applications only inside a parent Application (app of
apps); without one, sync cert-manager first. The box namespace comes from
`CreateNamespace=true` with its label (or create it yourself); the jobs
namespace is part of the chart, no option needed for it.

Argo CD renders the chart with `helm template` and the cluster's API
versions, so the chart sees cert-manager's API. Helm's `lookup` returns
nothing there, so the install-time checks that read the live cluster do
not run under Argo CD: the namespace's Pod Security level, a build node
that matches the selector, the keys of the runner token Secret and of a
provided TLS Secret, a jobs namespace that the release does not own or
that is still being deleted, and a 0.2.x
release in the way ([architecture](architecture.md#install-time-checks)
lists them). Check those yourself; the checks on the values themselves
still run.

## Certificates without cert-manager

With `buildkit.tls.source=existingSecret` you provide four certificates
from one CA dedicated to the box. `scripts/buildkit-certs.sh` makes them
(release `ci-accelerator` in the namespace `ci-accelerator`; the router's
names are needed while `global.router.enabled` is on, the default):

```sh
# the daemons' certificate (with the build router's names) and the CI jobs' client
scripts/buildkit-certs.sh --name ci-accelerator-buildkit --namespace ci-accelerator --router ci-accelerator
# the controller's client certificate for the daemons
scripts/buildkit-certs.sh --name ci-accelerator-buildkit --no-server --client controller
# the controller API's server certificate
scripts/buildkit-certs.sh --name ci-accelerator-buildkit --namespace ci-accelerator --controller ci-accelerator
```

Each run prints the `kubectl create secret` command for what it made:
`ci-accelerator-buildkit-tls` (box namespace), `ci-accelerator-buildkit-client`
(jobs namespace: the runners mount it into every job),
`ci-accelerator-buildkit-controller` (the controller's client certificate:
the second run prints it for `<jobs-namespace>`, create it in the box
namespace instead) and `ci-accelerator-controller-tls` (box namespace).
Values:

```yaml
jobsNamespace: { create: false }              # you create it, with its client Secret
buildkit:
  tls:
    source: existingSecret
    existingSecret: ci-accelerator-buildkit-tls
    client: { namespaces: [] }                # no cert-manager to issue into it
control-plane:
  tls:
    existingSecret: ci-accelerator-buildkit-controller
  controller:
    api:
      tls: { existingSecret: ci-accelerator-controller-tls }
```

The jobs namespace then belongs to you, so the client Secret can be
applied with it: label it `pod-security.kubernetes.io/enforce: baseline`
and keep its name equal to `global.jobs.namespace` and
`gitlab-runner.runners.jobNamespace` (`ci-accelerator-jobs` by default).
Keep `ca.key` out of the cluster: any certificate it signs drives the
daemons. With `global.router.enabled=false` the jobs connect to the
daemons directly: leave out `--router` and admit the jobs namespace in
`buildkit.networkPolicy.ingress.namespaces`.

## What the chart does for GitOps

- Every credential is an `existingSecret`; no `genCA`/`randAlphaNum` in the
  templates ([ADR-0002](adr/0002-helm-and-gitops.md)).
- The jobs namespace is part of the release (`jobsNamespace.create`); set
  it to `false` when your tool creates it, keeping its `baseline` Pod
  Security label and the client certificate the box issues into it (with
  cert-manager the Certificate is still the release's). On a release that
  created the namespace, `false` would delete it with everything in it:
  annotate it `helm.sh/resource-policy=keep` first (the chart refuses the
  change until then).
- An upgrade that changes the daemons' pod template rolls the DaemonSet one
  node at a time; `emptyDir` caches are lost then (README, "Where the
  cache lives"). A chart release that changes nothing in that template
  restarts no daemon.
- The daemons read their certificate only at start. The DaemonSet carries
  Stakater Reloader's annotation (`buildkit.tls.reloader`), so with
  Reloader in the cluster a renewed certificate rolls the daemons; under
  Argo CD or Flux run Reloader with `--reload-strategy=annotations`.
  Without Reloader, restart the daemons yourself before the old
  certificate expires.
- Cluster-scoped objects: the jobs Namespace, the daemons' PriorityClass
  (`<namespace>-<release>-buildkit`, which the build router's pods use
  too), the controller's ClusterRole and ClusterRoleBinding for
  TokenReviews (`<namespace>-<release>-controller-tokenreview`) and, with a
  CA the chart bootstraps, a ClusterIssuer
  (`<namespace>-<release>-buildkit-ca`). The tool's service account needs
  to create them (for Argo CD, an AppProject whose
  `clusterResourceWhitelist` allows `Namespace`,
  `scheduling.k8s.io/PriorityClass`, the two RBAC kinds and
  `cert-manager.io/ClusterIssuer`, and whose `destinations` include the
  jobs namespace and, with the bootstrapped CA, cert-manager's namespace,
  where the CA's Issuer and Certificate live). Without the right to create a
  PriorityClass, set `buildkit.priorityClass.create=false`, name an
  existing class in `buildkit.priorityClassName` and the same in
  `control-plane.router.priorityClassName`. The names stay the same
  across syncs.
- The controller's volume (`<release>-controller-data`) carries
  `helm.sh/resource-policy: keep`: it outlives the release, and Argo CD
  treats the annotation as `Delete=false`. Delete it by hand when you
  want the console's history gone.
- Images to mirror for a private registry: `moby/buildkit` (by digest,
  `buildkit.image`), the gitlab-runner images, and
  `ghcr.io/hoobiboobi/ci-accelerator`, which runs the controller, the web,
  the build router and the daemons' disk probe. The disk probe has its own
  pinned tag (it changes only when the probe does), so a mirror needs
  `buildkit.diskProbe.image` set as well as `control-plane.image`.
- 0.3.0 does not upgrade a 0.2.x release in place (the console became the
  control plane, with new names). Under GitOps:
  1. Delete the release (the Argo CD `Application` or Flux
     `HelmRelease`) and wait until its pods are gone; the jobs namespace
     goes with it.
  2. Delete the daemons' certificate Secret `<release>-buildkit-tls`: it
     outlives the release, and 0.3.0's certificate names the build router
     (without cert-manager, make it again with `--router` as above).
  3. Move your values from `console:` to `control-plane:` (the
     controller's pod settings and volume under `control-plane.controller`,
     the web's port under `control-plane.web`; a runner cache's
     `consoleSecretName` is now `controllerSecretName`) and point your
     Ingress at the Service `<release>-web` (it was `<release>-console`).
  4. Create the release again at 0.3.10.

  The box refuses values under `console:` even when rendered offline; the
  check for a running 0.2.x console needs the live cluster, so Argo CD
  does not run it. Details:
  [troubleshooting](troubleshooting.md#upgrading-from-02x-to-030).
