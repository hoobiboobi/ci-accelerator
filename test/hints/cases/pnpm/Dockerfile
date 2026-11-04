FROM node:22-bookworm-slim AS base
WORKDIR /app
RUN npm install -g pnpm@10.18.3
COPY package.json ./

FROM base AS probe
RUN echo "CACHEDIR=$(pnpm store path)"

FROM base AS install
ARG BUST=0
RUN pnpm install
