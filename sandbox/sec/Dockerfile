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

ARG BASE=ghcr.io/socialgouv/iterion-sandbox-full-base:edge
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
# trails behind, so install the NodeSource Node 24 LTS deb and pin
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
        && curl -fsSL https://deb.nodesource.com/setup_24.x | bash - \
        && apt-get install -y --no-install-recommends nodejs \
        && npm install -g "deepsec@${DEEPSEC_VERSION}" ; } \
    || echo "deepsec Layer 4 setup did not complete — run_deepsec_scanner will degrade gracefully (enable_deepsec=true users should rebuild with a Node 22+ base or pre-install deepsec)."

# ---------------------------------------------------------------------
# Layer 5 — supply-shield analyzers (MANDATORY): @nodesecure/js-x-ray for
# npm malware AST analysis + osv-scanner for the CVE floor. Used by the
# supply-shield (Shieldy) and supply-shield-cve (Vulny) bots.
#
# js-x-ray 15.x is ESM-only, so it cannot be imported by a bare specifier
# from an arbitrary cwd. We ship a SELF-CONTAINED tool at
# /opt/iterion-jsxray (its own node_modules + an ESM walker) and a PATH
# shim `iterion-jsxray`, so the import resolves regardless of the target
# repo's cwd. The bot's run_jsxray node just invokes `iterion-jsxray` with
# SCAN_DIR + WORKSPACE_DIR. Node 22 is required (js-x-ray hard requirement)
# — install it unconditionally here (Layer 4's nodejs is best-effort).
# ---------------------------------------------------------------------
ARG JSXRAY_VERSION=^15
ARG OSV_SCANNER_VERSION=2.4.0
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt/lists,sharing=locked \
    --mount=type=cache,target=/root/.npm \
    set -e; \
    command -v node >/dev/null 2>&1 || { \
        rm -f /etc/apt/apt.conf.d/docker-clean; \
        curl -fsSL https://deb.nodesource.com/setup_24.x | bash -; \
        apt-get install -y --no-install-recommends nodejs; }; \
    mkdir -p /opt/iterion-jsxray; \
    printf '%s\n' '{ "name": "iterion-jsxray", "private": true, "type": "module", "dependencies": { "@nodesecure/js-x-ray": "'"${JSXRAY_VERSION}"'" } }' > /opt/iterion-jsxray/package.json; \
    npm install --prefix /opt/iterion-jsxray --omit=dev --no-audit --no-fund; \
    printf '%s\n' '#!/bin/sh' 'exec node /opt/iterion-jsxray/scan.mjs "$@"' > /usr/local/bin/iterion-jsxray; \
    chmod +x /usr/local/bin/iterion-jsxray

# The ESM walker: walk node_modules + the workspace root, run js-x-ray
# analyseFileSync over each package's install scripts + entry points, and
# write $SCAN_DIR/jsxray.json. (Kept here so the tool is fully self-contained
# in the image; the bot only invokes it. Mirrors bots/supply-shield/skills/js-xray.md.)
COPY <<'JSXRAY_WALKER' /opt/iterion-jsxray/scan.mjs
import { AstAnalyser } from "@nodesecure/js-x-ray";
import fs from "node:fs";
import path from "node:path";
const SCAN = process.env.SCAN_DIR || ".";
const WS = process.env.WORKSPACE_DIR || ".";
const HOOKS = ["preinstall", "install", "postinstall", "prepare"];
// Bound the walk so a giant node_modules can't blow the run budget
// (matches run_generic_heuristics' MAX_PKGS guard). The diff-scoped
// reviewer only consults warnings for newly-added packages anyway.
const MAX_PKGS = Number(process.env.JSXRAY_MAX_PKGS || "4000");
const analyser = new AstAnalyser();
const out = [];
let pkgCount = 0;
function entryRels(pj) {
  const set = new Set();
  for (const h of HOOKS) {
    const s = pj.scripts && pj.scripts[h];
    if (s) for (const tok of String(s).split(/\s+/)) if (/\.[cm]?js$/.test(tok)) set.add(tok);
  }
  for (const k of ["main", "module"]) if (typeof pj[k] === "string") set.add(pj[k]);
  if (pj.bin && typeof pj.bin === "object") for (const b of Object.values(pj.bin)) if (typeof b === "string") set.add(b);
  return [...set];
}
function scanPkg(dir) {
  let pj; try { pj = JSON.parse(fs.readFileSync(path.join(dir, "package.json"), "utf8")); } catch { return; }
  for (const rel of entryRels(pj)) {
    const p = path.join(dir, rel);
    let st; try { st = fs.statSync(p); } catch { continue; }
    if (!st.isFile()) continue;
    try {
      const r = analyser.analyseFileSync(p);
      out.push({ package: pj.name || path.basename(dir), version: pj.version || "", file: path.relative(WS, p),
                 warnings: (r.warnings || []).map((w) => ({ kind: w.kind, value: String(w.value || "").slice(0, 120), location: w.location })),
                 flags: r.flags ? [...r.flags] : [] });
    } catch {}
  }
}
function walk(dir, depth) {
  if (depth > 2 || pkgCount >= MAX_PKGS) return;
  if (fs.existsSync(path.join(dir, "package.json"))) { pkgCount += 1; scanPkg(dir); }
  let ents = []; try { ents = fs.readdirSync(path.join(dir, "node_modules")); } catch { return; }
  for (const e of ents) {
    if (pkgCount >= MAX_PKGS) break;
    if (e.startsWith(".")) continue;
    const sub = path.join(dir, "node_modules", e);
    if (e.startsWith("@")) { let subs = []; try { subs = fs.readdirSync(sub); } catch {} for (const s of subs) walk(path.join(sub, s), depth + 1); }
    else walk(sub, depth + 1);
  }
}
walk(WS, 0);
fs.mkdirSync(SCAN, { recursive: true });
fs.writeFileSync(path.join(SCAN, "jsxray.json"), JSON.stringify(out));
process.stderr.write("iterion-jsxray scanned " + out.length + " files across " + pkgCount + " packages" + (pkgCount >= MAX_PKGS ? " (capped at " + MAX_PKGS + ")" : "") + "\n");
JSXRAY_WALKER

# osv-scanner — single Go binary (arch-derived like trivy/gitleaks), the
# OSV/GHSA lockfile CVE floor for supply-shield-cve.
RUN case "$(uname -m)" in \
        x86_64)  OSV_ARCH="linux_amd64" ;; \
        aarch64) OSV_ARCH="linux_arm64" ;; \
        *)       echo "unsupported arch $(uname -m)" && exit 1 ;; \
    esac \
 && curl -fsSL "https://github.com/google/osv-scanner/releases/download/v${OSV_SCANNER_VERSION}/osv-scanner_${OSV_ARCH}" \
        -o /usr/local/bin/osv-scanner \
 && chmod +x /usr/local/bin/osv-scanner

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
 && osv-scanner --version \
 && node --version \
 && mkdir -p /tmp/jsx \
 && printf '{"name":"t","version":"1.0.0","scripts":{"postinstall":"node p.js"}}' > /tmp/jsx/package.json \
 && printf 'eval(atob("MSsx"));\n' > /tmp/jsx/p.js \
 && SCAN_DIR=/tmp/jsx WORKSPACE_DIR=/tmp/jsx iterion-jsxray \
 && node -e 'const d=require("/tmp/jsx/jsxray.json");if(!d.some(f=>f.warnings.length)){console.error("iterion-jsxray produced no warnings on a known-malicious eval — FAIL");process.exit(1)}console.log("iterion-jsxray: present, detected eval on the self-test")' \
 && rm -rf /tmp/jsx \
 && { command -v deepsec >/dev/null 2>&1 \
        && deepsec --version \
        && echo "deepsec: present (optional, only used when enable_deepsec=true)" \
        || echo "deepsec: ABSENT — enable_deepsec=true runs will degrade with an unavailable-reason envelope"; }
