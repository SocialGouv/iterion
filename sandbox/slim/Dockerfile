# syntax=docker/dockerfile:1.7
#
# iterion-sandbox-slim — default image used by `sandbox: auto` when
# the workspace has no .devcontainer/devcontainer.json and no
# `sandbox.image:` field. Pinned to the iterion release version, so
# `iterion v1.2.3` always picks `iterion-sandbox-slim:v1.2.3`.
#
# Contract: provides a working /bin/sh + git + node + devbox + Nix.
# Iterion's docker driver bind-mounts the host worktree at /workspace
# and overrides the container PID 1 to `sleep infinity`, so this
# image's ENTRYPOINT/CMD are convenience-only (manual `docker run`
# testing).
#
# Base: jetpackio/devbox is the upstream-published devbox image
# (Nix + devbox + git pre-configured). We layer Node.js 22 on top
# because the claude_code / codex backends invoke their CLIs via
# node, and those need to be available system-wide regardless of
# what the user's devbox.json declares.

FROM jetpackio/devbox:latest

USER root

# Layered tools:
#   nodejs   — required for the LLM CLIs invoked by claude_code/codex
#              backends. Installed system-wide so it's available even
#              before devbox shell loads.
#   tini     — PID 1 reaper for manual `docker run` use; iterion's
#              driver overrides PID 1 anyway.
#   procps   — `ps`, used by recovery dispatch + diagnostics.
#   jq, ca-certificates, curl, gnupg — common shell-tool dependencies.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        ca-certificates curl gnupg jq procps tini \
 && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

# /workspace is the canonical bind-mount target used by iterion's
# docker driver (see pkg/sandbox/docker/driver.go DefaultWorkspace).
RUN mkdir -p /workspace && chown -R devbox:devbox /workspace

USER devbox
WORKDIR /workspace

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["bash"]
