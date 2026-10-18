# Three unique 400 MB layers: more than the e2e cap of 1GB (1 GiB), so GC has
# to delete cache records after the build.
FROM alpine:3.22
ARG BUST
RUN echo "$BUST" > /bust && dd if=/dev/zero of=/a bs=1M count=400 2>/dev/null
RUN dd if=/dev/zero of=/b bs=1M count=400 2>/dev/null
RUN dd if=/dev/zero of=/c bs=1M count=400 2>/dev/null
