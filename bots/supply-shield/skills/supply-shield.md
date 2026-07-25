---
name: supply-shield
description: |
  Operating playbook for the supply-shield bot (Shieldy). Read this
  first when authoring or modifying nodes in main.bot, when running a
  malware gate on a PR, or when adding an ecosystem/analyzer. Covers
  the pipeline phases and the contract between them.
---

# supply-shield — operating playbook

A PR/push-driven, diff-scoped MALWARE gate. The static-signals →
LLM-with-schema pattern from SocialGouv/no-package-malware, scoped to
what a change adds, fed back onto the forge, and deduped through a
shared cache. The CVE-focused sibling is `supply-shield-cve` (Vulny).

## Pipeline

1. **diff_scope** (tool) — resolve base..head, find changed lockfiles
   (globs in `[[diff-scope]]`), emit their unified diff. `scope_mode=full`
   → whole-tree.
2. **enumerate_deps** (claw, readonly) — parse the diff into the newly
   added/upgraded `(ecosystem, name, version, checksum)` list + the open
   `ecosystems` list. Full mode walks the tree.
3. **run_eco_heuristics** (tool) — per ecosystem, run the scanners listed
   in `lang-<id>.md`'s `iterion:heuristics` block (npm audit / pip-audit /
   govulncheck) and parse known-vuln output into signals.
4. **run_jsxray** (tool) — @nodesecure/js-x-ray AST analysis over npm
   install scripts + entry points → `jsxray.json` (see `[[js-xray]]`).
   The npm malware floor.
5. **run_generic_heuristics** (tool) — node_modules install-hooks,
   locale/homoglyph anomalies, trivy fs CVE baseline.
6. **heuristic_join** (compute) — merge {eco, generic} signals.
7. **coverage_gate** (tool) — anti-façade. HARD-FAIL when the always-on
   floor produced nothing; banner partial analyzer gaps (e.g. js-x-ray
   absent). A missing scanner must never read as "0 malware found".
8. **load_cache** + **filter_cached** — the shared store (`[[package-cache]]`).
   A `(ecosystem,name,version,checksum)` analysed once (kind=malware) is
   reused across runs/PRs/repos. Only cache misses become `pending[]`.
9. **llm_review** (claude_code, board.create+board.label) — validate each
   signal against source, fold in js-x-ray warnings, DEEP-READ install
   scripts/entry points when inconclusive, emit verdicts, create kanban
   issues for MEDIUM+, write the findings report. `risk = max(heuristic,
   llm)`; a `checksum-mismatch` forces HIGH.
10. **sarif_gen** (tool) — SARIF 2.1.0 from the verdicts.
11. **forge_report** (claude_code) — post sticky comment + inline review +
    code-scanning SARIF on the PR (native API — `[[forge-report]]`).
    Degrades to local-only with no PR/token.
12. **update_cache** (tool) — append one kind=malware line per analysed
    package.

## Verdict discipline (keeps FP rate low)

- **Heuristics + js-x-ray emit signals, not verdicts.** Context can
  exonerate them (test fixtures, dev-only paths, minified bundles).
- **The reviewer downgrades freely but never inflates beyond
  `max(heuristic, llm)`** — except `checksum-mismatch`, which forces HIGH
  (a republished `name@version` is a supply-chain attack).
- **Cache hits skip the LLM entirely.** Re-scanning an unchanged version
  wastes tokens; force a rescan by deleting its line from the cache.
- **One issue per package, not per signal.**

## Risk buckets

| score | bucket |
|---|---|
| 0–20 | LOW |
| 21–50 | MEDIUM |
| 51–100 | HIGH |

## Board + forge conventions

- Board labels: `severity:<level>`, `type:<primary_signal_id>`,
  `ecosystem:<id>`, `source:supply-shield`.
- Forge: a single sticky comment (marker `<!-- supply-shield -->`,
  updated in place on re-runs), inline review on HIGH packages, SARIF to
  code-scanning. See `[[forge-report]]`.

## Adding an ecosystem or analyzer

- A new ecosystem's known-vuln scanner → drop a `lang-<id>.md` with an
  `iterion:heuristics` block (+ a parser in `run_eco_heuristics` only if
  its output format is new).
- A new lockfile format to diff-scope → add its glob to
  `[[diff-scope]]`'s `iterion:lockfiles` block.
- Never enumerate languages/package managers in the DSL — stack knowledge
  lives in skills; deterministic gates verify coverage.

## See also

- `[[diff-scope]]`, `[[js-xray]]`, `[[forge-report]]`,
  `[[malware-signals]]`, `[[package-cache]]`,
  `[[lang-js]]`, `[[lang-py]]`, `[[lang-go]]`, `[[lang-generic]]`.
