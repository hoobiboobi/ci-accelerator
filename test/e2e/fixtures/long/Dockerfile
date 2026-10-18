# A build that is still running when its daemon gets SIGTERM. 45 s: longer
# than the Kubernetes default grace (30 s) after the SIGTERM at ~8 s.
FROM alpine:3.22
ARG BUST
RUN echo "start $BUST" && sleep 45 && echo finished
