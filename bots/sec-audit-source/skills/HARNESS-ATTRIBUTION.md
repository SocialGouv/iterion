---
name: harness-attribution
description: Provenance notice for Seki's threat-model, vuln-scan, triage, and patch skills — records that they are adapted from Anthropic's defending-code-reference-harness. Reference documentation, not a runtime skill.
disable-model-invocation: true
---

# Harness attribution

Seki's `threat-model`, `vuln-scan`, `triage`, and `patch` skills are
adapted from **Anthropic's `defending-code-reference-harness`** — a
reference implementation of an LLM-driven defensive code-review
pipeline.

- Upstream repo: https://github.com/anthropics/defending-code-reference-harness
- Upstream license: see the upstream repository for the canonical
  notice.
- Source path of the ported skills:
  `.claude/skills/{threat-model,vuln-scan,triage,patch}/SKILL.md`
  in the upstream tree.

## Ported skills

| Seki skill | Upstream skill | Adaptation |
|---|---|---|
| `threat-model.md` | `.claude/skills/threat-model/SKILL.md` | Output captured in `.sec-audit/context.md` (markdown only, no JSON emission). Maps section-4 threats to iterion's twelve-category `finding-taxonomy.md`. Findings reference the kanban surface defined in `iterion-board.md` with `severity:*`, `type:*`, `source:sec-audit-source` labels. |
| `vuln-scan.md` | `.claude/skills/vuln-scan/SKILL.md` | Scanner set delegated to the `iterion:scanners` data blocks in `lang-*.md` (no duplicated list). Validation step wired to Seki's `scan_health` smoke gate. C/C++/ASAN guidance replaced by Go/TS focus areas. Adds an explicit single-file re-attack lens used during remediation. |
| `triage.md` | `.claude/skills/triage/SKILL.md` | JSON checkpoint emission dropped — verdicts flow through Seki's `merge_verdicts` and `report_card` nodes. The N-vote majority is reframed as a "disprove" protocol (cross-link `disprove-voting.md`). FP memory is iterion-native (`.sec-audit/fp-known.yaml` via `fp-memory.md`). |
| `patch.md` | `.claude/skills/patch/SKILL.md` | Language scope narrowed to **Go and TS/JS** (the reference's C/C++/ASAN ladder is replaced by `go build`/`go test` and `npm|pnpm build|test` or `tsc --noEmit`). Re-attack tier cross-links Seki's per-category recipes in `reattack-oracles.md`. Adds a hard-stop policy for crypto findings (`crypto-handling.md`) and an explicit reviewer-isolation contract (`reviewer-isolation.md`). |

## What is retained from the reference

- The "threats persist after patching; vulnerabilities disappear"
  doctrine of `threat-model`.
- The "scanner is WRONG by default" adversarial stance of `triage`'s
  verifiers.
- The sixteen exclusion rules used to filter false positives.
- The two-pass dedup pattern (deterministic clustering, then a
  semantic LLM agent given minimal context).
- The severity-from-preconditions table (0 / 1-2 / 3+ × access level)
  with a one-step cap on threat-model boost.
- The reviewer-isolation pattern (reviewer sees only
  `{file, line, category, diff}`) in `patch`.
- The "all subagent Task calls for a phase in one message" parallelism
  contract.

## What is replaced

- **Language scope.** The reference targets C / C++ via ASAN-driven
  fuzzing. Seki targets Go and TS/JS via static scanners plus the
  per-category re-attack lens of `vuln-scan` and `reattack-oracles.md`.
- **State persistence.** The reference uses `./.triage-state/` and
  `./.patch-state/` checkpoint trees managed by a
  `python3 .claude/skills/_lib/checkpoint.py` helper. Seki uses its
  existing `file-records.md` (per-file append-only JSON) and
  `fp-memory.md` (curated YAML) memories — no checkpoint script,
  no JSON output files at the skill level.
- **Crypto policy.** The reference has no crypto carve-out. Seki
  introduces a hard-stop (`crypto-handling.md`): crypto findings are
  proposed but never auto-applied; verdict is forced to `uncertain`
  and the kanban issue is labeled `human-review` + `risk:crypto`.
- **Trust-boundary input.** The reference reads
  `<target>/THREAT_MODEL.md`. Seki reads
  `.sec-audit/context.md` (same shape, iterion-native path).

## License

Refer to the upstream repository
(https://github.com/anthropics/defending-code-reference-harness) for
the canonical license terms covering the original implementation.
This adaptation carries iterion's licensing for the Seki bot bundle
and inherits the upstream notice for the adapted content.

## See also

- `threat-model.md`, `vuln-scan.md`, `triage.md`, `patch.md` — the
  four ported skills.
- `disprove-voting.md` — the N-voter protocol triage uses.
- `crypto-handling.md` — the hard-stop iterion adds.
- `reviewer-isolation.md` — the reviewer contract patch enforces.
- `reattack-oracles.md` — per-category re-attack recipes.
- `sec-audit-source.md` — the six-phase orchestration these skills
  plug into.
