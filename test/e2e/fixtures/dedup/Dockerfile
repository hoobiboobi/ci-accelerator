# Deduplication of identical steps across concurrent builds.
#
# Build `main` twice in parallel with the same SEED: the RUN step has the
# same vertex digest in both, so buildkit must execute it once. Every
# execution appends a line to a shared cache mount; build `read` (with a
# fresh READ_BUST) prints the log so the executions can be counted.

FROM alpine:3.22 AS read
ARG READ_BUST
RUN --mount=type=cache,target=/count,id=e2e-dedup,sharing=shared \
    echo "read $READ_BUST" && cat /count/log

FROM alpine:3.22 AS main
ARG SEED
RUN --mount=type=cache,target=/count,id=e2e-dedup,sharing=shared \
    echo "exec seed=$SEED" >> /count/log && sleep 8
ARG WHO
RUN echo "build $WHO"
