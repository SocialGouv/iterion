# syntax=docker/dockerfile:1.26@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
#
# iterion-sandbox-browser — opt-in variant of iterion-sandbox-slim
# that bundles Chromium + Playwright MCP for the run console's
# Browser pane (PR 3 of the browser-simulation feature).
#
# Why this is a separate variant rather than piggybacking on
# iterion-sandbox-full: most workflows that use `full` (Go/Python/
# pnpm dev tooling) don't need a browser, and the reverse is also
# true. Splitting keeps first-pull costs predictable per use case.
#
# Operators select it via the standard launch flag:
#
#   --sandbox-default-image ghcr.io/socialgouv/iterion-sandbox-browser:<tag>
#
# (or ITERION_SANDBOX_DEFAULT_IMAGE env var). Workflows that declare
# a Playwright MCP server (matched by the C060 IR diagnostic) MUST
# point sandbox.image at this variant — slim and full do not ship
# Chromium and the MCP child crashes on first `browser_*` call.
#
# Image budget target: < 1.2 GB compressed. Chromium + Playwright
# add ~280 MB on top of slim's ~250 MB.

ARG BASE=ghcr.io/socialgouv/iterion-sandbox-slim-base:edge@sha256:9bda52529cb5dec74d4fb3c26da02dafe088628632b32e954fa3e6ee5570a6dc
FROM ${BASE}

USER root

# Headless Chromium (Debian's chromium package = full headless +
# headed support; we only use --headless --remote-debugging-pipe).
# Plus the runtime fonts + libs Chromium needs to render pages.
#
# fonts-noto-color-emoji + fonts-liberation cover the typical "page
# looks broken because emoji/sans fallbacks are missing" case
# without bloating the image with a CJK/Indic spread.
# apt cache mounts keep the ~280 MB chromium download warm on the operator
# daemon (dropping docker-clean so apt doesn't purge it); the mounts never
# land in the image layer, so no rm -rf cleanup is needed.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
        chromium \
        fonts-liberation \
        fonts-noto-color-emoji \
        libnss3 \
        libxss1 \
        libxtst6 \
        libgbm1 \
        libasound2

# Playwright MCP — the npm package the runtime spawns via stdio.
# Pinned global install for reproducibility; bumps land via PR.
# We rely on Node 24 inherited from slim. Corepack-managed pnpm is
# already enabled in the base image.
# renovate: datasource=npm depName=@playwright/mcp versioning=npm
ARG PLAYWRIGHT_MCP_VERSION=0.0.78
RUN --mount=type=cache,target=/root/.npm \
    npm install -g "@playwright/mcp@${PLAYWRIGHT_MCP_VERSION}"

# Chromium expects a writable XDG_CACHE dir; default to the devbox
# user's home so each run gets a fresh, isolated cache.
ENV CHROMIUM_FLAGS="--headless --no-sandbox --disable-dev-shm-usage --remote-debugging-pipe"

# Numeric UID (not the `devbox` name) so the image runs under platforms
# that forbid non-numeric / root-capable users (OpenShift `restricted`
# SCC, k8s `runAsNonRoot`). uid 1000 == devbox.
USER 1000
WORKDIR /workspace
