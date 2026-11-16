# The checks of the ci-accelerator sources as builds on a ci-accelerator box:
# the project's showcase pipeline (.gitlab-ci.yml) builds the targets test
# and vet next to the image. Their first steps are the image's own
# (cmd/ci-accelerator/Dockerfile, the same lines): jobs running at once
# share them on the daemon, and the Go caches live in cache mounts.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS src
ARG TARGETOS TARGETARCH
ARG VERSION=dev COMMIT=
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY internal internal

FROM src AS test
# The API test holds the responses to the contract (src stays the image's).
COPY docs/api docs/api
# The fleet test compares the values it generates with this golden file.
COPY charts/ci-accelerator/tests/values/fleet-member.yaml charts/ci-accelerator/tests/values/
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go test ./...

FROM src AS vet
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go vet ./...
