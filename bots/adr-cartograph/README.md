# adr-cartograph (Adry)

Read-only ADR cartographer + completeness auditor — **v2
minimal-framing** (ADR-058).

## What it does

Adry walks the code as currently implemented and produces committable
**Architecture Decision Records** in `docs/adr/` (Nygard format). Every
ADR Adry writes is a **constat** — a record of the decision the code
embodies, with the trade-off it implies and the alternative that was
not taken — so a future maintainer can re-challenge it. It also
surfaces feature-completeness gaps and hands them off as board issues.

## Shape (v2 — deterministic manifest + one campaign agent)

```
scan_adrs ──▶ survey_code ──▶ build_manifest ──▶ campaign
campaign ──▶ scope_check ──▶ verify_build ──▶ verify_run ──▶ gate
gate ──▶ mark_issue_for_review ──▶ update_cache ──▶ done   when converged
gate ──▶ build_manifest   as continuation_loop(max_passes) — re-diff, next pass
```

- **`scan_adrs`** — deterministic ADR inventory: front-matter parse,
  `next_adr_number`, duplicates, inter-run sha-cache pre-verification.
- **`survey_code`** — read-only adaptive survey producing structured
  `decisions[]` (three-check discipline from
  `skills/decision-vs-mechanic.md`) and `gaps[]`
  (`skills/completeness-taxonomy.md`), evidence-cited.
- **`build_manifest`** — deterministic differ: decisions vs the ADR
  directory → `decision_drift`; ADR citations vs the filesystem →
  `adr_orphans`; plus `gaps_for_handoff`, `adrs_aged_out` and the
  mechanical `coverage_pct`. **Re-globs the live `adr_dir` each pass**,
  so ADRs the campaign authors are seen and coverage rises.
- **`campaign`** — one adaptive claude_code agent: re-applies the three
  checks against the cited code, authors/updates ONE ADR per commit
  (`docs(adr): NNN …` + `Bot: adr-cartograph` trailer), dismisses
  mechanics honestly, files the `type:feature-gap` /
  `type:adr-rechallenge` handoff issues.
- **`scope_check`** — deterministic containment: only `.md` under
  `adr_dir` (+ the cache file) may change since the run base; anything
  else fails the gate and bounces back.
- **`verify_build` + `verify_run`** — the shared stack-agnostic build
  gate (cheap universal floor for a docs-only bot).
- **`gate`** — `converged = build green ∧ scope_ok ∧ adrs_aligned ∧
  coverage_pct ≥ coverage_target_pct`.

The v1 alternating cross-family review/fix relay (alt →
reviewer_claude/gpt → streak_check + dismissed-ids/pushback/chronic
accumulators → fix_claude/gpt → prepare_commit → commit_changes →
detect_changes → enforce_fix_scope) is retired — see the header comment
in `main.bot` and git history.

## Inputs (main vars)

| Var | Default | Description |
|---|---|---|
| `adr_dir` | `docs/adr` | ADR location (Nygard convention) |
| `code_scope_globs` | `""` | Survey surface (empty = whole workspace minus excluded_dirs) |
| `scope_notes` | `""` | Operator attention pin |
| `coverage_target_pct` | `80` | Mechanical coverage the gate requires |
| `rechallenge_after_days` | `0` | >0 files rechallenge issues for older ADRs |
| `diff_since` | `""` | Incremental prioritisation hint |
| `audit_cache_path` | `.adr-cartograph-cache.json` | Inter-run cache (gitignore it) |
| `baseline` | `""` | Known pre-existing failures to SKIP (G5) |
| `max_passes` | `6` | Continuation-loop cap |

## Run

```bash
iterion run bots/adr-cartograph/main.bot \
  --var scope_notes='Document the storage-engine and transport decisions' \
  --var rechallenge_after_days=90
```

Skills shipped: `adry`, `decision-vs-mechanic`, `completeness-taxonomy`,
`adr-format`, `adr-scope-detection`, `verify-build`. See
[main.bot](main.bot) for the full DSL.
