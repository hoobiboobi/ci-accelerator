FROM denoland/deno:latest AS base
WORKDIR /app
COPY main.ts ./

FROM base AS probe
RUN echo "CACHEDIR=$DENO_DIR" && deno info | head -3

FROM base AS install
ARG BUST=0
RUN deno cache main.ts
