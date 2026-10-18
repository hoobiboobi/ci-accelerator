# RUN steps share the daemon pod's network (host network mode): the
# unauthenticated debug server (:6060) must not be listening.
FROM alpine:3.22
ARG BUST
RUN echo "$BUST" && if nc -z -w 2 127.0.0.1 6060; then echo DEBUG_OPEN; else echo DEBUG_CLOSED; fi
