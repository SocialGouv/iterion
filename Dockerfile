# syntax=docker/dockerfile:1.7
# Iterion container image. Multi-stage:
#   1. editor-builder — vite build of the React editor → dist/
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
# Stage 1 — Editor frontend
# ---------------------------------------------------------------------
FROM node:22-bookworm-slim AS editor-builder
WORKDIR /app
# pnpm-workspace.yaml + pnpm-lock.yaml live at the repo root; the
# editor/ directory is a workspace member that doesn't carry its own
# lockfile. Copy the workspace anchor first so `pnpm install` can
# resolve from the locked deps tree, then layer the editor sources.
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml ./
COPY editor/package.json editor/.npmrc* ./editor/
RUN corepack enable && \
    corepack pnpm install --frozen-lockfile --prefer-offline \
        --filter ./editor...
COPY editor ./editor
RUN corepack pnpm --filter ./editor exec vite build

# ---------------------------------------------------------------------
# Stage 2 — Go binary
# ---------------------------------------------------------------------
FROM golang:1.26-bookworm AS go-builder
WORKDIR /src
ARG VERSION=0.0.0
ARG COMMIT=unknown
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY cmd ./cmd
COPY pkg ./pkg
COPY e2e ./e2e
COPY examples ./examples
# Embed the freshly-built editor assets the Go binary serves at GET /.
COPY --from=editor-builder /app/editor/dist ./pkg/server/static
ENV CGO_ENABLED=0 GOFLAGS="-mod=vendor -trimpath"
RUN go build \
    -ldflags="-X github.com/SocialGouv/iterion/pkg/internal/appinfo.Version=v${VERSION} \
              -X github.com/SocialGouv/iterion/pkg/internal/appinfo.Commit=${COMMIT}" \
    -o /out/iterion ./cmd/iterion

# ---------------------------------------------------------------------
# Stage 3 — Pinned LLM CLIs
# ---------------------------------------------------------------------
FROM node:22-bookworm-slim AS llm-clis
WORKDIR /llm
COPY docker/llm-clis/package.json ./package.json
# npm install (no lock yet) honours the exact pinned versions in
# package.json. `task docker:pin-llm-clis` (T-39) regenerates these.
RUN npm install --omit=dev --no-audit --no-fund

# ---------------------------------------------------------------------
# Stage 4 — Runtime
# ---------------------------------------------------------------------
FROM debian:12-slim AS runtime

ARG VERSION=0.0.0
ARG COMMIT=unknown
ENV ITERION_VERSION=${VERSION} \
    ITERION_COMMIT=${COMMIT} \
    PATH="/opt/iterion/llm-clis/node_modules/.bin:/usr/local/bin:/usr/bin:/bin"

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
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        tini \
        procps \
        curl \
        passwd \
 && rm -rf /var/lib/apt/lists/*

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

# Copy the Node 22 runtime + the pinned LLM CLIs from stage 3.
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

# Iterion binary.
COPY --from=go-builder /out/iterion /usr/local/bin/iterion

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
