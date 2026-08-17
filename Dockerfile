# syntax=docker/dockerfile:1
# Iterion container image. Multi-stage:
#   1. studio-builder — vite build of the React studio → dist/
#   2. go-builder     — go build (vendor mode, CGO disabled, ldflags
#                        injected) producing the static iterion binary
#   3. llm-clis       — npm install of @anthropic-ai/claude-code +
#                        @openai/codex into a portable node_modules
#   4. runtime        — debian-slim with git + node + the LLM CLIs +
#                        the iterion binary, runs as non-root user
#                        iterion (UID 10001).
#
# Cloud-ready plan §F (T-34, AD-12).

# ---------------------------------------------------------------------
# Stage 1 — Studio frontend
# ---------------------------------------------------------------------
# --platform=$BUILDPLATFORM: the studio bundle is JS (arch-independent), so build
# it once on the builder's native arch — never under emulation for an arm64 target.
FROM --platform=$BUILDPLATFORM node:26-bookworm-slim@sha256:cd565714d4da3e84bfd341e31448f81d47c6362198f152345297c9c1154e6341 AS studio-builder
WORKDIR /app
# pnpm-workspace.yaml + pnpm-lock.yaml live at the repo root; the
# studio/ directory is a workspace member that doesn't carry its own
# lockfile. Copy the workspace anchor first so `pnpm install` can
# resolve from the locked deps tree, then layer the studio sources.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY studio/package.json studio/.npmrc* ./studio/
# Cache mount for the pnpm content-addressable store: on buildkit-operator's
# warm daemon it persists across builds, so a source-only change (which
# invalidates this layer) still resolves every dep from the warm store instead
# of re-fetching from the registry. --store-dir pins it onto the mount.
# Node 25+ no longer distributes Corepack (nodejs/TSC#1697), so the official
# node:2x images ship none — install it from npm before enabling it. The pnpm
# version itself still comes from the root package.json `packageManager` pin,
# which corepack resolves; only corepack itself is pinned here.
RUN --mount=type=cache,target=/pnpm-store \
    --mount=type=cache,target=/root/.npm \
    npm install -g corepack@0.35.0 && \
    corepack enable && \
    corepack pnpm install --frozen-lockfile --prefer-offline \
        --store-dir=/pnpm-store \
        --filter ./studio...
COPY studio ./studio
# vite cache mount: incremental studio rebuilds reuse the transform/dep cache.
RUN --mount=type=cache,target=/app/studio/node_modules/.vite \
    corepack pnpm --filter ./studio exec vite build

# ---------------------------------------------------------------------
# Stage 2 — Go binary
# ---------------------------------------------------------------------
# --platform=$BUILDPLATFORM: run the Go toolchain on the builder's native arch and
# CROSS-compile to the target (CGO is off, so this is free) — never emulate the Go
# compile for an arm64 target. TARGETOS/TARGETARCH are injected by buildx.
FROM --platform=$BUILDPLATFORM golang:1.26-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36 AS go-builder
WORKDIR /src
ARG VERSION=0.0.0
ARG COMMIT=unknown
ARG TARGETOS
ARG TARGETARCH
COPY go.mod go.sum ./
COPY vendor ./vendor
# --exclude test files: a *_test.go edit no longer changes the copied content, so
# the go build layer stays CACHED (the binary never compiles test files anyway).
COPY --exclude=**/*_test.go cmd ./cmd
COPY --exclude=**/*_test.go pkg ./pkg
COPY --exclude=**/*_test.go internal ./internal
# bots/ holds the productised bot bundles. pkg/cli embeds them via the
# github.com/SocialGouv/iterion/bots package, so the source tree must be present
# for `go build` under -mod=vendor. (e2e/ and examples/ are NOT imported or
# embedded by ./cmd/iterion, so they are deliberately not copied — keeps the build
# layer from invalidating on test/example edits.)
COPY bots ./bots
# Embed the freshly-built studio assets the Go binary serves at GET /.
COPY --from=studio-builder /app/studio/dist ./pkg/server/static
ENV CGO_ENABLED=0 GOFLAGS="-mod=vendor -trimpath"
# Cache mount for the Go build cache (compiled package objects). Deps are
# vendored (no module download), so this only caches compilation — kept warm
# on the operator daemon it turns a full recompile into an incremental one
# when a source change invalidates this layer. -s -w strips the binary (smaller
# final image, faster link). GOOS/GOARCH come from buildx for cross-compilation.
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w \
              -X github.com/SocialGouv/iterion/pkg/internal/appinfo.Version=v${VERSION#v} \
              -X github.com/SocialGouv/iterion/pkg/internal/appinfo.Commit=${COMMIT}" \
    -o /out/iterion ./cmd/iterion

# ---------------------------------------------------------------------
# Stage 3 — Pinned LLM CLIs
# ---------------------------------------------------------------------
FROM node:26-bookworm-slim@sha256:cd565714d4da3e84bfd341e31448f81d47c6362198f152345297c9c1154e6341 AS llm-clis
WORKDIR /llm
COPY docker/llm-clis/package.json ./package.json
# npm install (no lock yet) honours the exact pinned versions in
# package.json. `task docker:pin-llm-clis` (T-39) regenerates these.
# Cache mount for npm's package cache (~/.npm): warm on the operator daemon,
# the pinned tarballs are reused instead of re-downloaded on every build.
RUN --mount=type=cache,target=/root/.npm \
    npm install --omit=dev --no-audit --no-fund

# ---------------------------------------------------------------------
# Stage 4 — Runtime
# ---------------------------------------------------------------------
FROM debian:12-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df AS runtime

# NOTE: the VERSION/COMMIT ARGs are consumed at the BOTTOM of this stage (just
# above the iterion binary COPY). COMMIT changes on every push; declaring it
# here would bust the cache of every layer below — including the pinned
# glab/gh/kubectl downloads, which must stay cache-stable across pushes.
ENV PATH="/opt/iterion/llm-clis/node_modules/.bin:/usr/local/bin:/usr/bin:/bin"

# Runtime deps:
#   git       — required for `worktree: auto` workflows.
#   ca-certs  — outbound HTTPS to Anthropic / OpenAI / Mongo Atlas etc.
#   tini      — PID 1 reaper so SIGTERM propagates correctly to the
#               iterion process and to any child shells/CLIs.
#   procps    — `ps`, used by recovery dispatch + diagnostics.
#   curl      — needed to fetch kubectl below; harmless to keep
#               available for runtime use.
#   passwd    — provides groupadd/useradd. debian:12-slim drops it
#               from the default footprint; without it the non-root
#               user setup below fails with `groupadd: not found`.
#   python3   — catalog bots (e.g. review-pr) shell out to `python3 -c …`
#               in their deterministic tool nodes (diff_precheck,
#               publish_health). review-pr runs in the runner pod (no
#               sandbox), so without python3 here those nodes exit 127.
#   jq        — forge-posting skills (forge-reply.md, forge-pr-review.md)
#               build JSON note payloads with `jq -nc --arg`; without it
#               every cloud converse/review run burned an LLM round-trip
#               on exit-127 before falling back to python3.
# apt cache mounts keep the downloaded .deb archives + package lists warm on
# the operator daemon (dropping docker-clean so apt doesn't purge them). Both
# dirs are cache mounts, so they never land in the final image layer — no need
# for the old `rm -rf /var/lib/apt/lists` cleanup.
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    rm -f /etc/apt/apt.conf.d/docker-clean \
 && apt-get update \
 && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        tini \
        procps \
        curl \
        passwd \
        python3 \
        jq

# glab (GitLab CLI) — review-pr (Revi) runs WITHOUT a sandbox, so in
# cloud it executes inside the runner pod and posts code reviews onto
# GitLab merge requests (inline comments + one-click ```suggestion
# blocks) from here. Single static binary from the gitlab-org/cli
# goreleaser archive (binary at bin/glab); dpkg arch matches the asset.
ARG GLAB_VERSION=1.102.0
RUN ARCH="$(dpkg --print-architecture)" \
 && curl -fsSL "https://gitlab.com/gitlab-org/cli/-/releases/v${GLAB_VERSION}/downloads/glab_${GLAB_VERSION}_linux_${ARCH}.tar.gz" \
        | tar -xz -C /usr/local/bin --strip-components=1 bin/glab \
 && chmod +x /usr/local/bin/glab

# gh (GitHub CLI) — the GitHub-webhook leg of review-pr posts its
# summary as an issue-comment (`gh api repos/{owner}/{repo}/issues/{n}/comments`)
# from the same runner pod as glab. Single static tarball; the asset
# layout is `gh_${VERSION}_linux_${ARCH}/bin/gh`, so `--strip-components 2`
# pulls the binary out of the versioned directory.
ARG GH_VERSION=2.65.0
RUN ARCH="$(dpkg --print-architecture)" \
 && curl -fsSL "https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${ARCH}.tar.gz" \
        | tar -xz -C /usr/local/bin --strip-components=2 "gh_${GH_VERSION}_linux_${ARCH}/bin/gh" \
 && chmod +x /usr/local/bin/gh

# kubectl — required by the kubernetes sandbox driver (Phase 5) when
# the runner pod creates per-run sibling pods. Pinned to a recent
# stable release; Kubernetes guarantees client/server compatibility
# within ±1 minor for the verbs we use (apply, exec, wait, delete).
# ~50 MB at this version, which is small relative to the Node + Go
# + LLM CLIs already in the image.
ARG KUBECTL_VERSION=v1.36.0
RUN ARCH="$(dpkg --print-architecture)" \
 && case "$ARCH" in \
        amd64) KARCH=amd64 ;; \
        arm64) KARCH=arm64 ;; \
        *) echo "unsupported arch for kubectl: $ARCH" >&2; exit 1 ;; \
    esac \
 && curl -fsSL -o /tmp/kubectl \
        "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${KARCH}/kubectl" \
 && curl -fsSL -o /tmp/kubectl.sha256 \
        "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${KARCH}/kubectl.sha256" \
 && echo "$(cat /tmp/kubectl.sha256)  /tmp/kubectl" | sha256sum -c - \
 && mv /tmp/kubectl /usr/local/bin/kubectl \
 && rm /tmp/kubectl.sha256 \
 && chmod +x /usr/local/bin/kubectl \
 && /usr/local/bin/kubectl version --client --output=yaml > /dev/null

# Copy the Node 26 runtime + the pinned LLM CLIs from stage 3.
#
# We deliberately do NOT carry over `npm` / `npx` nor the system
# /usr/local/lib/node_modules from the build image:
#  - npm itself isn't invoked at runtime (the CLIs `claude` and
#    `codex` are standalone Node apps that only need the `node`
#    binary + their own node_modules under /opt/iterion/llm-clis/).
#  - npm bundles its own copy of dependencies (picomatch, glob, …),
#    and Trivy was flagging at least one HIGH CVE in those bundled
#    deps that we'd otherwise inherit despite never executing.
# Dropping the system node_modules cuts ~30 MB and a long tail of
# transitive npm-vendored vuln noise.
COPY --from=llm-clis /usr/local/bin/node /usr/local/bin/node
COPY --from=llm-clis /llm/node_modules /opt/iterion/llm-clis/node_modules
RUN ln -s /opt/iterion/llm-clis/node_modules/.bin/claude /usr/local/bin/claude && \
    ln -s /opt/iterion/llm-clis/node_modules/.bin/codex  /usr/local/bin/codex

# Version metadata + iterion binary — the only per-push layers of this stage
# (COMMIT is the pushed sha; the binary embeds it via ldflags).
ARG VERSION=0.0.0
ARG COMMIT=unknown
ENV ITERION_VERSION=${VERSION} \
    ITERION_COMMIT=${COMMIT}
COPY --from=go-builder /out/iterion /usr/local/bin/iterion

# Productised bot catalog on disk. Cloud bot resolution
# (botregistry.ResolveBotPath, used by the inbound-webhook launch path) and
# the runner's per-run skill mirroring read recipes from a real path — the
# embedded `bots` package only carries the 3 single-file bots, while bundles
# like review-pr (Revi) need their skills/ tree, which exists only on disk.
# ITERION_BOTS_PATH points the server + runner at it (colon-sep, overridable).
COPY bots /opt/iterion/bots
ENV ITERION_BOTS_PATH=/opt/iterion/bots

# Non-root runtime user (UID/GID 10001 — high enough to avoid host
# overlap, matches Helm chart securityContext.runAsUser).
#
# Absolute paths to /usr/sbin/{groupadd,useradd}: the runtime PATH set
# above intentionally excludes /usr/sbin (sbin tools shouldn't appear
# in the iterion process's PATH at runtime), so the shell can't find
# groupadd by name during this build step. Hard-coding the path is
# cleaner than mutating PATH back and forth around the user setup.
RUN /usr/sbin/groupadd --system --gid 10001 iterion \
 && /usr/sbin/useradd  --system --uid 10001 --gid iterion --home /home/iterion --create-home iterion \
 && mkdir -p /var/lib/iterion /var/run/iterion \
 && chown -R iterion:iterion /var/lib/iterion /var/run/iterion /home/iterion

# System-wide git identity for in-pod commits. Without this, any commit
# from a webhook-launched bot (review-pr suggestion blocks, feature_dev
# patches, etc.) fails with "please tell me who you are". The clone
# path in pkg/runner/loop.go runGit explicitly sets GIT_CONFIG_NOSYSTEM=1,
# so this only affects subsequent commits made by the bot inside the
# pod — never the clone HTTPS auth — and is deliberately overridden by
# any per-repo `.gitconfig` the workflow stages.
RUN git config --system user.email "iterion-runner@bot.iterion.invalid" \
 && git config --system user.name "iterion-runner[bot]"

USER iterion
WORKDIR /home/iterion

# Default exposed port matches the server bind. Helm chart overrides
# via `--port`. /healthz and /readyz are served on the same port so
# the kubelet probes hit the same listener.
EXPOSE 4891

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/iterion"]
# Default command runs the cloud-mode `server` subcommand (T-30).
# Helm chart server.command / runner.command override this for the
# runner pool. Override at `docker run` time for ad-hoc CLI use.
CMD ["server", "--port", "4891", "--bind", "0.0.0.0"]
