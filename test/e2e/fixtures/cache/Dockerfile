# Layer cache and cache mount persistence.
FROM alpine:3.22

# "Dependencies": must be CACHED on every build after the first.
RUN sleep 3 && echo deps > /deps

# Changes on every build. The cache mount content must survive between
# builds (and, with hostPath, daemon restarts).
ARG BUST=0
RUN --mount=type=cache,target=/cache,id=e2e-cache \
    echo "bust=$BUST" && \
    if [ -f /cache/marker ]; then echo CACHE_MOUNT_WARM; else echo CACHE_MOUNT_COLD; date > /cache/marker; fi
