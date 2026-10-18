# Writing Dockerfiles for a shared cache

The daemons keep their cache between jobs, so a well-ordered Dockerfile
rebuilds only what changed. This guide covers what decides whether a step
comes from cache, and the habits that keep it there. Builds here run with:

```sh
buildctl --addr "$BUILDKIT_REMOTE_ADDR" --tlsdir "$BUILDKIT_TLS_DIR" \
  build --frontend dockerfile.v0 --local context=. --local dockerfile=. --progress plain
```

## How to see what was cached

With `--progress plain`, every step prints its header, then either
`CACHED` or its output and `DONE`:

```
#6 [2/5] RUN npm ci
#6 CACHED
#8 [4/5] COPY . .
#8 DONE 0.1s
```

Where the first `DONE` appears is where the cache stopped working. Every
step after it ran again. Remember that the cache belongs to one daemon:
check which daemon served the job (`buildctl ... debug workers -v | grep
hostname`) before blaming the Dockerfile.

## 1. Keep the build context stable

A `COPY` step is cached when the files it copies are unchanged. In CI,
every job starts from a fresh clone, so anything in the context that
differs between clones invalidates every step after the `COPY`.

Add a `.dockerignore` next to the Dockerfile:

```
.git
node_modules
dist
build
coverage
*.log
```

`.git` matters most: a fresh clone has new object files and timestamps
every time. On a test project, adding `.git` to `.dockerignore` turned
`COPY . .` and every step after it from "always rebuilt" into "always
cached".

`.dockerignore` also makes the upload smaller: the job sends the context
to the daemon over the network. The plain progress line
`transferring context: 12.3MB ... done` shows its size.

## 2. Order steps from least to most often changed

Dependencies change rarely; source code changes in every commit. Copy the
dependency manifests first, install, and only then copy the source:

```dockerfile
FROM node:22-alpine
WORKDIR /app

# Changes only when dependencies change: cached in most pipelines.
COPY package.json package-lock.json ./
RUN npm ci

# Changes with every commit: only the steps from here on run again.
COPY . .
RUN npm run build
```

The same shape works for every ecosystem:

| Ecosystem | Copy first | Then |
|---|---|---|
| Node | `package.json`, lock file | `npm ci` / `yarn install --frozen-lockfile` / `pnpm install --frozen-lockfile` |
| Go | `go.mod`, `go.sum` | `go mod download` |
| Python | `requirements*.txt` / `pyproject.toml` + lock file | `pip install -r ...` |
| Java (Maven) | `pom.xml` | `mvn dependency:go-offline` |
| Rust | `Cargo.toml`, `Cargo.lock` | a dependency-only build |

## 3. Use cache mounts for package managers

A layer is all or nothing: if the lock file changes, `npm ci` runs again
from an empty directory. A cache mount keeps the package manager's own
download cache between builds, even when the layer is rebuilt:

```dockerfile
RUN --mount=type=cache,target=/root/.npm npm ci
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod go build ./...
RUN --mount=type=cache,target=/root/.cache/pip pip install -r requirements.txt
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends curl
```

What to know about cache mounts here:

- **They live on the daemon.** Like the layer cache, a cache mount belongs
  to the daemon that ran the build.
- **They are kept for 14 days after last use.** BuildKit's default is
  48 h, which loses package caches over a weekend.
- **They are shared by every build on the daemon** that uses the same
  mount id (by default the target path). That is what makes them fast,
  and it also means one project's build can read and change what another
  project left there. One installation is one trust domain; use separate
  installations for builds that must not share.
- **Use `sharing=locked`** for tools that cannot handle concurrent use of
  their cache, such as apt. The default `shared` lets concurrent builds
  use the mount at once.
- **Cache mounts are not part of the image.** Nothing in them ends up in
  the pushed layers.

## 4. Be careful with build arguments

A build argument is part of the cache key of every `RUN` step after the
`ARG` line in its stage. A value that changes in every pipeline
(`CI_JOB_ID`, `CI_COMMIT_SHA`, a timestamp) therefore rebuilds all of
those steps:

```dockerfile
# Bad: every pipeline reinstalls dependencies.
ARG VERSION
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
RUN npm run build -- --version "$VERSION"
```

```dockerfile
# Good: declare it just before the step that needs it.
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
ARG VERSION
RUN npm run build -- --version "$VERSION"
```

Pass build arguments with `--opt build-arg:VERSION=$CI_COMMIT_SHORT_SHA`.

## 5. Never pass secrets as build arguments

Build arguments are visible in the image history, and the `buildctl`
command line, including `--opt build-arg:...` values, is recorded in this
installation's traces, which every build can read. Use secret mounts
instead. They are available to one `RUN` step and are not stored in the
image or in the cache:

```dockerfile
RUN --mount=type=secret,id=npmrc,target=/root/.npmrc npm ci
```

```sh
buildctl ... build ... --secret id=npmrc,src="$NPMRC_FILE"
```

Here `NPMRC_FILE` is a GitLab CI/CD variable of type *File*. Because the
secret's content is not part of the cache key, changing a token does not
rebuild the step that uses it.

## 6. Pin base images by digest

`FROM node:22-alpine` is resolved on every build. When the tag moves to a
new image, everything after `FROM` rebuilds. That is correct, but it
happens at a time you do not choose. Pin the digest and update it on
purpose, for example with Renovate:

```dockerfile
FROM node:22-alpine@sha256:<digest>
```

## 7. Let parallel jobs share their common steps

BuildKit runs identical steps of concurrent builds once. Several jobs of
one pipeline that build from the same base and install the same
dependencies on the same daemon wait for a single run of those steps.
Ten parallel jobs with a common `yarn install` run it once instead of ten
times.

To get the most out of it:
- keep the shared steps identical: same base image, same files copied
  before them, same arguments;
- put job-specific steps last.

Deduplication happens within a daemon, so jobs on different build nodes
still run the shared steps once per node.

## 8. Multi-stage builds and targets

Separate build tools from the runtime image with stages. Build one stage
with `--opt target=<stage>`. Stages that the target does not need are
skipped, and cached stages are reused across targets:

```dockerfile
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -o /out/app ./cmd/app

FROM gcr.io/distroless/static AS runtime
COPY --from=build /out/app /app
ENTRYPOINT ["/app"]
```

```sh
buildctl ... build ... --opt target=runtime
```

A Dockerfile with another name or in another directory:

```sh
buildctl ... build --local context=. --local dockerfile=docker --opt filename=Dockerfile.prod
```

## Checklist

- [ ] `.dockerignore` excludes `.git`, dependency directories and build
      output.
- [ ] Dependency manifests are copied and installed before the source.
- [ ] Package managers use cache mounts (`sharing=locked` for apt).
- [ ] `ARG`s with per-pipeline values come right before the step that
      needs them.
- [ ] Secrets use `--mount=type=secret`, never build arguments.
- [ ] Base images are pinned by digest.
- [ ] `--progress plain` shows `CACHED` for everything before the first
      changed file.
