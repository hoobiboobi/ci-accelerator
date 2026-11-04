FROM maven:3.9-eclipse-temurin-21 AS base
WORKDIR /app
COPY pom.xml ./

FROM base AS probe
RUN echo "CACHEDIR=$(mvn -q help:evaluate -Dexpression=settings.localRepository -DforceStdout)"

FROM base AS install
ARG BUST=0
RUN mvn -B -q dependency:go-offline
