# The open-files limit a RUN step gets (the chart caps the daemon's).
FROM alpine:3.22
ARG BUST
RUN echo "NOFILE $(ulimit -n) $(ulimit -Hn)" && echo "$BUST" >/dev/null
