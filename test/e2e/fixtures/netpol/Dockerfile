# RUN steps share the daemon pod's network, so the daemon's NetworkPolicy is
# what they can reach: DNS and the internet yes, in-cluster pods no. TARGET
# is a pod IP that answers on 8080 to anyone not restricted.
FROM alpine:3.22
ARG BUST
ARG TARGET
RUN echo "$BUST" && \
    if nslookup kubernetes.default.svc.cluster.local >/dev/null 2>&1; then echo DNS_OK; else echo DNS_FAIL; fi && \
    if nc -z -w 5 registry-1.docker.io 443; then echo WORLD_OK; else echo WORLD_BLOCKED; fi && \
    if nc -z -w 5 "$TARGET" 8080; then echo CLUSTER_OPEN; else echo CLUSTER_BLOCKED; fi
