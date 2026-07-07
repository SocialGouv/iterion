# devbox-setup (Devy) — dogfood bilan

Index + template: [README.md](README.md). Newest first.

## 2026-07-07 — converted to v2 minimal-framing (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — structural validation only this pass: `iterion validate` clean, catalog universality/typing/bundle-consistency green, stub e2e green where wired. NOT yet live-dogfooded in the v2 shape; treat the sections below as describing the RETIRED v1 shape.
- Versions: bot v0.1.0 (unchanged) · iterion worktree branch (rollout of 2026-07-07, see git log)
- Shape: Audited for the ADR-058 rollout: already minimal (4 nodes, deterministic verify_devbox + commit_devbox). No structural change — deliberate no-op.
- Reference proof of the shared mechanism: feature-dev v2 pilot run 019f3bb4 (one pass, 11m33s, 2 in-stride commits, deterministic gate converged — see docs/bot-runs/feature-dev.md) and the Willy/Billy v2 tours.
- Next: a dedicated live dogfood + bilan in this file before the bot counts as validated in its v2 shape.

## 2026-06-14 — failed at detect_stack: claude_code structured output is best-effort (runs 019ec59d, 019ec5a1)

- Status: **failed** (first node, both attempts). Not fixed — actionable finding
  recorded instead (low-value target: iterion already has a devbox.json).
- Method: dedicated worktree studio :4899 (C082 worktree binary), `worktree: auto`
  on a clean iterion clone, `sandbox: iterion-sandbox-sec:edge`, `merge_into=none`.
- Failure: `detect_stack` (agent, `backend: claude_code`, `model: opus`,
  `output: detect_output` — a simple 5-field flat schema) failed structured-output
  validation with **every** required field missing (`summary`, `packages`,
  `build_cmd`, `test_cmd`, `e2e_cmd`), on both node-level retries.
- Root cause (from run.log): the agent did ~8 tool calls (read manifests,
  Taskfile, package.json, the mirrored `.claude/skills/devbox-setup.md`), then
  emitted a **1486-char prose message** ("🏁 stream close: Result already
  populated (1486 chars)") that is **not** conforming `detect_output` JSON →
  validation failed. iterion retried the node once (same prose) → `failed_resumable`.
- **Finding — claude_code `--json-schema` is best-effort, not hard-enforced.**
  Unlike `claw` (which forces structured output via a tool call), the claude_code
  CLI only *instructs* the model with the schema; a heavy-exploration opus agent
  can drift to a prose summary and never emit the JSON object. Simple
  claude_code structured-output nodes emit clean JSON (proven: the C082 minimal
  bot's `{issue_id,created,note}`, Seki `report_card`), so this bites
  **exploratory** nodes specifically.
  - Hypothesis tested + **disproven**: `reasoning_effort: low` was NOT the cause
    — bumping to `medium` produced the identical all-fields-missing failure
    (change reverted; bot is back to `low`).
- Recommended fixes (untested, not applied):
  1. Prompt-harden `detect_system`/`detect_user` to end with a forceful "Respond
     with ONLY the detect_output JSON object — no prose, no markdown fences," the
     idiom reliable claude_code structured-output nodes use.
  2. Engine: on a claude_code structured-output-invalid result, re-prompt the
     same session with "your previous message was not valid JSON for the schema;
     emit ONLY the JSON" before failing the node (a general reliability win for
     all claude_code structured-output nodes, not just Devy).
- Lessons for next run: don't re-test on iterion (it has devbox.json → low signal);
  point Devy at a repo with NO devbox.json. The detect_stack reliability gap must
  be fixed (prompt and/or engine) before Devy is dependable.
