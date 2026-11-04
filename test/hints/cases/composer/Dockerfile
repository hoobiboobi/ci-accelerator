FROM composer:2 AS base
WORKDIR /app
COPY composer.json ./

FROM base AS probe
RUN echo "CACHEDIR=$(composer config --global cache-dir)"

FROM base AS install
ARG BUST=0
RUN composer install --no-interaction --no-progress
