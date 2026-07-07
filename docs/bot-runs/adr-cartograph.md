[← Bot runs](README.md)

# adr-cartograph (Adry) — bilans

ADR cartographer + completeness auditor. Read-only on code, reasonably
idempotent: observes what the code implements and emits committable Nygard
ADRs in `docs/adr/`, plus a feature-completeness audit; files optional
handoff issues to `adr-rechallenge` (re-challenge) and `feature-gap-fill`
(gap completion). Template: docs-refresh (Doki).

## 2026-07-07 — converted to v2 minimal-framing (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — structural validation only this pass: `iterion validate` clean, catalog universality/typing/bundle-consistency green, stub e2e green where wired. NOT yet live-dogfooded in the v2 shape; treat the sections below as describing the RETIRED v1 shape.
- Versions: bot v2.0.0 · iterion worktree branch (rollout of 2026-07-07, see git log)
- Shape: scan_adrs + read-only survey_code + build_manifest (re-globbing the live adr_dir each pass) are kept; the round_robin review/fix relay is replaced by ONE campaign that re-applies the decision-vs-mechanic checks, authors one ADR per commit in stride, and files the rechallenge/feature-gap handoffs itself (board caps). Deterministic scope gate = adr_dir/*.md-only vs run base. 15 nodes → 10 exec; worktree: auto (v1 committed terminally on the live tree).
- Reference proof of the shared mechanism: feature-dev v2 pilot run 019f3bb4 (one pass, 11m33s, 2 in-stride commits, deterministic gate converged — see docs/bot-runs/feature-dev.md) and the Willy/Billy v2 tours.
- Next: a dedicated live dogfood + bilan in this file before the bot counts as validated in its v2 shape.

## 2026-06-22 — 13 ADRs on GLM-5.2, survived anthropic failover (run 019ef04e-a02e)

- Status: **validated (high value)** — authored ADRs 029-041.
- Versions: bot adr-cartograph · iterion v0.16.0 (110ea1c33)
- Method: `iterion run` (CLI, branch `dogfood/wave1`, live-tree docs/adr scope). survey + review on **glm-5.2 via z.ai**; when z.ai's 5h cap hit mid-review, `reviewer_claude` surfaced a graceful **`acknowledge_recovery` human-pause** → resumed → review/fix loop completed on **anthropic forfait** (gpt-5.5 reviewer + opus). 1233 events.
- Result: commit **`86eeb81f9`** "docs(adr): record ADRs 029-041 for sandbox, SSO, quota, and ultracode" — **13 ADRs, +683 lines**, on `dogfood/wave1` (atop current origin/main). Converged to `done`.
- Value: captured the architecture decisions from the recent campaign wave (sandbox, per-org SSO, quotas, ultracode) into proper Nygard ADRs — the canonical "capture decisions after a code-mutating wave" use case.
- Findings: (1) **glm-5.2 survey is very verbose** — `survey_code` ran ~900+ tool calls / ~20 min (many empty greps) before converging, vs opus's tighter surveys; thorough but slow-to-stop on open-ended exploration. (2) **graceful rate-limit recovery** (`acknowledge_recovery` pause) worked here — contrast Nexie's hard fail on the same 429; **inconsistent rate-limit handling across nodes** is the cross-cutting finding.
- Lessons: glm-5.2 is a viable ADR author; budget more wall-clock for its survey. Recovery-pause + resume across a provider failover preserved all survey work.

## 2026-06-14 — finding #6 convergence tuning (runs 019ec2f3, 019ec4ec)

Two prompt-level fixes for #6, each validated by a live re-run on the
aligned tree (8 ADRs committed, 0 ADR-drift, coverage 100%):

- `7546d0d1` — `streak_check.stop` was stricter than its own `approved`
  passthrough (it omitted the low-confidence clause); aligned it so a
  low-confidence rejection is non-blocking (CLAUDE.md asymptote rule).
  **run4 (019ec2f3) still failed** — the blocker was `confidence: high`,
  not low, so the quick-win mis-aimed.
- `61e995bb` — the real root cause: the persistent high-confidence blocker
  was a **GAP** (the survey's file-diff-payload completeness finding), and
  both reviewers were treating the unfixable gap as a blocker ("file an
  issue for / address this gap"). Adry is read-only on code → it cannot
  close a gap → permanent oscillation. `review_system` rule 7: gaps are
  NEVER blockers (confirm-real → ride the deterministic handoff path to
  `feature-gap-fill`; only ADR drift / orphans / format defects block).

**Result: partial.** Rule 7 measurably helped — reviewers now sometimes vote
`approved=true, blocker_count=0` (they never did before) — but **run5
(019ec4ec) still exhausted `review_loop(10/10)`**: the LLM reviewers are
stochastic, and one still occasionally raises a gap as a blocker
(`bot-marketplace-shallow-clone`) despite rule 7, resetting the cross-family
streak. A prompt instruction alone is not a reliable gate.

**Structural fix `09910359` — DONE, and run6 (019ec51f) CONVERGED.** The fix:
stop passing `gaps`/`total_gaps` into the reviewers' `review_input` entirely
(both `alt -> reviewer_*` edges) and clean the review prompts of gap-review
references — gaps now flow `build_manifest -> prepare_commit`'s handoff,
never through the review loop. With the reviewers *structurally* blind to
gaps (not just instructed to ignore them) and rule 6 calibrating ADR prose,
an aligned tree has nothing to block on. **run6 result:** reviewer_claude
approved → reviewer_gpt approved (cross-family, 0 blockers, **0 fixer runs**)
→ streak_check stop → detect_changes → prepare_commit → commit_changes →
done. First clean convergence. prepare_commit ($0.83) committed **no** ADR
change (0 drift on the aligned tree — correct idempotent behaviour, the
`git diff --cached --quiet` guard skipped the commit) and **filed the 3
`type:feature-gap` handoff issues** to the board (`source:adr-cartograph`:
file-diff-payload, ShallowClone-tests, marketplace-transport) — the A→Fini
handoff is validated end-to-end. **#6 is FIXED.**

Minor follow-up: `detect_changes` counted the engine's bot-catalog
regeneration (an unrelated side-effect) as a working-tree change, so it
routed through `prepare_commit` ($0.83) instead of the pure
`update_cache -> done` no-op; the outcome was still correct (no spurious
commit). Filtering the catalog regen (like the cache) in `detect_changes`
would make the aligned re-run a true zero-LLM-commit no-op.

Cost: 6 Adry runs this campaign; survey ~$3.58 + ~10 min each is the floor
(survey_code always runs before the deterministic aligned-check).

## 2026-06-13 — first dogfood, scoped pkg/git (runs 019ec1f8 + 019ec25f)

- **Status: partial** — core machinery validated end-to-end and 2 real bugs
  fixed; the bot did not converge on *this* run (finding #6) — **since FIXED**
  (structural fix `09910359`; Adry converged on run6 019ec51f — see the
  2026-06-14 section).
- **Versions:** bot 0.1.0 · iterion @ `f3289632` (worktree `worktree-adr-bot-suite`, unmerged)
- **Method:** `iterion run bots/adr-cartograph/main.bot --var code_scope_globs='pkg/git/**'`,
  forfait forced (`ITERION_OPENAI_USE_OAUTH=1`, API keys unset), claude_code
  opus-4-8 (scan/survey/reviewer_claude/fix_claude/prepare_commit) + claw
  openai/gpt-5.5 (reviewer_gpt/fix_gpt). No sandbox, no worktree:auto (the
  repo-root sha-cache must persist across runs). Scoped to one decision-rich
  package to bound cost.
- **Result:** **did not converge** — both the authoring run (019ec1f8) and a
  re-run on the now-aligned tree (019ec25f) exhausted `review_loop (10/10)`
  and hit the `fail` backstop. Survey ≈ $3.58 and ~10 min each (pkg/git, 14
  files, tool_max_steps 80). The review loop never reached the cross-family
  double-approval streak (see #6). **But** the run did the valuable work: it
  authored **8 genuinely good ADRs** (`docs/adr/020`–`027`, committed
  `b60106ba`) recording real pkg/git decisions — arg/path-injection
  hardening, IsAncestor-via-merge-base, empty-tree-SHA root baseline,
  C-locale stderr classification, NUL-framed parsing, working-tree (not
  index) panel semantics, main-repo-root gitdir resolution, git-CLI-shell-out
  vs go-git. Format matched the repo's Nygard convention exactly.
- **Value:** high — produced 8 committable ADRs from a cold read of pkg/git,
  with accurate Context/Decision/Alternatives/Consequences + a Re-challenge
  hook each. The survey's `is_mechanic` self-critique correctly flagged 0
  mechanical decisions (all 8 were real). scan_adrs correctly detected the
  historical `002-*` duplicate and computed `next_adr_number`.

### Findings
| # | Finding | Severity | State |
|---|---|---|---|
| 1 | `build_manifest` passed survey JSON via raw `{{!input.x}}` wrapped in literal quotes — LLM prose (apostrophes/parens) broke `bash -c` (`syntax error near ')'`) | real bug | **FIXED** `1bbca1c0` (drop `!` + quotes → `{{input.x}}` routes through shellEscapeValue). docs-refresh shares the latent pattern (only passes apostrophe-free tokens) → cross-bot pitfall added to [workflow_authoring_pitfalls.md](../workflow_authoring_pitfalls.md) |
| 5 | `build_manifest` computed coverage from the **frozen** `scan_adrs.adrs` (entry snapshot), so ADRs the fixers authored mid-loop were invisible → `coverage_pct` froze at 71% < 80% → the gate was unreachable → loop exhausted with 8 good ADRs on disk | real bug, convergence-breaking | **FIXED** `f3289632` (re-glob `docs/adr/*.md` each pass, parse `../../<code>` refs). Proven deterministically + LIVE on run3: coverage 71%→**100%**, drift 8→0 |
| 6 | Even with coverage=100% (aligned tree, 0 drift), the review loop **never reaches cross-family double-approval** — root cause: reviewers treated unfixable GAPS as blockers → oscillate to `review_loop(10/10)` → `fail` | real, **FIXED** | structural fix `09910359` (gaps bypass review_input); **run6 (019ec51f) CONVERGED** — see the 2026-06-14 section |
| 2 | `fix_claude` (claude_code, output-schema+tools → two-pass) hit `StructuredOutput — No such tool available` in Pass 1 (it's a Pass-2 mechanism), then recovered | benign engine quirk, pre-existing (all fix-nodes; `claude_code.go:341`) | note only |
| 3 | `enforce_fix_scope` `git checkout -- <p>` reverts ALL tracked changes outside `docs/adr/`, incl. pre-existing uncommitted WIP (it ate my own mid-run hot-fix of main.bot) | known loop-bot behavior (docs-refresh idem) | mitigation: commit fixes BEFORE resume (in HEAD = not "changed"); don't hot-edit tracked files during a run |
| 4 | `reviewer_gpt` → 401 OpenAI "token is expired" mid-run (forfait `~/.codex` 10 days stale) | infra | operator re-authed codex → resumed on forfait. See workspace memory `project_openai_oauth_token_invalidation` |

### Engine / bot hardening
- `1bbca1c0` — shell-safe data passing in build_manifest (finding #1).
- `f3289632` — build_manifest re-globs docs/adr so coverage tracks authored ADRs (finding #5).
- `b60106ba` — the 8 dogfood-authored pkg/git ADRs (kept for review).

### Lessons for next run (finding #6 is the gate to production)
The review loop must converge to an asymptote; right now it does not. The
coverage gate (#5) is satisfied but the **cross-family double-approval streak
is unreachable** because the LLM reviewers treat ADR-prose imperfections as
blockers and re-litigate every iteration. Directions to try (each needs a
live re-run to validate — tune one at a time):
1. **Calibrate `review_system`**: an ADR is APPROVED when it *factually and
   accurately records the decision the code embodies* (right sections, valid
   Nygard format, code refs resolve). Prose polish / nuance-completeness must
   NOT be blockers. This is the anti-Goodhart line: the gate is accuracy, not
   style.
2. **Treat low-confidence rejections as non-blocking in `streak_check`** (per
   the CLAUDE.md asymptote rule): add `|| input.confidence == 'low'` to the
   stop condition so a noisy nitpick doesn't reset the streak.
3. **Strengthen `prior_pushback` feedback** so a reviewer cannot re-raise a
   resolved item without new evidence (already wired — verify it's actually
   suppressing re-litigation in the prompt).
4. Consider bounding the per-run authoring batch (8 ADRs at once gives the
   reviewers a large surface to nitpick); authoring fewer per pass may
   converge more reliably.
Do **not** "fix" #6 by short-circuiting the review when the manifest is
aligned — that trades oscillation for a bypassed quality gate (Goodhart).
The fix is to make the reviewers converge, not to remove them.

Other operational notes: run Adry on a **clean tree** (finding #3); the
forfait token expires (~hours) so long runs may need a mid-run re-auth +
resume (finding #4); survey cost is the floor (~$3.58/run on pkg/git) since
survey_code always runs before the deterministic aligned-check.
