# Install on a cluster that is already in use

[install.md](install.md) assumes an empty cluster and cluster-admin
rights. On a cluster a platform team runs, some of its steps meet
something that is already there: a cert-manager, a GitLab runner,
admission policies, named StorageClasses, limits on cluster-scoped
objects. This page goes through [install.md](install.md) step by step
and says what changes in each case; everything not named here stays as
on that page. Read the first section before you start: it is the list
to give the platform team.

## What the install needs from the cluster

Outside its own namespace the install creates, by default:

| Object | Name | Instead |
|---|---|---|
| Namespace for the job pods | `ci-accelerator-jobs` (Pod Security `baseline`) | pre-created by you: [below](#the-jobs-namespace-is-created-by-the-platform-team) |
| PriorityClass for the daemons and the routers | `ci-accelerator-ci-accelerator-buildkit` | an existing class: [below](#priorityclasses-are-managed-by-the-platform-team) |
| ClusterRole and ClusterRoleBinding for the controller's TokenReviews | `ci-accelerator-ci-accelerator-controller-tokenreview` | none: they cannot be pre-created or switched off |
| The CA of this installation, in cert-manager's namespace: an Issuer, a Certificate and its Secret | `ci-accelerator-ci-accelerator-buildkit-ca` (the Issuer: `...-buildkit-ca-selfsigned`) | a dedicated issuer of your own: [below](#cert-manager-is-already-installed) |
| ClusterIssuer that signs the daemons' and the jobs' certificates | `ci-accelerator-ci-accelerator-buildkit-ca` | the same |

(The names are `<namespace>-<release>-...`; the table shows them for the
namespace and release name `ci-accelerator`.)

So the account that runs `helm install` needs: `create` on ClusterRoles,
ClusterRoleBindings, PriorityClasses, ClusterIssuers and Namespaces;
`create` on Issuers, Certificates and Secrets in cert-manager's
namespace; and, for the chart's checks before the install, `list` on
Nodes and `get` on the box namespace, on the jobs namespace and on the
box namespace's Secrets. An account without the Node and Secret rights
can switch those checks off (`buildkit.preflight.buildNodes`,
`buildkit.preflight.podSecurity`, `preflight.runnerSecret`); the read of
the jobs namespace has no switch. An account without the ClusterRole
right cannot install at all: someone with it runs the install.

## Pod Security is managed by a policy

The box namespace must admit `privileged` pods (the daemons), the jobs
namespace `baseline`. A policy engine (Kyverno, Gatekeeper, a cluster
default that labels every namespace) can refuse or undo the label of
[install.md step 3](install.md#3-create-the-namespace). The chart's check
reads the label only: with the label in place and a policy that still
refuses privileged pods, `helm install` reports `deployed` and no daemon
pod ever appears (`kubectl -n ci-accelerator get ds` shows `DESIRED 1,
CURRENT 0`, and the DaemonSet's events say why). Ask for an exemption
for the namespace `ci-accelerator` before the install. When the team
creates the namespace for you, it exists before step 3: skip `kubectl
create namespace` there and set the label with `--overwrite` (a
namespace labelled `baseline` by a default policy stops the install with
`namespace "ci-accelerator" enforces Pod Security level "baseline"`).
Check:

```sh
kubectl label namespace ci-accelerator pod-security.kubernetes.io/enforce=privileged --overwrite
kubectl get namespace ci-accelerator --show-labels
```

The labels must contain `pod-security.kubernetes.io/enforce=privileged`
and nothing that overrides it (`enforce` is what counts; `warn` and
`audit` only report).

## cert-manager is already installed

Do not install a second one; `helm install cert-manager` on a cluster
that has it either fails (`cannot re-use a name`) or, in another
namespace, replaces the CRDs under the existing installation. Three
things to check instead.

**The version.** The charts' Certificates use `renewBeforePercentage`,
which cert-manager has since v1.16:

```sh
kubectl get deploy -A -l app.kubernetes.io/name=cert-manager \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.spec.template.spec.containers[0].image}{"\n"}{end}'
```

The image tag is the version; v1.16.0 or newer works, and the newest
release is what the project tests against. Older: ask the platform team
for an upgrade.

**The namespace for cluster-scoped issuers.** The chart puts the CA of
this installation (an Issuer, a Certificate, its Secret) into the
namespace cert-manager uses for ClusterIssuers' Secrets:
`cert-manager` by default, or what the controller runs with as
`--cluster-resource-namespace`. Find it:

```sh
kubectl get deploy -A -l app.kubernetes.io/component=controller,app.kubernetes.io/name=cert-manager \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.spec.template.spec.containers[0].args}{"\n"}{end}'
```

The first word is the namespace cert-manager runs in; if the arguments
contain `--cluster-resource-namespace=<other>`, that is the one
(`$(POD_NAMESPACE)` means the namespace it runs in). When it is not
`cert-manager`, set it in `values.yaml`:

```yaml
buildkit:
  tls:
    certManager:
      clusterResourceNamespace: <cert-manager's cluster resource namespace>
```

With the wrong namespace the ClusterIssuer never finds its Secret, the
daemons' Certificate stays not ready, the daemon pods wait in
`ContainerCreating` and `helm install --wait` times out.

**The platform team's ClusterIssuers.** Do not use them. Leave
`global.tls.issuerRef.name` empty: the chart makes a CA for this box
alone, because any certificate the issuer signs is accepted by the
daemons, and a shared issuer would let every certificate of every team
drive them. An ACME issuer (Let's Encrypt) cannot be used at all: it
writes no `ca.crt`. If the platform team insists on issuing, the issuer
must be a CA or Vault issuer dedicated to this box, named in
`global.tls.issuerRef` as a `ClusterIssuer`.

## There is no default StorageClass

The console's controller keeps its store on a volume
(`ci-accelerator-controller-data`, 10Gi, ReadWriteOnce). Without a
default class the claim stays `Pending`, the controller pod with it, and
`helm install --wait` times out. Name a class:

```sh
kubectl get storageclass
```

Pick a block class (not NFS: the store is SQLite) and set it in
`values.yaml`:

```yaml
control-plane:
  controller:
    persistence:
      storageClass: fast-ssd
```

An existing claim of yours goes into `persistence.existingClaim`
instead.

## PriorityClasses are managed by the platform team

The daemons get a PriorityClass so that, under node pressure, the
kubelet evicts CI jobs before them; the routers share it. Without the
right to create one, or with a class the team prefers, set all three
values together:

```yaml
buildkit:
  priorityClass:
    create: false
  priorityClassName: platform-build     # an existing class, or "" for none
control-plane:
  router:
    priorityClassName: platform-build   # the same (the box defaults to the daemons' class, which does not exist now)
```

Without the third value the chart stops before it creates anything:
`the router runs with the daemons' PriorityClass, which
buildkit.priorityClass.create=false or buildkit.priorityClassName leaves
out: set control-plane.router.priorityClassName ...`. The class
should sit above the CI jobs' priority (0) and below the system classes,
with `preemptionPolicy: Never`, so the daemons never evict running jobs
to get scheduled, and not be `globalDefault`.

## The jobs namespace is created by the platform team

The release creates `ci-accelerator-jobs` and refuses one that exists
(`Namespace ... exists and was not created by this release`). When
namespaces come from the team, let them create it with its label and
tell the chart:

```sh
kubectl create namespace ci-accelerator-jobs
kubectl label namespace ci-accelerator-jobs pod-security.kubernetes.io/enforce=baseline
```

```yaml
jobsNamespace:
  create: false
```

The name must stay `ci-accelerator-jobs` (or change `global.jobs.namespace`
and `gitlab-runner.runners.jobNamespace` together); the box still issues
the jobs' client certificate into it and still writes its NetworkPolicy
there. `helm uninstall` then leaves the namespace alone.

## The other nodes belong to everyone

The runner, the controller and the web run on any node without the
build-node taint. To place them (a node pool, a team label):

```yaml
gitlab-runner:
  nodeSelector: { team: platform }
  tolerations: []
control-plane:
  controller:
    nodeSelector: { team: platform }
    tolerations: []
  web:
    nodeSelector: { team: platform }
    tolerations: []
```

Nodes in several zones: the router keeps each job in its zone by
default; [production.md](production.md#more-build-nodes-zones-arm64)
says when to turn that off.

## The cluster has its own GitLab runner

The box's own runner is one `gitlab-runner` release with a configuration
that sends job pods to the build nodes with the daemons' address and
client certificate. A runner you already run can get the same
configuration instead. Two ways:

- **A new runner for the build jobs (recommended).** Create another
  runner in GitLab with its own tag (`ci-accelerator`), install it with
  the configuration below next to your existing one, and leave your
  existing runner as it is. Only jobs tagged `ci-accelerator` go to the
  build nodes.
- **Your existing runner.** Give it the configuration below. Every job
  of that runner then runs on the build nodes with the small job tier
  (50m CPU, 64Mi, 512Mi limit), not only the image builds.

Either way:

1. **Install the box without its runner**, following [install.md](install.md)
   with these values instead of the `gitlab-runner:` block of its step 6.
   The URL stays required: the router and the console key every job by
   it, and it must be the same string as your runner's `gitlabUrl`,
   trailing slash included (`helm get values <your runner release> -n
   <its namespace> | grep gitlabUrl`).

   ```yaml
   gitlab-runner:
     enabled: false
     gitlabUrl: https://gitlab.example.com/
   ```

   Skip step 5 (the token Secret) of install.md; in step 8 there is no
   `ci-accelerator-gitlab-runner` pod.
2. **The runner's token Secret**, in the runner's own namespace (not the
   box's; for a new runner create it first: `kubectl create namespace
   gitlab-runner`), with both keys, as in
   [install.md step 5](install.md#5-store-the-token-in-a-secret).
3. **The configuration.** Download the values fragment of the box's
   release:

   ```sh
   curl -fsSLO https://raw.githubusercontent.com/hoobiboobi/ci-accelerator/v0.5.2/integrations/gitlab-runner/values.yaml
   ```

   Change the lines marked `ADJUST`: the GitLab URL (the same string as
   in step 1), the Secret's name, the jobs namespace
   (`ci-accelerator-jobs`), the router's address
   `tcp://ci-accelerator-router.ci-accelerator.svc:1234` and the client
   Secret `ci-accelerator-buildkit-client` (both already right for the
   release name and namespace `ci-accelerator`), `kubernetes.io/arch`
   for arm64 build nodes.

   For a **new runner release**, that file is the whole values file:

   ```sh
   helm upgrade --install ci-accelerator-runner gitlab-runner \
     --repo https://charts.gitlab.io --version 0.93.0 \
     -n gitlab-runner --create-namespace -f values.yaml
   ```

   Check: `kubectl -n gitlab-runner get pods` shows one pod
   `ci-accelerator-runner-gitlab-runner-...` `1/1 Running`.

   For an **existing release**, merge it into the release's values:
   `helm get values <release> -n <namespace> -o yaml > mine.yaml`, then
   copy from the fragment everything under `runners:` and `rbac:` into
   `mine.yaml`. `runners.config` is one TOML string that Helm does not
   merge: paste the fragment's `config` block into yours and keep every
   line of it (`namespace`, the `BUILDKIT_*` environment, the node
   selector and toleration, the pod labels and annotations, the volume
   with the client certificate and the `pre_build_script`: the router
   and the console find the jobs by them); your own `concurrent`,
   `image`, polling and resource settings can stay. Then upgrade the
   release with `-f mine.yaml`, its own name, namespace and chart
   version (0.93.0 or newer with the runner image 19.4.1 or newer; the
   box ships chart 0.94.1 with 19.5.1, the fragment pins 19.4.2).
4. **RBAC across namespaces.** The runner's ServiceAccount needs the
   fragment's Role in the jobs namespace. The fragment's `rbac.create:
   true` makes it there; with `rbac.create: false` in your release,
   create the Role from the fragment's `rbac.rules` and a RoleBinding to
   your runner's ServiceAccount in `ci-accelerator-jobs` yourself.
5. **Check.** The runner's log says `Runner registered successfully`:
   `kubectl -n <namespace> logs deploy/<release>-gitlab-runner` (the
   official chart names the Deployment after the release, without the
   suffix when the release name starts with `gitlab-runner`;
   `kubectl -n <namespace> get deploy` shows it). The upgrade starts the
   new runner pod before the old one stops.
   Then a test job in a project the runner serves:

   ```yaml
   check-daemons:
     image: moby/buildkit:v0.34.2
     tags: [ci-accelerator]
     script:
       - buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" debug workers -v
   ```

   It prints one worker with the daemon's labels
   (`ci-accelerator.io/node=<build node>`), and the router's log on that
   node has a `routed` line for the job: `kubectl -n ci-accelerator logs
   -l app.kubernetes.io/component=router --tail=-1 | grep routed`. Then
   [first-build.md](first-build.md) as for the box's runner, with your
   tag.

## A values file for such a cluster

Everything above, for a cluster with its own cert-manager in namespace
`cert-manager` (nothing to set), no default StorageClass, PriorityClasses
and namespaces from the platform team, and an own runner:

```yaml
# values.yaml
gitlab-runner:
  enabled: false
  gitlabUrl: https://gitlab.example.com/

jobsNamespace:
  create: false

buildkit:
  priorityClass:
    create: false
  priorityClassName: platform-build
  resources:
    requests: {cpu: "8", memory: 16Gi}
    limits: {memory: 48Gi}

control-plane:
  router:
    priorityClassName: platform-build
  controller:
    persistence:
      storageClass: fast-ssd
    nodeSelector: { team: platform }
  web:
    nodeSelector: { team: platform }
```

Then [install.md](install.md) from step 7, and
[production.md](production.md) for the cache disk and the digests.
