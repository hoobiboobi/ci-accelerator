FROM node:22-bookworm-slim AS base
WORKDIR /app
COPY package.json ./

FROM base AS probe
RUN echo "CACHEDIR=$(npm config get cache)"

FROM base AS install
ARG BUST=0
RUN npm install --no-audit --no-fund
