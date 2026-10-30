# gitlab-runner integration

`values.yaml` here is a values fragment for the official
[gitlab-runner Helm chart](https://docs.gitlab.com/runner/install/kubernetes/).
It sends the CI jobs of a runner you install yourself to the buildkit
daemons, through the box's build router or straight to `charts/buildkit`,
and makes the job pods thin clients. It is the same runner configuration
the box gives its own runner (`make validate` checks that both stay
identical).

Tested with chart 0.93.0 and gitlab-runner 19.4.2: `make validate` renders
the chart with the fragment, and `make e2e` (scenario `runner`) runs the real
runner end to end. See "What the e2e proves" below.

## Two ways to use it

| | The box with your own runner | `charts/buildkit` alone |
|---|---|---|
| Install | `charts/ci-accelerator` with `gitlab-runner.enabled=false` | `charts/buildkit` |
| `BUILDKIT_REMOTE_ADDR` | The build router: `tcp://<release>-router.<namespace>.svc:1234` | The daemons' Service: `tcp://<buildkit>.<namespace>.svc:1234` |
| Client certificate | `<buildkit>-client`, issued by the box into its jobs namespace (`global.jobs.namespace`) | `<buildkit>-client`, issued into each namespace of `tls.client.namespaces`, or your own Secret with `tls.existingSecret` |
| Which daemon builds | The one where the job's project built before (see below) | The one on the job's node |
| The console | Yes (`control-plane.enabled`, the default) | Only with `charts/control-plane` next to it |

`<release>` is the box's release name (the router's name uses its first 40
characters) and `<namespace>` the namespace it is installed in.
`<buildkit>` is the daemons' full name: `<release>-buildkit`, or the
release name if it already contains "buildkit". The fragment's defaults
are the box installed as release `ci-accelerator` in namespace
`ci-accelerator`, with its jobs in `ci-accelerator-jobs`.

In the box with `global.router.enabled=false`, jobs connect to the
daemons' Service as with `charts/buildkit` alone.

## What it sets up

| Concern | How |
|---|---|
| Where the daemons are | `BUILDKIT_REMOTE_ADDR` in every job (see the table above) |
| Client certificate | The client certificate Secret in the jobs namespace, mounted read-only at `BUILDKIT_TLS_DIR=/certs`. Through the router the job keeps its mutual TLS with the daemon: the router passes the bytes through and holds no key, and the daemons' certificate names the router's Service |
| Warm cache | With the router: each job goes to the node where its project built in the last 7 days (`control-plane.router.warmDays`; with an `emptyDir` cache only builds since that daemon last started count), else to the project's node by a hash that every router agrees on, so a new project's parallel jobs share one daemon's steps; with zones (`control-plane.router.zones`), within the job's zone. Without the router: the daemons' Service prefers the daemon on the job's node |
| Placement | Job pods run on the build nodes (node selector + toleration), next to a router and a daemon: the router's and the daemons' Services prefer the job's own node |
| Job identity | Job pods get the label `ci-accelerator.io/component: ci-job` and `ci-accelerator.io/` annotations with the job's data from GitLab's CI variables (pipeline, project path, stage, start time, commit title and author, merge request); the runner adds the job's id, URL, name, ref, commit and project id. The router knows a job by its pod's address and reads its project there; the console shows builds by pipeline and job from them. A connection the router cannot tie to a job pod goes to the daemon on the router's own node (by hash when that node has none ready) |
| Small jobs | 50m CPU / 64Mi requested, 512Mi memory limit: the build runs in the daemon |
| Isolation | Jobs run in their own namespace (`runners.jobNamespace`, Pod Security `baseline` is enough), without a Kubernetes API token |
| Least privilege | The runner gets only the permissions the Kubernetes executor needs in the jobs namespace, not the chart's default `*` on core resources |
| Console link (optional) | With `CI_ACCELERATOR_CONSOLE_URL` in `environment` (commented out in `values.yaml`), the `pre_build_script` prints the job's page in the console into the job's log |
| S3 cache (optional) | A commented `[runners.cache]` block for GitLab's `cache:` keyword in S3-compatible storage; see [docs/runner-cache.md](../../docs/runner-cache.md) for the bucket, its layout and a lifecycle rule |
| Traces | A `pre_build_script` exports `TRACEPARENT` (one trace per job, its id derived from the GitLab URL and the pipeline and job ids) and `OTEL_RESOURCE_ATTRIBUTES` (pipeline, job, project) in every job, without changes to `.gitlab-ci.yml` |

A job then builds with:

```sh
buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" build ...
```

or with the [GitLab CI component](../../ci/gitlab/README.md), which does
that for you.

## Use it with the box

1. Install the box as the [README](../../README.md#quickstart) describes
   (prerequisites, its namespace), with its own runner switched off and the
   GitLab URL of your runner, exactly as your runner has it (slash
   included):

   ```sh
   helm install ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.3.8 \
     -n ci-accelerator \
     --set gitlab-runner.enabled=false \
     --set gitlab-runner.gitlabUrl=https://gitlab.example.com/ \
     --wait --timeout 10m
   ```

   The controller and the router read that URL (the box writes it into the
   ConfigMap `<release>-runner-gitlab`): every job's trace id carries a
   hash of exactly this string, and they do not start without it. The box
   creates the jobs namespace (`global.jobs.namespace`, default
   `ci-accelerator-jobs`, Pod Security `baseline`) and issues the client
   certificate `<buildkit>-client` into it.
2. Create a runner in GitLab and put its authentication token in a Secret in
   the runner's namespace, as described in
   [The runner token Secret](#the-runner-token-secret).
3. Change the lines marked `ADJUST` in `values.yaml`:
   - the GitLab URL (`gitlabUrl`), the same string as in step 1;
   - the token Secret's name (`runners.secret`);
   - the jobs namespace (`runners.jobNamespace`): the box's
     `global.jobs.namespace`. The box's NetworkPolicies let only that
     namespace reach the routers, and only the routers and the controller
     reach the daemons;
   - the router's address in `BUILDKIT_REMOTE_ADDR`,
     `tcp://<release>-router.<namespace>.svc:1234`, and the client Secret's
     name, `<buildkit>-client`;
   - `kubernetes.io/arch` for arm64 build nodes.
   - Optional: the console's address (`CI_ACCELERATOR_CONSOLE_URL` in
     `environment`), as the box's `global.console.url` has it, so every job
     prints the link to its page. The console is the Service
     `<release>-web`.
   - Optional: the S3 cache. Uncomment `cache.secretName` and add the
     `[runners.cache]` block from the comments to `config`
     ([docs/runner-cache.md](../../docs/runner-cache.md)).
4. Install the runner:

   ```sh
   helm upgrade --install gitlab-runner gitlab-runner \
     --repo https://charts.gitlab.io --version 0.93.0 \
     -n ci-accelerator -f integrations/gitlab-runner/values.yaml
   ```

## Use it with charts/buildkit alone

1. Create the jobs namespace with Pod Security `baseline` (the runner chart
   creates its Role there, and the client certificate is issued there):

   ```sh
   kubectl create namespace ci-accelerator-jobs
   kubectl label namespace ci-accelerator-jobs pod-security.kubernetes.io/enforce=baseline
   ```

2. Install the buildkit chart with the jobs namespace in
   `tls.client.namespaces`, so that the client certificate Secret
   `<buildkit>-client` exists there. With `tls.source=existingSecret`, make
   a client certificate with `scripts/buildkit-certs.sh` and create a Secret
   from it in the jobs namespace (keys `ca.crt`, `tls.crt`, `tls.key`), and
   with `networkPolicy.enabled` add the jobs namespace to
   `networkPolicy.ingress.namespaces` (the namespaces in
   `tls.client.namespaces` are admitted already).
3. Create a runner in GitLab and put its token in a Secret, as described in
   [The runner token Secret](#the-runner-token-secret).
4. Change the lines marked `ADJUST` in `values.yaml` as above, except:
   - `BUILDKIT_REMOTE_ADDR` is the daemons' Service,
     `tcp://<buildkit>.<namespace>.svc:1234`;
   - the client Secret is `<buildkit>-client`, or the one you created;
   - leave `CI_ACCELERATOR_CONSOLE_URL` out unless you run
     `charts/control-plane` too.
5. Install the runner with the command above.

The build router can also run with the standalone charts, both in the
same namespace: `charts/control-plane` with `router.enabled=true`,
`jobs.namespace` (the jobs namespace), `gitlab.urls` and
`daemons.discover` (the daemons' headless Service), and `charts/buildkit`
with `global.router.enabled=true` and `global.router.service` set to the
router's Service, `<prefix>-router` (`<prefix>` is control-plane's
`fullnameOverride` or its release name): the daemons' certificate then
names it, and their NetworkPolicy admits the routers instead of the jobs
namespace (add the controller with `networkPolicy.ingress.extraFrom`).
Point `BUILDKIT_REMOTE_ADDR` at `tcp://<prefix>-router.<namespace>.svc:1234`.
Each chart's values describe the rest; the e2e covers this setup only
through the box.

## The runner token Secret

The chart mounts both keys of `runners.secret`, so the Secret needs two:

| Key | Value |
|---|---|
| `runner-token` | the runner authentication token from GitLab (`glrt-...`) |
| `runner-registration-token` | empty, but it must exist |

Create it with the real token (not a placeholder):

```sh
kubectl -n <runner namespace> create secret generic gitlab-runner-token \
  --from-literal=runner-token=glrt-... --from-literal=runner-registration-token=
```

Check both keys are there without printing the token:

```sh
kubectl -n <runner namespace> get secret gitlab-runner-token \
  -o go-template='{{range $k, $v := .data}}{{$k}} len={{len $v}}{{"\n"}}{{end}}'
```

A working runner logs `Verifying runner... is valid` and `Runner registered
successfully`.

| Symptom | Cause | Fix |
|---|---|---|
| Runner pod stuck in `ContainerCreating`, event `FailedMount ... non-existent secret key: runner-registration-token` | The empty key is missing | `kubectl -n <ns> patch secret gitlab-runner-token --type merge -p '{"data":{"runner-registration-token":""}}'` (kubelet retries the mount by itself) |
| Runner log `Verifying runner... is not valid` | Wrong or placeholder token, or the runner was deleted in GitLab | Re-create the Secret with the right token, then `kubectl -n <ns> rollout restart deploy/<release>-gitlab-runner` (the token is read only at start) |

## Things to know

- **`runners.config` is one TOML string.** Helm does not merge it with yours.
  If you already have a `runners.config`, copy these lines into it and keep
  the `namespace = ...` line, the pod label and the annotations: the router
  and the console find the jobs by them.
- **Job variables win.** A job's `variables:` replace `BUILDKIT_REMOTE_ADDR`
  and `BUILDKIT_TLS_DIR`. With the router on, the box's NetworkPolicy
  admits only the routers and the controller to the daemons, so a job that
  names the daemons' Service itself is refused.
- **From 0.2.x:** the box's jobs now go through the router. Change
  `BUILDKIT_REMOTE_ADDR` from the daemons' Service to the router's address;
  the old address is refused.
- **Set the architecture.** Without `kubernetes.io/arch` in the node selector
  the Kubernetes executor assumes amd64 for its helper image. On arm64 build
  nodes jobs then fail to pull it.
- **Use 19.4.2.** 19.4.0 lists pod events on every poll and overloaded API
  servers; 19.4.1 fixed that, and 19.4.2 is the same line rebuilt with Go
  1.26.10 for its security fixes. The fragment pins the 19.4.2 image; chart
  0.93.0 alone would run 19.4.0.
- **Kubernetes versions.** GitLab publishes no support matrix. The charts
  need Kubernetes 1.34 or newer; our checks run on 1.34 and 1.37.

## What the e2e proves

The e2e runs the real runner, from the official chart with this fragment,
against a stand-in for the few GitLab API calls a runner makes
(`test/e2e/fake-gitlab`). It shows that:

- the runner registers and passes `gitlab-runner lint`;
- job pods land on a build node, in the jobs namespace (Pod Security
  `baseline`), with the variables, the client certificate, no API token and
  the small resource tier;
- `buildctl` in the job reaches the daemon on the same node, and a second job
  gets its dependency step from cache.

That scenario points the fragment at the daemons' Service. The router path
runs in the box scenario: the box's own runner, whose configuration is this
one with the names of its release, runs a job through the router; pods
labelled and annotated as the runner labels job pods show that jobs of a
new project on two nodes meet on one daemon, that a later job goes to the
node its project built on, and that a pod in the jobs namespace cannot
reach a daemon around the router.

It does not show compatibility with a real GitLab server: git checkout,
artifacts, or pushing with the job token. The stand-in follows the runner's
API at the pinned version, so bump the runner and chart together and run the
e2e again.
