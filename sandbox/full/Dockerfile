# syntax=docker/dockerfile:1.7
#
# iterion-sandbox-full — opt-in richer variant of iterion-sandbox-slim
# with common language toolchains AND operator/ops tooling pre-installed
# at the system level. Users select it via:
#
#   --sandbox-default-image ghcr.io/socialgouv/iterion-sandbox-full:<tag>
#
# (or the equivalent ITERION_SANDBOX_DEFAULT_IMAGE env var). The slim
# variant remains the implicit default to keep first-pull cheap.
#
# Toolchains covered:
#   - Languages: Go (apt), Python 3, pnpm (via corepack inherited).
#   - Node version management: fnm.
#   - Go version management: g (stefanmaric/g).
#   - VCS: gh (GitHub CLI).
#   - Data shaping: jq (inherited from slim), yq (mikefarah).
#   - Kubernetes ops: kubectl, helm, k9s.
#   - Dev env: direnv. (devbox + Nix inherited from slim base.)
#
# Heavier language stacks (Rust, JVM, Ruby, …) remain out of scope —
# users who need them should publish their own image, or declare the
# extras in a workspace-local devbox.json.

ARG BASE=ghcr.io/socialgouv/iterion-sandbox-slim:edge
FROM ${BASE}

USER root

# ---------------------------------------------------------------------
# Layer 1 — apt-installable toolchains + ops dependencies.
#
#   golang-go        — common Go workflows; baseline version pinned by
#                      Debian. Use `g` (installed below) to switch
#                      Go versions per bot when needed.
#   python3 + venv   — many tool-node shell snippets shell out to python.
#   direnv           — operator-grade env-loading; `direnv allow` works
#                      out of the box for repos that ship a `.envrc`.
#   unzip            — needed by the fnm binary install below.
#   gnupg, ca-cert,
#   curl             — already present via slim, but reaffirmed for
#                      the apt-repo setups below.
# ---------------------------------------------------------------------
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        golang-go \
        python3 \
        python3-pip \
        python3-venv \
        direnv \
        unzip \
        ca-certificates \
        curl \
        gnupg \
 && corepack enable pnpm \
 && rm -rf /var/lib/apt/lists/*

# ---------------------------------------------------------------------
# Layer 2 — gh (GitHub CLI) via official deb repo. Pinned to the apt
# stable line so updates roll with the rest of the image rebuild.
# ---------------------------------------------------------------------
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
        | gpg --dearmor -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
        > /etc/apt/sources.list.d/github-cli.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends gh \
 && rm -rf /var/lib/apt/lists/*

# ---------------------------------------------------------------------
# Layer 3 — single-binary tools. Each install respects $TARGETARCH so
# the multi-arch buildx pipeline produces a working image on both
# amd64 and arm64.
#
#   yq (mikefarah)   — YAML query/manipulation. Pinned for stability.
#   kubectl          — Kubernetes CLI. Pinned to a recent stable line.
#   helm             — Kubernetes package manager (official install
#                      script auto-pins to latest stable v3).
#   k9s              — Kubernetes TUI for observability work.
#   fnm              — Node version manager (Schniz/fnm). The slim
#                      base ships Node 22 system-wide; fnm is for
#                      bots that need a specific Node version.
# ---------------------------------------------------------------------
ARG YQ_VERSION=v4.44.5
ARG KUBECTL_VERSION=v1.31.4
ARG K9S_VERSION=v0.32.7
ARG FNM_VERSION=v1.38.1

RUN ARCH=$(dpkg --print-architecture) \
 && curl -fsSL "https://github.com/mikefarah/yq/releases/download/${YQ_VERSION}/yq_linux_${ARCH}" \
        -o /usr/local/bin/yq \
 && chmod +x /usr/local/bin/yq \
 && curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl" \
        -o /usr/local/bin/kubectl \
 && chmod +x /usr/local/bin/kubectl \
 && curl -fsSL "https://github.com/derailed/k9s/releases/download/${K9S_VERSION}/k9s_Linux_${ARCH}.tar.gz" \
        | tar -xz -C /usr/local/bin k9s \
 && chmod +x /usr/local/bin/k9s \
 && curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 \
        | DESIRED_VERSION="" bash

# fnm — Schniz/fnm. The release artefact name uses `linux` for amd64
# and `arm64` for arm64 (not the dpkg `amd64`/`arm64` convention), so
# we re-derive the suffix here from `uname -m`.
RUN case "$(uname -m)" in \
        x86_64)  FNM_ARCH="linux" ;; \
        aarch64) FNM_ARCH="arm64" ;; \
        *)       echo "fnm: unsupported arch $(uname -m)" && exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/Schniz/fnm/releases/download/${FNM_VERSION}/fnm-${FNM_ARCH}.zip" \
        -o /tmp/fnm.zip \
 && unzip -j /tmp/fnm.zip fnm -d /usr/local/bin \
 && chmod +x /usr/local/bin/fnm \
 && rm /tmp/fnm.zip

# ---------------------------------------------------------------------
# Layer 4 — `g` (stefanmaric/g) for Go version management. The binary
# is a self-contained bash script; we drop it into /usr/local/bin
# directly. `g` requires GOPATH + GOROOT at invocation time — bots
# that want to switch Go versions should export those (e.g. into
# `/home/devbox/.g/go` + `/home/devbox/go`) before calling
# `g install <version>`. We intentionally do NOT set them globally so
# the apt-installed `go` keeps working out of the box for the common
# case where one Go version is enough.
# ---------------------------------------------------------------------
RUN curl -fsSL https://raw.githubusercontent.com/stefanmaric/g/master/bin/g \
        -o /usr/local/bin/g \
 && chmod +x /usr/local/bin/g \
 && install -d -o devbox -g devbox /home/devbox/.g /home/devbox/go

USER devbox
WORKDIR /workspace

# Sanity check at build time — fail fast if any tool is missing/broken
# rather than producing a broken image users discover at runtime.
# `g` is checked via `command -v` rather than `--version`: the latter
# inspects $GOROOT and bots that haven't exported it yet would see a
# scary error even though the binary is fine.
RUN gh --version \
 && yq --version \
 && kubectl version --client --output=yaml \
 && helm version --short \
 && k9s version --short \
 && fnm --version \
 && command -v g \
 && direnv --version \
 && go version \
 && python3 --version \
 && pnpm --version
