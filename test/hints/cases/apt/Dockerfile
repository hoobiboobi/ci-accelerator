FROM debian:13-slim AS base

FROM base AS probe
RUN echo "CACHEDIR=/var/cache/apt" && echo "CACHEDIR=/var/lib/apt" && ls /etc/apt/apt.conf.d/

FROM base AS install
ARG BUST=0
RUN apt-get update && apt-get install -y --no-install-recommends curl git
