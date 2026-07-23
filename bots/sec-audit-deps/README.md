# sec-audit-deps

Universal supply-chain malware auditor — a bundled iterion workflow
that enumerates installed dependencies, runs static heuristics
adapted to each ecosystem, hands the structured signals to an LLM
reviewer with a strict JSON output contract, and emits one kanban
issue per dependency flagged MEDIUM or HIGH.

Inspired by
[SocialGouv/no-package-malware](https://github.com/SocialGouv/no-package-malware) —
the static-signals → LLM-with-schema pattern, generalised from npm to
multiple ecosystems and lifted out of the Verdaccio gateway it
originally shipped with.

## What it inspects

| Ecosystem | Manifest | Lifecycle hooks looked for | Vuln DB |
|---|---|---|---|
| npm / yarn / pnpm | `package.json`, `node_modules/**/package.json` | `preinstall`, `install`, `postinstall`, `prepare` | `npm audit --json` |
| pip / poetry / uv | `setup.py`, `pyproject.toml`, installed dist-info | `setup()` calls, custom commands | `pip-audit --format=json` |
| Go modules | `go.mod`, `go.sum`, `vendor/` | suspicious `replace` directives, `init()` side-effects | `govulncheck -json ./...` |
| Generic | lockfiles (all ecosystems) + `node_modules/**/package.json` | `trivy fs --scanners vuln` CVE floor, npm install-hooks, locale/homoglyph anomalies | `trivy fs --scanners vuln` |

Per-ecosystem coverage is documented in `skills/lang-*.md`. Add an
ecosystem by dropping one `skills/lang-<id>.md` (with an
`iterion:heuristics` block) — see *Adding an ecosystem* at the bottom.

## Quick start

```bash
devbox run -- iterion run bots/sec-audit-deps/main.bot \
  --var workspace_dir=$(pwd) \
  --var severity_threshold=medium

# Outputs:
#  - kanban issues: ready / labels = severity:* + type:supply-chain-* + ecosystem:* + source:sec-audit-deps
#  - package cache appended at cache_path (default: run scratch; see below)
#  - markdown export written by llm_review at <workspace_dir>/.sec-audit/deps-findings.md
```

## Cross-run memory — package cache

A package version is a universal artifact: `left-pad@1.3.0` is the
same tarball whether you `npm install` it in repo A or repo B. The
cache defaults to the engine's out-of-tree run scratch dir
(`${PROJECT_SCRATCH_DIR}/sec-audit-deps/cache/packages.jsonl` —
always writable, even in sandbox images pinning a non-host user, but
per-run under a sandbox). For true host-wide reuse across repos, opt
in with
`--var cache_path=$HOME/.iterion/security-cache/packages.jsonl`.

Schema (one JSON object per line, append-only):

```json
{"ecosystem":"npm","name":"left-pad","version":"1.3.0","checksum":"sha256:...","scanned_at":"2026-05-19T10:00:00Z","risk_score":3,"risk_level":"LOW","flags":[],"scanner_version":"sec-audit-deps@0.1.0"}
```

The cache key is `ecosystem:name:version`. The `load_cache` tool node
reads the file and passes the raw JSONL to the `filter_cached` tool
node, which builds an index and splits pending deps into *already
analysed at acceptable scanner_version* (skip) and *new or stale*
(scan).

The cache is **auto-mounted into the sandbox** when
`host_state: auto` is in effect (the default), so sandboxed runs
share the cache transparently. Pass `--sandbox-host-state=none` to
opt out — useful in multi-tenant cloud runners that must not share
operator state.

## Pipeline

```
enumerate_deps (agent: claw + openai/gpt-5.5, readonly)
     — walk lockfiles / node_modules / .venv / vendor → flat dep list + open `ecosystems` list
  └─→ normalize_deps (tool: coerce the LLM dep list into canonical [{ecosystem,name,version,checksum}])
  └─→ run_eco_heuristics (tool: ONE skill-driven node — for each detected ecosystem, read the
        `iterion:heuristics` block from skills/lang-<id>.md, run its SCA scanner, parse the output)
  └─→ run_generic_heuristics (tool: `trivy fs --scanners vuln` lockfile CVE floor + npm install-hooks + locale anomalies)
  └─→ heuristic_join (compute, await: best_effort — merge {eco, generic} signals)
  └─→ load_cache (tool: read the package cache at cache_path → raw JSONL)
  └─→ filter_cached (tool: split enumerated deps into already_scanned[] vs pending[])
  └─→ llm_review (agent: claude_code + opus-4-8, readonly, capabilities board.read/create/label —
        validates signals, scores + buckets LOW/MEDIUM/HIGH, creates kanban issues for MEDIUM+,
        writes deps-findings.md)
  └─→ update_cache (tool: append one JSONL line per analysed package to packages.jsonl)
  └─→ done
```

There is no router fan-out: `run_eco_heuristics` is a single
deterministic tool that dispatches on the open `ecosystems` list. There
is no separate `score_merge` or `export_report` node either — risk
scoring, bucketing, and the markdown report are all produced inside
`llm_review`.

## Adding an ecosystem

Drop a single skill — **no DSL edit, no router branch, no new tool
node**:

1. **Skill**: `skills/lang-<ecoid>.md` — manifest path, lockfile path,
   lifecycle-hook patterns, and an `<!-- iterion:heuristics ... -->`
   data block listing the SCA scanner command(s) to run. Mirror
   `lang-js.md`.

2. **Parser (only if the scanner's output format is new)**: extend the
   output-parsing tail of the `run_eco_heuristics` tool. Existing
   formats (npm-audit / pip-audit / govulncheck JSON) are already
   handled, so a new ecosystem reusing one of them needs no code.

`enumerate_deps` populates the open `ecosystems` list;
`run_eco_heuristics` reads each detected ecosystem's skill and runs its
heuristics block automatically. No per-ecosystem boolean, no router
branch. Pure composition.

## See also

- [sec-audit-source](../sec-audit-source/) — sibling bundle for in-repo source code.
- [docs/security-bots.md](../../docs/security-bots.md) — shared threat model + ops guide.
