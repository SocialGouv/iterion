# syntax=docker/dockerfile:1.7
#
# iterion-sandbox-sec — security-scanner variant layered on
# iterion-sandbox-full. It adds the SAST / SCA / secret scanners the
# sec-audit-source and sec-audit-deps bots shell out to, which neither
# the slim nor full base ships. Select it via:
#
#   --sandbox-default-image ghcr.io/socialgouv/iterion-sandbox-sec:<tag>
#
# (or a workflow `sandbox.image:`). Without it the sec-audit bots run
# their scanner tool-nodes against a host/base that has none of these
# binaries, every scan `|| true`-falls-through, and the audit silently
# reports zero findings — a façade.
#
# Scanners covered:
#   - semgrep   — broad-spectrum SAST (auto + per-language rulesets).
#   - gosec     — Go SAST.
#   - govulncheck — Go module vulnerability DB (SCA).
#   - bandit    — Python SAST.
#   - pip-audit — Python dependency vulnerabilities (SCA).
#   - trivy     — fs vuln/config/secret scanning (SCA + IaC + secrets).
#   - gitleaks  — secret detection.

ARG BASE=ghcr.io/socialgouv/iterion-sandbox-full:edge
FROM ${BASE}

USER root

# ---------------------------------------------------------------------
# Layer 1 — Python scanners via pip. Debian 13 enforces PEP 668, so the
# system install needs --break-system-packages; the console scripts land
# on /usr/local/bin which is already on PATH. semgrep is the heavy one.
# ---------------------------------------------------------------------
# pip cache mount (~/.cache/pip): warm on the operator daemon, the wheels for
# semgrep (the heavy one) + bandit + pip-audit are reused instead of refetched.
# The mount isn't part of the image layer, so dropping --no-cache-dir keeps the
# final image just as small while making rebuilds fast.
RUN --mount=type=cache,target=/root/.cache/pip \
    pip3 install --break-system-packages \
        semgrep \
        bandit \
        pip-audit

# ---------------------------------------------------------------------
# Layer 2 — Go scanners. The apt `go` from the full base drives
# `go install`; GOBIN pins the binaries into /usr/local/bin so they are
# on PATH for every user (the bots run as the `devbox` user).
# ---------------------------------------------------------------------
ARG GOSEC_VERSION=latest
ARG GOVULNCHECK_VERSION=latest
# Cache mounts for the Go module cache (/root/go/pkg/mod) + build cache: the
# gosec/govulncheck source + compiled objects stay warm on the operator daemon,
# so re-installs skip the module download + recompile. Both are mounts (not
# image layers), so they replace the old `rm -rf` cleanup for free.
RUN --mount=type=cache,target=/root/go \
    --mount=type=cache,target=/root/.cache/go-build \
    GOBIN=/usr/local/bin GOFLAGS=-mod=mod \
        go install "github.com/securego/gosec/v2/cmd/gosec@${GOSEC_VERSION}" \
 && GOBIN=/usr/local/bin GOFLAGS=-mod=mod \
        go install "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}"

# ---------------------------------------------------------------------
# Layer 3 — single-binary scanners. Arch-derived like the full image so
# the multi-arch buildx pipeline works on amd64 + arm64.
#   trivy    — official install script (handles arch + checksum).
#   gitleaks — release tarball; asset arch suffix is x64 / arm64.
# ---------------------------------------------------------------------
ARG TRIVY_VERSION=0.70.0
ARG GITLEAKS_VERSION=8.21.2
RUN case "$(uname -m)" in \
        x86_64)  TRIVY_ARCH="Linux-64bit"; GL_ARCH="x64" ;; \
        aarch64) TRIVY_ARCH="Linux-ARM64"; GL_ARCH="arm64" ;; \
        *)       echo "unsupported arch $(uname -m)" && exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/aquasecurity/trivy/releases/download/v${TRIVY_VERSION}/trivy_${TRIVY_VERSION}_${TRIVY_ARCH}.tar.gz" \
        | tar -xz -C /usr/local/bin trivy \
 && chmod +x /usr/local/bin/trivy \
 && curl -fsSL "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/gitleaks_${GITLEAKS_VERSION}_linux_${GL_ARCH}.tar.gz" \
        | tar -xz -C /usr/local/bin gitleaks \
 && chmod +x /usr/local/bin/gitleaks

# ---------------------------------------------------------------------
# Layer 4 — OPTIONAL deepsec backend (Enhancement 4 in the sec-audit
# uplift). https://github.com/vercel-labs/deepsec. Only exercised when
# the sec-audit-source workflow is run with --var enable_deepsec=true;
# the tool node degrades gracefully (envelope with errors[]) if any
# of these binaries are missing, so this layer NEVER hard-fails the
# build and a partial install only flips the degraded flag at runtime.
#
# Requires Node.js 22+ (deepsec hard-requirement). Debian's apt `nodejs`
# trails behind, so install the NodeSource Node 22 LTS deb and pin
# deepsec via build arg. npm install -g lands the `deepsec` binary on
# /usr/local/bin which is already on PATH for uid 1000 (devbox).
#
# A failure here MUST NOT break the build for users who don't want
# deepsec — wrap in `|| echo`. The Layer 4 sanity check below records
# whether deepsec is actually callable.
# ---------------------------------------------------------------------
ARG DEEPSEC_VERSION=latest
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    --mount=type=cache,target=/root/.npm \
    { rm -f /etc/apt/apt.conf.d/docker-clean \
        && curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
        && apt-get install -y --no-install-recommends nodejs \
        && npm install -g "deepsec@${DEEPSEC_VERSION}" ; } \
    || echo "deepsec Layer 4 setup did not complete — run_deepsec_scanner will degrade gracefully (enable_deepsec=true users should rebuild with a Node 22+ base or pre-install deepsec)."

# Numeric UID (not the `devbox` name) on the final stage so the image
# runs under platforms that forbid non-numeric / root-capable users
# (OpenShift `restricted` SCC, k8s `runAsNonRoot`). uid 1000 == devbox.
USER 1000
WORKDIR /workspace

# Build-time sanity check — fail the build rather than ship an image
# that silently lacks a scanner (the exact façade this image prevents).
# deepsec is intentionally NOT in the hard-fail list: it is the only
# optional scanner and the run_deepsec_scanner tool degrades gracefully
# when it is absent. A presence probe is logged for the image audit
# trail without affecting the build exit code.
RUN semgrep --version \
 && gosec --version \
 && govulncheck -version \
 && bandit --version \
 && pip-audit --version \
 && trivy --version \
 && gitleaks version \
 && { command -v deepsec >/dev/null 2>&1 \
        && deepsec --version \
        && echo "deepsec: present (optional, only used when enable_deepsec=true)" \
        || echo "deepsec: ABSENT — enable_deepsec=true runs will degrade with an unavailable-reason envelope"; }
