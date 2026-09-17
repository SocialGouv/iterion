# Evoly (`evolve`) — bot run bilan

Strategic / architectural evolution partner. Surveys a mature repo,
accumulates a long-horizon vision in **per-bot** memory across sessions,
elicits operator context, and proposes evolutions as dispatch-ready
backlog tickets + findings for Nexie. See
[bots/evolve/README.md](../../bots/evolve/README.md). Append newest-first.

---

## 2026-09-17 — profile 2: a one-quarter vision on a toy CLI, four operator questions, three tickets, closed at the continue gate (run 01a0afa6-aaf5)

- Status: **validated end to end** — `survey` → `investigate` (four `ask_user` questions,
  one at a time) → `synthesize_vision` → `review_claude` (mono) → `human_review_vision`
  (approved) → `carry_vision` → `propose_evolutions` → `emit_backlog` (3 tickets) →
  `ask_continue` (done) → done. The new `revise_vision -> vision_not_converged` exit was not
  reached — the vision was approved on its first turn; the strict dry run is its witness (the
  all-false pass ends `failed` on the declared fail node where main's copy died
  LOOP_EXHAUSTED).
- Versions: bot evolve 0.1.1 (`dsl: 2` + the VISION_NOT_CONVERGED exit, wave 3b of #1344) · iterion `5fa790d82` for the bots; the engine the branch binary `v3.157.0+c31559517` (built on the wave-3a branch from main at v3.157.0; main is at v3.159.0 as this is written, one PR of engine work ahead), served through the host's Anthropic-compatible facade (z.ai) as the runs' provenance records.
- Method: launched FROM a scratch greeter repository (a one-file CLI, one ADR, three stray
  branches), `--var review_mode=mono --var mono_family=claude --var scope_notes="A tiny
  standard-library greeter CLI. Propose at most two evolutions; keep the vision to one
  page."`, `--store-dir` the operator's workspace store, `--sandbox none`,
  `--merge-into none`, caps `--max-cost-usd 1.5 --max-duration 10m`, every LLM node on
  `claude_code`/`claude-opus-5` (the eight `claw`/gpt-5.5 nodes overridden; the forfait is
  closed until 2026-09-20). The four in-agent questions and the two gates answered with
  `iterion resume --answer ask_user_response=… / approved=true … / action=done`; the pauses
  count against the wall, so the run was resumed past the 10-minute cap twice
  (`--max-duration 40m --max-cost-usd 4`).
- Result: $2.93, 47 min 09 s from launch to done including six pauses. The survey read the
  repository as experimental (one commit, no CI, three branches pulling apart) and the
  investigator asked exactly the questions that decide a vision — real tool or toy,
  distribution story, which candidate branch leads, horizon — before writing "greet —
  one-quarter vision: shrink, then ship"; the claude reviewer raised no blocking concern; the
  proposals became three self-contained tickets on the operator's local board
  (`native:30c7877c…`, `native:644e86c1…`, `native:7a2d2bea…` — collapse to one lineage,
  every behaviour has a witness, ship `greet.pyz` — each with a suggested bot and typed
  `bot_args`; close them by hand). No commit, as expected of a vision run.
- Value: the eighteen prompts' paragraph breaks reach the models — proven in the run's
  `events.jsonl` for ten rendered prompts (`investigate_system` six times); the GPT reviewer,
  the revision and the Nexie hand-off prompts were not rendered on this route — and the
  bot's discipline held on a toy: it asked before spending a vision, and its tickets carry
  the arbitrations the operator gave.
- Findings / misses: none bot-side. The `investigate` agent's questions pause the run like a
  human node does; on a 10-minute wall they alone consume it.
- Engine hardening: none needed.
- Lessons for next run: `--max-duration 45m` from the start when the operator answers by hand;
  a resume does not carry `--merge-into none` (seen on the other resumed runs of this wave).

## 2026-06-22 — PARKED: gpt-5.5 forfait context-overflow in review fan-out (run 019ef05e-90ff)

- Status: **partial / parked** — survey + investigate (operator elicitation) completed; **`aggregate_review` (wait_all) failed: 2 branches `context_length_exceeded`** on the gpt-5.5 reviewer.
- Versions: bot evolve · iterion v0.16.0 (110ea1c33)
- Method: `iterion run` (CLI). survey + investigate on `claw` gpt-5.5 forfait; mid-turn `ask_user` ×2 answered via resume (objective + horizon → campaign stability-first vectors incl. the failover feature). **NOT the z.ai cap** (the claude branch ran fine on opus) — the **openai/gpt-5.5 ChatGPT-forfait context window** overflowed on the large review input.
- Findings: **gpt-5.5 forfait context-overflow in review fan-outs** — the reactive compact-and-retry (generation.go, c5f57c14) did **not** prevent `context_length_exceeded` here; the review input (synthesis + all findings) exceeds the forfait window. **Same root cause as Seki's triage stall** (large input to gpt-5.5 forfait). High-value engine/claw fix: route large-context reviewer/triage nodes to a 200K-context model (glm-5.2/opus), or make the compaction actually fire before the wire call.
- Lessons: Evoly's `aggregate_review` should cap/shard reviewer input or run the reviewer on a big-context model. Re-run candidate post z.ai-reset with the claude reviewer on glm-5.2 (200K).

## 2026-06-13 — mid-turn ask_user restored + validated (run 019ec2f6)

- **Status:** validated. After the engine fix (commit e93ccc1b — see
  finding #1), `investigate` reverted from the `ask_brief` human-node
  workaround back to the **original design: mid-turn `ask_user`** on the
  agent (`interaction: human`). Live on claw + openai/gpt-5.5 forfait:
  survey → investigate **asks the operator via ask_user** ("what objective
  + horizon?") → pauses cleanly → resume injects the answer → investigate
  **persists to memory + asks a follow-up ask_user** ("which backend
  combos must be conformance-tested?") → pauses again. Genuinely iterative
  mid-investigation elicitation, multiple ask_user round-trips on a
  schema+tools node, zero 400s. The `ask_brief` node + its schemas were
  removed; the graph is back to 17 nodes.
- **Misc:** the agent persisted the first answer to `vision_interrogation.md`
  rather than the prompt's `CONTEXT_BRIEF.md` — it persists correctly, just
  off the named file; tighten the prompt if strict filename adherence
  matters (non-blocking — the auto-index surfaces it either way).

## 2026-06-13 — first dogfood: per-bot memory + full pipeline (runs 019ec1d5, 019ec1dc)

- **Status:** validated — the **full** pipeline ran live end-to-end:
  survey → ask_brief → investigate → synthesize → cross-family review →
  human_review (approved) → **propose_evolutions (9 findings) →
  emit_backlog (9 dispatch-ready backlog tickets) → home base**.
- **Versions:** bot 0.1.0 · iterion worktree `worktree-evolve-bot` (base
  `d1fe421c`).
- **Method:** all memory-bearing nodes `claw` + `openai/gpt-5.5` (ChatGPT
  forfait); the one cross-family "claude" reviewer `claude_code` +
  `claude-opus-4-8` (Claude Code OAuth forfait). Run from the
  `.claude/worktrees/evolve-bot` worktree against the iterion repo itself.
  `--var scope_notes=...`. No `--store-dir` (workspace default). No
  worktree:/sandbox: (read-only bot).
- **Result:** converged to the `human_review_vision` pause across two
  sessions. Run 2 (019ec1d5): survey → ask_brief (paused, answered via
  resume) → investigate (wrote CONTEXT_BRIEF.md) → synthesize (wrote
  VISION.md) → **failed at review_fanout** (workspace-safety: reviewers
  missing `readonly`). Run 3/4 (019ec1dc, after the readonly fix): full
  pipeline through both reviewers → aggregate (wait_all) → carry →
  human_review pause. review_claude on claude_code: 48s, 7979 tok, $0.34.
  Per-node gpt-5.5 nodes ~$0.05–0.09 each.
- **Propose + emit (resumed 019ec1dc with approval):** `propose_evolutions`
  wrote **9 deep finding artifacts** to `findings/` (decompose-ClawExecutor,
  capability-context-policy, bot-contract-v1, run-lifecycle-transition-policy,
  observability-event-taxonomy, persisted-format-stable-subset,
  dogfood-data-contracts, dispatcher-claims-leases, cloud-local-alignment) —
  each with proper frontmatter (`kind:evolution` / `source_bot:evolve` /
  axis+horizon+severity tags) and a grounded Why/Plan/Acceptance body.
  `emit_backlog` then created **9 `backlog` kanban tickets** on the main-repo
  board, **`bot: feature-dev` set via `set_bot` on 8** (the 9th left bot-less,
  no confident match), labelled `<axis>` + `horizon:<now|next|later>`. So a
  human can drag any ticket to `ready` to launch it, or Nexie can ingest them
  — exactly the requested handoff. ~$0.09 propose + $0.09 emit.
- **Value:** genuinely useful strategic output — a 10-axis vision *"Iterion
  as a Trustworthy Local Team Engine"* (backend-pipeline decomposition,
  one RunLifecycle contract, an auditable CapabilityContext, dispatcher
  invariants, observability taxonomy, .bot/persisted-format v1 subsets…),
  each axis with direction / key_moves / target_state, plus 11 guardrails
  and a rationale. Grounded in real files (ClawExecutor overload,
  Engine option sprawl, checkpoint-vs-event authority). This is exactly the
  "architect a vision" deliverable.

### Headline feature PROVEN live: per-bot cross-session memory

- `visibility: bot` writes landed at
  `~/.iterion/projects/-home-jo-lab-ai-iterion/bots/evolve/memory/vision/`
  (CONTEXT_BRIEF.md + VISION.md), and the **legacy project path
  `…/memory/vision/` was ABSENT** — proving the bot-visibility axis, not the
  legacy project-shared path.
- **Stable across worktrees:** the run launched from the *worktree*, but the
  memory keyed off the **main repo root** (`-home-jo-lab-ai-iterion`), not
  the ephemeral worktree path — i.e. `memBase = task.RepoRoot` via
  `findGitRoot`. So a future run from the main checkout sees the same
  accumulated vision. (This is the G2 worktree-stability concern from the
  plan, validated live.)
- **Cross-session continuity proven:** run 3's survey autoloaded run 2's
  VISION.md/CONTEXT_BRIEF.md and produced *materially deeper* axes — the
  accumulated context made the second pass smarter, exactly the intent.

### Findings / engine hardening

1. **(ENGINE BUG, HIGH — FIXED) mid-turn `ask_user` failed on schema+tools
   interaction nodes.** The bot's first design used `interaction: human`
   on the investigate agent to ask mid-turn via the `ask_user` MCP tool.
   On claw+openai it failed: instead of pausing, the run hit `openai 400:
   No tool output found` / `tool_call_ids did not have response messages`.
   - **Root cause (iterion, NOT claw-code-go).** Reproduced minimally:
     ask_user pauses fine on a node WITHOUT an output schema, but FAILS on
     one WITH a schema. `ClawExecutor` (executor.go) ran schema validation
     BEFORE the `_needs_interaction` short-circuit. The pause Result
     (`{_needs_interaction:true, …}`) is a control signal, not schema data,
     so `ValidateOutput` failed → triggered the schema-validation backend
     RETRY → the retry replayed the unanswered tool_call into a fresh
     generation → orphaned function_call → 400. (My earlier "claw-code-go
     intercepts ask_user" hypothesis was wrong — instrumented logging
     showed `errors.As` matched and the pause Result was returned correctly;
     a *higher* layer re-invoked.)
   - **Fixed** in `pkg/backend/model/executor.go` (move the interaction
     short-circuit ahead of schema validation) + regression test
     `TestDelegation_InteractionSignalSkipsSchemaValidation`. Verified live:
     a schema+tools+interaction node now pauses on ask_user and resumes to a
     valid structured output. Commit e93ccc1b (on main).
   - **Evoly impact:** the `ask_brief` graph-level human node shipped as a
     workaround and still works; it can now optionally revert to the
     original mid-turn `ask_user` design (set `interaction: human` on
     `investigate` + restore the ask_user prompt).
2. **(BOT BUG, fixed) reviewers needed `readonly: true`.** The two parallel
   judge reviewers had mutation-capable tools (`bash`) without `readonly`,
   so the workspace-safety guard rejected 2 mutating parallel branches.
   Added `readonly: true` to both (they only inspect). Lesson for any
   fan-out of tool-equipped judges: mark them `readonly`.
3. **(BOT BUG, fixed) review prompts referenced a whole-node output.** The
   reviewers' verdicts revealed they received the **literal unsubstituted
   `{{outputs.synthesize_vision}}`** token — so they reviewed nothing (and
   correctly withheld approval). Root cause: in a **prompt body**,
   `{{outputs.<node>}}` (a whole node output) does NOT resolve — only
   `{{outputs.<node>.<field>}}` does — whereas edge `with` mappings DO
   resolve whole-output refs (that's why human_review got the full vision via
   `carry_vision.vision`). Fixed: the review prompts now reference the
   edge-mapped `{{input.vision}}`. *Underlying engine asymmetry worth noting:*
   whole-output `{{outputs.<node>}}` resolves in an edge `with` but silently
   no-ops in a prompt body (and structured outputs render as Go `map[...]`,
   not JSON, in prompts/forms).
4. **(BOT BUG, fixed) findings scope keyed off WorkDir, not RepoRoot.**
   `propose_evolutions` wrote its 9 findings to
   `projects/<WORKTREE-key>/memory/findings/`, while the board tickets and
   Nexie's inbox live at the stable `<REPO-ROOT-key>`. The per-bot **vision**
   scope (`visibility: bot`) correctly re-roots to `RepoRoot`; the **findings**
   scope used the bare legacy form (→ `WorkDir`), which diverges under a git
   worktree. In normal (non-worktree) use they coincide, but fixed by adding
   `visibility: "project"` to the findings block (re-roots to `RepoRoot`,
   matching the board + Nexie). Validated by analogy to the vision scope,
   whose `RepoRoot` keying is proven live.
5. **(NIT) `iterion validate` is more lenient than `iterion run`.** A literal
   `{{issue.title}}` mention in a prompt body (documentation text) passed
   `validate` but failed `run` with `C004 unknown reference namespace`.
   Validate should catch C004 too. (Fixed in the bot by de-bracing.)

> Dogfood side-effect: this validation created 9 real `backlog` tickets on the
> local main-repo board (gitignored `.iterion/dispatcher/`). They are genuine,
> useful iterion-evolution proposals, but their finding-file links point at the
> (ephemeral) worktree key — dispatch or delete them, then re-run Evoly from the
> main checkout for cleanly-keyed output.

### Lessons for next run

- The mid-turn ask_user limitation is real for forfait users — until the
  claw+openai fix lands, **author operator interaction as graph-level
  `human` nodes**, not mid-turn `ask_user`, on any claw+openai bot.
- Full cycle (incl. propose + emit) now proven. Next: confirm on `/board`
  that a drag-to-`ready` actually dispatches `feature-dev` from a ticket whose
  body is the spec (the dispatch_vars title+body path), and that the
  visibility:project findings now co-locate with the board on a real run.
- The reviewers should reference `{{input.X}}`, never `{{outputs.<node>}}`
  (whole-output) in prompt bodies. Consider an engine fix so whole-output
  refs resolve (and render as JSON) in prompts, matching edge `with`.
- Add `source:evolve` to emit's label set explicitly (this run labelled by
  axis + horizon but the `source:evolve` label didn't appear — verify the
  set_labels call includes it).
- The vision quality on gpt-5.5 was high; opus (via `claude_code`, or claw
  if an API key is ever acceptable) would only be worth it for the synthesis
  node if the operator wants more depth.
