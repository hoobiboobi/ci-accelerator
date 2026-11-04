FROM dart:stable AS base
WORKDIR /app
COPY pubspec.yaml ./

FROM base AS probe
RUN echo "CACHEDIR=${PUB_CACHE:-$HOME/.pub-cache}"

FROM base AS install
ARG BUST=0
RUN dart pub get
