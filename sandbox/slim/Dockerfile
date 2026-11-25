# syntax=docker/dockerfile:1.26@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
#
# iterion-sandbox-slim-base — tool-only base of iterion-sandbox-slim.
# Contains NO iterion binary: sandbox/finalize/Dockerfile layers the
# binary + LLM CLIs on top to produce the published iterion-sandbox-slim,
# so this heavy tool layer only rebuilds when sandbox/ inputs change.
#
# Contract: provides a working /bin/sh + git + node + devbox + Nix.
# Iterion's docker driver bind-mounts the host worktree at /workspace
# and overrides the container PID 1 to `sleep infinity`, so this
# image's ENTRYPOINT/CMD are convenience-only (manual `docker run`
# testing).
#
# Base: jetpackio/devbox is the upstream-published devbox image
# (Nix + devbox + git pre-configured). We layer Node.js 24 on top
# because the claude_code / codex backends invoke their CLIs via
# node, and those need to be available system-wide regardless of
# what the user's devbox.json declares.

FROM jetpackio/devbox:latest@sha256:0ce877daf8f83ce37163e94bae7a12af0a6eaf3e660117c696703a2fa9cece89

USER root

# Layered tools:
#   nodejs   — required for the LLM CLIs invoked by claude_code/codex
#              backends. Installed system-wide so it's available even
#              before devbox shell loads.
#   tini     — PID 1 reaper for manual `docker run` use; iterion's
#              driver overrides PID 1 anyway.
#   procps   — `ps`, used by recovery dispatch + diagnostics.
#   jq, ca-certificates, curl, gnupg — common shell-tool dependencies.
# apt cache mounts keep the downloaded .deb archives + package lists warm on
# the operator daemon (dropping docker-clean so apt doesn't purge them); the
# mounts never land in the image layer, so no rm -rf cleanup is needed.
# npm: NodeSource's nodejs 24 bundles an npm whose own dependencies
# (undici, ip-address, brace-expansion, postcss-selector-parser) carry
# fixed HIGH/CRITICAL advisories; a newer npm of the same major bundles
# the fixed versions. Pinned so the image is reproducible.
# renovate: datasource=npm depName=npm versioning=npm
ARG NPM_VERSION=11.21.3
# `apt-get upgrade`: the pinned base digest freezes the Debian packages
# it was built with, and its maintainers do not rebuild it for every
# security update; upgrading here applies the ones published since.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
 && apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get upgrade -y --no-install-recommends \
 && apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg jq procps tini \
 && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && npm install -g "npm@${NPM_VERSION}" \
 && test "$(npm --version)" = "${NPM_VERSION}"

# /workspace is the canonical bind-mount target used by iterion's
# docker driver (see pkg/sandbox/docker/driver.go DefaultWorkspace).
RUN mkdir -p /workspace && chown -R devbox:devbox /workspace

# Numeric UID (not the `devbox` name) so the image runs under platforms
# that forbid non-numeric / root-capable users (OpenShift `restricted`
# SCC, k8s `runAsNonRoot`). uid 1000 == devbox.
USER 1000
WORKDIR /workspace

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["bash"]
