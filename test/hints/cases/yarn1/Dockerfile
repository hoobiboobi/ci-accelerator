FROM node:22-bookworm-slim AS base
WORKDIR /app
COPY package.json ./

FROM base AS probe
RUN echo "CACHEDIR=$(yarn cache dir)"

FROM base AS install
ARG BUST=0
RUN yarn install --non-interactive
