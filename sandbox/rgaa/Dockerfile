# syntax=docker/dockerfile:1.29@sha256:572cebbc2b15e6f84c077742952674889cb63cd5e9b80abdffcbe587b6f19183
#
# iterion-sandbox-rgaa — the rgaa capability class (#2632, decision (a)):
# everything iterion-sandbox-browser carries (Chromium + Playwright MCP,
# ULTRA11Y_BROWSER_PATH naming /usr/bin/chromium) PLUS the ultra11y engine
# baked globally at the pin the rgaa-audit bot declares.
#
# Why a variant of browser rather than a line in it: the browser image
# serves the studio's Browser pane and every Playwright-MCP workflow, most
# of which never audit anything — the engine's ~5 MB tarball is not theirs
# to pull. The rgaa-audit bot (Acci) is the class's one consumer today; it
# opts in with `sandbox: { image: rgaa }`.
#
# The bot fingerprints the engine before running it (size + sha256 of
# scripts/ultra11y.mjs, against bots/rgaa-audit/ultra11y.pin), so the pin
# baked here MUST equal the bot's: a drifted image is a refused engine, not
# a faster one. bots/rgaa_audit_sandbox_rgaa_image_test.go holds the two
# together; renovate moves them in one PR via the comment below. The
# bot's per-run `npm install` fallback stays until the bot opts in.
#
# BASE is overridden by the image pipeline (.github/workflows/
# sandbox-images.yml) with the freshly-built browser-base; the default is
# the published one for local rebuilds.

ARG BASE=ghcr.io/socialgouv/iterion-sandbox-browser-base:edge@sha256:587df0492e268fd78fd250f004074704be02731b597f1336c656ecc4880967c6
FROM ${BASE}

USER root

# ultra11y, globally so the bot's npm-global resolution arm (`npm root -g`)
# finds it without a per-run install.
# renovate: datasource=npm depName=ultra11y versioning=npm
ARG ULTRA11Y_VERSION=9.6.6
RUN --mount=type=cache,target=/root/.npm \
    npm install -g "ultra11y@${ULTRA11Y_VERSION}"

# Numeric UID (not the `devbox` name), mirroring the browser variant, so the
# image runs under platforms that forbid non-numeric / root-capable users
# (OpenShift `restricted` SCC, k8s `runAsNonRoot`). uid 1000 == devbox.
USER 1000
WORKDIR /workspace
