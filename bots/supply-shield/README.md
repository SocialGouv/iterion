# supply-shield (Shieldy) — global supply-chain malware shield

A PR / push-driven, **diff-scoped** malware gate for dependency changes.
Where `sec-audit-deps` (Depsy) runs a full-tree weekly audit emitting
only to the board, Shieldy inspects **only the dependency versions a
change adds or upgrades**, reuses a **shared cache** so a version is
analysed once, and **reports back onto the PR** (merge request on
GitLab) via the native forge API. The CVE-focused sibling is `supply-shield-cve` (Vulny).

## What it does

1. **Diff-scopes** the run — diffs the changed lockfiles between
   `base..head` and inspects only the newly added/upgraded packages
   (`scope_mode=full` for a whole-tree pass).
2. **Analyses for malware** — js-x-ray AST analysis for npm (the
   @nodesecure analyzer no-package-malware relied on), install-hook +
   SHA-512 checksum-integrity, locale/homoglyph anomalies, osv/trivy CVE
   baseline, and an LLM **deep-read** of install scripts / entry points
   when heuristics are inconclusive.
3. **Anti-façade gate** — `coverage_gate` hard-fails when the scanner
   floor did not run, so a missing analyzer never reads as "0 malware".
4. **Shared store** — every `(ecosystem, name, version, checksum)` verdict
   (`kind: malware`) is cached and reused across runs / PRs / repos.
5. **Reports back** — sticky PR summary comment + inline review +
   SARIF / code-scanning upload (GitHub / GitLab / Forgejo), plus board
   cards labelled `source:supply-shield`.

## Run

```bash
# Diff-scope a PR working tree against main:
devbox run -- iterion run bots/supply-shield/main.bot \
  --var workspace_dir=$(pwd) --var base_ref=origin/main

# Whole-tree audit:
devbox run -- iterion run bots/supply-shield/main.bot \
  --var workspace_dir=$(pwd) --var scope_mode=full

# On-demand from the board / chat:  /shield <scope notes>
```

## Sandbox image

Pins `ghcr.io/socialgouv/iterion-sandbox-sec:edge`, which ships the
scanner toolchain (js-x-ray + osv-scanner + trivy + npm/pip/go SCA). On a
bare host the scanners are absent → `coverage_gate` fails the run rather
than reporting a 0-finding façade. Build it from
[`sandbox/sec/Dockerfile`](../../sandbox/sec/Dockerfile) and
`docker tag` it to the ref above until CI publishes it. The image pin is
the only sandbox setting the bot carries (ADR-082): isolation, the
claude CLI and host-state mounts are platform defaults; the forge-token
env passthrough remains until Phase 3 delivers tokens as file secrets.

## Shared cache

By default the cache lives under `${PROJECT_SCRATCH_DIR}` (sandbox-safe,
per-run). For true cross-run / cross-repo dedup point it at the host-wide
store:

```bash
--var cache_path=$HOME/.iterion/security-cache/packages.jsonl
```

(requires `host_state: auto` + a writable mount, or an unsandboxed run).
See [`skills/package-cache.md`](skills/package-cache.md).

## Forge reporting

`forge_report` posts via the native REST API using a token from the
environment: `GH_TOKEN`/`GITHUB_TOKEN`, `GITLAB_TOKEN`, or
`FORGEJO_TOKEN`/`GITEA_TOKEN`. With no PR context or token it degrades to
local-only (the report stays at `report_path`) — it never fails the run.
Endpoints + the sticky-comment marker protocol are in
[`skills/forge-report.md`](skills/forge-report.md).

## Triggers

V1 ships the on-demand `/shield` command (scope `any` → runs on a PR or a
branch) + board launch. Automatic PR-open/synchronize and push-to-main
triggers (via the inbound-webhook / trigger spine) are a planned
follow-on.

## Skills

`supply-shield` (playbook), `diff-scope`, `js-xray`, `forge-report`,
`malware-signals`, `package-cache`, `lang-js` / `lang-py` / `lang-go` /
`lang-generic`, `iterion-board`. The `lang-*` / `malware-signals` /
`package-cache` skills are shared with `sec-audit-deps`; iterion has no
skill-sharing primitive yet, so they are duplicated per bundle (keep them
in sync when you touch one).
