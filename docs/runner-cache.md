# Runner cache in S3, and runner tiers

The box's build cache lives on the BuildKit daemons. This page is about
the other cache: GitLab's `cache:` keyword, which keeps files such as
`node_modules`, Go modules or downloaded tools between jobs and pipelines.
With the Kubernetes executor that cache survives only in object storage
(S3 or a compatible store): until the box has a bucket, a job's `cache:`
is lost with the job pod, and every job prints `Failed to extract cache`.
Artifacts (`artifacts:`, results passed to later jobs of a pipeline) need
nothing from the box: the runner uploads them to GitLab.

The second part adds runner tiers: more runners next to the box's main
one, each with its own tags, job size, placement and cache.

Both are changes to `values.yaml`, applied with the install's command
and the version you run (`helm list -n ci-accelerator`):

```sh
helm upgrade ci-accelerator oci://ghcr.io/hoobiboobi/charts/ci-accelerator --version 0.5.2 \
  -n ci-accelerator -f values.yaml --wait --timeout 15m
```

## The runner cache

### Before you start: a bucket and a key

Any S3-compatible store works. Three that were checked:

- **AWS S3:** a bucket, and an IAM key that may read and write it.
  `server: s3.<region>.amazonaws.com`, `region: <region>`.
- **Hetzner Object Storage:** in the Hetzner Cloud Console, **Object
  Storage > Create bucket** in a location (`fsn1`, `nbg1`, ...), then
  **Security > S3 credentials > Generate credentials**. `server:
  <location>.your-objectstorage.com`, `region: <location>`; both path
  style and virtual-host style work.
- **A store in the cluster, for a trial:** SeaweedFS, one pod in the
  box's namespace, keys and bucket from its environment. Its data is in
  an `emptyDir`, so the cache is gone when the pod is: for production,
  give it a volume or use a real store.

  ```yaml
  # s3-trial.yaml
  apiVersion: v1
  kind: Pod
  metadata: {name: s3, namespace: ci-accelerator, labels: {app: s3}}
  spec:
    enableServiceLinks: false        # a S3_PORT variable from the Service would be read as a flag
    securityContext: {runAsNonRoot: true, runAsUser: 65534, runAsGroup: 65534, fsGroup: 65534, seccompProfile: {type: RuntimeDefault}}
    containers:
      - name: seaweedfs
        image: docker.io/chrislusf/seaweedfs:4.48@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d
        args: [mini, -dir=/data]
        env:
          - {name: AWS_ACCESS_KEY_ID, value: cacheaccess}
          - {name: AWS_SECRET_ACCESS_KEY, value: cachesecret123}
          - {name: S3_BUCKET, value: ci-cache}
        ports: [{containerPort: 8333}]
        securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, capabilities: {drop: [ALL]}}
        volumeMounts: [{name: data, mountPath: /data}, {name: tmp, mountPath: /tmp}]
    volumes: [{name: data, emptyDir: {}}, {name: tmp, emptyDir: {}}]
  ---
  apiVersion: v1
  kind: Service
  metadata: {name: s3, namespace: ci-accelerator}
  spec:
    selector: {app: s3}
    ports: [{port: 8333, targetPort: 8333}]
  ```

  `kubectl apply -f s3-trial.yaml`, then `kubectl -n ci-accelerator get
  pod s3` shows `Running`. `server: s3.ci-accelerator.svc:8333`,
  `region: us-east-1`, `insecure: true`, `pathStyle: true`.

The job pods download and upload the cache archives themselves, so they
must reach the store (the jobs namespace reaches everything but cloud
metadata by default); the controller lists the bucket for the console.

### 1. The key in a Secret

The keys must be named `accesskey` and `secretkey`; the Secret's name is
yours:

```sh
kubectl -n ci-accelerator create secret generic runner-cache \
  --from-literal=accesskey=<access key> --from-literal=secretkey=<secret key>
```

Check the key names without printing the values:

```sh
kubectl -n ci-accelerator get secret runner-cache -o go-template='{{range $k, $v := .data}}{{$k}} len={{len $v}}{{"\n"}}{{end}}'
```

`accesskey len=...` and `secretkey len=...`, nothing else.

### 2. The values

```yaml
gitlab-runner:
  runners:
    cache:
      secretName: runner-cache
      s3:
        server: s3.example.com   # host[:port], no https://; e.g. <location>.your-objectstorage.com on Hetzner
        bucket: ci-cache
        region: <region>         # e.g. the Hetzner location, or the AWS region
```

For the in-cluster trial store:

```yaml
gitlab-runner:
  runners:
    cache:
      secretName: runner-cache
      s3:
        server: s3.ci-accelerator.svc:8333
        bucket: ci-cache
        region: us-east-1
        insecure: true      # plain http
        pathStyle: true
```

`region` goes into the runner's signature: a wrong one makes the store
answer `SignatureDoesNotMatch` or a redirect, and the job prints `Failed
to extract cache` with that error. Optional keys: `s3.pathStyle` (true or
false; by default path style except on AWS), `controllerSecretName` (a
Secret with keys that may only read, for the console's listing of the
bucket: `s3:ListBucket` on the folder and `s3:GetLifecycleConfiguration`;
default: `secretName`), `path` (the folder in the bucket; default
`ci-accelerator/<GitLab id>`, where the id is the first six hex digits
of the sha256 of `gitlab-runner.gitlabUrl`, `993196` for
`https://gitlab.com/`), `maxArchiveBytes` (default 4.5 GiB; Hetzner
takes at most 5 GB per upload; a larger archive fails to upload, split
the cache by key).

### 3. Apply and check the runner

Apply with the command at the top. The main runner's pod restarts once
to read the new configuration:

```sh
kubectl -n ci-accelerator rollout status deploy/ci-accelerator-gitlab-runner
```

With `--reuse-values` instead of `-f values.yaml` the runner would keep
the configuration of the release it was installed with, without the
cache. Under Argo CD or Flux the same values go into the Application or
HelmRelease ([gitops.md](gitops.md)).

### 4. Use it in a job

A job names what to keep and a key that says when it is valid:

```yaml
install:
  tags: [ci-accelerator]
  image: node:22
  cache:
    key:
      files: [package-lock.json]     # the same cache while the lock file is unchanged
    paths: [node_modules/]
  script:
    - npm ci --prefer-offline
```

A job that only reads the cache adds `policy: pull`. Every runner of the
box uses the same folder, so jobs of a project share their caches
whichever runner or tier runs them. GitLab keeps separate caches for
protected and unprotected branches: the first run on `main` after many
merge-request pipelines still downloads nothing (project Settings >
CI/CD > General pipelines, "Use separate caches for protected
branches"). GitLab's own documentation of the keyword:
<https://docs.gitlab.com/ci/caching/>.

### 5. Check with two pipelines

Commit the job of step 4, then run the pipeline twice on the same branch
(a merge-request pipeline and a pipeline on `main` have different
caches): **Build > Pipelines > Run pipeline**, twice. The first job
prints

```
Checking cache for package-lock.json-...
Failed to extract cache         (nothing there yet)
...
Creating cache package-lock.json-...
Uploading cache.zip to https://s3.example.com/ci-cache/ci-accelerator/993196/project/<id>/package-lock.json-...
Created cache
```

and the second

```
Checking cache for package-lock.json-...
Downloading cache from https://... 
Successfully extracted cache
```

If neither pipeline prints a `Checking cache` line: the job has no
`cache:` block, or the runner did not restart (`kubectl -n
ci-accelerator logs deploy/ci-accelerator-gitlab-runner | grep -i cache`
shows an error in its configuration), or the upgrade used
`--reuse-values`. An upload that fails names the store's answer:
`AccessDenied` (the key may not write the bucket), `NoSuchBucket`,
`SignatureDoesNotMatch` (the region), `connection refused` or `x509`
(the server, the port, `insecure`).

### How it works

The runner signs a URL for each job's cache, and the job's helper container
downloads and uploads the archive itself: job pods need to reach the store
(port 443, or the store's port), the keys stay with the runner. The
archive's object key is `<path>/project/<project id>/<cache key>`, the
cache key as GitLab gives it (`-protected` or `-non_protected` is added by
GitLab).

### Old caches

The runner never deletes a cache. Give the bucket a lifecycle rule, for
example caches older than 14 days (checked with Hetzner), with the AWS
CLI and the bucket's key in the environment:

```sh
export AWS_ACCESS_KEY_ID=<access key> AWS_SECRET_ACCESS_KEY=<secret key> AWS_DEFAULT_REGION=<region>
aws --endpoint-url https://s3.example.com s3api put-bucket-lifecycle-configuration \
  --bucket ci-cache --lifecycle-configuration '{"Rules":[{"ID":"ci-cache-expiry","Status":"Enabled",
  "Filter":{"Prefix":""},"Expiration":{"Days":14},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":1}}]}'
aws --endpoint-url https://s3.example.com s3api get-bucket-lifecycle-configuration --bucket ci-cache
```

A cache that is read every day but never changes is not uploaded again, so
it still expires after the number of days: one job then finds nothing and
uploads it anew. The console's Nodes page warns while the bucket has no
such rule (the trial store has none; the warning stays).

### Rotate the keys

Re-create the Secret with the new keys, then restart the runners that
use it (they read the keys at start); the controller picks them up by
itself:

```sh
kubectl -n ci-accelerator create secret generic runner-cache \
  --from-literal=accesskey=<new> --from-literal=secretkey=<new> --dry-run=client -o yaml | kubectl apply -f -
kubectl -n ci-accelerator rollout restart deploy/ci-accelerator-gitlab-runner
```

(and `deploy/ci-accelerator-runner-<tier>` for every tier with that
cache).

### Jobs that may fail

The runner saves a cache only when the job passed, unless the job says
`cache: when: always`; it then writes nothing about the cache in the log.
A Terraform or OpenTofu plan with `-detailed-exitcode` exits with 2 when it
finds changes, so with `allow_failure: exit_codes: [2]` it "fails" every
time there is something to apply and never saves its providers:

```yaml
plan:
  script: terraform plan -detailed-exitcode -out=plan.tfplan
  allow_failure:
    exit_codes: [2]
  cache:
    key: terraform-providers
    paths: [.terraform/providers]
    when: always
```

GitLab's job log says when a failed job restored a cache; it says nothing
when the job saves none.

### In the console

The control plane's controller lists every runner's cache folder when it
starts and then every 10 minutes (`control-plane.caches.every`), so a
fresh cache shows within 10 minutes. Open a project in the console (a
build, or the project's page), then **Nodes**: under "Runner cache in
S3" it shows how much that project stores and in how many cache keys,
when the bucket was last listed, a warning when no enabled lifecycle
rule expires the objects in the runners' folder ("Old caches are never
deleted"; a rule for another prefix, for tagged objects or object sizes,
or only for delete markers does not count), and the error when the last
listing failed. The controller's log has a `cache listed` line per
folder (objects and bytes) or `cache not listed` with the reason:

```sh
kubectl -n ci-accelerator logs deploy/ci-accelerator-controller | grep 'cache '
```

The box writes the folders of the main runner and of every tier into the
ConfigMap `ci-accelerator-runner-caches` (`kubectl -n ci-accelerator get
cm ci-accelerator-runner-caches -o jsonpath='{.data.caches\.json}'`) and
gives the controller `get` on exactly the Secrets they name, nothing
else. The listing needs `s3:ListBucket` (for the folder's prefix);
reading the lifecycle rule needs `s3:GetLifecycleConfiguration` (without
it the warning is not shown). A key with only these rights, in
`controllerSecretName`, keeps the controller from writing to the bucket.
A folder with more than 200,000 objects is not listed in full (the page
shows the error).

With your own runner and `charts/control-plane` installed on its own,
name the folders in `caches.stores` (`server`, `bucket`, `region`,
`path` as the runner's `Path`, `gitlabUrl` exactly as the runner has it,
`existingSecret` with `accesskey` and `secretkey`) in the control plane's
values. Runners with `Shared = false` (the runner's default) keep their
own copy of a cache key under `runner/<token>/`: the page counts such a
key once, with the size of all its copies.

### What a cache can do

Any job of a project can read and replace that project's caches: a cache
is not a trust boundary. Do not cache secrets. Several releases may share
a bucket (each has its own folder); keys of the bucket are keys to all of
it.

## Runner tiers

A tier is another runner next to the box's main one, with its own GitLab
runner (and so its own tags), job size, placement and cache. Jobs choose a
tier by tags: `tags: [deploy]`. Each tier is its own pod (Deployment
`ci-accelerator-runner-<tier>`): a tier with a bad token or a full node
does not stop the others. Every tier's jobs get the same setup as the
main runner's: the build router's address and the client certificate,
the trace context, the job data for the console, and the jobs
namespace.

### 1. The runner in GitLab

As for the main runner ([install.md](install.md#4-create-the-runner-in-gitlab)):
Settings > CI/CD > Runners > **Create project runner** (or a group or
instance runner), tag `deploy`, **Run untagged jobs** unchecked (or
every job without tags of the project lands on this tier), **Protected**
ticked if only protected branches may deploy; copy the `glrt-...` token
before leaving the page.

### 2. The token in a Secret

Only the `runner-token` key; a tier is rendered by the box, not by the
runner chart, so the empty registration key of the main runner is not
needed (an extra key does no harm):

```sh
kubectl -n ci-accelerator create secret generic runner-deploy --from-literal=runner-token=glrt-...
```

Create it before the upgrade: when the Secret exists, the chart checks
that it has the key; a Secret that is missing is not caught at render
time and leaves the tier's pod in `CreateContainerConfigError`.

### 3. The values

```yaml
runnerTiers:
  deploy:
    token:
      existingSecret: runner-deploy
    resources:                     # each job's container; all three keys are required
      requests: {cpu: 250m, memory: 512Mi}
      limits: {memory: 2Gi}
    nodes: any                     # any: any node of the jobs' architecture (default);
                                   # build: the build nodes, next to a daemon
    concurrent: 4                  # jobs at once (default 4)
    cache:                         # optional; a tier without it has NO S3 cache,
      secretName: runner-cache     # even when the main runner has one: repeat
      s3:                          # the main runner's block to share the caches
        server: s3.example.com
        bucket: ci-cache
        region: <region>
```

- **`resources`** is the size of every job of the tier (jobs cannot raise
  it themselves). The main runner's jobs get 50m CPU, 64Mi, a 512Mi
  limit: the work happens in the daemon. A deploy tier (kubectl, helm):
  250m, 512Mi, limit 1-2Gi. A tier that runs tests or code: 1-2 CPU,
  2-4Gi.
- **`nodes`:** `any` for jobs that do not build images (deploy, tests,
  lint), which keeps the build nodes for the daemons; `build` only when
  the tier's jobs build images and should sit next to a daemon. Through
  the router a tier's build reaches the project's warm cache from any
  node. `any` needs the default routing (PreferSameNode) or Cluster;
  with `Local` the chart refuses it.
- Optional: `image` (the default job image, `alpine:3.22`), `manager`
  (`resources`, `nodeSelector`, `tolerations` of the runner pod) and
  `terminationGracePeriodSeconds` (how long a rollout waits for running
  jobs; default 3600).

### 4. Apply and check

Apply with the command at the top, then:

```sh
kubectl -n ci-accelerator get deploy ci-accelerator-runner-deploy
kubectl -n ci-accelerator logs deploy/ci-accelerator-runner-deploy | grep -i -E 'registered|valid|error'
```

One replica ready and `Verifying runner... is valid` (a tier starts
from its configuration file and does not register, so there is no
`Runner registered successfully` line); in GitLab the runner shows
**Online**. The install notes list every tier with its pod,
placement and cache.

### 5. A pipeline across tiers

The build on the main runner ([first-build.md](first-build.md): a
Dockerfile in the project, the `include:` block with its hash from the
install notes), the deploy on the tier. The component's `build-image`
job hands `IMAGE_REF` (`<repository>@sha256:...`), `IMAGE` and
`IMAGE_DIGEST` to the jobs that `needs:` it, as a dotenv report, when
the build pushed:

```yaml
include:
  - remote: https://raw.githubusercontent.com/hoobiboobi/ci-accelerator/v0.5.2/ci/gitlab/build.yml
    integrity: sha256-Th5CoYA2g9uRP3gWvlve0s/5hmho1UK04sDgK23i8rc=
    inputs:
      runner-tags: [ci-accelerator]

deploy:
  stage: deploy
  tags: [deploy]
  needs: [build-image]
  script:
    - echo "deploying $IMAGE_REF"
```

The top of the deploy job's log names the runner (`ci-accelerator-deploy`).
A job without tags goes to whichever runner takes untagged jobs; on
gitlab.com, to the shared runners.

On a rollout a tier's new runner pod starts before the old one stops; the
old one finishes its running jobs first (up to the grace period).

## When it does not work

| Symptom | Cause | Fix |
|---|---|---|
| Tier pod in `CreateContainerConfigError` or `CrashLoopBackOff` right after the upgrade | The cache Secret lacks `accesskey` or `secretkey` | Re-create the Secret with both keys |
| Tier log `Verifying runner... is not valid` | Wrong token, or the runner was deleted in GitLab | Fix the Secret, `kubectl -n ci-accelerator rollout restart deploy/ci-accelerator-runner-<tier>` |
| Tier jobs stay `Pending` | No node of the jobs' architecture without the build-node taint (`nodes: any`), or every node full | Add a node, or `nodes: build` |
| Every job prints `Failed to extract cache` | Nothing uploaded yet (the first job), the cache key differs (protected vs unprotected branches, another `key:`), a `policy: pull` job before any push, or the store refuses the key (its error follows) | Read the store's answer in the first job's log |
| The console's Nodes page shows no cache for the project | The controller lists every 10 minutes; or the key in `controllerSecretName` may not list the folder | Wait, then `kubectl -n ci-accelerator logs deploy/ci-accelerator-controller \| grep 'cache not listed'` |
