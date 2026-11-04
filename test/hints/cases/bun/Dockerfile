FROM oven/bun:1 AS base
WORKDIR /app
COPY package.json ./

FROM base AS probe
RUN echo "CACHEDIR=$(bun pm cache)"

FROM base AS install
ARG BUST=0
RUN bun install
