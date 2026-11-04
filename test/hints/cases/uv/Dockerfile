FROM ghcr.io/astral-sh/uv:python3.13-bookworm-slim AS base
WORKDIR /app
COPY requirements.txt ./

FROM base AS probe
RUN echo "CACHEDIR=$(uv cache dir)"

FROM base AS install
ARG BUST=0
RUN uv pip install --system -r requirements.txt
