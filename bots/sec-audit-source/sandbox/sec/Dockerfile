# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
#
# iterion-sandbox-sec — Seki's scanner + agent toolchain sandbox image.
#
# Augments the PUBLISHED scanner base (gosec, gitleaks, trivy, semgrep,
# bandit, … — built upstream and pushed to ghcr) with the two agent CLIs
# Seki drives INSIDE the sandbox container:
#
#   - claude-code : the `claude_code` backend spawns `claude` per agent
#                   node (detect, voters, patch_author, reviewer, …).
#   - deepsec     : optional regex + LLM scanner, enabled with
#                   `--var enable_deepsec=true` (tool run_deepsec_scanner).
#                   The bot degrades gracefully when it is absent.
#
# Provenance: the scanner base is maintained in the image's own build
# pipeline; this file is the augmentation layer used for local dogfooding.
# Because it retags `:edge` in place, back up the pristine base first:
#
#   docker tag ghcr.io/socialgouv/iterion-sandbox-sec:edge \
#             ghcr.io/socialgouv/iterion-sandbox-sec:edge-base
#   docker build --build-arg BASE=ghcr.io/socialgouv/iterion-sandbox-sec:edge-base \
#     -t ghcr.io/socialgouv/iterion-sandbox-sec:edge \
#     -f bots/sec-audit-source/sandbox/sec/Dockerfile bots/sec-audit-source/sandbox/sec
#
ARG BASE=ghcr.io/socialgouv/iterion-sandbox-sec:edge-base
FROM ${BASE}

USER root

# claude_code backend — the official Claude Code CLI, run per agent node.
# Scripts ON + --include=optional so the platform binary postinstall
# (install.cjs) runs; hardened npm defaults otherwise skip it.
RUN npm install -g --ignore-scripts=false --include=optional @anthropic-ai/claude-code@latest \
    && claude --version >/dev/null

# deepsec (Enhancement 4) — regex matchers + LLM investigation, ingested
# by triage like any other scanner. --ignore-scripts skips the unused
# @openai/codex native-binary postinstall (Seki drives `deepsec --agent
# claude`, never codex). Pin the version so scans stay reproducible.
RUN npm install -g deepsec@2.0.12 --ignore-scripts --no-audit --no-fund \
    && deepsec --version >/dev/null

# Node 22+ is required by deepsec and ships in the base; fail loudly if a
# future base regresses it.
RUN node -e 'process.exit(parseInt(process.versions.node) >= 22 ? 0 : 1)' \
    || (echo "deepsec requires Node 22+; base image regressed" >&2; exit 1)

USER 1000
