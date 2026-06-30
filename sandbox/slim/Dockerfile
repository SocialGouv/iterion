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

# ITERION_IMAGE: the iterion image whose static `iterion` binary gets baked
# onto PATH (see the COPY in the final stage). A global ARG (before any FROM)
# so it can parameterize the puller stage. Defaults to the published :edge so a
# bare local `docker build sandbox/slim` works; CI passes the matching tag
# (:vX.Y.Z on a release, else :edge).
ARG ITERION_IMAGE=ghcr.io/socialgouv/iterion:edge
FROM ${ITERION_IMAGE} AS iterion-bin

FROM jetpackio/devbox:latest@sha256:04562895294750bab6fcf75533a35566a854d272256933fa9c416e7f9859186f

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
 && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

# /workspace is the canonical bind-mount target used by iterion's
# docker driver (see pkg/sandbox/docker/driver.go DefaultWorkspace).
RUN mkdir -p /workspace && chown -R devbox:devbox /workspace

# Bake the iterion binary onto PATH (from the iterion-bin stage above). The
# sandboxed `claw` backend shells out to `iterion __claw-runner` *inside* the
# container. The docker driver bind-mounts the host binary
# (pkg/runtime/sandbox_mounts.go addClawBinaryMount); the kubernetes driver has
# no host filesystem — type=bind is rejected (SupportsHostBindMounts=false) — so
# on k8s the binary must ship in the image. Static (CGO_ENABLED=0, see the root
# Dockerfile go-builder) so it runs as-is on this devbox/debian base.
COPY --from=iterion-bin /usr/local/bin/iterion /usr/local/bin/iterion

# Bake the LLM CLIs (claude_code) so claude_code-backed bots don't install them
# per-run. The per-run path is `sudo npm install -g @anthropic-ai/claude-code`
# (the npm global prefix is root-owned), but the kubernetes sandbox pod runs
# runAsNonRoot with allowPrivilegeEscalation=false, so sudo can't escalate and
# the bot's post_create exits 1. Reuse the iterion image's pinned llm-clis
# (docker/llm-clis: claude-code 2.1.175) and symlink `claude` onto PATH; it runs
# on this image's node 22. With `claude` present, post_create's `claude --version`
# check passes and the sudo branch is skipped (docker + k8s alike).
COPY --from=iterion-bin /opt/iterion/llm-clis/node_modules /opt/iterion/llm-clis/node_modules
RUN ln -sf /opt/iterion/llm-clis/node_modules/.bin/claude /usr/local/bin/claude

# Numeric UID (not the `devbox` name) so the image runs under platforms
# that forbid non-numeric / root-capable users (OpenShift `restricted`
# SCC, k8s `runAsNonRoot`). uid 1000 == devbox.
USER 1000
WORKDIR /workspace

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["bash"]
