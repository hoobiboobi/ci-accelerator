FROM opensuse/leap:15.6 AS base

FROM base AS probe
RUN echo "CACHEDIR=/var/cache/zypp"

FROM base AS install
ARG BUST=0
RUN zypper --non-interactive install tree jq
