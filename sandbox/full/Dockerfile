# syntax=docker/dockerfile:1.7
#
# iterion-sandbox-full — opt-in richer variant of iterion-sandbox-slim
# with common language toolchains pre-installed at the system level.
# Users select it via:
#
#   --sandbox-default-image ghcr.io/socialgouv/iterion-sandbox-full:<tag>
#
# (or the equivalent ITERION_SANDBOX_DEFAULT_IMAGE env var). The slim
# variant remains the implicit default to keep first-pull cheap.
#
# Toolchains here are intentionally narrow: Go, Python, pnpm. Heavier
# language stacks (Rust, JVM, Ruby, …) are explicitly out of scope —
# users who need them should publish their own image, or declare the
# extras in a workspace-local devbox.json (which keeps working since
# devbox + Nix are inherited from the slim base).

ARG BASE=ghcr.io/socialgouv/iterion-sandbox-slim:edge
FROM ${BASE}

USER root

# Toolchains:
#   golang   — common Go workflows; matches the iterion project's own
#              devbox.json toolchain version (1.25 line).
#   python3  — many tool-node shell snippets shell out to python.
#   pnpm     — corepack-based dispatch (ships with the inherited Node 22).
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        golang-go \
        python3 \
        python3-pip \
        python3-venv \
 && corepack enable pnpm \
 && rm -rf /var/lib/apt/lists/*

USER devbox
WORKDIR /workspace
