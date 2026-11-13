# The ci-accelerator image: the control plane's roles (controller, web) and
# the daemons' disk probe. Build from the repository root:
#   docker buildx build -f cmd/ci-accelerator/Dockerfile .
# Pure Go (no cgo): one static binary on a distroless base, non-root.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS build
ARG TARGETOS TARGETARCH
# The version the binary reports: the image tag (v<appVersion> for a
# release) and the commit it was built from.
ARG VERSION=dev COMMIT=
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd cmd
COPY internal internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -buildid= -X main.version=$VERSION -X main.commit=$COMMIT" -o /out/ci-accelerator ./cmd/ci-accelerator
# The licences and notices of every module linked into the binary (the
# Apache-2.0 ones ask for their NOTICE files to go with it), under
# /licenses/<module path>/, and the project's own.
COPY LICENSE /out/licenses/ci-accelerator/LICENSE
RUN --mount=type=cache,target=/go/pkg/mod \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}}{{end}}{{end}}' ./cmd/ci-accelerator \
    | sort -u | while read -r path dir; do \
        mkdir -p "/out/licenses/$path"; \
        for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE*; do [ ! -f "$f" ] || cp "$f" "/out/licenses/$path/"; done; \
        [ -n "$(ls "/out/licenses/$path")" ] || { echo "no licence file in $path ($dir)" >&2; exit 1; }; \
    done

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/ci-accelerator /ci-accelerator
COPY --from=build /out/licenses /licenses
USER 65532:65532
ENTRYPOINT ["/ci-accelerator"]
