FROM mambaorg/micromamba:latest AS base

FROM base AS probe
RUN echo "MAMBA_ROOT_PREFIX=$MAMBA_ROOT_PREFIX" && micromamba info | grep -i 'package cache' && echo "CACHEDIR=$MAMBA_ROOT_PREFIX/pkgs"

FROM base AS install
ARG BUST=0
RUN micromamba install -y -n base -c conda-forge zlib xz
