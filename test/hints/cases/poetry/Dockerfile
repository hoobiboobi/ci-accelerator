FROM python:3.13-slim AS base
RUN pip install --no-cache-dir poetry==2.2.1
WORKDIR /app
COPY pyproject.toml ./

FROM base AS probe
RUN echo "CACHEDIR=$(poetry config cache-dir)/cache" && echo "CACHEDIR=$(poetry config cache-dir)/artifacts"

FROM base AS install
ARG BUST=0
RUN poetry install --no-root
