# The first build

After the install ([install.md](install.md) or [quickstart.md](quickstart.md)),
this page adds one CI job to a GitLab project: it builds the project's
`Dockerfile` on the daemons and pushes the image to the project's
container registry as `$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA`. Then it
runs the job twice, so you see a cold build and a cached one, and finds
both in the console.

## Before you push

- **The runner is online.** In the project: **Settings > CI/CD >
  Runners**; the runner's row has a green dot and says **Online**. Or:
  `kubectl -n ci-accelerator logs deploy/ci-accelerator-gitlab-runner | grep -i registered`
  prints `Runner registered successfully` (on the one-machine install,
  `sudo k3s kubectl ...`).
- **The project is one the runner serves.** A project runner serves the
  project it was created in; a group runner every project of its group.
- **The project's container registry is on.** On gitlab.com it is. On
  your own GitLab: **Settings > General > Visibility, project features,
  permissions**, the **Container registry** toggle. Without a registry
  the job stops with `this GitLab has no container registry`; see
  [the component's README](../ci/gitlab/README.md) for `push: false` or
  another registry.
- **GitLab 17.9 or newer** for the `integrity:` line below (**Help**, the
  `?` in the top bar, shows the version). On 17.0 to 17.8, leave that
  line out.
- **Your GitLab server can reach GitHub.** It fetches the job's
  definition from `raw.githubusercontent.com` when the pipeline starts.
  If it cannot, copy `build.yml` into a project of your GitLab and use
  `include: project` ([the component's README](../ci/gitlab/README.md#use-it)).
- **A `Dockerfile` at the repository root.** Elsewhere: the `dockerfile`
  input, below.

## 1. Keep `.git` out of the build context

Create `.dockerignore` next to the `Dockerfile`, with at least:

```
.git
```

Every job clones the repository afresh, so `.git` differs in every
pipeline; with it in the context, every step after `COPY . .` runs again
every time. Add build output too (`node_modules`, `dist`, ...). Do this
before the first push: the second run below shows the cache only then.

## 2. Write `.gitlab-ci.yml`

For a project without a `.gitlab-ci.yml`, the whole file is:

```yaml
include:
  - remote: https://raw.githubusercontent.com/hoobiboobi/ci-accelerator/v0.5.2/ci/gitlab/build.yml
    integrity: sha256-Th5CoYA2g9uRP3gWvlve0s/5hmho1UK04sDgK23i8rc=
    inputs:
      runner-tags: [ci-accelerator]
```

- **The version and the hash belong together** and must match the box
  (`helm list -n ci-accelerator` shows its version in the CHART column).
  The install notes print this block with the right hash:
  `helm get notes ci-accelerator -n ci-accelerator`; the release page
  (<https://github.com/hoobiboobi/ci-accelerator/releases/tag/v0.5.2>)
  prints it too. The hash above is the one of 0.5.2.
- **`runner-tags`** lists the tag you gave the runner in GitLab
  (`ci-accelerator` in [install.md](install.md#4-create-the-runner-in-gitlab));
  the runner's row under Settings > CI/CD > Runners shows its tags. It is
  a YAML list even for one tag. Leave the line out only if the runner
  runs untagged jobs and the project has no other runners.
- **The job is `build-image` in stage `build`.** A `.gitlab-ci.yml` that
  already has `stages:` must list `build` (or set the `stage` input to
  one of its stages), or GitLab refuses the pipeline with `chosen stage
  build does not exist`.
- **A Dockerfile elsewhere:** `dockerfile: docker/Dockerfile` under
  `inputs`, relative to the repository root; the context stays the
  repository. Several images, build arguments, secrets, another context,
  merge requests that build without pushing, large repositories: [the
  component's README](../ci/gitlab/README.md).

## 3. Push and watch the job

Commit both files and push. In GitLab, **Build > Pipelines** shows a new
pipeline with one job, `build-image`; it starts within seconds (the
runner asks GitLab for jobs every few seconds) and the first run takes
about a minute: the job pod pulls its image once, then sends the context
to a daemon, which builds and pushes. The end of the job's log:

```
#8 exporting to image
#8 pushing manifest for registry.gitlab.com/<group>/<project>:<short sha>@sha256:... done
Dockerfile steps: 0 from cache, 2 run; context sent: 13.99kB
digest sha256:...
Job succeeded
```

`0 from cache` is right for a first build: the daemon has never seen
this project. The image is in the project under **Deploy > Container
Registry**, tagged with the commit's short SHA.

## 4. Run it again

Push a small change (a line in the README), or open the finished job and
select **Retry**. This time the log says, for the Dockerfile above,

```
Dockerfile steps: 2 from cache, 0 run; context sent: 13.99kB
```

and the job takes seconds. A change to the Dockerfile, or to a file a
`COPY` step takes, runs that step and everything after it; the steps
before it stay cached.

## 5. Find the build in the console

```sh
kubectl -n ci-accelerator port-forward svc/ci-accelerator-web 8080
```

Open <http://localhost:8080/>. The home page lists the pipelines, newest
first; the pipeline opens into its jobs, the job into its build: every
Dockerfile step with its time, which steps came from the cache, and one
of five verdicts (from cache, your change, build farm, unclear, failed).
The second pipeline's build says "from cache"; the first says why it was
not. Until the console has a GitLab token, a job's result shows as
"unknown" and no runner waits are drawn: [console.md](console.md#setting-up-the-console)
adds them. With `global.console.url` set to the console's address, every
job's log prints a link to its page.

## When it does not work

**The job stays pending.** In this order:

1. Settings > CI/CD > Runners: the runner is **Online**. If not: the
   runner check in [install.md](install.md#8-check-the-install).
2. The job's tags equal the runner's tags, or the runner runs untagged
   jobs. GitLab's own message is "This job is stuck because you don't
   have any active runners that can run this job".
3. The runner is not **Protected** while the branch is not, and not
   **Paused**.
4. `kubectl -n ci-accelerator-jobs get pods`: a job pod `Pending` cannot
   be scheduled on a build node; `ImagePullBackOff` means the build node
   cannot pull `moby/buildkit` from Docker Hub (mirror it and set the
   `buildkit-image` input).

**The job fails at once with `BUILDKIT_REMOTE_ADDR/BUILDKIT_TLS_DIR are
not set: this job must run on a ci-accelerator runner (check
runner-tags)`.** Another runner took the job: a shared runner of
gitlab.com, or one of your own. Set `runner-tags` to the box's runner's
tag, or disable the other runners for the project (Settings > CI/CD >
Runners).

**`no Dockerfile at ./Dockerfile`:** the file is not at the repository
root; set the `dockerfile` input.

**`this GitLab has no container registry`:** see "Before you push".

**`chosen stage build does not exist`:** see step 2.

**buildctl cannot connect** (`tls: ...`, `i/o timeout`, `connection
refused`): the job reached the box's runner but not a daemon;
[troubleshooting.md](troubleshooting.md#a-job-cannot-reach-the-daemons)
has the errors and their causes.

**Docker Hub refuses the base image (`toomanyrequests`).** The build
node pulls base images anonymously. Use GitLab's Dependency Proxy
(`FROM ${CI_DEPENDENCY_PROXY_GROUP_IMAGE_PREFIX}/alpine:3.22`, which the
job logs in to; it needs a group) or a registry mirror
(`buildkit.config.extraConfig`).

## Without the component

The component is one job script around `buildctl`. To call it yourself,
in the `moby/buildkit` image:

```sh
buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" build \
  --frontend dockerfile.v0 --local context=. --local dockerfile=. \
  --output type=image,name=$CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA,push=true
```

The runner gives every job `BUILDKIT_REMOTE_ADDR` and the client
certificate in `BUILDKIT_TLS_DIR`. To push, write the registry
credentials to `$DOCKER_CONFIG/config.json` and run `unset
DOCKER_AUTH_CONFIG` first: buildctl reads that variable too, and a
read-only entry in it overrides the job's credentials. Add
`--opt "ci-accelerator.io/traceparent=$TRACEPARENT"` and the console
shows the build live on the job's page.
