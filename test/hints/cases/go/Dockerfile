FROM golang:1.27-alpine AS base
WORKDIR /src
COPY go.mod go.sum main.go ./

FROM base AS probe
RUN echo "CACHEDIR=$(go env GOMODCACHE)" && echo "CACHEDIR=$(go env GOCACHE)"

FROM base AS install
ARG BUST=0
RUN go build -o /bin/app .
