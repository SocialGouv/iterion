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
COPY editor/package.json editor/pnpm-lock.yaml editor/.npmrc* ./editor/
RUN corepack enable && \
    cd editor && corepack pnpm install --frozen-lockfile --prefer-offline
COPY editor ./editor
RUN cd editor && corepack pnpm exec vite build

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
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        git \
        ca-certificates \
        tini \
        procps \
 && rm -rf /var/lib/apt/lists/*

# Copy the Node 22 runtime + the pinned LLM CLIs from stage 3.
COPY --from=llm-clis /usr/local/bin/node /usr/local/bin/node
COPY --from=llm-clis /usr/local/bin/npm /usr/local/bin/npm
COPY --from=llm-clis /usr/local/bin/npx /usr/local/bin/npx
COPY --from=llm-clis /usr/local/lib/node_modules /usr/local/lib/node_modules
COPY --from=llm-clis /llm/node_modules /opt/iterion/llm-clis/node_modules
RUN ln -s /opt/iterion/llm-clis/node_modules/.bin/claude /usr/local/bin/claude && \
    ln -s /opt/iterion/llm-clis/node_modules/.bin/codex  /usr/local/bin/codex

# Iterion binary.
COPY --from=go-builder /out/iterion /usr/local/bin/iterion

# Non-root runtime user (UID/GID 10001 — high enough to avoid host
# overlap, matches Helm chart securityContext.runAsUser).
RUN groupadd --system --gid 10001 iterion \
 && useradd  --system --uid 10001 --gid iterion --home /home/iterion --create-home iterion \
 && mkdir -p /var/lib/iterion /var/run/iterion \
 && chown -R iterion:iterion /var/lib/iterion /var/run/iterion /home/iterion

USER iterion
WORKDIR /home/iterion

# Default exposed port matches the editor server bind. Helm chart
# overrides via `--port`. /healthz and /readyz are served on the same
# port so the kubelet probes hit the same listener.
EXPOSE 4891

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/iterion"]
# Default command runs the editor (currently the only HTTP server).
# When T-30 lands the dedicated `server` subcommand the Helm chart
# will override this to `["server"]`; until then the editor is the
# canonical HTTP entry point.
CMD ["editor", "--port", "4891", "--bind", "0.0.0.0", "--no-browser"]
