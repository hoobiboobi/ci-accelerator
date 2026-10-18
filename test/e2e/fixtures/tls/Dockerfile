# RUN steps share the daemon pod's network (host network mode), so they can
# reach the daemon's own port 1234. Without a client certificate they must
# not get in. Built FROM the daemon image, which has buildctl.
ARG BUILDKIT_IMAGE
FROM ${BUILDKIT_IMAGE}
ARG BUST
RUN echo "$BUST" && \
    if nc -z -w 2 127.0.0.1 1234; then echo PORT_OPEN; else echo PORT_CLOSED; fi && \
    if buildctl --addr tcp://127.0.0.1:1234 --timeout 5 debug workers >/tmp/out 2>&1; \
    then echo DAEMON_OPEN; else echo DAEMON_REFUSED; fi && \
    sed 's/^/  buildctl: /' /tmp/out | tail -3
