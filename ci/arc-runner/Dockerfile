# Keep ARC's immutable runner binary current through Renovate's Docker manager.
FROM ghcr.io/actions/actions-runner:2.337.0@sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4

USER root
# Go's race detector needs the C compiler and libc headers. Keep Go itself in
# setup-go so this shared CI image does not follow Iterion's release toolchain.
RUN apt-get update \
    && apt-get install --yes --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*
USER runner

LABEL org.opencontainers.image.source="https://github.com/SocialGouv/iterion" \
      org.opencontainers.image.description="ARC Actions runner with a C compiler for Go race tests"
