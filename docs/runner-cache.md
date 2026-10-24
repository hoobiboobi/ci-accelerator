# Runner cache in S3, and runner tiers

The box's build cache lives on the BuildKit daemons. This page is about the
other cache: GitLab's `cache:` keyword, which keeps files such as
`node_modules`, Go modules or downloaded tools between jobs and pipelines.
With the Kubernetes executor that cache survives only in object storage
(S3 or a compatible store): without it, `cache:` is lost with each job pod.

Artifacts (`artifacts:`, results passed to later jobs of a pipeline) need
nothing from the box: the runner uploads them to GitLab.

## Turn it on

1. A bucket, and a key that can read and write it. Any S3-compatible store
   works: AWS S3, Hetzner Object Storage, MinIO, Ceph. Checked with
   Hetzner Object Storage (path style and virtual host).
2. A Secret with the key, in the release namespace:

   ```sh
   kubectl -n ci-accelerator create secret generic runner-cache \
     --from-literal=accesskey=<access key> --from-literal=secretkey=<secret key>
   ```

3. Values:

   ```yaml
   gitlab-runner:
     runners:
       cache:
         secretName: runner-cache
         s3:
           server: s3.example.com   # host[:port], no https://
           bucket: ci-cache
           region: eu-1
   ```

   Optional: `s3.insecure: true` (plain http, e.g. MinIO in the cluster),
   `s3.pathStyle` (true or false; by default path style except on AWS),
   `path` (the folder; default `<release>/<GitLab id>`), `maxArchiveBytes`
   (default 4.5 GiB; Hetzner takes at most 5 GB per upload).

4. `helm upgrade` with `--reset-then-reuse-values` (with `--reuse-values`
   the main runner keeps the runner configuration of the release it was
   installed with).

The main runner restarts once to read the new configuration. Jobs then
print `Checking cache for …`, `Downloading cache …` with the size, and
`Successfully extracted cache` or `Failed to extract cache` (nothing there
yet).

### How it works

The runner signs a URL for each job's cache, and the job's helper container
downloads and uploads the archive itself: job pods need to reach the store
(port 443, or the store's port), the keys stay with the runner. The
archive's object key is `<path>/project/<project id>/<cache key>`, the
cache key as GitLab gives it (`-protected` or `-non_protected` is added by
GitLab). Every runner of a release uses the same folder, so jobs of a
project share their caches whichever tier runs them.

### Old caches

The runner never deletes a cache. Give the bucket a lifecycle rule, for
example caches older than 14 days (checked with Hetzner):

```sh
aws --endpoint-url https://s3.example.com s3api put-bucket-lifecycle-configuration \
  --bucket ci-cache --lifecycle-configuration '{"Rules":[{"ID":"ci-cache-expiry","Status":"Enabled",
  "Filter":{"Prefix":""},"Expiration":{"Days":14},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":1}}]}'
```

A cache that is read every day but never changes is not uploaded again, so
it still expires after the number of days: one job then finds nothing and
uploads it anew.

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

### What a cache can do

Any job of a project can read and replace that project's caches: a cache
is not a trust boundary. Do not cache secrets. Several releases may share
a bucket (each has its own folder); keys of the bucket are keys to all of
it.

## Runner tiers

A tier is another runner next to the box's main one, with its own GitLab
runner (and so its own tags), job size, placement and cache. Jobs choose a
tier by tags: `tags: [deploy]`. Each tier is its own pod: a tier with a bad
token or a full node does not stop the others.

1. Create the runner in GitLab: Settings > CI/CD > Runners > New project
   runner (or a group or instance runner), with the tags its jobs will use.
   GitLab shows the runner authentication token (`glrt-…`).
2. Put it into a Secret:

   ```sh
   kubectl -n ci-accelerator create secret generic runner-deploy --from-literal=runner-token=glrt-…
   ```

3. Values:

   ```yaml
   runnerTiers:
     deploy:
       token:
         existingSecret: runner-deploy
       resources:                     # each job's container
         requests: {cpu: 250m, memory: 512Mi}
         limits: {memory: 2Gi}
       nodes: any                     # any: any node of the jobs' architecture;
                                      # build: the build nodes, next to a daemon
       concurrent: 4                  # jobs at once (default 4)
       cache:                         # optional, as gitlab-runner.runners.cache
         secretName: runner-cache
         s3: {server: s3.example.com, bucket: ci-cache, region: eu-1}
   ```

   Optional: `image` (the default job image, `alpine:3.22`), `manager`
   (`resources`, `nodeSelector`, `tolerations` of the runner pod) and
   `terminationGracePeriodSeconds` (how long a rollout waits for running
   jobs; default 3600).

Every tier's jobs get the same setup as the main runner's: the daemons'
address and client certificate (a tier on `nodes: any` reaches a daemon on
another node, with that node's cache), the trace context, the job data for
the console, and the jobs namespace. A job's size is the tier's: jobs
cannot raise it themselves.

On a rollout a tier's new runner pod starts before the old one stops; the
old one finishes its running jobs first (up to the grace period).
