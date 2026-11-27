# syntax=docker/dockerfile:1.29@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183
#
# Shared finalize layer — produces every published iterion-sandbox-* image
# (slim/full/sec/browser) by stamping the iterion binary + LLM CLIs onto the
# matching tool-only *-base image. The bases carry the heavy, rarely-changing
# tool layers (rebuilt only on sandbox/ changes); this build is a few cheap
# COPY layers, which CI refolds nightly with main's `:edge` binary — so an
# `:edge` sandbox image lags the `:edge` app image by up to a day (#3742),
# while a release's `:vX.Y.Z` sandbox carries that release's own binary.
#
#   BASE          — the tool base to finalize (…-sandbox-<x>-base:<tag>).
#   ITERION_IMAGE — the iterion app image whose /usr/local/bin/iterion and
#                   pinned llm-clis get baked in.

ARG BASE
ARG ITERION_IMAGE=ghcr.io/socialgouv/iterion:edge@sha256:cee7a0f6e71d55dd1249cd16db1844f4e49a3888ec4a7248efe3deb6142b1614
FROM ${ITERION_IMAGE} AS iterion-bin

FROM ${BASE}

USER root

# Bake the iterion binary onto PATH. The sandboxed `claw` backend shells out to
# `iterion __claw-runner` *inside* the container. The docker driver bind-mounts
# the host binary (pkg/runtime/sandbox_mounts.go addClawBinaryMount); the
# kubernetes driver has no host filesystem — type=bind is rejected
# (SupportsHostBindMounts=false) — so on k8s the binary must ship in the image.
# Static (CGO_ENABLED=0, see the root Dockerfile go-builder) so it runs as-is
# on the devbox/debian bases.
COPY --from=iterion-bin /usr/local/bin/iterion /usr/local/bin/iterion
COPY --from=iterion-bin --chown=0:0 --chmod=0555 /usr/local/libexec/iterion-execenv-guest /usr/local/libexec/iterion-execenv-guest
COPY --from=iterion-bin --chown=0:0 --chmod=0555 /usr/local/libexec/iterion-execenv-capsule /usr/local/libexec/iterion-execenv-capsule
RUN chmod 0755 /usr/local/libexec

# Bake the LLM CLIs (claude_code) so claude_code-backed bots don't install them
# per-run. The per-run path is `sudo npm install -g @anthropic-ai/claude-code`
# (the npm global prefix is root-owned), but the kubernetes sandbox pod runs
# runAsNonRoot with allowPrivilegeEscalation=false, so sudo can't escalate and
# the bot's post_create exits 1. Reuse the iterion image's pinned llm-clis
# (docker/llm-clis) and symlink `claude` onto PATH; it runs on the base's
# node 24. With `claude` present, post_create's `claude --version` check passes
# and the sudo branch is skipped (docker + k8s alike).
COPY --from=iterion-bin /opt/iterion/llm-clis/node_modules /opt/iterion/llm-clis/node_modules
RUN ln -sf /opt/iterion/llm-clis/node_modules/.bin/claude /usr/local/bin/claude \
 && ln -sf /opt/iterion/llm-clis/node_modules/.bin/pi /usr/local/bin/pi

# Same non-root contract as the bases (uid 1000 == devbox); ENTRYPOINT/CMD and
# env inherit from the base image.
USER 1000
WORKDIR /workspace

# Execute the actual copied pair as the final non-root user on each architecture.
RUN --mount=type=bind,source=execenv-smoke.sh,target=/tmp/execenv-smoke.sh \
    sh /tmp/execenv-smoke.sh
