---
name: sec-audit-source
description: |
  Operating playbook for the sec-audit-source bot. Read this first
  when authoring or modifying nodes in `main.bot`, when running a
  scan and inspecting findings, or when writing custom matchers.
  Covers the five execution phases, the contract between phases,
  and the discipline that keeps false-positive rate low.
---

# sec-audit-source — operating playbook

Six phases. Each one has a single responsibility. Crossing the
responsibility boundary (e.g. having `triage` filter for FPs, or
having `revalidate` invent severities) breaks the FP-reduction
guarantee that makes this bot useful — keep them separate.

The 6th phase (FileRecords) is a cache pass that lets re-runs skip
the expensive revalidate step on unchanged files. See
[[file-records]].

## Phase 1 — `detect_tech` (claw, readonly)

Outputs the structured techstack: `{ langs: [], frameworks: [],
package_managers: [], build_systems: [] }`. Used by the router in
phase 2 to gate per-language scanners.

Be **conservative**: only include a language/framework if the
evidence is unambiguous (e.g. `go.mod` + `*.go` files for Go;
`package.json` with a `next` dep for Next.js). False positives here
waste scanner time but don't corrupt results — false negatives skip
real coverage. Bias toward inclusion when uncertain.

## Phase 2 — `run_scanners` (router fan_out_all, tool nodes)

Each branch is a `tool` node that:
1. Runs its scanner(s) with deterministic flags.
2. Writes raw JSON to `{{vars.scan_dir}}/<branch>.json`.
3. Returns `{ scanner: "<id>", finding_count: N, json_path: "..." }`.

The branch MUST NOT swallow scanner failures: a non-zero exit code
from the scanner becomes a workflow failure. The retriable
classifier in iterion's runtime will retry transient errors
automatically.

Branches in V1:
- `run_generic_scanners` — always-on (gitleaks + trivy + semgrep auto)
- `run_js_scanners` — gated on `tech.langs ∋ js`
- `run_go_scanners` — gated on `tech.langs ∋ go`
- `run_python_scanners` — gated on `tech.langs ∋ python`

## Phase 3 — `triage` (claw, readonly)

Reads:
- the scanner outputs from `{{vars.scan_dir}}`,
- `.sec-audit/fp-known.yaml` (if present),
- `[[finding-taxonomy]]` (this skill),
- one or more `[[lang-*]]` skills (which match the detected stack).

Emits a flat list of normalized **candidates**:

```json
{
  "id": "C-001",
  "finding_type": "ssrf",         // from [[finding-taxonomy]]
  "severity": "high",             // low | medium | high | critical
  "file": "pkg/server/proxy.go",
  "line_range": [120, 145],
  "matcher": "trivy-config-allowoutbound",
  "scanner": "trivy",
  "snippet": "...",
  "scanner_rationale": "...",
  "exploit_hypothesis": "...",    // 1-2 sentences — how a human would attack this
  "status": "candidate" | "known_fp"
}
```

Rules:
- Every candidate MUST have `finding_type` from
  `[[finding-taxonomy]]`. If a scanner emits something exotic, map
  it to `config` or `other` rather than inventing a category.
- Severity is set from the scanner's signal **moderated** by the
  hypothesis: a "high" CVSS finding with no concrete exploit path
  drops to "medium".
- `status: known_fp` is set when the candidate matches an entry in
  `fp-known.yaml` on `(finding_type, file, line_range ⊆ entry, matcher)`.
  These are written to the output for trace but NOT promoted to the
  board.

## Phase 3.5 — `filter_cached_files` (tool) + `merge_verdicts` (compute)

Inserted between `triage` and `revalidate` to short-circuit the
expensive revalidate phase on files that haven't changed since the
last analysis. See [[file-records]] for the full design.

`filter_cached_files`:
- Computes the current sha256 of each file referenced by the
  triage candidates.
- Loads `.sec-audit/files/<sha1(path)>.json` records.
- Emits two streams: `fresh_candidates` (file changed, cold, or
  stale TTL/scanner_version) and `cached_candidates` (cache hit
  — its verdict is replayed from the FileRecord's latest history
  entry into `cached_verdicts`).

`revalidate` then operates on `fresh_candidates` only.

`merge_verdicts` concatenates the fresh verdict stream from
`revalidate` with the replayed `cached_verdicts`, producing a
single combined `confirmed[] / dismissed[] / uncertain[]` for
`report_card`.

Tuning knobs (workflow vars):
- `records_ttl_days` (default 30) — beyond this age, even an
  unchanged file is re-revalidated to catch newly-added matchers.
- `scanner_version` — lexical compare; a bump invalidates all
  caches downstream of the bump.

## Phase 4 — `revalidate` (claw judge, two-phase)

Two-phase as documented in
[memory feedback_judge_two_phase]:
1. **Pass 1 — promote**: for each candidate, judge concludes
   `confirm` (real positive), `dismiss` (false positive), or
   `uncertain` (cannot tell without runtime context).
2. **Pass 2 — self-critique**: judge re-reads its own pass-1
   verdicts looking for façades (cf. [memory
   feedback_workflow_facade_goodhart]) and dismissals that
   over-rely on regex signature absence. Demotes false negatives,
   promotes survivors.

`dismiss` verdicts with a strong rationale are appended to
`fp-known.yaml` (capability: `file_edit` on that exact path only).

The judge sees the candidate body and the file context (±50 lines)
but NOT the LOW/MED/HIGH bucketing applied later — that's
deterministic, per [memory feedback_goodhart_data_hiding].

## Phase 5 — `report_card` (claude_code, board.create + board.label)

Per surviving finding:
1. `mcp__iterion_board__create_issue` with:
   - `title`: short noun phrase ("SSRF in admin proxy handler")
   - `body`: markdown — file/line anchor, exploit hypothesis,
     reproduction recipe, fix sketch
   - `state`: `ready`
   - `labels`: `severity:<lvl>`, `type:<finding-type>`,
     `source:sec-audit-source`, `scanner:<id>`
2. Capture `issue.id`; passed to `export_report` for the markdown
   table.

## Phase 6 — `update_file_records` (tool)

After `report_card` succeeds, appends one history entry per file
mentioned in the run's candidates to
`<workspace>/.sec-audit/files/<sha1(rel_path)>.json`. The
entry captures the file's current content_hash, the candidates
targeting it, the verdicts produced for those candidates, and the
board issue ids created. See [[file-records]].

Markdown export is folded into `report_card` (claude_code has
`write_file`); there is no separate `export_report` node.

## Discipline that keeps the FP rate low

- **Never let the LLM invent matchers.** The scanners produce raw
  signal; the LLM normalises and explains, it doesn't grep.
- **Never collapse "uncertain" into "dismiss".** Promote uncertain
  to the board with a `severity:medium` + `triage-uncertain` label
  so a human reviews.
- **Always check `fp-known.yaml` BEFORE the LLM sees the candidate**
  — saves tokens and prevents the LLM from contradicting curated
  human knowledge.
- **Two-phase judge is non-optional** for scans larger than ~20
  candidates. Skip it for tiny PR-mode runs.

## Cross-bundle conventions

- Findings go to the kanban via `[[iterion-board]]` (see that
  skill for tool input shapes).
- Label conventions match `sec-audit-deps`:
  `severity:*`, `type:*`, `source:*`.
- The `findings.md` schema is shared between the two bots so
  downstream tooling can consume either.
