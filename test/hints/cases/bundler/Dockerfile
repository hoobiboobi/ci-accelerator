FROM ruby:3.4-slim AS base
WORKDIR /app
COPY Gemfile ./

FROM base AS probe
RUN echo "CACHEDIR=$(gem env gemdir)/cache"

FROM base AS install
ARG BUST=0
RUN bundle install
