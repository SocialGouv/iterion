---
name: supply-shield-cve
description: |
  Operating playbook for the supply-shield-cve bot (Vulny). Read this
  first when authoring or modifying nodes in main.bot, when running a
  CVE gate on a PR, or when adding an ecosystem/scanner. Covers the
  pipeline phases and the contract between them.
---

# supply-shield-cve — operating playbook

A PR/push-driven, diff-scoped CVE gate. Same pipeline as the malware
sibling supply-shield (Shieldy), but the axis is KNOWN VULNERABILITIES.
It scopes to what a change adds, matches against the advisory DBs,
validates applicability, feeds the result back onto the forge, and
dedups through the shared cache.

## Pipeline

1. **diff_scope** (tool) — resolve base..head, find changed lockfiles
   (globs in `[[diff-scope]]`), emit their unified diff. `scope_mode=full`
   → whole-tree CVE baseline.
2. **enumerate_deps** (claw, readonly) — parse the diff into the newly
   added/upgraded `(ecosystem, name, version, checksum)` list + the open
   `ecosystems` list.
3. **run_eco_heuristics** (tool) — per ecosystem, run the deeper SCA
   scanners listed in `lang-<id>.md` (npm audit / pip-audit / govulncheck)
   and parse advisories into `vuln-db-known` signals.
4. **run_generic_heuristics** (tool) — the always-on CVE floor: trivy fs
   **and** osv-scanner over lockfiles (cross-ecosystem, no install). See
   `[[cve-scanning]]`. (Also keeps the install-hook/locale signal.)
5. **heuristic_join** (compute) — merge {eco, generic} signals.
6. **coverage_gate** (tool) — anti-façade. HARD-FAIL when the CVE floor
   produced nothing; banner partial per-ecosystem gaps. A missing scanner
   must never read as "0 CVEs found".
7. **load_cache** + **filter_cached** — the shared store (`[[package-cache]]`).
   Consults only `kind: cve` lines; short TTL + `advisory_db_date` because
   advisories land daily.
8. **llm_review** (claude_code, board.create+board.label) — validate each
   advisory against the resolved version (affected range / fixed version /
   Go reachability), de-dup cross-scanner duplicates, emit verdicts, create
   kanban issues for MEDIUM+, write the findings report. Risk anchors to
   the worst applicable advisory severity.
9. **sarif_gen** (tool) — SARIF 2.1.0 from the verdicts.
10. **forge_report** (claude_code) — sticky comment + inline review +
    code-scanning SARIF on the PR (native API — `[[forge-report]]`).
    Degrades to local-only with no PR/token.
11. **update_cache** (tool) — append one `kind: cve` line per package.

## Verdict discipline

- **A `vuln-db-known` signal is a PUBLISHED advisory** — HIGH-confidence.
  Mark not-applicable ONLY when: dev/test-only, the installed version is
  outside the advisory's affected range, or a `fixed_version` is already
  in use. Go: `called:true` (reachable) is more urgent than imported-only.
- **De-duplicate** advisories reported by multiple scanners (trivy + osv
  frequently overlap) — one flag per advisory id.
- **Risk anchors to the worst applicable severity** — any CRITICAL/CVSS≥9
  → HIGH.
- **Cache hits skip the LLM.** The short TTL + `advisory_db_date` force a
  rescan once advisories have moved on.

## Risk buckets

| score | bucket |
|---|---|
| 0–20 | LOW |
| 21–50 | MEDIUM |
| 51–100 | HIGH |

## Board + forge conventions

- Board labels: `severity:<level>`, `type:cve`, `ecosystem:<id>`,
  `source:supply-shield-cve`.
- Forge: a single sticky comment (marker `<!-- supply-shield-cve -->`),
  inline review on HIGH packages, SARIF to code-scanning (trivy/osv emit
  SARIF natively). See `[[forge-report]]`.

## Adding an ecosystem or scanner

- A new ecosystem's SCA scanner → drop a `lang-<id>.md` with an
  `iterion:heuristics` block (+ a parser in `run_eco_heuristics` only if
  its output format is new).
- A new lockfile format to diff-scope → add its glob to
  `[[diff-scope]]`'s `iterion:lockfiles` block.
- The universal floor (trivy + osv) is cross-ecosystem — never enumerate
  languages in the DSL.

## See also

- `[[diff-scope]]`, `[[cve-scanning]]`, `[[forge-report]]`,
  `[[malware-signals]]`, `[[package-cache]]`,
  `[[lang-js]]`, `[[lang-py]]`, `[[lang-go]]`, `[[lang-generic]]`.
