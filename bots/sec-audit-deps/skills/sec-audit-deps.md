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

## Phase 2 — `normalize_deps` (tool)

Deterministic. Coerces `enumerate_deps`' output into the canonical
`[{ecosystem, name, version, checksum}]` list plus the open
`ecosystems[]` list. Every downstream node reads
`outputs.normalize_deps`, never `outputs.enumerate_deps`.

## Phase 3 — `run_eco_heuristics` → `run_generic_heuristics` → `heuristic_join`

Heuristics run **before** the cache is consulted, not after.

`run_eco_heuristics` (tool) dispatches per detected ecosystem by reading
each `skills/lang-<id>.md`'s `iterion:heuristics` block — there is no
router and no per-ecosystem node. `run_generic_heuristics` (tool) is the
always-on CVE floor: `trivy fs --scanners vuln` over the workspace,
matching every pinned version in the lockfiles against OSV/GHSA/NVD from
a bare checkout. `heuristic_join` (compute) merges both into one signal
set.

Each scanner emits structured signals per package:

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

## Phase 4 — `load_cache` (tool) → `filter_cached` (tool)

`load_cache` reads the package cache at `{{vars.cache_path}}` (see
`[[package-cache]]` for the default + the host-wide override) line by
line and builds an index keyed by
`ecosystem:name:version:checksum`. If the file doesn't exist the index
is empty and the path is recorded so phase 6 can create it.

`filter_cached` then splits the normalized `deps[]` into:
- `already_scanned[]`: cached entry exists AND `cached.scanner_version >= current` AND `now - cached.scanned_at < ttl` (default 30 days).
- `pending[]`: everything else (cache miss, stale, or newer scanner).

The TTL prevents permanent staleness on packages that were "low risk"
two years ago and have since been compromised.

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

## Phase 6 — `update_cache` (tool)

Scoring is folded into `llm_review`, not a separate node: for each
package `risk_score = max(heuristic_score, llm.risk_score)`, bucketed
`<= 20 → LOW`, `<= 50 → MEDIUM`, `> 50 → HIGH`. The markdown summary is
written by the same node to
`{{workspace_dir}}/.sec-audit/deps-findings.md`, and its location comes
back as the `report_path` field of `review_output`.

`update_cache` is the terminal node:
- Appends one JSONL line per analysed package to the package cache
  at `{{vars.cache_path}}`. Atomic via temp file
  + rename (POSIX guarantees).
- Format: see `[[package-cache]]` for the exact schema.

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
