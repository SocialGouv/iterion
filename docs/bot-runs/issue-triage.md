[← Bot runs](README.md)

# issue-triage — Triagy

Single-shot card router: fired by the trigger spine on `triage:auto`
(consumed one-shot), reads ONE board card, stamps the handler bot
(`set_bot`) + `source:issue-triage`, comments its rationale, leaves the
card in inbox. Companion of the ingest author-trust gate (trusted
authors → `triage:auto`, external authors → `needs:approval` + studio
"Approve & triage").

## 2026-07-22 — treatment follow-through: 3 triaged cards driven to delivery (runs 019f8949 / 019f8979 / 019f898e)

- Status: **validated** (triage→ready→dispatch→feature-dev→delivery, end-to-end)
- Versions: iterion main @0da792861 → @e6f419892 · feature-dev v2
- Method: cards #250/#246/#202 dragged to ready one wave at a time; native
  dispatcher (studio-embedded) claimed and ran feature-dev per card; runs
  observed live; deliveries landed on local main (cherry-pick when finalize
  couldn't FF). ~$14 total for 3 real engine fixes by Viczei's specs.
- Result: 3/3 features delivered on main — #250 json-field schema type
  union (+ADR-083), #246 quoted loop-cap fix, #202 subbot child Manager
  registration. Cards ended in review with claims released.
- Engine/bot hardening from observed frictions (all committed same-day):
  1. orphan reconciler killed a live dispatcher run 16s after dispatch
     (no flock + slow boot scan) → EngineRunner run-lock + 2-min grace.
  2. stale committed catalog → every worktree run wip-banked a garbage
     branch each 15s heartbeat; the freshness guard had passed on a stale
     go-test CACHE (reads files outside the package) → catalog refreshed;
     follow-up: move the freshness check to a cache-proof surface.
  3. drift-gate precheck false positive on brew-update.yml's
     commit-if-changed `git diff --staged --quiet` → heuristic now demands
     build-failing semantics; regression subtests run the real command.
  4. author-quoted ref `ITER="{{input.iteration}}"` → shellEscape's own
     quotes made python int() fail → probe stuck on "first pass", paying
     verify_build + rewriting verify.sh EVERY pass → ref unquoted across
     6 bots; e2e pins the engine mapping (which was correct). Diagnostic
     candidate: compile warning for quoted refs in tool commands.
  5. mid-flight "recovered finalize" marked the run finalized → the true
     completion finalize skipped and stranded delivery on the per-node
     GC refs → finalize now re-runs when the worktree HEAD moved past the
     recorded FinalCommit.
  6. verify_build generated gateless verify.sh (skill 1b ignored) →
     MANDATORY CI-drift-mirror clause added to the authoring prompts
     fleet-wide.
- Lessons: dispatcher-claimed treatment is solid once the above are in;
  the campaign/precheck tug-of-war (agent stripping "irrelevant" gates)
  is fully resolved by fixing the probe reuse (bug 4) + prompt (6);
  remaining cards (#204/#205/#203) intentionally deferred — the operator's
  checkout moved to a test branch mid-session and fresh worktrees would
  have forked it.

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
