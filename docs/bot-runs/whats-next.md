# Nexie — `whats-next` run bilans

Conversational co-CTO (v3: ONE agent in a chat loop — board intelligence,
ticket curation against code reality, dispatch, and the roadmap-study
cycle per ADR-075). See [bots/whats-next/](../../bots/whats-next/).
Bilans before 2026-07-07 cover the v1 form state machine (survey →
priorities form → roadmap → review form → emit → dispatch pickers).

## 2026-07-16 — v3 first study turn: adaptive pivot instead of re-study (run 019f69c8)

- Status: **validated (turn 1, high value)** — the fan-out path stayed
  unexercised by the bot's own (correct) judgment; session handed to the
  operator at the arbitrage pause.
- Versions: bot whats-next 0.3.0 · iterion worktree `a75e4fa5e` (v3 branch).
- Method: CLI `iterion run` from the v3 worktree, `--store-dir` the main
  workspace store (operator-studio visible), `--var workspace_dir=<main
  repo>`, seed « Quels sont les prochains chantiers ? ». claude_code +
  opus-4-8, effort ultracode, budget 45m/$15/80 steps.
- Result: turn 1 in 4m20 / $1.43 / 11 tool calls, then
  `paused_waiting_human` (standby). `dispatched_ids: []` — nothing
  promoted before arbitrage, as contracted.
- Value: instead of mechanically re-running the 3-audit fan-out, Nexie
  **found yesterday's epic** (`epic:chantiers-2026h2`, 8 chantiers, ~67
  tickets), mapped C1–C8 with per-chantier counts, and answered with the
  STATE, not a list: review wave 1 parked (~7 done-but-unmerged tickets),
  factory OFF (0 ready, no dispatcher process — she checked), wave 2
  (10 `feature-dev` tickets, now×5/next×5) undispatched. Recommendation
  ordered (drain review first, then top-3 tier-now), plus real
  **pushback**: the tier-now tickets are chantier-sized, not
  feature-sized, and lack the `## Context/## Done criteria/## Verify`
  frame — dispatching them as-is to one feature-dev run is a façade
  risk; she proposes decomposition. Ends on two sharp decision blocks
  (A: verify+close the review wave with `comment_issue` traces — asks
  before the bulk, guardrail respected; B: decompose C1 vs dispatch
  whole) + 4 aligned quick_replies.
- Turn 2 (operator picked decision A — verify the review lot): $1.28,
  9 `get_issue` + 2 git checks, ZERO mutations. Per-ticket verdict
  table with line-level evidence; caught 2 tickets parked in `review`
  without the work done, and re-asked before the batch because the
  action set had changed (re-bucketing was not in decision A) — the
  bulk ritual held. **But 3 of 9 verdicts were right for the wrong
  tree**: her shell ran cwd-relative in the run's working directory (a
  worktree cut from the morning's origin/main) while the audited
  workspace was live main — NATSBus (`d346b6ec7`), T-42 IRRef
  (`cd333b3b3`) and LaunchSpec (`38799ea38`) had merged mid-session,
  so her "not delivered / façade" calls on those three were stale.
  The operator's own base-drift phenomenon, biting the AUDIT phase.
- Turn 3 (operator-delegate correction: "your base was stale — re-verify
  anchored, cite the HEAD"): $1.04, the correction loop closed clean.
  Re-verified on `main @ 9d74c1e7d` with the HEAD cited, self-diagnosed
  the wrong-tree first pass, found all 3 flips on her own — and
  out-verified the reviewer on NATSBus (impl merged `d346b6ec7` but
  `NewNATSBus(` wired nowhere → residual C2 wiring, matching the
  15/07 session note). Re-proposed the corrected batch (4 proven
  closes + 2 rebuckets + 3 operator calls), still zero mutations.
  **Both skill fixes hot-loaded mid-session via the resume re-mirror**:
  the anchor discipline (this turn's method) and the
  quick_replies-as-buttons rule (chips now carry complete decision
  answers — "Applique le lot complet (4 close + 3 close+followup +
  2 backlog)" etc.).
- Turn 4 (operator named the batch): $0.93, 21 board mutations —
  **all ground-truth verified on the store**: 7 tickets → `done`, each
  with one `comment_issue` trace citing commit + `main @ 9d74c1e7d`;
  2 mis-parked tickets → `backlog` with a why-comment; 2 follow-ups
  created (SSRF cloud-e2e C4, NewNATSBus wiring C2) with the full
  `## Context/## Done criteria/## Verify` frame and vocabulary labels,
  bot left unassigned as instructed. The review lane is drained. She
  also wrote the wrong-tree lesson into her CONTEXT_BRIEF unprompted.
  Board-execution stage: VALIDATED. Session total: 4 turns, ~$4.67.
- One more cwd slip, mechanical this time: the CONTEXT_BRIEF was
  written under the memory key derived from her turn-1 `ws=$(cwd)`
  choice (the worktree — a temporary tree) instead of the prompt's
  resolved `workspace_dir`; the turn-4 write reused the turn-1
  derivation from conversational memory. Relocated by hand to the
  workspace key; the anchor discipline (already shipped) covers the
  class going forward.
- Operator UX feedback (jo): a two-choice arbitrage turn read as
  "answer in free text" — expected radio + submit. Bot-side mitigation
  = the chips fix (`eb07d1f22`); structural fix filed as framed board
  ticket `native:7849f5f9` (studio renders grouped decision blocks as
  a form). ask_user stays single-question by contract.
- Consent boundary (harness): the operator-delegate session could
  steer verification but NOT confirm the bulk close/rebucket — the
  auto-mode classifier requires the operator to NAME the batch for
  mass-modifying pre-existing tracker items (same lesson as the
  2026-07-15 GitHub closes). Even a standing "do whatever you deem
  relevant on the board" delegation did not clear that bar (single
  ticket creation did). Board execution therefore awaits the
  operator's own click/message — plan bulk closes into the initial
  ask, or hand the operator the exact batch to send.
- Fixture run (019f6b42, /tmp/iterion-probe-nexie, $0.79): a 6-file
  virgin repo + 3-ticket board, launched cwd≠workspace ON PURPOSE.
  She correctly did NOT fan out ("repo volontairement fin — je
  calibre, pas de faux chantiers de prestige"), read the tree
  directly, caught the planted obsolete ticket with anchored evidence
  (`95896e1` @67b7c35), classified the ADR-blocked cleanup as
  non-dispatchable, named the real blind spot (zero tests), and wrote
  her memory brief under the CORRECT fixture key — the anchor fixes
  hold. Scale-trigger validated in both directions.
- Scale run (019f6b46, workspace=.works/claw-code-go, 2 turns ~$2.09):
  **the mechanical fan-out validated** — 3 parallel read-only
  sub-agents with fully conformant briefs (Area per canonical axis,
  absolute Workspace, evidence-cited envelope) into which she
  propagated the anchor rule herself ("never trust cwd"). Edge found:
  she ended turn 1 with audits in flight and the sub-agents did not
  survive the chat pause — turn 2 detected the loss, said so, and
  relaunched the missing two while delivering the synthesis on the
  solid base (full v3 shape: tension line, 8 chantiers now/next/later,
  quick-wins routed per catalog — Doki/Testy/Adry/Fini —, argued
  top-3, honest blind spots, 3 decision blocks, zero board writes
  before arbitrage). Skill patched: await audits within the turn;
  relaunch, never invent, on an inherited loss.
- Findings / misses:
  - **Anchor bug (fixed)**: verification commands must anchor at
    `workspace_dir` (`git -C`, absolute paths), never the shell cwd —
    the run can execute from a different tree. Skill fix `1b4c30fae`
    (playbook + repo-survey; skills sit outside the resume hash and
    re-mirror on resume, so the live session gets it next turn). The
    matching one-line prompt hardening in main.bot is deferred until
    the session closes — editing the source would invalidate the
    run's resume hash and break the operator's next chat answer.
  - 3-audit Task fan-out not exercised live (adaptive skip — a fresh
    study would have duplicated the 2026-07-15 one; the doctrine's
    "skipping a stage is judgment" clause worked as intended). Still to
    be exercised on a repo/board without a prior study.
  - Workspace memory: only the legacy root-level
    `memory/CONTEXT_BRIEF.md` exists; the v3 recipe targets
    `memory/whats-next/` (absent) so Nexie correctly skipped — but the
    legacy brief is invisible to v3. Follow-up: migrate the brief or
    teach the recipe the legacy fallback.
  - Only the playbook skill was Skill-loaded; behavior still matched
    roadmap-synthesis/operator-arbitrage/factory-ops (the prompt +
    playbook summaries carried enough). Watch whether deeper turns load
    the domain skills when they act (board execution, bilan).
- Engine hardening surfaced by this run:
  - **Bundle skills mirror died on `.gitkeep`** (`fix(runtime)`
    `a75e4fa5e`): a non-`.md` placeholder in `bots/*/skills/` made the
    directory form and the flat alias collide on the same path — every
    fresh-workspace run of whats-next AND secured-renovacy crashed at
    startup since the flat-alias mirroring landed. Mirror now skips
    non-markdown entries (regression test added).
  - Zombie `iterion server` found listening on :7799 against a deleted
    worktree store (leftover from a 2026-07-11 session) — flagged, not
    killed; Nexie herself spotted it during her operational-state check
    and reasoned about the store mismatch.
- Lessons for next run: exercise the full fan-out on a study-less
  fixture (the ticket's e2e criterion); answer decisions A/B in the
  studio chat to validate board execution + bilan stages on the real
  lot; consider the legacy-brief fallback before the next session.
- Post-merge threads (same day, after #214 landed @9cc03584b):
  - **Claw roadmap exploited** (study run 019f6b46, +1 arbitrage turn
    $1.25): answered with her own recos (reliability axis, C1-only
    ready lot), the 2 relaunched audits' delta integrated (binary
    quick-win self-invalidated — already gitignored; C3 re-scoped on
    `internal/auth` 5 src/0 test; NEW C9 release/distribution — single
    tag ~110 commits behind). Durable output committed in the claw
    repo: `docs/ROADMAP-2026H2.md` (9 tiered chantiers + 4 framed
    ticket bodies). Note: the full deliverable (7241 chars) rode the
    MID-TURN narration (chat bubble) while the turn's `reply` was a
    recap claiming "livré dans le fil" — accurate but envelope-fragile;
    watch whether operators miss narration-borne content.
  - **Wave-2 decomposition** (fresh session 019f6b71, 2 turns ~$5.96 —
    CONTEXT_BRIEF continuity picked decision B up without re-brief):
    re-audited terminal-state semantics on fresh main via a 3-agent
    fan-out (4 contradicting terminal predicates, `runtime.ErrorCode`
    never persisted), sliced C1 into 3 framed feature-dev tickets with
    CASCADING BLOCKERS (f26342ab now/8 → d9ac6af9 next/7 → 9f550afa
    later/6), recreated the regen-catalog quick-win as e2950020
    (closed 8ca25d98 superseded, traced — no body-edit tool exists),
    converted both chantier-sized parents to `horizon:theme` tracking
    cards (bot cleared). Zero promotions — the factory stays
    operator-owned. Total v3 dogfood: 4 runs, 11 turns, ~$17.5; zero
    anchoring drift across three distinct workspaces after the fixes.

## 2026-07-08 — first cloud-prod session + skills-format engine fix (run 019f412x)

- Status: **validated after two engine fixes** — Nexie ran conversationally in
  cloud prod for the first time; two blockers found + fixed on the way.
- Versions: bot whats-next 0.2.0 · iterion cloud prod `:edge` (fixes deployed mid-session up to 6a03866)
- Method: cloud prod (ovh-prod), studio → What's Next → "What's next?". No repo
  connected (empty workspace), so board+survey only.
- Result: after the fixes, Nexie loaded the board (empty), surveyed the
  workspace, wrote a CONTEXT_BRIEF to workspace memory, and returned a precise
  "J0 — board+repo empty, tell me the project" recommendation. Board MCP tools
  (list_issues/list_labels/set_bot/transition_issue) all wired.
- Engine hardening surfaced by this run:
  - **Nexie couldn't launch in cloud at all**: the What's Next SessionLauncher
    posts `createRun({file_path})` with no source, and handleLaunchRun rejects a
    bare file_path in cloud; whats-next is a non-embedded bundle. Fixed:
    handleLaunchRun/handleResumeRun now resolve a catalog `bots/<name>` path (or
    `bot_id`) off the pod FS and carry BotID so the runner mirrors the bundle's
    skills — the same gesture the webhook/scheduler/board launchers use. SPA
    passes `bot_id`.
  - **`Skill(whats-next)` → "Unknown skill"** even though the mirror reported
    `skills mirrored=11`: iterion mirrored flat `<name>.md` files, but Claude
    Code's Skill tool only discovers the directory form `<name>/SKILL.md` (Agent
    Skills spec). Fixed: mirror always writes the directory form (satisfies both
    claude_code and claw). Nexie ran WITHOUT its playbook skill until this
    landed — it still functioned via board tools + bash, just degraded.
- Lessons for next run: launching a catalog bundle by `bot_id` is the clean cloud
  path (no source upload). A repo-connected session (forge-synced board) is the
  next thing to exercise — this one had an empty workspace.

## 2026-07-07 — v2 conversational rewrite, first live session (run 019f3beb)

- Status: **validated (high value)** — replayed the exact scenario that killed v1 the same morning (run 019f3afc-era session 019f3b6b: operator asked for quick wins, got raw checkbox forms, gave up) and delivered everything v1 couldn't, in 2 turns.
- Versions: bot whats-next **0.2.0** (v2 single-agent rewrite) · iterion c5960220e (worktree branch `worktree-nexie-v2-conversational`)
- Method: CLI `iterion run` from the feature worktree, `--store-dir /home/jo/lab/ai/iterion/.iterion` (operator-visible store + REAL board, 13 backlog items), claude_code + claude-opus-4-8 forfait. Turn 2 via `iterion resume --answer message=…`. Containment: no dispatch instructed; one explicitly-instructed close of a verified-obsolete ticket.
- Result: **2 turns, ~1m40 each** (turn 1: 09:31:33→09:33:15; turn 2: +99s LLM, 6007 tok, $0.40). Session left in standby (paused at `chat`) — the living co-CTO surface, reachable from the studio.
- Value:
  - Turn 1 ("quels quick wins ?"): analysed all 13 items, correctly split the 10 `source:evolve` epics from real candidates, flagged `0bc0c9ab` (Revi pass) as **blocked** (reviews branches that never ran) and `2304ee89` (Willy gap) as **probably obsolete — citing the commits that obsoleted it** (dc22b626 explore-mode, fb60b075 v2 rewrite, ADR-057) *unprompted*, recommended `2047e34d` (deadline robustness) with a sharp rationale, and offered next steps as quick-reply chips. Recommendation-first, French-mirrored — the exact contract.
  - Turn 2 ("vérifie en détail, si confirmé ferme-le, ne touche à rien d'autre"): re-verified against git history (timeline v0.5.0 explore-mode → v2 rewrite 2026-07-03, prescribed mechanism gone, intent covered by v2), closed `2304ee89` → `done` **with a trace comment** (`comment_issue` + `close_issue`), touched nothing else. Guarded-curation behavior exactly as designed.
  - Session continuity confirmed: turn 2 reused turn 1's claude session (`_session_id` loop-edge mapping) — zero re-analysis.
  - 3 `assistant_text` narration events landed in the transcript (B2 works live); `quick_replies`/`dispatched_ids` emitted as real arrays after the prompt-contract fix.
- Findings / engine hardening (both fixed in-branch):
  1. **json-typed schema fields can arrive stringified** (`"[\"…\"]"`) from claude_code's formatting pass — first golden recording caught it. Hardened the studio quick_replies reader (server-side `extractStringIDs` already tolerated it) + pinned "real arrays" in the prompt. (`c5960220e`)
  2. **Linked-worktree promotion claimed foreign workspaces** (HIGH): this run, launched from the Claude Code session worktree with `worktree: none`, got stamped `Worktree=true` with the session worktree as work_dir — close-time finalization would have created an `iterion/run/*` branch there and best-effort **FF'd the operator's checked-out `main` onto the feature branch's HEAD**. Root cause: `runPersistWorkspace`'s promotion matched ANY linked worktree, not just delegated ones. Fixed: promotion now requires explicit `WithWorkDir` delegation (dispatcher/studio paths keep it); pinned by `TestRunPersistWorkspace_WorkspaceAuthority`. (`c01f96fd5`; this run's run.json neutralized by hand.)
- Lessons for next run: keep dogfooding from the operator store — the real board is what makes the curation behaviors measurable. The studio process must run the new build to render narration + chips.

### Round 2 (same session, turns 3–5): bulk confirmation + real dispatch

- **Turn 3** (explicit bulk: "ajoute `epic:evolve-2026h2` aux 10 source:evolve, c'est moi qui définis cet epic, vas-y"): `list_labels` FIRST (vocabulary ritual), then 10× `set_labels`, existing labels preserved — and the confirmation ritual **adaptively skipped** because the instruction itself was the confirmation ("vas-y" + enumerated scope). Defensible reading of the contract; noted, not fixed.
- **Turn 4** (ambiguous destructive bulk: "fais le ménage dans le backlog"): full narrated triage (the 10 epics = "tes épics définis il y a 2 min — je n'y touche pas"; duration-deadline = keep; Revi 0bc0c9ab = genuinely non-actionable, correct reasoning), then **`ask_user` with 3 structured options** (`close`/`downgrade`/`keep`, free-text off) — the B1 envelope persisted on the pause and the CLI answer (`--answer ask_user_response=close`) resumed the SAME turn, which commented + closed the ticket. 30 s for the answer→close round-trip. The guarded-bulk ritual works end-to-end live.
- **Turn 5** ("dispatche le 2047e34d"): transitioned to `ready` (bot `whole-improve-loop` + bot_args intact — parked for the next dispatcher session; none was running, by design of this dogfood), `dispatched_ids` emitted, and Nexie **spontaneously warned about Willy's watchexec footgun** (live-tree edits under `task studio:dev` drain the run) — catalog knowledge surfacing exactly when relevant. 25 s turn.
- Turn latencies round 2: 24–40 s per turn (vs ~1 m 40 for the analysis-heavy round 1 turns).
- Note: `watched_issue_ids` stays null on CLI-driven runs — the dispatched_ids→watch stamp is a runview-service hook; studio-launched sessions get the WatchPanel wiring.
- Session left in **standby** again; backlog now: 10 epic'd evolve tickets + `2047e34d` in ready. Both cleanup closes (`2304ee89`, `0bc0c9ab`) carry trace comments.
- ADR: the pattern is recorded as [ADR-060 — conversational single-agent bots](../adr/060-conversational-single-agent-bots.md).

## 2026-06-22 — z.ai/GLM-5.2 dogfood + mid-run anthropic failover (run 019ef04f-a5ff)

- Status: **validated (high value)** — full chain on the z.ai/GLM-5.2 stack, completed across a live provider failover.
- Versions: bot whats-next 0.1.0 · iterion v0.16.0 (110ea1c33)
- Method: `iterion run` (CLI; project store `~/.iterion/projects/…-bots-whats-next`, surfaced in studio `global-active`). `claw` gpt-5.5 forfait (explore/propose/assign) + `claude_code` **glm-5.2 via z.ai** (emit_action). When z.ai's 5h cap hit mid-`emit_action`, the node went **`fail_resumable` → resumed on anthropic/opus forfait** (.env flipped). Every gate driven via `--answers-file`.
- Result: complete chain `explore → ask_priorities → propose_roadmap → human_review → emit_action → ask_which_to_process → ask_continue(close)` → **finished**. Captured the campaign priorities into a sharp roadmap and **created 5 backlog tickets**, incl. the standout `native:dfb12ef5` "Make claude_code provider fallback swap provider-specific models" + `native:dfb5f3f7` "Refresh z.ai GLM-5.x provider metadata". Auto-hygiene archived 2 stale findings.
- Value: the roadmap + dispatch-ready tickets shaped the rest of the campaign; the failover ticket it filed was **proven necessary** by the z.ai cap that hit the same run minutes later.
- Findings: (1) **emit_action 429 → hard `fail_resumable`** (no graceful recovery-pause, unlike Adry's reviewer which paused with `acknowledge_recovery`) → **rate-limit handling is inconsistent across node types**. (2) non-fatal `Tool error: Skill — Unknown skill: whats-next` (skills not registered as slash-skills; agent recovered by reading the mirrored dir). (3) finalize created a storage branch even for a board/memory-only bot.
- Lessons: resume-on-failover works cleanly from checkpoint (zero work lost); `--answers-file` with proper JSON types (bool/array) is reliable for every gate (`context`; `approved`+`selected_titles`; `action:close`).

## 2026-06-13 — full survey→roadmap→triage dogfood (run 019ec0a1)

- Status: **validated (high value, several findings)**
- Versions: bot whats-next 0.1.0 · iterion 9197bcfd (v0.14.0)
- Method: launched via Studio `/whats-next` ("Explore" focus), driven through every
  human gate with Playwright. Backends: `claw` gpt-5.5 (explore, propose_roadmap,
  assign_to_bots), `claude_code` opus-4.7 (emit_action, triage_board). No sandbox.
  Workspace store (`.iterion`), no `--store-dir`. ~68k tokens counted, ~28 min wall
  (mostly inter-gate human latency; cost not cleanly tracked for the claw+claude_code mix).
- Result: ran the **complete chain** — `explore → ask_priorities → propose_roadmap →
  human_review → emit_action → (dispatch picker) → assign_to_bots → triage_board →
  standby`. Materialised **5 backlog issues** (board 88→93), reassigned 1 via triage,
  ended on standby (the intended reachable co-CTO state). Zero node failures.
  Audit markdown was produced as a local run artifact (`docs/plans/whats-next-20260613-130840.md`)
  and was not committed with this bot-run note.

### Value (genuinely high)
- **Survey is accurate and grounded**: enumerated all 13 `bots/*/main.bot` paths,
  the real stack (Go 1.26 / React 19 / pnpm10), even spotted `.botz/review-pr/main.bot`.
  It even picked up the `docs/bot-runs/<bot>.md` bilan requirement I had committed
  minutes earlier — i.e. it read the current CLAUDE.md, not a cache.
- **Roadmap is high-leverage and honest**: next_action = the real open HIGH
  `source:sec-audit-self` SSRF (`pkg/server/runs_preview.go`) + path-traversal
  (`pkg/server/runs_files.go`) findings, with concrete acceptance criteria; correctly
  **referenced existing board items** (`native:f3a888dc`, `native:3a81df64`,
  `native:26870`) instead of always inventing new ones; correctly **deferred board
  cleanup** to the in-session `triage_board` step rather than emitting it as a ticket.
- Issues created with clean labels (`source:whats-next`, `horizon:{next-action,short-term,long-term}`,
  `axis:{security,reliability}`) and per-item bot assignees; long-term themes left unassigned.
- Findings auto-hygiene was conservative (archived 2 of 11, safe under-archive default).

### Findings / misses
1. **`set_bot`/`list_labels` truly absent at runtime — STALE INSTALLED BINARY (medium
   — dev-infra, ROOT-CAUSED).** Both `emit_action` and `triage_board` routed the bot
   via the human `assignee` field and reported *"this board build registers no
   set_bot/list_labels MCP tools"*. **The agent was correct, not confabulating.** The
   studio under `task studio:dev` runs via `go run`, whose `os.Executable()` is a
   volatile build path, so `proc.LocateIterionBinary()` skips it and falls back to the
   **installed `/usr/bin/iterion`** to serve the `__mcp-board` stdio MCP. That installed
   binary was **stale (commit 62aac3cc, pre-dating `set_bot`/`list_labels`)** and its
   `tools/list` advertises only **7 tools** (`assign_issue, close_issue, create_issue,
   get_issue, list_issues, set_labels, transition_issue`) — no `set_bot`, no
   `list_labels`. Proof: `ITERION_BOARD_CAPS=<6 caps> /usr/bin/iterion __mcp-board`
   tools/list → 7 tools; the **freshly-built** binary (current code) → all 9. So the
   bot prompt (`emit_action_system` l.709-713 already maps `item.assignee → set_bot`)
   and the `iterion-board` skill are **correct**; the agent faithfully used what the
   stale board server offered (assignee fallback, which the dispatcher honours). **Fix
   is operational, not a bot/code change:** refresh the installed binary
   (`sudo cp ./iterion /usr/bin/iterion`) or run the studio with
   `ITERION_BIN=<fresh>` so delegated subprocesses match the running code. **This skew
   affects EVERY delegated capability** (board MCP, the sandboxed `__claw-runner`, the
   `__mcp-ask-user` server) — see the CLAUDE.md note added under the live-dogfood
   section. (The chained findings #2–#5 below are downstream of routing via `assignee`
   and may differ once the binary is fresh and `set_bot` is used.)
2. **`emit_action` dedup miss (low-medium — bot improvement).** It created a *new*
   "Restore sec-audit-source scanner output under dispatcher sandbox" item even though
   its own body says *"Fix the existing backlog item native:f3a888dc"* — duplicating
   the pre-existing ticket instead of promoting/linking it. (It did correctly avoid
   recreating `native:3a81df64`.) The promote-don't-duplicate rule needs to be firmer.
3. **Non-deterministic empty dispatch (medium — reliability).** Submitting the dispatch
   picker **empty** made `assign_to_bots` move **all 5** issues to `ready` this run; the
   stale 2026-06-04 session's *identical* empty reply moved **nothing**. Same input,
   opposite outcome.
4. **Dispatch picker fell back to free-text (low — UX).** The picker rendered its
   free-text JSON-array fallback instead of a checkbox list — its own helptext says
   *"this free-text shows only when the upstream summary message is missing"*. Likely
   the upstream trigger of finding #3.
5. **Phantom `[]` watched item (low — UX).** The empty dispatch submit created a 6th
   "dispatched" watch entry rendered as `[]` → *"API error 404: issue not found"*,
   spamming the console with repeated 404s.
6. **Minor.** Transient `GET /api/runs/{id}` 404 console error right after launch
   (run.json flush race); the human-gate form lagged the backend pause by a moment
   (a page reload always showed it).
7. **Bot discovery double-counts across roots (low — engine/botregistry).** The
   auto-regenerated `iterion-bot-catalog.md` (regen runs before every Nexie launch)
   grew a **duplicate Revi / `review-pr` card** — one for `bots/review-pr/main.bot`,
   one for a stray gitignored `.botz/review-pr/main.bot` (a leftover `iterion bundle
   pack` artifact, also surfaced in Nexie's own survey). `pkg/botregistry` treats both
   `bots/` and `.botz/` as discovery roots but **does not dedupe by bundle name**, so a
   local packed copy of a source bot shows up twice in the catalog Nexie routes from.
   Worked around locally by removing the stray `.botz/review-pr` + restoring the regen
   artifact; the proper fix is dedupe-by-bundle-name (precedence `bots/` > `.botz/`) in
   `pkg/botregistry` discovery — deferred to a focused follow-up.

### Engine hardening
- **Dev-mode delegated-binary skew (finding #1):** real, root-caused — the studio
  under `go run` serves `__mcp-board` (and the claw runner / ask-user MCP) from the
  **stale installed `/usr/bin/iterion`**, which silently lacks capabilities added since
  the last install (`set_bot`/`list_labels` here). Code is correct; refresh the binary
  or set `ITERION_BIN`. Documented in CLAUDE.md so it doesn't re-bite the campaign.
- **`pkg/botregistry` cross-root dedupe (finding #7): FIXED** — `discoverBots` now
  dedupes by normalized bundle name across roots (precedence `bots/` > `.botz/`), with
  a regression test (`TestList_DedupesSameBotAcrossRoots`). So a stray packed `.botz/`
  copy can no longer duplicate a catalog card.

### Lessons for next run
- Apply the finding-1 fix to the bot before the next Nexie run, then confirm the
  `bot` field (not `assignee`) is set on materialised issues.
- At the dispatch picker, type explicit IDs or `"all"`; do **not** submit empty
  (ambiguous + creates the phantom `[]` watch).
- A stale paused Nexie session from a prior repo layout will mislead — "Abandon &
  restart" for a clean survey against the current tree.
