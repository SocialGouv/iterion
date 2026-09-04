---
name: sec-audit-deps
description: |
  Operating playbook for the sec-audit-deps bot. Read this first
  when authoring or modifying nodes in main.bot, when running a
  scan and inspecting findings, or when adding a new ecosystem.
  Covers the execution phases and the contract between them.
---

# sec-audit-deps — operating playbook

The static-signals → LLM-with-schema pattern from
SocialGouv/no-package-malware, generalised to multiple ecosystems
and bridged to the iterion kanban board.

The graph is a single linear chain — no router, no fan-out:

```
enumerate_deps → normalize_deps → run_eco_heuristics
  → run_generic_heuristics → heuristic_join → load_cache
  → filter_cached → llm_review → update_cache → done
```

Note the order: the heuristics run **before** the cache is consulted.
The cache spares the expensive LLM review, not the cheap deterministic
scanners.

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

Normalises `enumerate_deps`' output into the flat `deps[]` the rest of
the chain consumes, and emits the open `ecosystems[]` list that drives
phase 3. Deterministic, no LLM.

## Phases 3–4 — `run_eco_heuristics` + `run_generic_heuristics` + `heuristic_join`

Three deterministic nodes, no LLM and no router.

`run_eco_heuristics` is **one** node that loops over the open
`ecosystems[]` list from `normalize_deps`. For each entry it reads the
`iterion:heuristics` data block out of
`.claude/skills/lang-<id>.md`, runs the SCA scanner that block names
(npm audit / pip-audit / govulncheck / …) and parses the output into
signals. Adding an ecosystem is dropping a `lang-<id>.md` — no DSL edit,
unless the scanner's output format is one no parser handles yet.

`run_generic_heuristics` is the always-on floor that runs whatever the
ecosystems are: `trivy fs --scanners vuln` over the lockfiles, plus the
npm install-hook and non-ASCII/locale name checks.

`heuristic_join` (`await: best_effort`) merges the two into the single
`{eco, generic}` signals object the reviewer receives.

Each emits structured signals per package:

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

## Phase 5 — `load_cache` (tool)

Reads the package cache at `{{vars.cache_path}}` (see
`[[package-cache]]` for the default + the host-wide override) line by
line and builds an in-memory index keyed by
`ecosystem:name:version:checksum`.

Outputs: `{ cache: {<key>: <cached_entry>, ...}, cache_path: "..." }`.

If the file doesn't exist, the index is empty and the cache_path
is recorded so `update_cache` can create it.

## Phase 6 — `filter_cached` (tool)

Splits `deps[]` from phase 1 into:
- `already_scanned[]`: cached entry exists AND `cached.scanner_version >= current` AND `now - cached.scanned_at < ttl` (default 30 days).
- `pending[]`: everything else (cache miss, stale, or newer scanner).

The TTL prevents permanent staleness on packages that were "low risk"
two years ago and have since been compromised.

## Phase 7 — `llm_review` (claude_code, readonly, board.read + board.create + board.label)

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
`bash, read_file, write_file, glob, grep` only — `write_file` exists
solely so the node can write its own markdown report.

Scoring happens **inside this node**, not in a downstream compute node:
start from the input's `heuristic_score`, subtract the weight of each
signal ruled a false positive, add 10 when `install-hook` +
`network-on-import` + `obfuscated-string` are all valid (the textbook
supply-chain shape), clamp to `[0, 100]`, then bucket
`<=20 LOW`, `<=50 MEDIUM`, `>50 HIGH`.

The same node writes the markdown report to
`{{workspace_dir}}/.sec-audit/deps-findings.md` after its board calls.

For each package whose `risk_level` lands MEDIUM or HIGH (above
`severity_threshold`), the node creates a kanban issue. Label
convention:
- `severity:<level>` — same scale as sec-audit-source
- `type:supply-chain-<signal-id>` — primary flag (e.g.
  `type:supply-chain-install-hook`)
- `ecosystem:<id>` — `npm`, `pypi`, `gomod`, …
- `source:sec-audit-deps`

Title: `<ecosystem> · <name>@<version> — <one-line risk summary>`.

## Phase 8 — `update_cache` (tool)

- Appends one JSONL line per analysed package to the package cache
  at `{{vars.cache_path}}`, creating the parent directory if needed.
  Append-only, relying on POSIX write atomicity up to `PIPE_BUF`.
- An empty `cache_path` is not an error: the node reports
  `appended_count: 0` and skips the write.
- Format: see `[[package-cache]]` for the exact schema.

There is no separate score-merge or report-export node — both jobs
belong to `llm_review` (phase 7), and `update_cache` is the last node
before `done`.

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
