FROM rust:1-slim AS base
WORKDIR /app
COPY Cargo.toml ./
RUN mkdir src && echo 'fn main() { println!("{}", itoa::Buffer::new().format(1)); }' > src/main.rs

FROM base AS probe
RUN echo "CACHEDIR=$CARGO_HOME/registry" && echo "CACHEDIR=$CARGO_HOME/git"

FROM base AS install
ARG BUST=0
RUN cargo build --release
