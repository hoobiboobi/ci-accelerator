FROM alpine:3.22 AS base

FROM base AS probe
RUN echo "CACHEDIR=/etc/apk/cache"

FROM base AS install
ARG BUST=0
RUN apk add --no-cache git curl
