FROM node:22-bookworm-slim AS base
USER node
WORKDIR /home/node/app
COPY --chown=node package.json ./

FROM base AS probe
RUN echo "HOME=$HOME" && echo "CACHEDIR=$(npm config get cache)"

FROM base AS install
ARG BUST=0
RUN npm install --no-audit --no-fund
