# The checks of the ci-accelerator sources as builds on a ci-accelerator box:
# the project's showcase pipeline (.gitlab-ci.yml) builds the targets test
# and vet next to the image. Their first steps are the image's own
# (cmd/ci-accelerator/Dockerfile, the same lines): jobs running at once
# share them on the daemon, and the Go caches live in cache mounts.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS src
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY internal internal

FROM src AS test
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go test ./...

FROM src AS vet
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go vet ./...
