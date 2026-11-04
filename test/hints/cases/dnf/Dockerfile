FROM fedora:43 AS base

FROM base AS probe
RUN echo "CACHEDIR=/var/cache/dnf" && echo "CACHEDIR=/var/cache/libdnf5" && dnf --version | head -1

FROM base AS install
ARG BUST=0
RUN dnf install -y tree jq
