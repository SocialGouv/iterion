# Keep ARC's immutable runner binary current through Renovate's Docker manager.
FROM ghcr.io/actions/actions-runner:2.337.0@sha256:e5496277be5d09bc968b3d64911b74e219ac4a3f2edce956a3ecf9271bea1ef4

USER root
# Go's race detector needs the C compiler and libc headers.
RUN apt-get update \
    && apt-get install --yes --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

# Toolchains in the layout actions/setup-go and actions/setup-node read before
# they download: <tool cache>/<tool>/<version>/x64 plus an x64.complete marker.
# The runner takes its tool cache from RUNNER_TOOL_CACHE; its default,
# /home/runner/_work/_tool, sits under the volume ARC mounts over _work and
# would hide anything baked there. GO_VERSION follows the Go setup-go reads
# in go.mod (internal/ciguard holds the two together); NODE_VERSION follows
# Renovate, and its sha256 must move with it. A job whose go.mod asks for
# another version still gets it, downloaded by setup-go as before, and the
# `test` job warns about it, reading what was baked from
# $RUNNER_TOOL_CACHE/iterion-ci-runner.env.
ENV RUNNER_TOOL_CACHE=/opt/hostedtoolcache
ARG GO_VERSION=1.26.0
ARG GO_SHA256=aac1b08a0fb0c4e0a7c1555beb7b59180b05dfc5a3d62e40e9de90cd42f88235
# renovate: datasource=node-version depName=node versioning=node
ARG NODE_VERSION=24.21.0
ARG NODE_SHA256=6e1db87ef58b8819e5d5402eff1536491b18edd8eb7bee5ef7897876e88dc5ff
RUN set -eu \
    && go_dir="$RUNNER_TOOL_CACHE/go/$GO_VERSION/x64" \
    && curl -fsSL -o /tmp/go.tgz "https://dl.google.com/go/go$GO_VERSION.linux-amd64.tar.gz" \
    && echo "$GO_SHA256  /tmp/go.tgz" | sha256sum -c - \
    && mkdir -p "$go_dir" \
    && tar -xzf /tmp/go.tgz -C "$go_dir" --strip-components=1 \
    && touch "$go_dir.complete" \
    && node_dir="$RUNNER_TOOL_CACHE/node/$NODE_VERSION/x64" \
    && curl -fsSL -o /tmp/node.tgz "https://nodejs.org/dist/v$NODE_VERSION/node-v$NODE_VERSION-linux-x64.tar.gz" \
    && echo "$NODE_SHA256  /tmp/node.tgz" | sha256sum -c - \
    && mkdir -p "$node_dir" \
    && tar -xzf /tmp/node.tgz -C "$node_dir" --strip-components=1 \
    && touch "$node_dir.complete" \
    && rm -f /tmp/go.tgz /tmp/node.tgz \
    && printf 'GO_VERSION=%s\nNODE_VERSION=%s\n' "$GO_VERSION" "$NODE_VERSION" > "$RUNNER_TOOL_CACHE/iterion-ci-runner.env" \
    && chown -R runner:docker "$RUNNER_TOOL_CACHE"
USER runner

LABEL org.opencontainers.image.source="https://github.com/SocialGouv/iterion" \
      org.opencontainers.image.description="ARC Actions runner with a C compiler, Go and Node for iterion's CI"
