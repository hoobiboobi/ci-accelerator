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
# /licenses/<module path>/: the module's own and those of the folders on
# the way from each linked package up to the module's root (forked or
# vendored code keeps its licence there), at the same relative paths; Go's
# own (the standard library and runtime are in every Go binary) under
# /licenses/go/; and the project's own. /licenses/README lists the
# modules, their versions and where their source is (MPL-2.0 asks that its
# recipients are told), naming the MPL-2.0 ones.
COPY LICENSE /out/licenses/ci-accelerator/LICENSE
RUN --mount=type=cache,target=/go/pkg/mod \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' ./cmd/ci-accelerator \
    | sort -u > /tmp/modules \
    && GOOS=$TARGETOS GOARCH=$TARGETARCH go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Dir}} {{$.Dir}}{{end}}{{end}}' ./cmd/ci-accelerator \
    | sort -u > /tmp/packages \
    && while read -r path moddir dir; do \
        while :; do \
            for f in "$dir"/LICENSE* "$dir"/LICENCE* "$dir"/COPYING* "$dir"/NOTICE* "$dir"/*-LICENSE*; do \
                [ ! -f "$f" ] || { to="/out/licenses/$path${dir#"$moddir"}"; mkdir -p "$to"; cp "$f" "$to/"; }; \
            done; \
            case "$dir" in "$moddir"/*) dir=$(dirname "$dir") ;; *) break ;; esac; \
        done; \
    done < /tmp/packages \
    && while read -r path version dir; do \
        [ -n "$(find "/out/licenses/$path" -maxdepth 1 -type f 2>/dev/null)" ] || { echo "no licence file in $path ($dir)" >&2; exit 1; }; \
    done < /tmp/modules \
    && mkdir -p /out/licenses/go && cp "$(go env GOROOT)/LICENSE" /out/licenses/go/LICENSE \
    && { echo "/ci-accelerator is Apache-2.0 (ci-accelerator/LICENSE). It links the Go modules below, unmodified;"; \
         echo "each one's licence and notices are in the directory of its path. The source of each, at the"; \
         echo "version listed, is published by the Go module proxy: https://proxy.golang.org/<path>/@v/<version>.zip"; \
         echo "(also: go mod download <path>@<version>)."; \
         echo "It also links the Go standard library and runtime of $(go env GOVERSION): its licence is go/LICENSE."; \
         mpl=$(grep -rlE --include=LICENSE --include=LICENSE.txt --include=LICENSE.md 'Mozilla Public License,? [Vv]ersion 2\.0' /out/licenses | sed -e 's|^/out/licenses/||' -e 's|/[^/]*$||' | sort -u); \
         [ -z "$mpl" ] || { echo; echo "Under MPL-2.0 (its source as above):"; for m in $mpl; do grep "^$m " /tmp/modules | cut -d' ' -f1,2; done; }; \
         echo; echo "Modules (path version):"; cut -d' ' -f1,2 /tmp/modules; } > /out/licenses/README

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /out/ci-accelerator /ci-accelerator
COPY --from=build /out/licenses /licenses
USER 65532:65532
ENTRYPOINT ["/ci-accelerator"]
