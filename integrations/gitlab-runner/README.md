# gitlab-runner integration

`values.yaml` here is a values fragment for the official
[gitlab-runner Helm chart](https://docs.gitlab.com/runner/install/kubernetes/).
It sends CI jobs to the buildkit daemons of `charts/buildkit` and makes the
job pods thin clients.

Tested with chart 0.93.0 and gitlab-runner 19.4.1: `make validate` renders
the chart with the fragment, and `make e2e` (scenario `runner`) runs the real
runner end to end. See "What the e2e proves" below.

## What it sets up

| Concern | How |
|---|---|
| Where the daemons are | `BUILDKIT_REMOTE_ADDR` in every job: the box's build router, `tcp://<release>-router.<namespace>.svc:1234`, which sends each job to the daemon where its project's cache is; with `charts/buildkit` alone, the daemons' Service, `tcp://<buildkit>.<namespace>.svc:1234` |
| Client certificate | The Secret cert-manager issues into the jobs namespace (buildkit chart: `tls.client.namespaces`), mounted read-only at `BUILDKIT_TLS_DIR=/certs` |
| Warm cache | Job pods are placed on the build nodes (node selector + toleration). The router (or, without it, the buildkit Service) prefers the client's node |
| Small jobs | 50m CPU / 64Mi requested, 512Mi memory limit: the build runs in the daemon |
| Isolation | Jobs run in their own namespace (`runners.jobNamespace`, Pod Security `baseline` is enough), without a Kubernetes API token |
| Least privilege | The runner gets only the permissions the Kubernetes executor needs in the jobs namespace, not the chart's default `*` on core resources |
| Console link (optional) | With `CI_ACCELERATOR_CONSOLE_URL` in `environment` (commented out in `values.yaml`), the `pre_build_script` prints the job's page in the console into the job's log |
| S3 cache (optional) | A commented `[runners.cache]` block for GitLab's `cache:` keyword in S3-compatible storage; see [docs/runner-cache.md](../../docs/runner-cache.md) for the bucket, its layout and a lifecycle rule |
| Traces | A `pre_build_script` exports `TRACEPARENT` (one trace per job, its id derived from the pipeline and job ids) and `OTEL_RESOURCE_ATTRIBUTES` (pipeline, job, project) in every job, without changes to `.gitlab-ci.yml`; job pods get `ci-accelerator.io/` annotations with the job's data from GitLab's CI variables (pipeline, project path, stage, start time, commit title and author, merge request) |

A job then builds with:

```sh
buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" build ...
```

## Use it

1. Create the jobs namespace with Pod Security `baseline` (the runner chart
   creates its Role there, and the client certificate is issued there):

   ```sh
   kubectl create namespace ci-accelerator-jobs
   kubectl label namespace ci-accelerator-jobs pod-security.kubernetes.io/enforce=baseline
   ```

2. Install the buildkit chart with the jobs namespace in
   `tls.client.namespaces`, so that the client certificate Secret exists there.
3. Create a runner in GitLab and put its authentication token in a Secret in
   the runner's namespace, as described in
   [The runner token Secret](#the-runner-token-secret).
4. Change the lines marked `ADJUST` in `values.yaml`:
   - the GitLab URL;
   - the token Secret's name (`runners.secret`);
   - the jobs namespace;
   - the buildkit Service address and client Secret name: `<fullname>` is
     `<release>-buildkit`, or the release name if it already contains
     "buildkit";
   - `kubernetes.io/arch` for arm64 build nodes.
   - Optional: the console's address (`CI_ACCELERATOR_CONSOLE_URL` in
     `environment`), so every job prints the link to its page.
   - Optional: the S3 cache. Uncomment `cache.secretName` and add the
     `[runners.cache]` block from the comments to `config`
     ([docs/runner-cache.md](../../docs/runner-cache.md)).
5. Install:

   ```sh
   helm upgrade --install gitlab-runner gitlab-runner \
     --repo https://charts.gitlab.io --version 0.93.0 \
     -n ci-accelerator -f integrations/gitlab-runner/values.yaml
   ```

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
  the `namespace = ...` line.
- **Job variables win.** A job's `variables:` replace `BUILDKIT_REMOTE_ADDR`
  and `BUILDKIT_TLS_DIR`.
- **Set the architecture.** Without `kubernetes.io/arch` in the node selector
  the Kubernetes executor assumes amd64 for its helper image. On arm64 build
  nodes jobs then fail to pull it.
- **Use 19.4.1, not 19.4.0.** 19.4.0 lists pod events on every poll and
  overloaded API servers. The fragment pins the 19.4.1 image; chart 0.93.0
  alone would run 19.4.0.
- **Kubernetes versions.** GitLab publishes no support matrix. Our checks run
  on Kubernetes 1.34 and 1.37.

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

It does not show compatibility with a real GitLab server: git checkout,
artifacts, or pushing with the job token. The stand-in follows the runner's
API at the pinned version, so bump the runner and chart together and run the
e2e again.
