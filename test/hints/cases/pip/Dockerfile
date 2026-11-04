FROM python:3.13-slim AS base
WORKDIR /app
COPY requirements.txt ./

FROM base AS probe
RUN echo "CACHEDIR=$(pip cache dir)"

FROM base AS install
ARG BUST=0
RUN pip install --no-cache-dir -r requirements.txt
