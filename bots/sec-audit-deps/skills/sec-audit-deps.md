---
name: sec-audit-deps
description: |
  Operating playbook for the sec-audit-deps bot. Read this first
  when authoring or modifying nodes in main.bot, when running a
  scan and inspecting findings, or when adding a new ecosystem.
  Covers the six execution phases and the contract between them.
---

# sec-audit-deps — operating playbook

Six phases. The static-signals → LLM-with-schema pattern from
SocialGouv/no-package-malware, generalised to multiple ecosystems
and bridged to the iterion kanban board.

## Phase 1 — `enumerate_deps` (claw, readonly)

Output: `{ deps: [{ ecosystem, name, version, checksum, manifest_path, lockfile_path }, ...] }`.

Per ecosystem, the enumeration source is:

| ecosystem | manifest | lockfile | resolution rule |
|---|---|---|---|
| npm / yarn / pnpm | `package.json` | `package-lock.json` / `yarn.lock` / `pnpm-lock.yaml` | walk `node_modules/**` for installed pkgs |
| pip / poetry / uv | `pyproject.toml` / `setup.py` | `poetry.lock` / `requirements.lock` / `uv.lock` | parse lockfile for resolved versions |
| go modules | `go.mod` | `go.sum` | parse go.sum for exact resolved versions |

If `node_modules/` / `vendor/` / `.venv/` are absent the bot warns
and runs on manifest+lockfile inferred versions only (a "shallow"
audit; signals that require tarball inspection are skipped).

`checksum` is sourced from the lockfile (integrity field, sha256,
or `h1:` go.sum hash). When unavailable, the bot computes it from
the installed artifact.

## Phase 2 — `load_package_cache` (compute)

Reads the package cache at `{{vars.cache_path}}` (see
`[[package-cache]]` for the default + the host-wide override) line by
line and builds an in-memory index keyed by
`ecosystem:name:version:checksum`.

Outputs: `{ cache: {<key>: <cached_entry>, ...}, cache_path: "..." }`.

If the file doesn't exist, the index is empty and the cache_path
is recorded so phase 5 can create it.

## Phase 3 — `filter_cached` (compute)

Splits `deps[]` from phase 1 into:
- `already_scanned[]`: cached entry exists AND `cached.scanner_version >= current` AND `now - cached.scanned_at < ttl` (default 30 days).
- `pending[]`: everything else (cache miss, stale, or newer scanner).

The TTL prevents permanent staleness on packages that were "low risk"
two years ago and have since been compromised.

## Phase 4 — `heuristic_scan` (fan_out_all → tool nodes per ecosystem)

Each ecosystem-specific tool node:
1. Takes `pending[]` filtered to its ecosystem.
2. Runs static heuristics + scanner-specific vuln DB (npm audit /
   pip-audit / govulncheck).
3. Emits structured signals per package:

```json
{
  "packages": [
    {
      "name": "left-pad",
      "version": "1.3.0",
      "checksum": "sha256:...",
      "signals": [
        {"id": "install-hook",       "evidence": "package.json:scripts.postinstall=node setup.js"},
        {"id": "eval-on-startup",    "evidence": "node_modules/left-pad/setup.js:14"},
        {"id": "obfuscated-string",  "evidence": "high entropy in setup.js:22"}
      ],
      "heuristic_score": 35
    }
  ],
  "errors": []
}
```

The catalogue of signal ids is in `[[malware-signals]]`. Ecosystem
skills (`[[lang-js]]`, `[[lang-py]]`, `[[lang-go]]`,
`[[lang-generic]]`) document which scanners + how to interpret
their output.

## Phase 5 — `llm_review` (claude_code, readonly, board.create + board.label)

Receives the structured signals. Reads `[[malware-signals]]` for the
canonical signal catalogue and applies the LLM-reviewer prompt from
the system block. Emits one verdict per package:

```json
{
  "name": "left-pad",
  "version": "1.3.0",
  "checksum": "sha256:...",
  "risk_score": 25,
  "risk_level": "LOW",
  "summary": "Install hook runs a small setup script; no network calls; no obfuscation triggers fired in context.",
  "flags": [
    {"type": "install-hook", "severity": "low", "description": "..."}
  ],
  "files_audited": ["node_modules/left-pad/setup.js"]
}
```

The LLM CAN read package files (read_file tool) to confirm or
discount signals. It MUST NOT execute any code. Tools are
`bash, read_file, glob, grep` only.

For each package whose `risk_level` lands MEDIUM or HIGH (after
score merge in phase 6), the node creates a kanban issue. Label
convention:
- `severity:<level>` — same scale as sec-audit-source
- `type:supply-chain-<signal-id>` — primary flag (e.g.
  `type:supply-chain-install-hook`)
- `ecosystem:<id>` — `npm`, `pypi`, `gomod`, …
- `source:sec-audit-deps`

Title: `<ecosystem> · <name>@<version> — <one-line risk summary>`.

## Phase 6 — `score_merge` (compute) + `update_package_cache` (tool) + `export_report`

`score_merge`:
- For each package: `risk_score = max(heuristic_score, llm.risk_score)`.
- Bucket: `<= 20 → LOW`, `<= 50 → MEDIUM`, `> 50 → HIGH`.

`update_package_cache`:
- Appends one JSONL line per analysed package to the package cache
  at `{{vars.cache_path}}`. Atomic via temp file
  + rename (POSIX guarantees).
- Format: see `[[package-cache]]` for the exact schema.

`export_report`:
- Markdown summary at
  `{{workspace_dir}}/.sec-audit/deps-findings.md`.

## Discipline that keeps the FP rate low

- **Heuristics emit signals, not verdicts.** A package can have 5
  signals and still be LOW risk if context exonerates them.
- **The LLM reviewer can downgrade but never upgrade beyond what
  the merged max(score) allows.** This prevents LLM speculation
  inflating risk.
- **Cache hits skip the LLM entirely.** Re-scanning a HIGH-risk
  package without code change wastes tokens; the operator can
  force a rescan by deleting that line from `packages.jsonl`.
- **Per-package issue, not per-signal.** A package with 5 signals
  is one kanban issue with 5 flags in the body, not 5 issues.

## Cross-bundle conventions

- Issue labels start with `source:sec-audit-deps` so a remediation
  bot can filter to supply-chain findings only.
- `findings.md` exported alongside the boards updates is the same
  shape as `sec-audit-source` so downstream tooling can consume
  either.
