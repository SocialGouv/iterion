[← Bot runs](README.md)

# issue-triage — Triagy

Single-shot card router: fired by the trigger spine on `triage:auto`
(consumed one-shot), reads ONE board card, stamps the handler bot
(`set_bot`) + `source:issue-triage`, comments its rationale, leaves the
card in inbox. Companion of the ingest author-trust gate (trusted
authors → `triage:auto`, external authors → `needs:approval` + studio
"Approve & triage").

## 2026-07-22 — first dogfood: 7 real GitHub-synced cards (runs 019f88e7…019f88f2)

- Status: **validated** (local engine; the cloud spine shipped the same
  day, un-dogfooded — see "next run")
- Versions: bot 0.1.0 · iterion worktree-issue-auto-triage @5594b6395
- Method: `iterion issue import --forge github --repo SocialGouv/iterion
  --min-author-role` default (developer) → 17 cards (open ones stamped
  `triage:auto` — Viczei + devthejo both hold write; closed ones landed
  in done, unstamped, author persisted on every card). Dedicated studio
  from the worktree binary on :4899 bound to the WORKSPACE store
  (`--store-dir $PWD/.iterion`, operator-visible), sandbox off
  (`ITERION_SANDBOX_DEFAULT=none`), triage fired via the spine by
  re-emitting a card event (identity label update). Backend claw,
  final model pin `openai/gpt-5.5`.
- Result: 7/7 cards triaged end-to-end (spine → consume → direct launch
  with `vars.issue_id` → get_issue → skill catalog → set_bot →
  set_labels → comment). ~$0.005–0.012 and ~20–40s per card, ≈5¢ total.
  Cards stayed in inbox; label consumed exactly once per fire; re-adding
  `triage:auto` re-armed correctly.
- Value: exactly the intended operator loop — fresh issues arrive
  pre-routed; launching = one drag to Ready. Classifications all
  plausible: Renovate Dependency Dashboard → `secured-renovacy`,
  "improve existing pipeline-board behaviors" → `whole-improve-loop`,
  concrete bugs/features (#202/#204/#205/#246/#250) → `feature-dev`.
- Findings / misses (bot-side, all fixed in-run):
  - `max_iterations: 1` in the budget killed the run after the FIRST
    LLM step — dropped (duration+cost caps suffice for a 1-node bot).
  - claw with an empty `tools:` list = a TOOL-LESS LLM call; the
    capability-granted board tools are only appended when `tools:` is
    non-empty. `tools: [skill]` fixed it (and the skill tool is needed
    anyway for the catalog).
  - gpt-5.4-mini (claw auto-default) skipped procedure steps (no skill
    read, no labels, no comment) even under a MANDATORY-labelled system
    prompt. gpt-5.5 + a NUMBERED tool sequence in the USER prompt
    follows the contract fully → model pinned.
- Engine hardening (found by this run):
  - **`set_bot`'s own input schema suggested the obsolete underscore
    spelling** (`e.g. feature_dev`) — both models faithfully copied it
    over the catalog's dash form. Fixed in boardops; a stale example in
    a tool description outweighs a whole skill.
  - The whats-next catalog regen was mono-bundle; generalized to every
    bundle shipping the static template (issue-triage ships its own).
  - Same-day follow-up (@8f2b10934): the CLOUD half of the board spine
    (board_events poll-tail + CAS cursor + atomic ConsumeLabels +
    team-scoped /api/v1/triggers) so the flow works on prod — written,
    tested (mongo conformance), NOT yet dogfooded.
- Lessons for next run:
  - Dogfood on the PROD CLOUD instance (operator's ask): enable Triagy's
    board trigger for the team (`POST /api/v1/bots/issue-triage/triggers/
    from-invocation`), sync a repo with `MinAuthorRole` set, verify an
    external author's issue parks (`needs:approval` + banner) and that
    "Approve & triage" fires exactly one triage run across replicas.
  - The trigger-label nudge (identity label re-set) is the manual replay
    gesture; document it for operators (already in the vocabulary skill).
  - Watch for `axis:` label reuse quality on boards with a rich existing
    vocabulary — this board's history made the model reuse `area:*`
    (anti-pattern) once before the source-labels rule was tightened.
