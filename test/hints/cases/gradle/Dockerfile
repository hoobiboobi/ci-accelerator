FROM gradle:8-jdk21 AS base
WORKDIR /app
COPY build.gradle settings.gradle ./

FROM base AS probe
RUN echo "USER=$(id -un) HOME=$HOME GRADLE_USER_HOME=$GRADLE_USER_HOME" && echo "CACHEDIR=${GRADLE_USER_HOME:-$HOME/.gradle}/caches"

FROM base AS install
ARG BUST=0
RUN gradle --no-daemon -q dependencies --configuration runtimeClasspath
