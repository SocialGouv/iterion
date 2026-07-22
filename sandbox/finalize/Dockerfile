# syntax=docker/dockerfile:1.7
#
# Shared finalize layer — produces every published iterion-sandbox-* image
# (slim/full/sec/browser) by stamping the iterion binary + LLM CLIs onto the
# matching tool-only *-base image. The bases carry the heavy, rarely-changing
# tool layers (rebuilt only on sandbox/ changes); this build is a few cheap
# COPY layers, so CI can refresh the binary in all variants on every push —
# the published `:edge` sandbox images never lag the `:edge` app image.
#
#   BASE          — the tool base to finalize (…-sandbox-<x>-base:<tag>).
#   ITERION_IMAGE — the iterion app image whose /usr/local/bin/iterion and
#                   pinned llm-clis get baked in.

ARG BASE
ARG ITERION_IMAGE=ghcr.io/socialgouv/iterion:edge
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

# Bake the LLM CLIs (claude_code) so claude_code-backed bots don't install them
# per-run. The per-run path is `sudo npm install -g @anthropic-ai/claude-code`
# (the npm global prefix is root-owned), but the kubernetes sandbox pod runs
# runAsNonRoot with allowPrivilegeEscalation=false, so sudo can't escalate and
# the bot's post_create exits 1. Reuse the iterion image's pinned llm-clis
# (docker/llm-clis) and symlink `claude` onto PATH; it runs on the base's
# node 24. With `claude` present, post_create's `claude --version` check passes
# and the sudo branch is skipped (docker + k8s alike).
COPY --from=iterion-bin /opt/iterion/llm-clis/node_modules /opt/iterion/llm-clis/node_modules
RUN ln -sf /opt/iterion/llm-clis/node_modules/.bin/claude /usr/local/bin/claude

# Same non-root contract as the bases (uid 1000 == devbox); ENTRYPOINT/CMD and
# env inherit from the base image.
USER 1000
WORKDIR /workspace
