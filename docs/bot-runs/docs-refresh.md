[← Bot runs](README.md)

# docs-refresh (Doki) — bilans

Documentation refresh bot (v3: adaptive paradigm). ONE campaign agent
aligns the docs with the current code — docs follow code, never the
reverse: repair stale claims AND write missing documentation. A
deterministic `scan_hints` producer feeds ADVISORY hints (missing
tracked paths, dead links/anchors, unmentioned areas — never a gate);
convergence is `scope_ok ∧ docs_aligned` only (since v3.3 a docs-only
bot can't break the build, so there is no build/verify gate). Dismissals +
promises ledgers persist the agent's adjudications; the opt-in
`open_mr` tail publishes ONE PR. Runs on ANY repo; iterion is the
reference self-host case.

## 2026-08-11 — the two prod weeklies both died on the cost cap, 60 commits stranded (runs 019fc5c7, 019fe9d3)

- Status: **failed to deliver, twice**. Read after the fact, from the runs
  alone — nobody was watching either Monday. The 2026-07-27 configuration
  fix landed and worked; the schedule then failed one layer down.
- Versions: bot 3.5.4 (baked catalog, `/opt/iterion/bots/docs-refresh/main.bot`)
  · iterion prod `:edge` · backend `claude_code` on the OAuth forfait.
- Method: prod cloud schedule `306ecbc6`, `0 4 * * 1`, repo
  `SocialGouv/iterion.git`, vars `{mode: incremental, open_mr: "true"}`.

| Run | Window (UTC) | Passes | Commits | Died at |
|---|---|---|---|---|
| `019fc5c7` (08-03) | 04:00 → 20:20 | 4 | 31 | cost_usd 128/120 |
| `019fe9d3` (08-10) | 04:00 → 21:36 | 3 | 29 | cost_usd **231**/120 |

- Result: `failed_resumable` both times, `final_branch` and `final_commit`
  empty, no PR either week. The PR tail is the only push path
  (`mr_gate → forge_auth_probe → finalize_mr`), and a hard budget death at
  `campaign` never reaches it — so ~60 real alignment commits died with
  their pods. Neither run was resumed.
- **The schedule config is NOT the problem this time.** The `vars: null`
  defect of 2026-07-27 is fixed: both runs carry `mode: incremental` and
  `open_mr: true`, and the cron fired to the second (04:00:02, 04:00:03).
  Everything up to the cap worked. What is left is the cap itself.

### Three things the events say

- **The ceiling prices an estimate, not money.** Both runs are forfait:
  `rate_limited (claude_code): You've hit your weekly limit · resets 7pm
  (UTC)`, then `session limit · resets 8:40pm`. A subscription bills
  nothing per call, so `$231` values tokens against a plan already paid
  for. The gate killed a run that cost zero, and the provider's usage
  window was already bounding it.
- **The overshoot is structural, not marginal: +92%.** The budget is
  checked BETWEEN nodes. Pass 3 started with `budget_warning … used:
  96.96` — $23 of headroom — for a `campaign` node that is a whole
  claude_code session and had just cost $63. The engine let a pass start
  that it could not fund, then discovered the fact $111 later. The 90%
  hard limit does not catch this: 80.8% is under it.
- **The usage-window retry works, and is why a 6h budget spans 17h.**
  Each `USAGE_LIMIT_BLOCKED` parks the run and re-launches the node after
  the reset (visible as iterations 1 and 2 each starting twice), and
  parked time does not count against `max_duration`. The retry is not the
  problem — it is what carried the run far enough to reach the cap.

### Engine hardening

- **Back-edge affordability guard** (this change,
  [pkg/runtime/loop_budget.go](../../pkg/runtime/loop_budget.go)): a loop's
  back-edge is declined when the budget cannot fund another iteration,
  priced by what the previous one consumed, on every capped axis. The run
  leaves through its own exit path — for Doki `gate → mr_gate`, the same
  fall-through that already served loop exhaustion — so the PR tail ships
  what was banked. Applied to 08-10's numbers it declines the third pass
  at $97/$120 and opens a PR with the 29 commits. Generic: every v2
  campaign bot banks work in stride and has this exposure. Visible as
  `budget_warning{reason: loop_budget_guard}`; `ITERION_LOOP_BUDGET_GUARD=off`
  is the escape hatch.
- Budget raised 120 → 400 (bot 3.5.5) so the asymptote converges on its own
  terms. Safe now only because of the guard above — a higher ceiling used
  to mean a bigger pile of stranded commits.
- **Already fixed, and the runs prove it.** 08-03 shows the resume/refail
  loop: `campaign` restarted **9 times** at pass 4, 8 `budget_exceeded`
  events, each turn re-provisioning a sandbox to instantly re-hit the same
  spent budget. 08-10 has exactly one. `fix/budget-terminal-ack` (#361,
  merged 08-04) landed between the two — this is its live confirmation.

### Lessons for next run

- **A budget cap is a delivery decision, not just a spend decision.** On a
  bot that banks work in stride, where the cap falls decides whether the
  run ships or throws everything away. That is the engine's job to get
  right, not the operator's to size around — hence the guard.
- **An unattended schedule needs an alert, not a reader.** Three weeks of
  Monday runs produced nothing and nobody knew until someone asked. The
  07-27 lesson was "a green run is not a delivered run"; the sharper
  version is that `final_commit` empty on a bot with `open_mr: true` is a
  detectable condition, and nothing is watching for it.

## 2026-07-31 — closing run 019fae96: where the yield curve turns (run 019fae96, cont.)

- Status: **partial, deliberately stopped**. Never reached `converged`. The run
  was resumed twice today and then abandoned on a judgment call, not a crash —
  the marginal value of a pass had clearly fallen below its cost.
- Versions: bot v3.5.x · iterion `dev+00060908f` (the pi state-root work) ·
  pi 0.82.1 · `backend: "pi"`, two different models.
- Method: `iterion resume --run-id 019fae96 --store-dir "$PWD/.iterion"
  --backend pi`, `ITERION_SANDBOX_DEFAULT=none` (resume does not replay the
  launch's sandbox decision), `ITERION_PI_NO_CONTEXT_FILES=1`.

### What each model actually did

| Pass | Model | Wall | Commits | Cost | Outcome |
|---|---|---|---|---|---|
| resume A | `zai/glm-5.2` | 14 min | 1 | ~$0 | **failed the termination contract** |
| resume B | `openai-codex/gpt-5.6-sol` | 56 min | 50 | ~$63 | stopped on the DURATION ceiling |

**GLM cannot hold the v2 termination contract.** Two attempts, both ending in
`structured output parsing fell back to text wrapper` then seven missing
required fields — the model returned prose where the contract wants an object.
This is not a backend fault: the parse tried and degraded honestly. It is the
v2 shape (a whole-session agent that must close with a machine-checkable
object) meeting a model that will not close it. Worth knowing before choosing a
cheap model for a campaign bot.

### The finding that matters: yield falls off a cliff

Codex produced 50 commits in 56 minutes — the same VOLUME as the $47 pass of
2026-07-29 (48 commits). The density is not the same:

- **2026-07-29**: substantive blocks. One commit added 13 lines documenting
  `author_scope: exclusive` — what it is, that provisioning adds those logins
  to every OTHER co-enabled bot's denylist, and a cross-link to the runtime
  fan-out. Verified against `pkg/bundle/manifest.go`: the surface is real.
- **2026-07-31**: **8 lines added per commit on average** (427 across 50). The
  `async diagnostic band` commits are ONE-LINE edits — inserting `plus async
  C240–C242` into a table sentence — repeated verbatim across `README.md`,
  `docs/README.md`, `docs/visual-editor.md`, `cli-reference.md` and more. True,
  and genuinely missing; but a commit per file for a mechanical propagation.

So: **~15¢ per line of documentation**, much of it one sentence fanned across a
tree. The bot is not padding — every claim I sampled checks out against the
code — it is simply working through a long tail of correct, low-value edits
with no signal that says "this is worth less than the last one".

### Budget mechanics worth knowing

- Total for the run's life: **$109.63** across all passes.
- The stop was **`budget hard limit reached: duration at 98%`**, not cost
  ($130 cap, $109 spent). `--max-duration` is CUMULATIVE over the run's whole
  life, exactly like cost — a 3h ceiling on a resume means "3h total", not "3h
  more". Size it accordingly when resuming.
- The known engine gap still stands: `max_cost_usd` is evaluated at node
  boundaries, and a v2 campaign puts a whole pass in ONE node, so the cap
  cannot bite mid-pass.

### Lessons for next run

1. **Do not run a campaign bot on a model that cannot emit the contract.**
   Cheapness is irrelevant if the pass cannot close. Test the contract first
   with a one-node probe.
2. **Add a value floor to the campaign contract.** The bot has no way to say
   "the remaining drift is not worth a commit". A minimum-substance rule (or
   grouping a mechanical propagation into ONE commit across files) would have
   turned this pass's 50 commits into perhaps 10 with the same information.
3. **`drift_remaining` is not a value signal.** It counts what is left, not
   what it is worth — which is why the loop would happily keep going. That is
   the asymptote question ADR-058 answers for correctness loops but not for
   docs volume.
4. Resume ceilings are cumulative. Say `--max-duration 6h` on a resume if you
   want 3 more hours.

### Where the work landed

All 124 commits merged to `main` (`27bb98655..1dbf3a64e`) via
`dogfood/doki-pi-019fae96`. Seven doc conflicts against a `main` that had moved
37 commits meanwhile, each resolved on the facts rather than by preferring a
side — notably keeping `main`'s "pi IS bridged to the ChatGPT forfait" over
Doki's stale "does not consume", and `main`'s `v3.15.0` over Doki's `v3.7.6`.
The generated bot catalog needed `iterion bots regen-catalog` afterwards, with
a binary rebuilt AFTER the merge — the older one could not parse the new
manifests' `consumes`/`produces` fields.

## 2026-07-29 — first real bot run on the `pi` backend (run 019fae96)

- Status: **partial** — pass 1 delivered 15 doc-alignment commits; pass 2 died
  ~7 min in on the z.ai 5-hour usage cap. `failed_resumable`, resumable after
  the 04:54 UTC reset. The purpose was validating the **pi backend** with a
  real bot, and on that it succeeded.
- Versions: bot v3.5.x · iterion `dev+3a61d2da4` (main tip, pi merged in
  v3.15.0) · pi 0.82.1 · model `zai/glm-5.2` over `backend: "pi"`.
- Method: `mode=incremental`, `diff_since=4ef39f9e3` (J-7 → 245 commits, 645
  files changed), `bundle_self_path=bots/docs-refresh`, `--merge-into none`,
  `--sandbox none` (isolating the pi variable), `ITERION_PI_NO_CONTEXT_FILES=1`
  (the repo's CLAUDE.md costs ~26k input tokens per call), cap `--max-cost-usd 15`.
- Result: pass 1 = 232,965 tokens, **$0.2968**, ~33 min, 15 commits,
  `scope_ok: true`, `gate.converged: false` (drift remaining — the bounded
  continuation loop behaving as designed). **~10x cheaper than the ~$3/pass
  this bot costs on claude_code.**
- Value: real alignment of the week's features — the `file` schema field type
  and `--answer key=@./path` (PR #315), the `iterion models pricing` audit, a
  dead cross-reference, the model names across the corpus realigned on the
  Claude 5 fleet, and the pi backend itself added to the backend lists.

### Pass 2 — resumed on a ChatGPT/Codex credential (same run, 19:37→20:23)

- Status: **partial, and over budget** — 48 further commits, then
  `BUDGET_EXCEEDED`. Still `failed_resumable`.
- Method: `iterion resume --backend pi --model openai-codex/gpt-5.6-sol`,
  `--max-cost-usd 15`, `ITERION_SANDBOX_DEFAULT=none`.
- Result: **63 commits total** (15 from pass 1), 53 files, +467/−261, in 46
  minutes. **$47 against a $15 cap.**

**The cost cap does not bound a v2 campaign.** `max_cost_usd` is evaluated at
NODE boundaries, and the v2 shape (ADR-058) puts an entire pass inside ONE
agent node that ran 46 minutes uninterrupted. Nothing checks the budget in
flight, so the overshoot is only observed when the node finally reports — here
at 3x the cap. This is not specific to pi or to this bot: it applies to every
bot in the v2 fleet. Either the budget must be evaluated against streaming
usage events, or the documentation must say plainly that it bounds nodes
crossed, not dollars spent.

**Three defects the RESUME path exposed, none visible from `iterion run`:**

- **The codex bridge gave up instead of falling back.** `piLoadCodexView`
  preferred the credential directory the run context announces (the cloud
  path) and abandoned the search when it was unreadable. On `run` the context
  announces nothing, so it worked; on `resume` it announces an empty
  directory, so it failed with "No API key found for openai-codex". The two
  sources are alternatives, not a chain — preferring one must not cost the
  other. Fixed.
- **`resume` does not replay the launch's sandbox decision.** The run was
  launched `--sandbox none`; the resume started IN a container, where pi is
  not installed (`exec: pi: not found`). There is no `--sandbox` flag on
  resume, while `--model`, `--backend` and the budgets are all re-appliable —
  so a run silently changes execution environment on resume. Worked around
  with `ITERION_SANDBOX_DEFAULT=none`; the engine gap stands.
- **`ITERION_PI_BIN` was documented and never implemented.** The escape hatch
  for a host that cannot run the npm CLI existed only in the reference.
  Implemented on both transports.

Diagnosis note: the first two fixes from the adversarial review made the
credential failure WARN instead of erroring, which is right — but it also made
this defect silent. It took capturing pi's real argv through a fake binary on
PATH to find it, after too long spent theorising.

### Pass 3 — resumed on z.ai after the cap reset (15 min, 5 commits)

- Status: **partial** — 5 more commits, then the z.ai 5-hour cap again (reset
  pushed to 20:48). **68 commits total.** Still `failed_resumable`; the gate
  never declared `converged`.
- The 04:54 reset gave only a partial window: 15 minutes of campaign consumed
  it. One Doki pass simply exceeds a z.ai 5-hour allowance.
- **The `--skill` fix is confirmed live.** Zero ENOENT against
  `~/.claude/skills` this pass, against three in pass 1 before the agent
  recovered by listing the worktree. The skills reached pi and no turns were
  spent hunting for them.

**Where a run's spend actually lives, and where it does not.** Three facts,
each of which cost a wrong assumption to establish:

- The accumulated spend IS persisted — `checkpoint.budget_cost_usd`
  ($46.67 here), plus the per-node figure under
  `checkpoint.outputs.<node>._cost_usd` ($46.37 for the codex campaign alone).
- It is NOT derivable from the event stream the way a monitor naturally reaches
  for it: `_cost_usd` rides `node_finished`, so summing those events reports
  **$0.30** for this run — pass 1, the only pass whose node ever finished.
- And the ledger only books on node COMPLETION. Pass 3 did 15 minutes of
  billed work and produced 5 commits; `budget_cost_usd` did not move. A node
  killed mid-flight — by the budget, a 429, a cancel — contributes nothing to
  the run's recorded cost.

Together with the cap's node-boundary evaluation, that is why $46 was spent
under a $15 cap inside a single node, and why the resume then refused
immediately with `cost_usd (47/25)`: the ledger is consulted at node ENTRY,
cumulatively over the run's whole life, so any cap below the sunk cost blocks
before doing work. Raising it to 60 was the mechanical requirement to continue.

### Findings

- **`--skill` was never passed to pi for a bundle bot — engine bug, fixed.**
  The flag was gated on `task.SkillHints`, which carries only the DSL `skills:`
  field (the skill *library*); a bundle's skills are mirrored into
  `<workDir>/.claude/skills/` without ever touching it. So pi had zero skill
  awareness while Doki's prompt ordered "LOAD YOUR SKILLS FIRST", and the agent
  burned turns on three ENOENTs against `~/.claude/skills/` before recovering
  by listing the worktree. A pi-only hole in a mechanism claude_code (native
  discovery) and claw (its `skill` tool) both cover.
- **Doki's prompt names its skills by a bare relative path** (``under
  `.claude/skills/` ``), which an agent can and did resolve against `$HOME`.
  Anchoring it (`${PROJECT_DIR}/.claude/skills/`) would remove the ambiguity
  independently of the fix above.
- **pi cannot reuse `~/.codex/auth.json`** — its `openai-codex` provider is the
  one provider with no API-key env var, OAuth-only via an interactive `/login`.
  Asymmetric with claw, which does read that file. Bridged since: iterion
  seeds a throwaway agent dir per run.
- **Rate-limit typing is correct**: the z.ai 429 was classified
  `USAGE_LIMIT_BLOCKED`, retried twice with backoff, and reported with the
  reset time. The run stayed `failed_resumable`.
- **The stdio MCP transport ran for real** — `iterion __mcp-board` appeared as
  a child of pi, exercising code previously validated only against test servers.
- Not a defect, checked before counting it: the `Tool error … bash` lines are
  the agent's own `a && b && c` chains failing on a missing directory. pi
  reports the non-zero exit correctly.

### Lessons for next run

- **A cost cap does not stop a v2 campaign** — see pass 2. Watch the running
  cost, do not trust `--max-cost-usd` to bound a single-node pass. Read it from
  `checkpoint.budget_cost_usd`, not by summing `node_finished` events, and know
  that a node killed mid-flight books nothing.
- **Pick a model whose quota survives a pass.** GLM is ~10x cheaper than
  gpt-5.6-sol but its 5-hour cap ends a campaign mid-flight; the expensive
  model finishes the pass and blows the budget instead. Neither converged.
- **A failed run leaves its commits on a detached HEAD with no branch.**
  `finalizeWorktree` creates the anti-GC branch only on a clean exit, so 15
  commits were reachable only through the preserved worktree — one
  `git worktree prune` from being garbage. Branch them by hand
  (here: `dogfood/doki-pi-019fae96`).
- Pick a model whose quota fits the work: GLM-5.2 is cheap but its 5-hour cap
  ended this run mid-pass-2. Resume after the reset, or run a smaller scope.
- Use ABSOLUTE binary paths. The first launch died on `unknown backend "pi"`
  because a `cd` made `./iterion` resolve to the main checkout's stale binary.

## 2026-07-27 — forfait weekly-cap catch-up, and the weekly schedule is throwing its work away (run 019fa533)

- Status: **failed to deliver** — the run finished cleanly and produced
  ~71 doc-alignment commits, and **every one of them was discarded**. Not a
  regression: a standing misconfiguration of the prod weekly schedule, found
  by looking past the green status.
- Versions: bot v3.5.x · iterion prod `:edge`.
- Why it ran: the Monday 04:00 weekly (`019fa1ba`) died at `campaign` on the
  forfait **weekly** quota (`resets Jul 28, 9pm (UTC)`), one of seven prod
  runs killed by the same wall that morning. Relaunched by hand at 20:11 via
  a one-shot cloud schedule once the forfait was verified available.
- Result: 3h10, 5 campaign passes committing 14 / 27 / 11 / 6 / 13 = **71
  commits**, `docs_aligned: false` on every pass, `continuation_loop`
  exhausted 4/4, then `mr_gate → open_mr: false` → `done`. No MR, no push,
  no `final_commit`. The clone died with the pod.
- **The finding.** The prod weekly schedule (`306ecbc6`, `0 4 * * 1`) carries
  **`vars: null`**. Two defaults follow from that, and both are wrong for it:
  - `open_mr: bool = false`, and `mr_gate` computes
    `vars.open_mr || vars.pr_url` — both empty ⇒ the PR tail never runs.
    **The schedule has never been able to deliver anything**; it has been
    burning a multi-hour full sweep every Monday and dropping the result.
  - `mode: string = "full"`, so it runs the whole-corpus sweep (3h+) rather
    than the incremental delta the weekly cadence was designed around (see
    the 2026-07-24 entries). The memory of this schedule being "hebdo
    incrémental" refers to an earlier row; the current one was recreated
    2026-07-24 without vars.
  Fix is configuration only: set `vars: {open_mr: "true", mode:
  "incremental"}` on the schedule. Worth doing before next Monday — until
  then the weekly is pure cost.
- **What was salvaged.** The commits are gone, but the campaign's five
  `human_note` payloads were recovered from the run events before the run
  aged out. Two were cheap and are fixed in this branch:
  - `Taskfile.yml:378` — `test:live:feat:rtk` ran `-run 'TestLive_Feat_Rtk$'`,
    which matches **no test** (the real one is `TestLive_Feat_Compress`).
    `go test -run` with no match exits 0, so the target passed green while
    executing nothing. Retargeted.
  - `pkg/bundle/manifest.go` — the unknown-forge-event error listed two of
    the three valid events, omitting `issue_labeled`, so an author
    declaring a *valid* event was told it was unknown. The list is now
    derived from `KnownForgeEvents` instead of hardcoded at the error site,
    which is what let it drift.

  Three are handed off (not `.md`-fixable, so outside Doki's writeable set):
  - `bots/security-bots` — `reviewer_isolation` declares
    `tools: [read_file, glob, grep]` on `claude_code`, which is a **no-op
    under bypassPermissions**: the node keeps bash. The injection guarantee
    rests solely on the `project_review_input` schema projection. If
    bash-denial is meant as defense-in-depth, switch to the claw backend or
    add `permission: deny`. The DSL comment (`main.bot:4101-4102`) repeats
    the overstatement.
  - CLAUDE.md and `docs/scheduling.md` say sec-audit findings are labeled
    `source:sec-audit-self`, but the shipped bots hardcode
    `source:sec-audit-source` / `-deps` and read no `label_source` var —
    the self-audit distinction is **documented but unwired**.
  - `docs/grammar/iterion_v1.ebnf` lags the shipped async surface; five
    catalog `manifest.yaml` descriptions drifted from their `main.bot`.

  Also worth noting as infrastructure: the **board MCP
  (`mcp__iterion_board__*`) was unavailable in the cloud runner session** on
  every pass, so Doki could not file any of these as board issues — which
  is precisely why they nearly died with the pod.
- Engine hardening: this run is one of the seven that exposed the
  usage-window retry defects (dead reset-aware wait, no recovery dispatcher
  in the cloud runner, unparseable dated reset hint). See
  [feed-watch.md](feed-watch.md) (entry for 2026-07-27)
  for the full account; the fix ships as the `retry:` contract.
- Lessons for next run:
  - **A green run is not a delivered run.** `status: finished` here means
    "the graph reached done", not "work landed". For Doki the delivery
    signal is `open_mr`/`final_commit`, and both were empty.
  - When a schedule is recreated, its `vars` do not come along. A bot whose
    useful behaviour depends on non-default vars should say so loudly —
    `mr_gate` deciding `false` in silence is what let this run for weeks.

## 2026-07-24 — amend-on-PR validated live END-TO-END + scope_check base fix (runs 019f9429 → 019f949a)

- Status: **validated** (full amend-on-PR flow, live on a real iterion PR). Enabled by a scope_check base-derivation fix (v3.5.3) the dogfood surfaced.
- Versions: bot 3.5.2 → **3.5.3** · iterion `d184a1f93` → `40ae43322` (rode into release **v3.3.0** `1d392563e`)
- Method: cloud PROD (ovh), backend claude_code / opus-4-8 forfait, repo-targeted launch (`POST /api/runs`) on **SocialGouv/iterion PR #290** (head `worktree-aidd-projected-improvements`, base main), vars `pr_url`/`base_ref`/`source_branch`, `merge_into=none`, conn `f73ba902`. Target PR = the AIDD-framework feature (39 files; 16 code incl. `bundlelint/lint.go` skill-lint, `dispatcher/tracker/blockers.go` dependency-gating, `skilllib/frontmatter.go`; 3 docs already updated by the PR).
- **THE BUG (found live, run 019f9429 cancelled at pass 3)**: `scope_check` based its writeable-set diff on `reflog[-1]` (OLDEST reflog entry). Works for `worktree:auto` (fresh worktree from HEAD) but BREAKS in amend: the cloud runner `git clone`s the base branch (HEAD=main) then `checkout -B <pr-head>`, so the oldest reflog entry is **main** → `git diff main` folds the PR author's OWN code (`bundlelint/lint.go`, `review-pr/main.bot`…) into the changed set → phantom SCOPE VIOLATION → `scope_ok` stuck false → `converged = scope_ok ∧ docs_aligned` never fires → the amend run burns every pass (would be ~5 passes / ~90 min instead of converging at 2 / ~26 min). Confirmed NON-destructive: the campaign made zero reverts, recognised those as the PR's own files, and ignored the "revert these" feedback — but it can't fix a deterministic gate. This was the answer to "why is Doki slower than the PR": not thoroughness, a convergence bug.
- **THE FIX (v3.5.3)**: base the diff on the **run-start HEAD** — the newest reflog entry that is NOT one of this run's own `Bot: docs-refresh` commits. Lands on the PR head (amend) or the worktree base (worktree:auto), the true run-start in both. Mode-agnostic, **bot-local** (no engine change — reinforces "the ENGINE stays bot-agnostic": the engine supplies generic `pr_url`/`base_ref`/`source_branch` + clone/checkout; the bot owns its own scope semantics). Real fixture test `bots/docs_refresh_scope_test.go` reproduces the clone+checkout reflog shape (real `git clone` + fetch/checkout PR head): 4/4 (amend scopes to Doki's own commits, PR code never flagged; still flags Doki's own non-.md; worktree:auto no-regression; zero-commit clean).
- **Re-run 019f949a (post-fix) — the validation**: **26 min**, converged in **2 passes** (not max_passes):
  - pass 0: `docs_aligned=False` (contract: productive pass reports false), 2 commits — the C200–C230→C200–C234 bundlelint-range ripple BATCHED across 12 docs in ONE commit (vs the buggy run's 13 file-by-file) + the webhooks filter-count fix. **`scope_ok=TRUE, out_of_scope=[]`** ← the fix; before it this was always false + a list of the PR's `.go` files.
  - pass 1 (confirming sweep): `docs_aligned=True`, 1 commit (review-pr stale-review guard) → `gate.converged=True`.
  - `mr_gate {open_mr:true}` (opened on `pr_url`) → `forge_auth_probe {available:true}` → `finalize_mr {opened:true, back_linked:true, branch: worktree-aidd-projected-improvements, skipped_reason:""}` → `surface_pr_link` (`preview_url_available kind=pr`) → **finished**.
- **Side-effects on GitHub PR #290** (the harvest): head `9d71a072` → `62d8b314`, **9 → 12 commits** (+3 Doki), **0 → 1 comment**. All 14 files touched are `.md`, every commit carries the `Bot: docs-refresh` trailer (scope-clean, matching `scope_ok=true`). The comment traces all 8 code-delta clusters to a doc conclusion, dismisses 3 non-drift candidates with reasons, and declares "fully aligned — no remaining drift."
- Value: proved the amend-on-PR feature works end-to-end (docs land IN the contributor's PR, reviewed with the code) AND hardened the engine-adjacent scope gate for the repo-targeted clone path. The C-range was duplicated across ~15 docs; the PR author updated only `diagnostics.md`, Doki caught every stale mention — the drift a human misses.
- Efficiency note (operator's question "is the length justified?"): the "N passes for N discovery waves" is partly BY DESIGN (the `docs_aligned` decline-to-zero asymptote — a productive pass reports false by contract). The optimizable lever is front-loading discovery (a deterministic changed-identifier→doc map) so pass 0 sees all subsystems at once. The real cost driver is the doc-corpus smell (a C-range hardcoded in 15 docs) — Doki pays that redundancy; it is not a Doki bug.
- Lessons for next run: (1) the amend clone+checkout reflog shape is a trap for ANY bot node deriving "what did I change this run" from `reflog[-1]` — base on run-start HEAD via the own-trailer walk. (2) monitor the WORKING result, not just the status: `scope_ok` false-forever presented as a slow-but-running campaign; profiling per-pass edits + the scope_check output surfaced the bug. (3) an admin push to `main` can get folded into a release-it cut mid-flight — verify your commit is an ancestor of the new HEAD (it rides the release) rather than assuming the build is on your bare sha.

## 2026-07-24 — v3.5 incremental (git-detected base) + amend-PR modes, incremental validated live (run 019f92fc)

- Status: **validated** (incremental mode); amend mode built + unit-tested, live-validation deferred to the PR-trigger follow-up.
- Versions: bot 3.4.0 → **3.5.1** · iterion `bb2291edd`
- What shipped — two alignment strategies so Doki keeps docs fresh cheaply, without abandoning the lean paradigm:
  - **`mode: incremental`** (+ default `full`). scan_hints AUTO-DETECTS the last alignment point from git — the newest commit with a `Bot: docs-refresh` TRAILER LINE (`git log -E --grep '^Bot: docs-refresh'`) — and scopes the campaign's SEMANTIC pass to the code changed since it. The zero-LLM STRUCTURAL scan (dead links, missing paths) stays corpus-wide in both modes. First run with no prior alignment degrades to a full sweep. This git-native base detection is the durable, cloud-safe successor to the noop cache removed in v3.4 (git IS the state).
  - **`mr_mode: amend`** (+ default `new_pr`). When the run is checked out from an existing PR's head branch, finalize_mr pushes the alignment commits ONTO that branch + comments, instead of opening a new PR (forge-mr-create skill gains the amend path). For the PR-open trigger — docs land IN the contributor's PR.
- Live bug caught on the FIRST real check (garant discipline): the initial loose `git log --grep 'Bot: docs-refresh'` matched any commit MENTIONING the trailer in prose — including this bot's own v3.5 feature commit (`1a5eddf29`) — making base=HEAD and the delta empty. Anchored to `^Bot: docs-refresh` (v3.5.1); a fixture test now commits a prose-mention after the alignment commit and asserts the base stays the real alignment commit.
- Incremental dogfood **019f92fc** (cloud PROD, SocialGouv/iterion, opus-4-8 ultracode, `mode=incremental` + `open_mr=true`, conn f73ba902): **15.8 min** (vs 20.5 full in v3.3), converged 1 pass, **PR #289 (+23/-25)**, result-link fired. `incremental_base` resolved LIVE to **PR #288 (9d8edab7c)** — the real last alignment, NOT the code commit — confirming the anchoring fix in prod. The delta was 61 files; the campaign correctly saw the substantive change was "the docs-refresh bot itself (v3.4→v3.5.1)" and scoped to it.
- Value: the dogfood **caught my own incomplete v3.4 cleanup** — Doki's own skills (`docs-refresh.md`, `doc-scope-enumeration.md`) still described the removed `author_docs`/noop-cache/`mark_issue` machinery (I updated README/main.bot/manifest but missed the skills). PR #289 aligned them + documented v3.5 incremental, and self-found an ADR-052 broken fixture path (`e2e/…` → `e2e/testdata/…`, verified via `git log --follow` as a typo not frozen history). Dismissed 25 advisory hints WITH reasons (VitePress cleanUrls links ×14 correctly recognized as valid) — no manufactured noise. Promises 0, code bugs 0.
- Deterministic fixture tests (bots/docs_refresh_hints_test.go) pin the base-detection, delta-scoping, prose-mention-ignored (anchoring), and first-run→full degrade — the real scan_hints python run against temp git repos.
- Mise en place (prod cloud, integration `429d7b5e` = push creds confirmed): **weekly incremental** schedule `f6b89ded` (Mon 04:00 UTC) + **monthly full** `b2d3654a` (1st 03:00 UTC). Strategy: cheap weekly delta-alignment + monthly cross-cutting reconciliation.
- Deferred (operator's "option 2"): the auto PR-open→amend TRIGGER. The amend mode is built + invocable; auto-wiring needs the forge invocation on the manifest + the PR head branch mapped into `repo_ref`+`mr_branch` and PR base into `diff_since`. The spine emits forge PR events (`trigger_forge_emit.go`) and supports `InvocationKindForge`/`ForgeEventPullRequest`, but the dynamic PR-head→var mapping is unverified — a clean follow-up rather than unverified plumbing. Beta testers can invoke amend manually today (API launch with `mr_mode=amend`, `repo_ref=<PR head>`, `mr_branch=<PR head>`, `diff_since=<PR base>`).
- Lessons for next run: (1) trailer-based base detection MUST anchor to the line (`^`) — bots that write about themselves trip a loose grep. (2) when removing DSL machinery, grep the SKILLS too, not just README/main.bot/manifest — the skills are separate prose that drift (Doki caught this). (3) incremental is meaningfully faster (15.8 vs 20.5) AND more focused than full, at the cost of missing cross-cutting drift — hence the monthly full reconciliation.

## 2026-07-23 — v3.3 build-verify apparatus REMOVED, then validated lean (runs 019f9085 → 019f90da)

- Status: **validated**
- Versions: bot 3.2.1 → **3.3.0** · iterion `8aee22894`
- Method: cloud PROD (ovh), target SocialGouv/iterion, claude_code opus-4-8 ultracode (self-orchestrated), `open_mr=true` + `merge_into=none`, GitHub App forge conn, budget 6h / $120 / 4 passes.
- Result — **run 1 exposed the waste, run 2 validated the fix:**
  - **019f9085 (WITH verify, bot 3.2.1)** — 31 min, converged pass 1, **no PR** (corpus already aligned by #284). Timing: campaign ~8 min · **verify_build (agent composing verify.sh) ~19 min** · verify_run ~2 min · tail ~2 min → the verify chain was **~21/31 min = 68% of the run**, and verify_build alone was more than the actual alignment work — all to verify a docs-only diff that couldn't affect the build.
  - **019f90da (LEAN, bot 3.3.0)** — **20.5 min**, converged pass 1, **PR #288 (+58/-22)**. Lean path confirmed live: `campaign → scope_check → gate` DIRECT (no verify_precheck/verify_build/verify_run). `surface_pr_link` fired → `preview_url_available kind=pr` — the #286 result-link, validated live.
- Value: the dogfood closed on itself. PR #288 aligned the docs to THIS session's code: a new "Feed-fetch security (SSRF posture)" section documenting the feed-watch proxy-aware SSRF fix (#287, +35 in feed-config.md), the docs-refresh build-gate removal (CLAUDE.md + README + 2× bot-catalog), and it **caught a stale "8 skills / verify-build skill" line the operator had missed** in the same README. Real, grounded alignment — not façade.
- The change (v3.3.0): removed 3 nodes (verify_build/verify_run/verify_precheck) + their schemas/prompts, the `baseline` + `go_comment_globs` vars (Doki is now cleanly `.md`-only), the verify-build skill, and the gate's verify verdict → `converged = scope_ok ∧ docs_aligned`. main.bot 1626→1321 lines, 14→11 nodes. Rationale (operator insight): a docs-only bot edits `.md` only, which cannot break `go build`/`go test`, so the ADR-058 build gate verified an invariant the campaign structurally cannot violate — at ~68% of the wall-clock.
- Diligence (verified from the PR body — the campaign's honest self-report): the 18.5-min campaign was a genuine COMPREHENSIVE sweep, not laziness. It fanned out **6 sub-auditors** (ultracode) over all 27 bots + core engine + cloud/webhooks/CLI/dispatcher + a completeness-critic across studio/desktop/references/grammar/CI/recent-ADRs — all returned clean beyond the 3 fixes. It verified the whole recent-change frontier since #284 (#286 result-links → found already documented in browser-pane.md, correctly NOT edited; #287 feed-watch → the real gap, fixed; #283/CI/VitePress → covered). It ran `iterion bots regen-catalog` (idempotent-verified) for the generated catalog rather than hand-editing, and handed off ONE out-of-scope finding it couldn't touch (a stale `opus 4.7` vs `claude-opus-4-8` comment in `sec-audit-deps/main.bot`, a `.bot` code file). So **+58/-22 is small because the corpus is already well-aligned — the asymptote proven, not assumed.**
- Misses / lessons for next run: (1) the lean campaign ran LONGER (~18.5 min vs 8) because it did MORE real work (a full 6-sub-auditor sweep + 3 real fixes) — campaign time isn't apples-to-apples, but the ~21-min verify removal is a gain independent of campaign length. (2) run cost is not exposed via the remote runs API (`$?`) mid/post-run — duration/seq were the budget proxy. (3) node output artifacts (campaign's docs_aligned/drift_remaining) aren't retrievable via the remote `runs artifacts` API either — the PR body (finalize_mr embeds `campaign.summary` + `drift_remaining`) is the accessible self-report.
- Engine hardening: none needed (lean path worked first try). Session's engine-adjacent win is the feed-watch proxy-aware SSRF fix (#287, tracked separately).

## 2026-07-23 — v3.2 SELF-ORCHESTRATED coverage: the big comprehensive PR, at last (runs 019f8e08 → 019f8e9c)
- Status: **validated** — the fix for v3.0/3.1's tiny PRs; delivered **PR #284 (+1020/−429, ~29 self-found fixes across cloud/forge/DSL/webhooks)** in one run
- Versions: bot 3.1.0 → 3.2.1 · iterion `21816a00b` → `266e6ad98` · cloud prod, k8s sandbox pods, forfait Claude
- Method: catalog launch, one-line brief ("aligne la documentation de façon exhaustive avec l'état de l'art du repo"), `open_mr=true`; campaign node **`reasoning_effort: ultracode`** on opus-4-8 → the `agent` subagent tool + a one-line "cover the whole corpus, fan out by cluster"
- The diagnosis (3-way live benchmark, same repo): a SINGLE campaign agent — like a free native agent on the one-liner — self-scopes to the headline `docs/` and misses the long tail. Rising coverage: Doki 3.1 (~3 commits/pass, docs/ only) < native one-liner (6 fixes, docs/ only, MISSED cloud + bot READMEs) < native with a demanding prompt (reached the WHOLE corpus). The only run that got there **DECOMPOSED into per-cluster sub-auditors on its own**. Lever = coverage via the agent's own orchestration — NOT a mechanical checklist (an audited.json gate was drafted and rejected as excess determinism), NOT engine-wired fan-out (scaffolding). Just give the campaign the capability (ultracode subagent tool) + the lean brief, like native Claude Code.
- Result: run 019f8e08 (3.2.0) PROVED it — pass 1 self-orchestrated **40 doc-alignment commits across the whole corpus** (bots + cloud + studio) — but the 2h/$60 caps guillotined it mid-pass-2 as a hard failed_resumable, and all 40 in-pod commits were LOST. Re-run 019f8e9c (3.2.1, after the two fixes below): **finished in ~3h40 / ~$42.78 / 5 passes** (exhausted max_passes=4 gracefully — docs_aligned stayed false, the agent kept finding residual, so ADR-058 "exhaustion ships banked" carried it), finalize_mr opened **PR #284**, no work lost.
- Value: the real drift a shallow pass never reaches — `current-state.md` version `0.51.0`→`2.0.1`, a renamed live-test (`TestLive_Feat_Rtk`→`_Compress`, which also exposed a dead Taskfile target), the async `C240–C242` diagnostic band + the `await_answers` node omitted from node lists, non-existent `cloud-rest-api.md` endpoints removed, Mongo collection names, `.botz`=ZIP not tar.gz. It even documented this session's own engine fixes.
- Engine hardening (both landed + deployed before the successful re-run):
  - `7213fc6` (bot 3.2.1) — budgets sized for self-orchestration: `max_duration` 2h→6h, `max_cost_usd` 60→120, `max_passes` 8→4, so the asymptote reaches GRACEFUL exhaustion (which finalizes + exports + opens the PR) instead of being axed by a cap mid-pass.
  - `266e6ad` — **budget-exceeded now Acks, never auto-resumes**: `ErrBudgetExceeded` fell through to the generic Nak, so the run auto-redelivered → a fresh pod re-cloned → `recordRunGitMeta` overwrote the first attempt's good git metadata with base==head, silently destroying the 40 exported commits. The operator now resumes manually with a raised cap.
  - Open ticket (defense-in-depth): incremental push of in-pod commits to a storage branch on failure, so nothing is lost even on a pre-finalize hard failure.
- Lessons for next run: the asymptote exhausted rather than converged (docs_aligned never reached true across 5 passes — the agent is cautious / there is always residual on a 250-doc corpus). Board MCP was unavailable this family of runs (findings recorded in summary, not filed). `manifest.yaml` drift is out of the `.md` writeable set — Doki sees stale bot manifests but can't fix them (a scope question). Cost is real (~$16/self-orchestrated pass); the one-liner brief + ultracode is the whole mechanism — resist adding determinism.

## 2026-07-23 — v3 adaptive realignment: 4 runs, 3 stop-fix-relaunch cycles, ratio ×4 (runs 019f8ba3 / 019f8bb4 / 019f8bdd)
- Status: **validated** — the "excessive determinism is counterproductive" realignment (Billy/Willy paradigm), driven by an interrupt-fix-relaunch dogfood loop on cloud prod
- Versions: bot 3.0.0 → 3.0.2 · iterion `57b9853ec` → `ffe9ea7d9`
- Method: catalog launch POST /api/runs, vars reduced to `open_mr` + `scope_notes` (v3 dropped the v2 scanner/chunk vars), forfait Claude, k8s sandbox pods
- The three cycles:
  1. Run 019f8ba3 (3.0.0) stopped at the scan checkpoint — 604 `missing_path` hints (43% of checked paths): example paths (`bots/my-bot/…`) and runtime files (`.claude/settings.json`) passed the first-segment rule. Fix `2251b32` (3.0.1): a missing path hints iff **git ever tracked it** (index ∪ deletion history) — 604 → 14 hints (−98%). The stopped run's pod meanwhile ran to completion and delivered **PR #277** (CLAUDE.md module map: 20 missing packages) — the advisory paradigm absorbed even the noisy input without burning on it.
  2. Run 019f8bb4 (3.0.1): **14 min / ~$3.1 / 3 substantive commits** (SSRF baseline pointer, Valkey deploy dependency, hidden-subcommand list), converged pass 1 → **PR #279**. Friction: the campaign pushed its own PR (#278, duplicate). Fix `fa35e22` (3.0.2): mission contract "commit locally only — the finalize tail is the single publisher".
  3. Run 019f8bdd (3.0.2, asymptote check after #277/#279 merged): **~29 min / ~$2.6 / 1 commit + 11 dismissals**, converged pass 1, single PR #281, zero manufactured work. 22 of the 29 min = first-pass `verify_build` (agent authors verify.sh + full build/test; the docs-only fast-path only helps pass ≥ 2) — the remaining bottleneck.
- Ratio v2.5.1 → v3.0.x: 55 min/$10.12/5 commits over 5 passes → 14-29 min/~$3/pass-1 convergence; adjudication burn 120+ candidates → 10-14 plausible hints.
- Engine hardening: `ffe9ea7` cancelled-run resurrection (ticket native:85cea410) — a redelivered launch against a cancelled-with-checkpoint run was auto-resumed; run 019f8ba3 resurrected 3× (incl. via plain redelivery, runner up). Cancelled now acks; explicit resume only. Validated live: post-roll redelivery did NOT revive it.
- Lessons: hints precision belongs in the producer (git-tracked rule), publication belongs to the tail (contract line), and the next perf lever is first-pass verify (persist verify.sh across runs needs per-project scratch on cloud runners — existing follow-up). Double quotes inside inline `python3 -c` script comments break the command — caught by the emulation tests.

## 2026-07-22 (soir) — enrichment validation ×2: real work produced, then thrown away, then shipped (runs 019f8af6 → 019f8b50)
- Status: validated (2nd run) after a failed-honest 1st run — **PR #276 opened by the bot** (5 commits, 4 docs, +61 lines) with the "Unfulfilled documented promises" section live (2 real promises: ADR-044 `iterion __commit` layer, ADR-050 `C099`)
- Versions: bot 2.5.0 → 2.5.1 · iterion `69c932529` → `15c92caa1` · cloud prod, k8s sandbox pods
- Method: catalog launch via POST /api/runs (`open_mr=true`, `enrich` default on, `cli_surface_globs=cmd/iterion/*.go[,pkg/cli/*.go run 2]`, `diagnostic_surface_globs=pkg/dsl/ir/*.go`), repo SocialGouv/iterion@main, forfait Claude
- Result run 1 (019f8af6, 2.5.0): converged in 6 passes / 44 min — but **finalize threw the work away**: the campaign's real commits (`d9ef7685` architecture map of 4 undocumented packages + a cloud-cli flag table) never left the pod because the skill's ahead-check `git rev-list main..HEAD` is vacuously 0 when committing ON the checked-out base (the cloud-clone case). ~40 of the 44 min burned adjudicating ~200 false-positive `cli_flag` candidates (git/docker/gh flags quoted in docs).
- Result run 2 (019f8b50, 2.5.1): converged in 5 passes / **55 min / ~$10.12** → PR #276 (+61/-0). Pass timing: campaign 3.5-8 min/pass (productive), **verify gate 5-16 min/pass (~half the wall-clock)** re-building the whole repo for docs-only commits.
- Value: the enrichment mechanism works end-to-end — mechanical `undocumented` candidates → real documentation written (remote flags reference, `issue update --blockers`, `iterion memory` CLI, package map) → pushed PR with promises section. The dismissals ledger advanced every pass (~120 entries).
- Findings / misses: ratio production/coût still poor (55 min/$10 for +61 lines); root cause = excessive determinism (operator lesson): the anchor scanner is an *obligation generator* (every artifact must be adjudicated to satisfy `undocumented_count == 0` and coverage gates), the priority/chunk pipeline buried the real enrichment work behind scanner noise for 4 passes.
- Engine/bot hardening (all landed on main same evening):
  - `ebc7df0` — forge-mr ahead-check compares `origin/<base>..HEAD` (skill ×5 copies + 5 bots' inline prompts); the bug that discarded run 1's commits.
  - `15c92ca` — foreign-flag heuristic keys on the repo's *binary name* (repo-agnostic derivation) instead of command words (`run`, `merge` matched prose/other tools); 2.5.1.
  - `14246f4` — verify_precheck docs-only fast-path: green verdict reused when every changed path since the green HEAD is `.md` (deterministic extension rule).
  - `2dc71f5ed` — engine gap 7: `script:` tool nodes broke in copy-based (k8s) sandboxes (workspace tar-copied, temp file invisible in-pod) → write-through via `WorkspaceFileRefresher` (killed the scheduled Vigie run 019f8ac5).
  - `ca09a6292` (#275) — umbrella authMiddleware fronted the per-run `X-Iterion-Run` token surfaces: `/api/v1/forge/publish-review` 401'd on its first live exercise (Revi run 019f8ad0) and `/api/v1/mcp/board` shares the root cause (ticket native:1ec7b869).
- Lessons for next run: **Doki 3.0 realignment decided** (operator): scanner output becomes ADVISORY hints to the campaign, gate reduced to `verify ∧ scope ∧ docs_aligned`, chunk/priority pipeline dropped, ledgers kept as agent memory, universality strict (stack knowledge in skills). Also: run artifacts + cost fields are absent from the cloud API for runner-launched runs (observation to ticket); sum per-node costs from the run log instead.

## 2026-07-22 — SANDBOXED cloud validation: converged + PR opened in-pod (runs 019f8a05→019f8a8f)
- Status: validated — the full ADR-082 Phase 3 stack end-to-end
- Versions: bot 2.4.0 · iterion :edge (#268 head) · prod chart runner.sandbox.enabled=true, default image -full
- Method: API launch (bot_id + repo_url SocialGouv/iterion + connection), open_mr=true, iterion surface globs; six iterations, each failure fixed+deployed within ~15-30 min (user-sanctioned CI bypass)
- Result: run 019f8a8f — sandbox pod boots, workflow runs IN-POD, 6 passes (~35 min), gate CONVERGED (docs_aligned ∧ green ∧ coverage), 3 real doc commits (pass 4), finalize_mr pushed from the pod and opened PR #270. Doki 2.4.0 economy confirmed live: verify_precheck reuse on 0-commit passes (~3.5 min vs ~9), chunks of 22 docs (denoised), dismissals ledger advancing every pass.
- Cutover gaps found and fixed by these validation iterations (each observed live, fixed, redeployed):
  1. Runner entry point had no sandbox default (#263 + chart ITERION_SANDBOX_DEFAULT=auto) — cloud runs stayed unsandboxed after the override lift.
  2. Unusable devcontainer (iterion's own --privileged) degraded to unsandboxed forever → default-image fallback at the ambient tier (#264).
  3. k8s driver hard-required sandbox.user → defaults to the published images' 1000:1000 (#265).
  4. git safe.directory: root-owned emptyDir mountpoint vs uid-1000 exec broke repo discovery (exit 128) → GIT_CONFIG_* protected-config env injected pod-wide (#266).
  5. Prod forfait rides the runner pod's AMBIENT env; kubectl-exec spawns only get the SDK env map → claude "Not logged in" in-pod, masked as structured-output schema errors (the known gotcha) → ambient Anthropic env forwarded verbatim for sandboxed spawns (#268).
  6. Board MCP HTTP endpoint not wired on runner-launched sandboxed runs (board handoff degraded to summary) — ticket native:1ec7b869, open.
- Also: cancelled cloud runs resurrected after runner restarts (NATS redelivery ignores terminal status) — ticket native:85cea410, fixed in `ffe9ea7d` (cancelled is now terminal for redelivery: the runner acks a redelivered launch against a cancelled-with-checkpoint run unconditionally, so only an explicit resume can continue it — no auto-resume resurrection).
- Lessons: validation-by-real-run caught six gaps no test suite had; "auth failure masquerading as schema error" struck again (test auth FIRST); the argv idiom has a kernel ceiling (ride files); merge-queue drops are silent — verify the gh-readonly-queue branch, and a red gate ON MAIN (OpenAPI drift from direct pushes) starves the whole queue.

## 2026-07-22 — first repo-targeted CLOUD dogfood + PR tail (runs 019f86ac, 019f86ce)
- Status: validated (infrastructure) / low direct value (docs) — the whole cloud PR-tail path + ledger + fixes proven live; zero real doc drift found (all candidates were scanner noise, now denoised in 2.4.0)
- Versions: bot 2.2.0→2.2.1→2.3.1 (runs) / 2.4.0 (post-run) · iterion 37c36f7→aff5e02 (prod :edge)
- Method: PROD cloud instance, studio LaunchView launch (repo-first Target repository = SocialGouv/iterion via GitHub App connection), open_mr=true, cli_surface_globs=cmd/iterion/*.go, diagnostic_surface_globs=pkg/dsl/ir/*.go, defaults 2h/$60/opus-4-8 (forfait runner)
- Result: run 1 (019f86ac, 2.2.0) failed in seconds — build_manifest exit 127; cancelled. run 2 (019f86ce, 2.2.1) ran the full campaign loop: verify gate red pass 1 (verify.sh lacked the CI drift gate → verify_build self-corrected pass 2), then 8 more green-but-not-aligned passes re-judging the SAME 41 cli_flag false positives (the livelock), pass 4 also hit error_max_structured_output_retries (auto-resume recovered). Loop exhausted at 9/9 passes → mark_issue_for_review → update_audit_cache CRASH-LOOPED (E2BIG, bug 3 below) → failed_resumable at 00:21. 9 passes, $14.81, 248k tokens, ~1h48. The PR tail (mr_gate→finalize_mr) was never reached
- Value: the dogfood surfaced 3 real defects (2 engine-adjacent, 1 bot-design) in one evening — exactly what dogfooding is for; the doc-alignment value itself was nil (all adjudicated candidates were cli_flag false positives)
- Findings / misses:
  - ENGINE (space-join): all-string list substituted into a tool command as env assignment executes the 2nd element (`DOC_FILES={{input.doc_files}}` → `bash: README.md: command not found`). Regression exposure since 55d7170e6 (raw-interpolation removal); invisible until now because the 2026-07-20 noop dogfood short-circuited before build_manifest. Bot-side fix: argv section markers (update_audit_cache idiom) — PR #251, bot 2.2.1. Fleet sweep ticket: native:d673c4df (sec-audit langs, rgaa examined_files, adr-cartograph…).
  - BOT DESIGN (livelock): zero-commit adjudication pass → manifest byte-identical → severity-sorted chunker re-surfaces the SAME chunk → run re-judges the same 41 cli_flag false positives every pass (~$1/pass) until max_passes. v2 had lost v1's dismissed-pairs accumulator. Fix: dismissals ledger (campaign records {doc,kind,value,reason} in vars.dismissed_path; build_manifest excludes before sort/chunk; dismissed_excluded telemetry) — PR #254, bot 2.3.0, proven by two-pass emulation (chunk window advances).
  - BOT SCALE (E2BIG): update_audit_cache took verified_pairs INLINE as argv; ~4200 pairs on this repo blew MAX_ARG_STRLEN (fork/exec bash: argument list too long) — crash-looping the terminal node until auto-resume gave up. The argv idiom (the fix for the space-join bug!) has a scale ceiling; large handoffs must ride FILES. Fix: build_manifest writes <scratch>/verified_pairs.json, update_audit_cache reads the path — bot 2.3.1 (same PR #254).
  - BACKEND (structured-output flake): pass-4 campaign died on error_max_structured_output_retries (missing is_code_bug then summary in the termination contract) → failed_resumable → cloud auto-resume relaunched the node fresh. Auto-resume behaved exactly as designed.
  - SCANNER NOISE: cli_flag extraction flags foreign-tool flags in command examples + real iterion flags outside cli_surface_globs — dominant FP source; candidate for a scanner tightening pass (require the doc to document iterion CLI context).
  - CLOUD LIMIT (expected): PROJECT_SCRATCH_DIR is per-run on the runner → inter-run audit cache + noop short-circuit inert on cloud; the new dismissals ledger is also per-run there (works within a run — the livelock fix — but not across runs). Follow-up: per-project persistent scratch on runners.
- Engine hardening: PR #251 (argv fix), PR #253 (async e2e -race flake: 4s ceiling under the 5s level-poll), PR #254 (ledger); the dogfood also motivated ADR-082 sandbox-by-default (Phase 1 shipped, PR #252) after the "Launch without sandbox?" modal question.
- Lessons for next run:
  - Launch catalog bots by bot_id (inline upload strips bundle skills); repo targeting only via studio/API (repo_url+connection_id).
  - The PR tail (mr_gate→forge_auth_probe→finalize_mr) is the ONLY delivery path for repo-targeted cloud runs (ephemeral clone; merge_strategy and runs merge are local-only).
  - A zero-commit "all false positives" pass is a convergence hazard for ANY chunked-manifest bot: persistence of dismissals is part of the termination contract (check adr-cartograph for the same class).
  - Validate with a 2.3.0 run: expect dismissed_excluded > 0 on pass 2+, chunk window advancing, and either real-drift commits + a PR opened by finalize_mr, or an honest 0-commit no-PR finish.
  - VALIDATION RUN (019f874d, bot 2.3.1): the ledger works live — dismissed_excluded 0→40→…→318 across 9 passes, docs_with_drift 196→160, chunk window advancing every pass (livelock dead). Full tail again: update_audit_cache wrote 234 entries via the pairs file (no E2BIG), probe found the forge_token, finalize_mr honestly refused the empty PR (rev-list-verified 0 commits). 9 passes, $16.43, ~1h35, finished. Still zero REAL drift found: candidates were foreign-tool cli_flags + unverifiable symbol_refs — 100% false positives across both runs.
  - BUDGET/PRODUCTION (operator finding): ~$31 / ~3h20 for zero doc fixes is a terrible ratio. Root causes and fixes shipped as bot 2.4.0 (PR #256): deterministic verify_precheck (skip the ~5-6min/~1$ verify chain when HEAD unchanged since last green verify — 8 of 9 passes qualified), foreign-flag heuristic (drifted cli_flag lines that reference no known CLI command → unverifiable), unverifiable symbol_refs no longer surfaced by default (include_unverifiable_symbols=true to opt in). Expected effect on this repo: adjudication-only passes drop from ~9min to ~3-4min and the candidate pool shrinks to real drift.
  - Studio replay pane shows "No log captured." for these cloud runs — diagnosis/fix delegated (separate PR).

## 2026-07-07 — v2 dogfood on iterion's own docs: 4 real fixes in stride, honest non-convergence on manifest FPs, self-filed findings (run 019f3d4d-1aed)
- Status: **VALIDATED** — every v2 mechanism behaved as designed over 3 passes, including the honesty-under-chunking clause and the findings handoff; the residual is manifest-heuristic quality, filed as issues.
- Versions: bot v2.0.0 · iterion `dev+239203525cc8` · no sandbox, worktree: auto.
- Method: CLI run, `--store-dir <workspace>/.iterion`, `--merge-into none`, `diff_since=origin/main`, `bundle_self_path=bots/docs-refresh`, `max_passes=2`, `--max-cost-usd 20 --max-duration 1h`. 27m32s wall, 3 campaign passes (NB: `as loop(N)` bounds LOOP-BACKS, so N=2 ⇒ up to 3 passes — wording fixed across the v2 bots after this run).
- Result: `finished` (loop exhausted → ship-what-is-banked). Scan: 197 docs, 3592 anchors, 48 mechanically drifted, coverage 97%. **Pass 1: 4 real doc fixes, one commit each in stride** on `iterion/run/ash-throb-laserdoom-aa99` (dead ADR-010/ADR-053 links after renames, dead pkg/ links in the totality doc, goldens scenario list aligned with the Nexie v2 rewrite). Passes 2-3: the re-manifested chunks were **100% heuristic false positives** — the campaign verified each at the anchor (ls/find evidence), refused to "fix" non-drift, reported `docs_aligned=false` with `commits_this_pass=0`, and did NOT rubber-stamp under chunking (106 docs still deferred). scope_check clean ×3; verify green ×3; cache rewritten with **178 mechanically-verified entries** (the new verified_pairs path); mark_issue no-op (no issue_id).
- Value: real doc repairs landed + the run itself produced its follow-up work: the campaign **self-filed** the manifest-bug finding (native:cbf91e1f — .tsx→.ts truncation ×17, repo-root-relative link base, [link-text] vs ](target) extraction) after checking the inbox first and REFERENCING the operator-filed dismissed-pairs improvement (native:8c6dc311) instead of duplicating it.
- Findings / misses: build_manifest's extractor needs the 3 fixes above (severity low — FPs cost passes, not correctness); without persisted dismissals each pass re-adjudicates the same FPs (native:8c6dc311).
- Engine hardening: none needed by this run.
- Lessons for next run: on a healthy doc tree, `max_passes=1` (2 passes) suffices — extra passes only re-judge FPs until the manifest fixes land; `diff_since=origin/main` correctly prioritised the fresh drift.

## 2026-07-07 — converted to v2 minimal-framing (ADR-058 fleet rollout) — structural-validated, dogfood pending
- Status: **converted, dogfood pending** — structural validation only this pass: `iterion validate` clean, catalog universality/typing/bundle-consistency green, stub e2e green where wired. NOT yet live-dogfooded in the v2 shape; treat the sections below as describing the RETIRED v1 shape.
- Versions: bot v2.0.0 · iterion worktree branch (rollout of 2026-07-07, see git log)
- Shape: The deterministic audit machinery (scan_docs, scan_code_surface, build_manifest with the bounded severity-sorted/doc-chunked working set, author_docs DEFAULT-CREATE, mark_issue, audit cache) is kept in full; the alternating review/fix relay + its accumulators are replaced by ONE campaign that adjudicates the manifest and commits each aligned doc in stride. New deterministic gates: scope_check (writeable-set vs run base) + verify + coverage-in-gate; the cache is now fed by the manifest's mechanical verified_pairs. 16 nodes → 11 exec; worktree: auto.
- Reference proof of the shared mechanism: feature-dev v2 pilot run 019f3bb4 (one pass, 11m33s, 2 in-stride commits, deterministic gate converged — see docs/bot-runs/feature-dev.md) and the Willy/Billy v2 tours.
- Next: a dedicated live dogfood + bilan in this file before the bot counts as validated in its v2 shape.

## 2026-06-22 — docs/studio-visuals branch self-review (run 019eef81)

- **Status: validated** — caught two real doc/code drifts, fixed both,
  and respected the steering (left the just-added screenshots, Mermaid,
  and cross-links untouched).
- **Versions:** bot manifest v0.14.0 · iterion binary v0.14.0
  (`36f19723f`), branch `docs/studio-visuals`.
- **Method:** dogfood right after a large docs round (new
  `human-in-the-loop.md`, a studio screenshot gallery, ASCII→Mermaid
  conversions, README hero). Ran **in place** on the worktree (no
  `worktree: auto`), host mode (claude_code/opus-4-8 + claw/gpt-5.5).
  Scoped to the **19 docs the branch changed** via `doc_globs`, plus
  `bundle_self_path=bots/docs-refresh`,
  `code_scope_globs=pkg/**/*.go,cmd/**/*.go`, `diff_since=main`, and
  `scope_notes` pinning "do not strip the intentional screenshots /
  mermaid / links". Store = worktree `.iterion`.
- **Result: converged in one review pass (~14 min), committed
  `727e384c0`** ("docs(dispatcher): correct per-ticket Bot routing and
  webhook test references", 2 files, +11/−7). `mark_issue_for_review`
  skipped (standalone run, no `issue_id`) → no board writes;
  `.docs-refresh-cache.json` gitignored.
- **Value: two genuine drift fixes.** (1) `dispatcher.md` documented
  the per-ticket `Bot` field as resolving into
  `DispatchSpec.WorkflowPath` — a struct field that **no longer
  exists** — and dismissed it as non-functional "future plumbing".
  Doki verified against `loop.go`/`runner.go` and rewrote it to the
  real behaviour (`Bot` → routing key `DispatchSpec.Assignee` →
  `RoutingRunner` selects the per-bot `EngineRunner`). (2) `byok.md`:
  stale test reference `TestGitLabWebhook` → `TestGitLabWebhook_HappyPath`
  (confirmed at `pkg/server/webhooks_gitlab_test.go:47`) + linked the file.
- **Findings / misses:** **zero over-reach** — it did not touch the
  freshly-added `images/studio/*.png`, ```mermaid blocks, or
  cross-links, and didn't churn the rest of the branch's prose.
- **Engine hardening:** none — clean run.
- **Lessons for next run:** scoping `doc_globs` to the branch's changed
  docs + `diff_since=main` + `code_scope_globs` gave a fast, focused,
  accurate pass (one cycle vs the 3 of the full-tree 2026-06-14 run).
  The `Bot`→`WorkflowPath` catch is textbook docs-refresh value:
  finding docs that name removed/renamed symbols.

## 2026-06-14 — repo-wide .bot→.bot CLI-example drift (run 019ec7ba)

- **Status: validated** — fixed exactly the drift the ticket flagged; correct,
  complete, scoped, intentional mentions preserved.
- **Versions:** bot v0.15.0 · iterion @ `8a00e93b` (main)
- **Method:** board ticket `a3b57a17` ("docs-refresh missed repo-wide .bot→.bot
  CLI-example drift"). docs-refresh has **no `worktree: auto` and no sandbox** →
  ran on the **live tree** (host mode: `claude` 2.1.177 on PATH, forfait forced),
  scoped to the 5 drifted files via `doc_globs`, `bundle_self_path=bots/docs-refresh`,
  `store-dir=.iterion`. Committed directly to main.
- **Result: converged + committed `211e69f7`** ("docs(cli): update CLI examples
  to use the .bot extension", **5 files, 23/23**). 3 reviewer cycles, **$8.40 /
  127k tokens**. Independently verified mid-loop: **0 runnable `.bot` left** in
  all 5 files, **zero over-reach** (nothing outside the 5 in-scope docs),
  `examples.md`/README/CLAUDE intentional "rejected/legacy" mentions untouched.
  `.docs-refresh-cache.json` is gitignored (no repo pollution). Board a3b57a17 → done.
- **Value: correct + scoped.** The commit message even states "Prose references
  to .bot as the DSL raw/source form are intentionally left unchanged" — the bot
  understood the preserve-intentional-mentions instruction.

### Findings / misses
- **Doki's automated scanners do NOT auto-detect CLI-example extension-literal
  drift.** `iterion run X.bot` in a code fence is not a dead link/anchor, so
  `md_link`/`build_manifest` don't flag it. Doki fixed it **only because
  scope_notes pointed the reviewers at it** — an *unguided* run would miss
  a3b57a17 again. The "miss" is a real scanner-coverage gap. Improvement idea:
  a CLI-example scanner that cross-refs example arg extensions against the
  CLI's accepted extensions (.bot/.botz).
- **cwd foot-gun (caught live by the Monitor).** For a bot with NO `worktree: auto`,
  the claude_code agents' cwd = the launch *process* cwd, **not** `--var
  workspace_dir`. First attempt isolated Doki in a worktree but launched from the
  main repo → reviewers got `File does not exist` (reading the wrong tree). Fix:
  to isolate a non-worktree:auto bot, launch **from** the target dir (cwd ==
  workspace_dir), or just run on the live tree. (workspace_dir only affects
  tool-node `git -C {{workspace_dir}}` commands, not agent cwd.)
- **Launch lesson:** a backgrounded `iterion run … | head -N` gets SIGPIPE-killed
  after N lines (head closes the pipe). Never pipe a long-running background
  command to `head` — redirect only.
- **Cost note:** $8.40 for a 23-line mechanical fix. Doki's opus review loop is
  expensive; for pure mechanical drift a manual edit is ~free. Reserve Doki for
  genuine doc-vs-code mismatches that need judgment, not trivial find-replace.
- Engine health clean: no `StructuredOutput` error; reviewer_gpt (claw/forfait)
  fine; the `fix_claude` Read-before-Edit + grep-exit-2 errors were transient
  self-corrections, not failures.

## 2026-06-14 — first dogfood + md_link scanner improvement (runs 019ec675, 019ec69f)

First recorded dogfood, on the real board ticket `c4043495` ("Align the
.bot documentation boundary"). Run in an isolated git worktree
(`--merge-into none`), store pointed at the operator's `.iterion` so the
run was visible in studio. Bot launched via standalone `iterion run` (not
the watchexec studio backend) and the install was a fresh static binary at
HEAD — both per the CLAUDE.md dogfood discipline.

- Status: **validated (with one real coverage finding, since fixed)**.
- Versions: bot 0.13.1 → **0.14.0** (this session) · iterion `e9148046`.
- Method: claude_code `claude-opus-4-8` + claw `openai/gpt-5.5`; isolated
  worktree; `--var doc_globs=CLAUDE.md,README.md,docs/**/*.md,pkg/cli/templates/*.bot,*.bot`
  `--var scope_notes="resolve .bot tension"` `--var bundle_self_path=bots/docs-refresh`.
- Result: **converged in 4 review iterations**, `$7.68`, ~126k tokens,
  ~27 min. Commit `e9520f11` on `dogfood/docs-refresh-boundary`.
  `.md`-only contract held; `prepare_commit` re-verified every code ref
  before committing (anti-façade discipline working).

### Value produced
- Caught + fixed **real drift**: `docs/secrets-reference.md` linked a dead
  path `pkg/auth/auth.go:GenerateRandomToken` — the function actually lives
  at `pkg/auth/password.go:118` (auth.go does not exist). Fixed, verified.
- `docs/bot-runs/whats-next.md` — clarified a local run-artifact path that
  read as a committed repo path.

### Finding (bot coverage gap) → FIXED this session
The bot **converged without resolving the ticket's headline item**:
`CLAUDE.md:3` still claimed "`.bot` / `.bot` — identical semantics" and
linked a **dead anchor** `README.md#iter-vs-bot` (the README heading was
removed; the CLI now rejects `.bot` outright — `unsupported workflow
extension`). The reviewers verify doc→**code** refs (symbols, CLI surface,
file paths under known roots) but nothing systematically audited
doc→**doc** internal links / `#heading-anchors`. `FILE_RE` in
`build_manifest` only matches paths under known roots (so bare `README.md`
slipped through) and never captured the `#anchor` fragment. The
`dead_link` taxonomy existed but had no deterministic candidate feeder.

**Fix (v0.14.0, `build_manifest`):** added an `md_link` anchor kind that
extracts `[text](path#anchor)` links and verifies BOTH the target file's
existence AND, for `.md` targets, the `#heading-anchor` (GitHub-slug
match: lowercase, strip non-`[\w\s-]`, spaces→hyphens, strip leading/
trailing hyphens to handle emoji headings; line anchors `#Lnn` skipped).
Drifted `md_link`s flow through the existing candidate pipeline at high
priority; `doc-mismatch-taxonomy.md` now points `md_link` → `dead_link`
(`anchor_kind: external`). Validated standalone over the full 153-doc tree
(**764 verified / 16 drifted, 0 false positives** after the slug fix), and
in a real scoped re-run (019ec69f) `build_manifest` flagged exactly the two
dead anchors (`CLAUDE.md:3` + `docs/examples.md:12` → `README.md#iter-vs-bot`,
`drifted_anchors: 2` of 288, zero FP). The scanner is generic — dead
internal links/anchors are a universal doc-drift class, not iterion-specific.

### Engine hardening
- Ticket **`d8e8dde1`** — **FIXED this session** (`3b29efb1`). Every
  claude_code node with schema + tools emitted `tool_error: No such tool
  available: StructuredOutput`: the agent (behaving natively, as the
  adaptivity work intends) reached for the SDK's `StructuredOutput` tool —
  available only under `--json-schema`, which iterion set in Pass-2, never
  Pass-1 — wasting its Pass-1 final turn (`raw_output_len: 0`) before the
  **unconditional** Pass-2 formatting round-trip. Root insight (verified
  empirically against claude 2.1.177): `--json-schema` composes with
  `--allowedTools` in ONE pass — the agent does its tool work, then calls the
  native StructuredOutput tool, populating `result.structured_output`. So the
  fix sets `WithOutputFormat` in Pass-1 even with tools, returns Pass-1's
  structured output directly when valid, and keeps the two-pass `formatOutput`
  as a fallback (max-turns / sandbox edge cases). Validated on run `019ec727`:
  both `reviewer_claude` (reader) and `prepare_commit` (writer) →
  `formatting_pass_used=false`, no error, valid output; converged; A/B vs the
  pre-change binary shows no regression. Saves one LLM round-trip per
  schema+tools claude_code node across all bots.
- Side: closed a **stale "ready" board ticket** (`native:21065752`, Revi
  "scan_shards.go:458 blocks until shard timeout") — already fixed on HEAD
  by `59cfedcc` + covered by `TestAwaitTerminal_PreDispatchFailureDoesNotHang`
  (passes). A dispatch would have wasted a run on an already-fixed bug.

### Lessons for next run
- **Cost**: `$7.68` to fix 2 lines of incidental drift is high — the 80%
  coverage gate over a **114-file** footprint makes every reviewer pass
  heavy. For a focused ticket, scope `doc_globs` tightly (a 3-file scope
  re-run cost a fraction). `scope_notes` is only a HINT; the mandatory
  full-footprint coverage dominates, so a reviewer can converge on
  incidental drift while leaving the operator's stated focus untouched.
  Consider weighting `scope_notes`-named files into the coverage gate.
- The `md_link` scanner now closes the dead-anchor class; re-run the
  original `c4043495` scope to land the CLAUDE.md:3 / examples.md fixes.

## 2026-06-14 — synthetic clone-validation + 2nd real-bot C082 proof (run 019ec58a)

A second, independent dogfood from the **C082 board-emit** session (parallel
to the real-board run above). Purpose was narrower: confirm Doki's machinery +
convergence on a clean iterion **clone** and, incidentally, exercise the C082
sandboxed-board fix end-to-end on a real catalog bot. (Lower-value target than
the real-board run above — kept for the C082 proof + the gitignore finding.)

- Status: **validated.** Converged with **zero** false fixes (the audited docs
  were accurate).
- Method: dedicated worktree studio :4899 (C082 worktree binary, forfait env),
  clean iterion clone; `doc_globs=docs/resume.md,docs/routers.md`,
  `code_scope_globs=pkg/store/**/*.go,pkg/runtime/**/*.go,pkg/dsl/ir/**/*.go`,
  `merge_into=none`. claude_code/opus + claw `gpt-5.5` forfait.
- Result: **converged in ~2 rounds to a cross-family double-approval**, no
  oscillation. reviewer_gpt audited 18 symbol refs in `docs/resume.md`,
  confirmed them in the Go code, and concluded "No documentation changes
  needed" → `commit_changes` a correct no-op. Correct verification, no
  hallucinated drift.
- **C082: confirmed on a 2nd real catalog bot.** `prepare_commit` (sandboxed
  claude_code, board.create cap) invoked `mcp__iterion_board__create_issue`
  twice through the C082-fixed HTTP transport → board 0→2, real native ids.
  Independent of the minimal C082 validation bot — proves the fix works in a
  real bot.
- Findings:
  1. **`.claude/skills/` runtime mirror not gitignored — FIXED** (`.gitignore`
     `.claude/skills/` + `.docs-refresh-cache.json`). The engine mirrors
     `<bundle>/skills/*.md` into `<workspace>/.claude/skills/` at run start; it
     was uncovered, so it shows as `?? .claude/` and can be swept into a code
     bot's commit (later confirmed live: Bmady's commit included the mirror on
     a clone without this fix). Broader runtime-level exclusion is tracked.
  2. **Empty `code_scope_globs` → every symbol "unverifiable"** (a first
     attempt with `doc_globs` only marked all 22 symbols unverifiable). Always
     pass `code_scope_globs`; an empty default arguably should mean "scan the
     workspace."
  3. Same `StructuredOutput` tool-error as ticket `d8e8dde1` above (non-fatal).
- Lessons: pass `code_scope_globs`; the bot is safe (no false fixes) +
  doctrine-compliant (neutral cache path, flags code issues to the board).
