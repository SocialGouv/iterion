# Billy — branch-improvement validation

## 2026-09-05 — the plan-budget guard reads the run, and both refusals are typed (no run; bot 1.5.0)

- Status: **validated** by the engine, not by a live run — this is a bot
  change made against two engine features that landed the same day
  (PR #764: the `run.*` expr namespace, #738; the typed `fail <name>:`
  node, #739), issue #752/#762. The next dogfood should confirm the
  refusal reads right in the studio and that a real `iterion remote runs
  resume --max-cost-usd …` walks past the guard.
- Versions: bot `branch-improve-loop` 1.4.0 → **1.5.0** · iterion at
  `c2898c08f` (v3.108.1, the first build carrying `run.*` and typed fails).
- Method: no LLM run. The guard is exercised through the ENGINE with the
  scenario stub against the bot's own shipped `budget:` block
  (`e2e/branch_improve_loop_test.go`, five cases): the stubbed plan nodes
  bill a chosen amount, and the readout is which tail the run took.
- Result — what changed in the bot:
  - `plan_budget_gate` is now **one `compute`** reading
    `run.elapsed_seconds` / `run.cost_usd` against
    `run.max_duration_seconds` / `run.max_cost_usd`. It replaces the tool
    node that shelled out to python for `time.time()` arithmetic.
  - **The two mirror vars are gone** (`budget_max_duration_minutes` /
    `budget_max_cost_usd`). They existed only because no primitive exposed
    the run's caps, and they were kept in sync **by hand**: `iterion run
    --max-cost-usd 200` re-budgeted the run and never reached them, so the
    guard went on refusing against a literal `75` nobody had updated. The
    `run.max_*` members are the caps IN FORCE — after the CLI flags, the
    recipe, the platform ceiling and any live `raise_budget` — so the
    drift is structurally impossible now, not merely documented.
    (`TestBranchImproveLoop_PlanBudgetFollowsTheCapInForce` is that
    property: the same $37.50 spend refuses under the shipped $75 cap and
    passes under a re-budgeted $200 one.)
  - `plan_scope_probe`'s `started_epoch` is gone with it; the run's clock
    is monotonic and survives a resume, which a `time.time()` stamp did
    not.
  - `plan_cost_probe` is gone: it existed to hand the tool node a nil-safe
    per-node cost SUM. Nothing but the plan phase has spent when the guard
    runs, so `run.cost_usd` IS the phase's spend — and it also counts
    anything a hand-written sum would have forgotten. Its relay role moved
    onto the guard, which now receives the hand-off from all three
    upstream routes directly.
  - Both refusals are **named fail nodes**, so the code reaches the RUN
    (`failure_code` / `error`) instead of only the guard's output:
    `plan_exhausted` (PLAN_BUDGET_EXHAUSTED, **resumable**) and
    `workspace_not_a_repo` (WORKSPACE_NOT_A_REPO, terminal). The 09-05
    dogfood below recorded both as debt: two production runs ended
    `failed` reading `workflow reached fail node`, and the operator had to
    open the artifacts to learn which refusal fired.
- **How to resume a `PLAN_BUDGET_EXHAUSTED` run.** The refusal is
  `failed_resumable` and the checkpoint anchors on `plan_budget_gate`, not
  on the fail node — so the resume **re-evaluates the guard** against the
  caps then in force:

  ```sh
  iterion resume --run-id <id> --file bots/branch-improve-loop/main.bot \
    --max-duration 5h --max-cost-usd 150
  # or, same effect from the other side:
  iterion resume --run-id <id> --file … --var plan_budget_ratio=0.6
  ```

  The plan phase this run already paid for is NOT re-run (the checkpoint
  keeps `plan`/`plan_review`/`plan_revise`'s outputs); the campaign starts
  on the plan already in hand. Nothing picks the refusal up by itself —
  not `--auto-resume`, not the cloud runner's retry: a deliberate refusal
  only changes verdict when an operator changes an input. Widening the cap
  is the only thing that flips it, which is exactly why re-paying the plan
  phase would be the wrong cure.
- Value: the two workarounds the 09-05 bilan filed as debt (#738, #739)
  are both retired, and the guard's arithmetic can no longer disagree with
  the budget the run is actually under.
- Findings / misses (engine, filed as observations for #762's PR):
  - `{{run.*}}` renders EMPTY inside a `fail` node's `message:` — the fail
    message resolves through `resolveMapping(scope)`, which carries
    `outputs`/`vars` but not the run snapshot the expr evaluator and the
    prompt/tool templates read. Measured on a probe bot: `run.elapsed=
    cap=`. The bot works around it by putting the four figures on the
    guard's own output and referencing `{{outputs.plan_budget_gate.…}}`,
    which is arguably better anyway (the numbers are then also in the
    artifact) — but the authoring instinct is to write `{{run.cost_usd}}`
    there and get silence.
  - A **tiny `max_duration` cannot be the lever** for a deterministic test
    of the duration axis: the engine refuses a new node at 90% of a cap,
    so with a small enough cap `BUDGET_EXCEEDED` fires before the guard
    ever runs. The e2e drives the COST axis instead (a figure the stub
    sets exactly); the duration axis is covered by the `> 0` unbounded
    case and by the guard's own expression.
  - A compute's output schema does **not** coerce a float into an `int`
    field (probed: `used_pct: int` kept `0.0952…`), and float refs render
    at full precision in a template. The gate therefore reports seconds
    and USD rather than a rounded percentage.
- Lessons for next run: launch with `--var plan_budget_ratio` small to
  force the guard as before, then check `iterion remote runs list` shows
  PLAN_BUDGET_EXHAUSTED (not FAIL_NODE) and that the resume with a widened
  cap starts `campaign` without re-running `plan`.

## 2026-09-05 — lock-delivery follow-up on PR #770: an "open question" was a two-part defect, and one comment cited a function that never existed

- Status: **delivered**. Run
  [01a07283-16f2-7b55-bf66-db10c5453fdf](https://iterion.fabrique.social.gouv.fr/runs/01a07283-16f2-7b55-bf66-db10c5453fdf),
  a focused follow-up to `01a07243` on the same
  [PR #770](https://github.com/SocialGouv/iterion/pull/770) /
  [issue #703](https://github.com/SocialGouv/iterion/issues/703).
- Method: seeded with Revi's single new finding `R1dca02` plus five open
  questions, and with a plan a cross-model peer had already critiqued. The
  peer's catch changed the shipped result — see below.
- Result: nine commits, `38f2d98e2` … `eaa33d0ce`. `R1dca02` fixed (the
  non-contention lock class logs at Error again, so a broken lock store
  raises a tracker event instead of a breadcrumb nobody ships); one open
  question promoted to a defect and closed; one phantom cross-reference
  corrected; two ratchets landed.
- Verification — **passed** with the exit code captured before any pipe:
  `task test` (rc 0), `go test ./e2e/...` (rc 0, 769s), `task lint` (rc 0),
  `go test -race` on runner/queue-nats/server (rc 0), and the three studio
  targets `studio:lint` / `studio:typecheck` / `studio:test` (rc 0, 1308
  tests). **Unavailable locally**, left to the GitHub checks: the
  `mongo-conformance` and `nats-conformance` jobs (each needs a service
  container) and the Playwright UI suite (opt-in browser download).
- Verification gotcha worth keeping: the first attempt at each of these
  piped through `grep`/`tail` and then read `$?`, which reports the LAST
  pipeline stage, not the test command. A studio `tsc` invocation that died
  on `exit 127` (no `node_modules`) printed `EXIT=0` that way. Redirect to a
  log, save `$?` immediately, then filter — or set `pipefail`.
- Value — the open question was the bigger finding. Registering LockTTL in
  `RedeliveryWindow` is the obvious half, and on its own it is **inert**:
  `cmd/iterion/server.go`'s `natsq.Connect` literal never passed LockTTL, so
  `applyDefaults` pinned the sweeper's own connection to 60s and the widened
  formula would have read a value no deployment configured. Passing it also
  closes a second, pre-existing hazard this branch made load-bearing —
  `EnsureSchema` writes the KV bucket TTL from `cfg.LockTTL`, so server and
  runner disagreeing meant the effective lease lifetime flapped by restart
  order. Neither edit protects an `ITERION_LOCK_TTL=15m` deployment alone.
- Findings the review missed: `archiveLockFailure`'s audit-deadline comment
  justified itself as "same hazard, same remedy as parkAdmissionMismatch's
  status flip" — a function that exists nowhere in the tree, and whose real
  sibling (`parkOnDLQOnFinalDelivery`) does the OPPOSITE, handing its spent
  publish context straight to the status flip. A citation asserting a settled
  pattern for a remedy no other site applies.
- Ratchets: the log-level regression asserts the hook LEVEL, not the message
  (verified failing against the unfixed tree first); and a wrapped-`ErrLockHeld`
  test pins the classification against the shape production actually delivers
  — every other double returns the sentinel bare, so swapping `errors.Is` for
  `==` kept the whole suite green while the fleet would page on every sibling
  collision (verified: that one token turns the new test red and nothing else).
- Lessons for next run: an "open question" in a review is not automatically
  out of scope — this one was a real defect whose fix needed a second edit in
  a file the reviewer never named. And a comment citing a precedent deserves
  the same grep a code reference gets; a phantom name reads as authority.
- One open question answered with evidence rather than left open: a foreign
  `run_delivery_exhausted` does NOT disturb a live run's observers, and the
  reason is narrow enough to be worth a comment at the emission site.
  `alert.Manager` treats **any** event as liveness (it clears `stallAlerted`
  and can fire a spurious `stall_recovered`) — but it is fed only by the
  local `events.jsonl` tailer and in-process run observers, never by the
  Mongo store this path writes to; and the cloud twin `alert.OpsDispatcher`
  filters the bus to `KindRunFailed`, which a store event never becomes.
  Wiring a cloud event source into the Manager would turn this row into a
  false liveness signal.
- Left deliberately: `MaxAckPending` headroom during a lock outage and
  sweeper-vs-operator DLQ-replay ownership — genuine open questions, not
  findings. `parkOnDLQOnFinalDelivery`'s inherited publish context is a real
  smell but pre-existing and outside this branch.

## 2026-09-05 — lock-delivery hardening on PR #770: seven commits delivered; the publisher still says nothing was pushed

- Status: **first pass delivered and verified; review follow-up pending**.
  Run [01a07243-13f3-7229-8aea-801a2fc3569e](https://iterion.fabrique.social.gouv.fr/runs/01a07243-13f3-7229-8aea-801a2fc3569e)
  finished on 05/09 at 16:46Z, with 54m42s of recorded active duration.
  Target: [PR #770](https://github.com/SocialGouv/iterion/pull/770),
  [issue #703](https://github.com/SocialGouv/iterion/issues/703).
- Method: a maintainer `/billy` comment named Revi findings `Rd41f5d` and
  `R1cae68`; auto-merge stayed off. Plan, peer review and revision preceded
  the campaign. No interactive session edited the branch while Billy ran.
- Result: seven commits reached the PR, from `b8166f730` to
  `eb58773074357201f544d11525090a262be732a3`. The run's final commit equals
  that PR head; its storage branch is
  `iterion/run-01a07243-13f3-7229-8aea-801a2fc3569e`.
  `fca63105e` gives the audit an independent deadline after the DLQ publish;
  `9a2b771af` distinguishes confirmed contention from unconfirmed ownership.
  Further commits correct the lost-PubAck wording, document both unknowns,
  add the event to the studio union and update the coverage matrix.
- Proof: `TestExhaustedPublishDeadlineStillRecordsTheAuditRow` waits until
  the publication context expires and uses a store that honours cancellation;
  the old implementation loses the audit row. The reason-classification
  regression rejects both an asserted owner and asserted absence when the
  lock service did not answer. Billy's verification gate returned exit 0
  (format/build/vet, touched Go suites, race checks and lint); the new head's
  NATS conformance CI also passed. The full PR test check was still running
  when this record was written.
- Value of the peer review: a failed lock acquisition does **not** prove
  absence of an owner, and a missing PubAck does **not** prove the DLQ copy
  is absent. The final code reports both as unknown instead of inviting an
  unsafe replay or discard. No lock-failure branch mutates the run outcome
  or checkpoint.
- Friction: despite the campaign having pushed all seven commits,
  `publish_verdict` opened its review with “No commits pushed. nothing to
  push: HEAD not ahead of origin/codex/fix-703-lock-delivery-dlq” and reported
  a failure status. The same review then correctly listed the delivered
  commits and fixed findings. This is additional evidence for
  [#773](https://github.com/SocialGouv/iterion/issues/773), not missing work
  in this run: the PR head and final commit were equal. Revi's independent
  review subsequently replaced that status with success.
- Remaining work: the independent review found `R1dca02` — infrastructure
  lock failures had lost their error-level log, suppressing the tracker
  event. Keep auto-merge off for the follow-up correction. Also check that
  a configured lock delay larger than AckWait is represented in the
  redelivery window used by the queued-run sweeper.

## 2026-09-05 — plan-budget guard dogfooded on prod: the gate fires typed, before `campaign`, for $0.67–$1.80 (runs 01a0714a, 01a07156)

- Status: **validated** (the guard itself; the entry below, written before
  any live run, is superseded on its "unvalidated" point).
- Versions: bot `branch-improve-loop` 1.3.0, pushed to the prod platform
  bot-override tier (`iterion remote admin bots push bots/branch-improve-loop
  --slug branch-improve-loop`) so the runners used it without an image
  rollout · iterion runners `8727674c` (v3.102.6).
- Method: `plan_budget_ratio=0.001` forces the guard (2h30 × 0.001 ≈ 9 s of
  plan phase, so any real plan trips it); target SocialGouv/iterion#749;
  `post_to_board=false`, auto-merge off. Two launches: `iterion remote runs
  launch --bot branch-improve-loop --var pr_url=…` — which attaches NO
  repository, so the plan node authored a plan against an empty
  `/tmp/iterion` — then `POST /api/runs` with `repo_url` + `repo_ref` +
  `connection_id`, the only shape that carries a checkout.
- Result:

  | run | repo | active | cost | LLM nodes served | outcome |
  |---|---|---|---|---|---|
  | `01a0714a` | none (empty workspace) | 5 m 21 s | $0.67 | plan, plan_review, plan_revise | `plan_budget_gate` → `fail`; `campaign` never entered; 0 push |
  | `01a07156` | #749 checkout | 7 m 54 s | $1.80 | plan, plan_review, plan_revise | same |

  Both runs end `failed` with the engine's own `workflow reached fail node`:
  the typed `PLAN_BUDGET_EXHAUSTED` lives on the gate node's output only,
  because a `fail` node cannot carry a code yet (#739), and a `fail`
  terminal is non-resumable by design — the right call for a probe, the
  wrong one for an operator who would rather widen the plan budget and
  continue (#739's follow-up comment).
- Value: the class of death of the three runs below — 2h30 of planning,
  zero commits, $8.59 — is closed. The guard stops the run at the plan
  phase's own ceiling, having spent 1–2 % of the budget instead of all of it.
- Findings / misses (each filed): `plan_review=off` skips the WHOLE plan
  phase, not only the peer review, so a single-provider deployment never
  plans (#751); six of the seven campaign bots start an opus-class plan node
  without checking the workspace is a repository — `01a0714a` paid $0.67 to
  plan against nothing (#752); the guard has to self-measure wall-clock and
  mirror the budget through two hand-maintained vars because no expr
  primitive exposes the run's elapsed budget (#738); a bot-declared refusal
  reads as "workflow reached fail node" at the top level (#739).
- Engine hardening: #751/#752 (bot-side) and #738/#739 (engine) in flight.
- Lessons for next run: attach the repository through `POST /api/runs`
  (`repo_url`, `repo_ref`, `connection_id`) — a CLI `--bot` launch carries
  `pr_url` and nothing to check out; force the guard with
  `plan_budget_ratio` rather than waiting on a real long plan. The 04/09
  friction this closes: Billy `01a06d80`, launched by the zero-touch lane on
  #683's red gate, died 4 s over `max_duration` (2h30m01s / 2h30m) with a
  plan and nothing pushed, and the deployed binary had no re-budget flag on
  `remote runs resume` (#689, since merged) — exactly the shape the guard
  now refuses in the first minutes.

## 2026-09-05 — plan-phase budget guard shipped (native:695); three production deaths never reached campaign (runs 01a06d80, 01a06e72, #705)

- Status: **failed** (the three cited production runs, pre-fix) — the fix
  itself is unvalidated by a live dogfood as of this entry; see "What the
  session should dogfood" below.
- Versions: bot `branch-improve-loop` 1.3.0 (this change) · the three cited
  runs were on 1.2.1.
- Method (the failures being fixed): `/billy` on SocialGouv/iterion#683
  (+3870/-273 across 55 files, 30 commits), `review_mode` auto, budget
  `max_duration 2h30m` / `max_cost_usd 75` (the shipped default).
- Result (the three cited runs, unchanged bot):

  | run | started | duration | cost | nodes executed | stopped at | commits |
  |---|---|---|---|---|---|---|
  | `01a06d80` | 2026-09-04 17:38Z | 9004.7s/9000 (+4.7s) | $3.77 | plan_topology, plan, plan_review, plan_gate, plan_revise | `campaign` (never entered) | 0 |
  | `01a06e72` | 2026-09-05 22:03Z | 9001.9s/9000 (+1.9s) | $4.82 | *(identical set)* | `campaign` (never entered) | 0 |

  Same target, same node set, same stopping point, same failure, twice.
  ~5 h of runner pod + $8.59 of LLM spend produced two triage plans and zero
  code (#695). #705 catalogs a third death on the SAME cap (`01a0517a`,
  2026-08-30, +1s) and argues the complementary half: a run that dies with
  nothing banked has no recovery path, whereas the 2026-09-03 entry above
  survived three deaths only because it had banked 17 commits in stride.
  Neither ticket's underlying wall existed until the plan phase itself
  starved `campaign` of the time to make its own first commit.
- Value: N/A for the three historical runs (zero commits, two duplicated
  triage plans neither of which reached code).
- Findings / misses: the failure was invisible in the run's own status —
  both runs closed `failed_resumable` with an ordinary "budget exceeded:
  duration" message, indistinguishable from a run that did real work and
  ran long. Only the executed-node list showed `campaign` was never
  entered. A resume restarts from a fresh clone (the planning cost is paid
  again), so retrying was not a fix.
- Engine hardening (this change, native:695): the planning chain
  (`plan` → `plan_review` → `plan_gate` → `plan_revise`) had no ceiling of
  its own — it could (and did) spend the ENTIRE run budget before
  `campaign`, the node that writes code, ever started. Added:
  - `plan_scope_probe` (deterministic tool, before `plan`): captures a
    capped `git diff --stat` footprint, a `large` classification (over
    `plan_large_diff_lines`, default 1500 added lines), and the chain's
    wall-clock start (`started_epoch`) — the ONLY way to measure the
    phase's own elapsed time, since no DSL primitive exposes a run's
    elapsed duration to a node (see below).
  - `plan_gate` now also bypasses `plan_revise` on a large diff, not only
    on a skipped peer (`skip_revise = skipped || large`) — the peer's
    critique reaches `campaign` unrevised rather than paying a second
    full-diff read.
  - `plan_cost_probe` (compute): a NIL-SAFE sum of `plan`/`plan_review`/
    `plan_revise`'s own `_cost_usd` — a skipped/never-run node's cost key
    is absent, not zero, and a naive sum errors on that; `if(x, x, 0)` is
    the nil-safe idiom (`truthy(nil) == false`).
  - `plan_budget_gate` (deterministic tool, the SOLE choke point before
    `campaign`): compares the real elapsed minutes and the nil-safe cost
    sum against `plan_budget_ratio` (default 0.3) of two new mirror vars
    (`budget_max_duration_minutes` / `budget_max_cost_usd`), and routes to
    a typed early failure instead of letting `campaign` start on whatever
    the plan phase left behind — guaranteeing `campaign` at least
    (1-ratio) of the budget whenever it does start.
  - `plan`/`plan_review` read the diff-stat footprint and skip the full
    unified diff above `plan_large_diff_lines`.
  - **DSL gap found and NOT worked around**: no primitive exposes a run's
    actual elapsed duration/cost or its resolved budget caps to a
    compute/tool node (`pkg/dsl/expr`'s `run` namespace resolves only
    `run.id` — `pkg/runtime/expr_eval.go`); `plan_scope_probe` /
    `plan_budget_gate` self-measure wall-clock via `time.time()` instead.
    Also: the DSL's `-> fail` terminal has no way to carry a custom
    `RuntimeError` code/message (`pkg/runtime/engine_exec.go` hardcodes
    "workflow reached fail node" + a fixed `FailureFailNode` code) — the
    typed `PLAN_BUDGET_EXHAUSTED` code + comparison detail live on
    `plan_budget_gate`'s own persisted output (readable via `iterion
    report` / the run's events), not on the run's top-level failure
    message. Both are flagged as follow-up engine work, not hacked around
    (out of this change's bot-only scope).
- Lessons for next run: dogfood the fix on a large diff BEFORE trusting it
  in production — a stub-driven e2e proves the graph routes correctly on a
  controlled cost sum, not that `plan_budget_ratio`'s default (0.3) is the
  right split on a real 4000-line diff, nor that `plan_large_diff_lines`
  (1500) is the right threshold for the diff-stat adaptation to actually
  keep `plan`/`plan_review` inside their share. If the guard still trips
  too early/late on iterion#683 itself, tune the ratio/threshold vars
  before touching the mechanism.

## 2026-09-03 — `/billy` on the watchdog PR: three deaths, the banked chain delivered by hand (run 01a06728)

- Status: **partial.** The fixer's work landed (17 commits on
  SocialGouv/iterion#646, the ADR-096 claim-lease + watchdog PR) but never
  through his own delivery tail: the run died three times before
  `push_back`, and the operator fast-forwarded the banked chain onto the PR
  branch. The PR merged the same evening (`e194aebe0`, v3.99.0, deployed).
- Versions: iterion cloud prod (runner v3.96.1 digest, server ~v3.97) · bot
  `branch-improve-loop` 1.2.1 · branch base `fa9c8b1be`.
- Method: the documented habit — Revi left 1 medium (`Ra74e4c`) + 2 low + 7
  questions on #646; `/billy` as maintainer; `review_mode=mono`; bot budget
  `max_duration 2h30m` / `max_cost_usd 75`. An Anthropic incident was in
  progress during the first attempt.
- Result:

  | attempt | wall-clock | outcome |
  |---|---|---|
  | 1 — 12:05→14:37Z | 2h32 | `BUDGET_EXCEEDED: duration (9004.7s/9000s)` mid-`campaign` (iter 3/8), $34.47; **6 commits banked** on `iterion/run-01a06728…` |
  | 2 — bare `remote runs resume` | 15 min | re-died at 9901.6 s = the 110 % exit-grace ceiling (the consumed duration axis rides the checkpoint) |
  | 3 — resume with `source` inline (`max_duration: 6h`) + `force` | 1h47 | restarted the campaign FROM SCRATCH (the resume re-clones the branch head, the banked chain is not re-imported), **17 commits** banked, died `USAGE_LIMIT_BLOCKED` (Claude session limit, usage_window retry armed) |
  | 4 — auto retry 20:09Z | — | cancelled by the operator: it would have died on iterion's own weekly cap (95 %, hard) |

  Operator delivery 17:43Z: fast-forward of the banked chain (17 commits,
  verbatim) + one test-only fix + merge of `origin/main` → PR head
  `196b18966`; the re-review fired by itself 3 s later (`review_on_sync`)
  and came back green (0 ≥high, 2 low + 7 questions → #660). A merge-queue
  ejection (#658 merged ahead, same flaky test touched) forced a second
  merge (`385d8677e`); that head's re-review then died on the **weekly**
  usage cap (`seven_day window at 95% ≥ 95%`, reset 2026-09-08) and parked
  the gate for five days — `/revi approve` failed on the GitHub App
  integration (#662), the override status was posted by hand.
- Value: **high on substance.** Beyond Revi's findings (all addressed —
  `launchTicketNow` CAS anchored on the read state, the reaper gate
  misspelling made loud, the CI mongo-gate guard scoped), the campaign found
  defects a 20-round adversarial loop had missed: a false "claim lost" when
  the owner's own release races an in-flight heartbeat (`Releasing()`
  latch), `cmdClaimLost` cancelling the NEXT run holding the card (run id on
  the message), a lost fence at launch that still launched (both twins), the
  mongo terminal sink as check-then-act (now a CAS with re-evaluation),
  `SetStateFrom(x→x)` disagreeing across twins, the reconciler's tokenless
  write overwriting an operator's drag (`SetStateFromReason` CAS), "a
  release is the last act of a disposition", `store.RunAbsent` shared across
  the four run-pointer authorities, the FS renew blocking `Stop()` on the
  actor.
- Findings / misses: his new `TestAdapterRenewClaim_HonoursCancelMidCall`
  failed deterministically (10/10, plain and `-race`: the detached renewal
  wrote into `t.TempDir()` during cleanup) — **a banked chain is committed
  in stride but not gate-verified** (`verify_run` only runs at the end of a
  pass). He applied `Releasing()` on the local twin and missed the cloud
  `processCard` (Revi's `Rf238b1`, #660). His delivery tail was never
  exercised: he never pushed himself, so by design he would have stamped
  nothing on a head the operator pushed.
- Engine hardening (GitHub board): #652 — resume re-clones the branch head
  and restarts the campaign, ignoring the banked chain (proposal: re-anchor
  on `FinalBranch` when it fast-forwards from the clone base); the cloud
  `POST /api/runs/{id}/resume` has no budget overrides (workaround: `source`
  inline + `force`); the consumed duration axis rides the checkpoint; the
  exit grace does not protect a node cancelled in flight. #650 — the
  gate-paused notice says "Review paused … a new push restarts it sooner"
  for a FIXER (`forge_gate_pause_notice.go` filters on `gate_context`,
  which a gating fixer carries). #662 — `/revi approve` → `set commit
  status: forge: insufficient scope`, webhook 502. #663 — the parked review
  revived 2 s after `stopRunsForDeadPR` (redelivery race). Verified working:
  the death bank (`pushBank`, richer-chain supersede), the usage-window
  retry (`run_retry_scheduled`, reset parsed from the typed error),
  `review_on_sync`, stop-on-close for the auto-heal lane run.
- Lessons for next run: size the fixer's `max_duration` for this repo's
  verify gate (2h30 with ~1h of plan phases is too tight) or slim the plan
  phases when `consumes: review` carries few findings; push in stride on
  the PR branch (the work branch IS the PR branch); never bare-resume a
  duration death on cloud; a banked chain is deliverable by hand only after
  the full validation; when the weekly cap parks the gate, the documented
  override is broken until #662 lands — budget a manual status or the admin
  bypass.

## 2026-08-30 — three launches, zero delivery: the duration cap and the weekly cap (runs 01a0517a, 01a051dd, 01a05216)

- Status: **failed.** `/billy` on SocialGouv/iterion#579 (the CHANGELOG
  feature) produced no commit and no push across three launches. Nothing was
  wrong with the hand-off — `prior_review` seeded, campaign working, tool
  calls flowing — the run simply ran out of wall-clock, and the retries hit a
  wall that was not Billy's.
- Versions: iterion cloud prod (edge ~v3.74/3.77) · bot `branch-improve-loop`
- Method: documented habit — Revi left 1 medium + 1 low on #579, `/billy`
  comment as maintainer, no `skip`.
- Result:

  | run | wall-clock | outcome |
  |---|---|---|
  | `01a0517a` 07:02 | **2h31** | `budget exceeded: duration (9001s/9000s)` |
  | `01a051dd` 08:51 | — | `failed_resumable` |
  | `01a05216` 09:53 | 3 min | `usage cap: seven_day window at 75% ≥ 70% (week, hard)` |

- Value: none delivered. The findings were fixed by hand instead, after the
  weekly cap made a fourth attempt impossible before its 2026-09-01 reset.

### Lessons

- **The 2h30 duration cap is not sized for this repo's verify gate.** Each
  campaign pass re-runs the repo's real build+test, measured at ~10 min a pass
  (one tool call spanned 07:59:04 → 08:09:05). A handful of passes and the cap
  is spent before the delivery tail. The loop budget guard declines a further
  iteration when the budget cannot fund one, but nothing here shortened the
  *first* pass, and the run died mid-pass with nothing committed — the exact
  shape "commit in stride" exists to avoid. Either raise `max_duration` for
  targets whose gate is expensive, or make the gate cheaper (scope the test
  command to the touched packages).
- **A run burning its whole cap with zero commits should be cancellable on
  sight.** Nothing in the run view said "2h in, nothing banked"; the operator
  reads `running` and waits. `commits_this_pass` exists in the contract —
  surfacing it (or a zero-commit warning past N minutes) would have turned
  2h31 of spend into a 20-minute decision.
- **The habit has a precondition worth stating: Billy must be able to run.**
  `docs/revi-billy-loop.md` says "don't hand-fix, comment /billy". When the
  weekly cap is hard-blocking until a reset two days out, that instruction has
  no path, and waiting is worse than fixing. The runbook should name the
  fallback rather than leave the operator to infer it.

## 2026-08-27 — first `/billy` of the Revi→Billy habit on iterion itself (runs 01a0428a, 01a042b9, 01a042d7)

- Status: **partial.** The habit's whole chain worked — `/billy` comment →
  launch with `prior_review` seeded (3.6–6 KB, the kind-matched hand-off),
  campaign converged in ONE pass (7 commits, verify green, in-loop review
  clean), honest verdict posted — but the **push died on an expired forge
  token** after 1h11, so the fixes never reached the PR. Three engine/bot
  defects found, two fixed in this change, one filed
  (native:54412d84). This bilan inaugurates [docs/revi-billy-loop.md](../revi-billy-loop.md).
- Versions: iterion cloud prod (edge ~v3.65) · bot `branch-improve-loop` 1.2.0
- Method: the documented habit — Revi reviewed SocialGouv/iterion#541
  (1 high: "OAuth meter key follows the access token"), then `/billy`
  comments (maintainer). Weekly usage cap was at 95% → rotated the team +
  platform claude_code OAuth credential (fresh token), temporarily raised
  the runtime week cap 95→99 to get past the pre-#541 shared-meter stale
  reading, resumed the parked review. Three Billy attempts:
  - **Run 1 (01a0428a):** paused on `plan_review` — the team's codex OAuth
    record was DEAD (401), and `plan_review: auto` had resolved `on` from
    its mere existence. Uploaded a fresh codex auth.json → peer served on
    retry — then `plan_revise` wedged forever (~2.6s
    `error_during_execution` × 9): the mid-plan-phase resume had replaced
    the sandbox container, the author session's files died with it, and
    `inherit_if_available` only tolerated a MISSING session id, not an
    unloadable one. Cancelled.
  - **Run 2 (01a042b9):** `plan_review` 400 — `gpt-5.6-sol requires a newer
    codex-cli` (the ChatGPT-wire version gate; runner lacks a recent
    `ITERION_CODEX_VERSION`). Cancelled; pinned `plan_review: "off"` in the
    iterion repo integration's launch_vars (operator directive: **the fixer
    must never depend on a second family — Anthropic alone suffices**).
  - **Run 3 (01a042d7):** clean campaign, pure Anthropic. Converged in one
    pass: `branch_clean`, 7 commits, verify_run green, in-loop review clean,
    $9.13 / 90 K tokens / 1h11. Fixed Revi's finding the right way
    (refresh-invariant identity: fingerprint stamped at connect/seal,
    `PledgeID` for pool grants) — notable because the PR author had
    hand-fixed the finding mid-dogfood and Revi's re-review had STILL found
    1 high; Billy's ledger disposed of every id with arguments. Then
    `push_back_tool`: `Invalid username or token` — the sealed GitHub App
    installation token (minted at launch, ~1h validity) had expired. The 7
    commits died with the pod; the verdict honestly posted
    `failure — 3 fix(es) never reached this head`.
- Value: the habit is real — the hand-off, the one-pass convergence, the
  honest gate all worked; and the dogfood surfaced defects no test had.
- Findings / misses: (1) a DEAD second-family credential blocks the whole
  fixer via `plan_review: auto` + `policy: wait`; (2) `inherit_if_available`
  wedges forever when the session's backing state died with the container;
  (3) `push_auth_probe` validates token PRESENCE, not validity (answered
  `available` with a 10-min-dead token); (4) the launch-minted forge push
  token cannot outlive the App-token validity, so any >1h fixer run loses
  its push.
- Engine/bot hardening (this change): `plan_review_policy` defaults to
  `skip` for Billy (bot 1.2.1 — the peer is optional enrichment, never a
  blocker); `Task.SessionOptional` — the executor retries ONCE with a fresh
  session when an optional session fails to serve (red-first tests). Filed:
  mint-at-use forge push token + a real auth probe (native:54412d84).
- Follow-up — **both delivery runs landed the same day**:
  - **Run 4 on #541 (01a0431d, 53 min):** re-derived the lost fixes from the
    fresh prior_review, converged, **pushed 6 commits** (refresh-invariant
    OAuth meter identity + tests actually executing the refresh path +
    operator docs), verdict `success — 3 finding(s) fixed, re-review by the
    fixer clean, build green` on `556d6f4e`.
  - **Run 5 on #544 (01a0431e, 1h53):** `/billy` on the habit PR itself for
    Revi's 3 findings on OUR fixes — **pushed 11 commits** that materially
    hardened the session-degrade change: loud degrade (event + output
    stamp, R051957), narrowed to `unclassified` failures (R1486ff),
    claw node-session eviction, backend gate (only backends that resume by
    SessionID), the SessionOptional mapping pinned across all six session
    modes (R552e44), plus docs/ADR alignment — and it improved the habit
    runbook itself (the `review_on_sync` dependency of step 3).
  - The 1h53 push succeeding **contradicts** the "App token dies at 1h"
    hypothesis for run 3's failure; the board card (native:54412d84) was
    corrected — the standing defect is `push_auth_probe` validating
    presence, not validity; run 3's root cause is unreproduced (plausible:
    the integration PATCH 2 min before launch re-provisioned the managed
    forge_token).
- Lessons for next run: keep fixer runs under the App-token validity until
  mint-at-use lands (tighter `scope_notes`, or `max_duration` < 1h so the
  budget guard ships what is banked while the token is alive); the usage-cap
  runbook rotation is CLI-only now (`iterion remote admin llm oauth set` +
  team `/oauth/{kind}/credentials` — no k8s secret edit, no runner restart);
  after any `/billy`, `git pull` before touching the branch.

## 2026-08-13 — the write path on a self-hosted GitLab, end to end (run 019ffb9e)

- Status: **validated.** Every link a fixer needs on a forge that had never
  hosted one: clone, commit in stride, push onto the MR branch, post the
  verdict, and gate the head it just produced.
- Versions: iterion cloud prod v3.40.6 · bot `branch-improve-loop` 1.1.1
- Method: `POST /api/runs` with `repo_url` + `repo_ref` + `connection_id`
  (no webhook — the target instance blocks outbound hooks), `push_branch` =
  the MR source branch, `pr_url` set, `gate_context: iterion/review`,
  `post_to_board=false`. Scope notes named the defects Revi had reported on
  the same MR (run `019ffadb`); the credential is a **group access token**, so
  the push authenticates as the bot rather than as a human.
- Result: converged in ~46 min. **18 commits pushed** onto the MR branch, one
  concern each (`fix(fetch): handle http.Get error instead of dereferencing a
  nil response`, `fix(fetch): refuse destinations outside the public
  internet`, `fix(fetch): ignore HTTP_PROXY so the destination guard cannot be
  bypassed`, …), interleaved with the tests that pin them. Verdict comment
  posted on the MR, and `iterion/review=failure — 1 finding(s) unresolved`
  landed on the new head under the bot identity.
- Value: it did not stop at the reported list. Revi flagged the missing
  destination validation; Billy shipped the guard, then found the ways around
  its own guard — an IPv6 literal embedding a private IPv4 address, a redirect
  hop landing inside the perimeter, `HTTP_PROXY` steering the dialer past it —
  and pinned each with a test. It also noticed the handler was never routed
  and wired it, which is what made the rest reachable.
- Notes for the next run: the sandbox image carries no `go` on `PATH`; the
  agent recovered on its own (`export PATH=$PATH:/usr/local/go/bin`) but paid
  a couple of turns for it — worth a `devbox.json` if this becomes routine.
  Commits are authored as `iterion-runner[bot]`, distinct from the group-token
  identity that performs the push; the loop guard keys on the pusher, so the
  two must not be conflated when reading a branch's history.

## 2026-08-01 (fin de journée) — the zero-touch lane, and the crash it took to prove it (run 019fbd98)

- Status: **validated after a crash-level fix.** The opt-in lane had never fired once since it was written; its first real firing panicked a server pod.
- Method: `auto_fix_on_gate_failure` enabled on the e2e integration, a concurrency defect planted by hand (`Snapshot` handing the internal map out past the lock, `RecordHit` mutating it without one), pushed as a human. **No `/billy` comment at any point.**

### First attempt: the lane crashed the server

The gate went red at 12:51:30 and no fixer launched. The reason was in the pod's
pre-restart log:

```
panic: store/mongo: tenant-scoped query without tenant in ctx
  ← cloudpublisher.SubmitLaunch ← runview.Launch ← launchWebhookTarget
  ← autofixForRun ← eventbus NATSBus subscriber
```

The lane stamped the **auth** identity the admission gate reads and not the
**store** identity every tenant-scoped query asserts on. The inbound-webhook
middleware stamps both; the retry sweeper stamps both; this lane had copied half
the precedent. Missing, the launch does not fail — it trips the tenancy guard
deep inside `SaveRun` and takes the process down.

Its own tests could not see it: they stub `webhookLaunchBot`, which is the seam
directly **above** where the launch reaches the store. Fixed in `b68f8a39f`,
with the positive control now asserting on the context handed across that seam.

A second, wider fix rode along: the bus fans one event out to independent
consumers that share nothing but the dispatch, so a panic there is the widest
blast radius for the narrowest bug. It is now recovered per delivery, logged
with its stack, and dropped exactly as a returned error already is.

### Second attempt: it works, untouched

```
gate auto-fix: iterion/review red on SocialGouv/iterion-test-appy-e2e#2@de9df24
               → launched branch-improve-loop (run 019fbd98)
```

`restarts=0` on all three replicas. The launched run carried everything needed
to close the loop back: `head_sha` exactly the head the gate was red on,
`gate_context: iterion/review` (the repo's pin, not the bot's default), the
publish grant, 7141 characters of prior review, and a `scope_notes` saying why
it was launched.

### Lessons for the next run

- An opt-in feature that has never fired is not shipped, it is written. This one
  passed a `/simplify`, an adversarial review and a refusal table, and still had
  a process-killing bug on line one of its only untested path.
- When a lane runs off the event bus rather than an HTTP request, it inherits
  none of the request's context. Copy the middleware's stamping in full, or copy
  the retry sweeper, which already had it right.


## 2026-08-01 (soir) — the loop closes: red gate → fix → push → independent supersede (run 019fbd1b)

- Status: **validated** — the four links of the Revi↔Billy loop exercised live, two of them for the first time ever.
- Versions: bot 1.1.0 · iterion `a9f32534b` (v3.19.0, carrying `db2676c5b`).
- Method: a real defect planted by hand on `feat/cache-layer` (a `Purge` ranging over the entry map and a `Len` reading it, both without the mutex, on a type documented "safe for concurrent use"), pushed as a human. Revi reviewed it, the gate went red, `/billy`.
- Result: Revi found 4 findings (1 critical, 1 high, 2 medium) and the gate went `failure — 2 blocking finding(s) ≥high`. Billy fixed all four, pushed, and posted `iterion/review=success on a3a734324f33`. The independent re-review landed 14 minutes later and superseded it with `success — no blocking findings (≥high); 2 total`.

### The measurement that mattered

The same actor, the same event, the same repo — before and after `db2676c5b`:

| heure (UTC) | `synchronize` pushed by | delivery | sha |
|---|---|---|---|
| 10:29:33 | the fixer, before the fix | **filtered** | `6dd691c1b` |
| 11:28:28 | a human | launched | `e8ca8c0be` |
| 11:56:49 | the fixer, after the fix | **launched** | `a3a734324` |

And the two verdicts that then sat on that one head, in order:

| heure | `iterion/review` on `a3a73432` | by |
|---|---|---|
| 11:56:50 | `success — 4 finding(s) fixed, re-review by the fixer clean, build green` | the fixer, about its own code |
| 12:10:57 | `success — no blocking findings (≥high); 2 total` | the reviewer, independent |

### The return ledger, exercised for the first time

`prior_pushback` reached the re-review at 1920 characters, finding by finding with the commit that fixed each. The reviewer re-raised **none** of the four and instead found two new low ones — plus a question worth more than the findings: *does CI run `go test -race`? I could not here, and I confirmed by mutation that removing Len's mutex leaves the whole suite green.* A locking discipline with no automated guard, surfaced by the reviewer's own falsifiability channel.

### Lessons for the next run

- A guard keyed on the sender treats "our bot pushed" and "our bot opened this PR" as the same thing. They are opposites: the second converged in its own loop, the first is exactly the moment an independent judgement is needed.
- Plant a defect and drive the whole loop rather than asserting each link separately. Both defects fixed today were compositions, not units.


## 2026-08-01 — the board lane could not publish, and the fixer's push was invisible to the gate (run 019fbcbb)

- Status: **validated** — first end-to-end run where Billy answers a review, pushes, posts its verdict AND closes the merge gate. Two engine defects found live, both fixed and deployed in-session.
- Versions: bot 1.1.0 · iterion `cdfedc124` (server), the run itself on `claude_code` / `claude-opus-5`.
- Method: `/billy` comment on [iterion-test-appy-e2e#2](https://github.com/SocialGouv/iterion-test-appy-e2e/pull/2) (`feat/cache-layer`, a real Go module with planted defects), cloud, board lane, GitHub App connection, `gate_context: iterion/review` pinned on the integration.
- Result: converged in **one pass** — 38m12s active, 390 events, 152 tool calls, 11 nodes, `continuation_loop` never re-entered. Pushed 6 commits onto the PR branch; `publish_verdict` returned `verdict posted; iterion/review=success on 6dd691c1be95`.
- Value: the hand-off paid for itself. Billy received 3388 characters of prior review carrying the stable id `Rcae144`, **reproduced the finding before fixing it** (a 503 origin was memoized and served with a nil error, and a recovered origin was never re-contacted), fixed it in `57c7a00`, then found four more real issues the review had not — among them a `Warm` semaphore allocated per call instead of per cache, so the fan-out cap was not a cap.

### The two defects the run surfaced

**1. A bot launched off the board could not publish anything** (fixed, `515714482` + `cdfedc124`).
The cloud coordinator launches a card from its BotArgs alone, so the forge-publish grant and the repo's launch policy — both composed inline by the webhook tail — never reached it. Measured side by side with the reviewer on the same PR:

| | `forge_publish_url` | `forge_publish_token` | `gate_context` |
|---|---|---|---|
| Revi (`mode: direct`) | oui | oui | `iterion/review` |
| Billy (`mode: board`), avant | — | — | absent |
| Billy (`mode: board`), après | oui | oui (64 car.) | `iterion/review` |

Both are now resolved at claim time, never carried on the card: a grant expires, so a card claimed hours later would hold a dead token, and a board document is the wrong place for a credential at rest.

A second defect rode along, found while preparing the fixture: the repo carried **two integrations** (a stale personal-token one from 2026-07-17 and the GitHub App it was re-provisioned onto), two live webhooks, and a double delivery on every comment. Resolving them by lowest id — deterministic but arbitrary — elected the stale one, which would have posted the verdict under the operator's own account, the identity the loop guard refuses. The latest provisioning now wins.

**2. The fixer's push was the one delivery the gate never saw** (fixed, `db2676c5b`).
The iterion-bot guard skips a PR our own loop produced, keyed on the SENDER. On a merge-gate resync the sender is by construction the forge bot — the fixer that just pushed — so the resync on `6dd691c1` was recorded `filtered — PR authored by iterion's forge bot`. Two consequences, both the failures the merge gate exists to remove: the required check stayed on the pre-push revision (absent on the head that needs judging), and the fixer's own verdict about code it wrote was never superseded by the independent re-review [docs/merge-gate.md](../merge-gate.md) promises. A PR the bot *opens* is still skipped — that one did converge in its own loop.

### Lessons for the next run

- A `mode: board` bot gets none of the launch context the webhook lane composes. When a board-mode bot behaves as if a var were unset, read the RUN's inputs before reading the bot.
- Both defects were invisible to every test because each test supplied by hand the thing production had to produce. The same shape as the `publish:` defect in [review-pr.md](review-pr.md): when a contract crosses two components, at least one test must run the producer for real.
- Deprovision a stale integration when re-provisioning a repo onto another connection. Two live webhooks on one repo double every delivery, and the extra one refuses everything with a misleading reason.


## 2026-07-10 — /billy command on a Dependabot PR: 4 engine gaps peeled live, then a clean push-back under the App identity (runs 019f4bd4 / 019f4c46 / 019f4c86 / 019f4ccb)

- Status: **validated (cloud E2E, command path) — 5 engine fixes found live, all landed in-session.**
  Mission: make Dependabot PR [#80](https://github.com/SocialGouv/iterion/pull/80)
  mergeable (bump `x/crypto` to 0.52.0 past a CRITICAL advisory cluster Vetty
  flagged + fix the failing CI OpenAPI drift). Final run 019f4ccb: 22 min,
  `push_back: true`, **2 commits pushed onto the PR** as
  `iterion-forge-83fde406[bot]` (`52a4072d9` crypto bump + tidy/vendor,
  `d766e1d03` openapi regen) + verdict comment posted under the App.
- Versions: bot v2 · iterion `499957c31`→`6dd452c2a` · chart 0.37.2→0.37.4.
- Method: `/billy <mission>` PR comment (args → `scope_notes`) → board-mode
  card → dispatcher → cloud runner. ~$4/run. The mission text included the
  memory guidance (skip the vite build, `go test -p 2`) after an OOM.
- The onion, one layer per run (each failure = a real engine fix):
  1. run 019f4bd4: campaign did PERFECT work (crypto bump + openapi regen,
     verified) but `mr_gate.push_back: false` → commits stranded on the
     runner's ephemeral storage branch. Fix `642b1ba0d`: the command path now
     stamps `push_branch`/`open_mr` (parity with the pull_request-event path).
  2. Same run also exposed that `ensureBoardCard` rebuilt BotArgs from scratch
     — cloud launches use card BotArgs ONLY, so `pr_url`/branch vars never
     reached the run (also why Billy never commented). Fix `bc2918024`.
  3. run 019f4c46: **OOMKilled at 4Gi** during the verify suite (exit 137, pod
     restart, banked commits lost with the pod FS). Infra fix: runner limit
     8Gi (infra-apps `44b7fb4`). JetStream redelivered the run — recovery
     worked — but the re-attempt then pushed with a DEAD App token.
  4. runs 019f4c46/019f4c86: `Invalid username or token` on push + gh 401 on
     comment — the sealed bundle snapshots the 1h installation token at
     LAUNCH; long/redelivered runs outlive it. Fix `6dd452c2a` (#99): the
     publisher records generic-secret store IDs on the bundle and the runner
     re-reads them every 5 min, atomically rewriting
     `/run/iterion/secrets/<name>`; tools `cat` the file per use.
- Engine hardening (this session, all deployed): `499957c31` PR head/base
  resolution for PR-surface commands · `642b1ba0d` push-back vars ·
  `bc2918024` board-card PR context · `6031a357e` executor agent-stream lines
  now persist to the run log (the studio per-node Logs tab was empty on every
  cloud run) · `6dd452c2a` mid-run file-secret refresh (#99).
- Lessons for next run: a dep-bump branch is typically BEHIND main — the CI
  drift gate runs on the merge-ref, so regen-style fixes must happen on an
  updated branch (`gh pr update-branch` first, or Billy should update it);
  keep verify memory-aware on 8Gi pods (skip frontend builds, cap `-p`).

## 2026-07-09 — first CLOUD runs: PR-webhook → Billy on the devbox runner (runs 019f43a3 / 019f43c7 / 019f4551)

- Status: **validated (cloud E2E) — 3 engine/bot gaps found live, all fixed in-session.**
  The full production trigger chain ran for the first time: real GitHub PR
  ([#84](https://github.com/SocialGouv/iterion/pull/84), `Fixes #83`) → forge
  webhook (`selectForgePRBot` routes Billy, not Revi) → NATS queue → devbox
  runner pod (uid 1000) → campaign/verify/gate.
- Versions: bot @ `1d076a618` (adds the push-back tail) · iterion `:edge`
  36fb786b5→56a5680f1 · runner image `iterion-runner-devbox:edge` ·
  webhook `d291059c` (bots review-pr + branch-improve-loop + feature-dev,
  block_fork_prs, author allowlist Viczei+devthejo).
- Method: PR-open/reopen events on SocialGouv/iterion#84 (a real fix: the
  vv0.32.0/vmain version-injection bug). Re-triggers need a NEW head sha +
  close/reopen — the delivery idem key is (PR#, head sha) and launch-success
  rows are terminal; `/billy` comments from the connection identity are
  loop-guard-filtered (self).
- Result per run:
  - `019f43a3` **failed in 17s**: Billy's inline `sandbox:` block → kubernetes
    driver → `ITERION_POD_IP env var is empty` (the chart deliberately doesn't
    provision sibling-pod sandboxing). → **Fix 1** `36fb786b5`:
    `ITERION_SANDBOX_OVERRIDE=none` (CLI-strength, beats the workflow block;
    chart auto-sets it when `runner.sandbox.enabled=false`) — the runner pod IS
    the isolation boundary.
  - `019f43c7` **finished, 1-pass converged (~7 min)**: verify.sh settled on
    `devbox run -- go build ./...` + targeted tests — the repo's devbox-pinned
    Go toolchain, i.e. the devbox-first-class runner goal proven live. campaign
    caught a REAL defect (the PR's new test file wasn't gofmt-clean — CI lint
    would have rejected it) and committed the fix (`ef540115`)… which **died
    with the pod-ephemeral worktree**: `mr_gate` open_mr=false went straight to
    done. → **Fix 2** `1d076a618`: deterministic push-back — webhook passes
    `push_branch` (PR source branch); `mr_gate → push_auth_probe →
    push_back_tool` (no-LLM python3 push, rev-list oracle so a
    converged-no-commits pass no-ops, token redacted from failures).
  - `019f4551` **finished; new path routed** (`…gate>mr_gate>push_auth_probe>done`)
    but the probe found NO credential: `materializeFileSecretsNoSandbox` gated
    on the STATIC `wf.Sandbox != nil` — under the override the run executes
    in-pod and nobody materialized `forge_token` (the launch HAD sealed it into
    the bundle: the stored secret's last_used_at == launch instant). → **Fix 3**
    `56a5680f1`: gate on the RESOLVED decision via new
    `runtime.WorkflowSandboxActive(wf, override, default)`.
- Value: the run 019f43c7 catch (gofmt) was real and manually reapplied as
  `a2cf464a9`; the three fixes harden the entire cloud-runner class of bots
  (any bot with a sandbox block + file secrets), not just Billy.
- Findings / misses: secret-resolution failures are SILENT at several layers
  (`buildGenericResolution` ok=false without a log; publisher skips empty
  plaintexts) — an erreurs-explicites hardening candidate. CI race job flake:
  `TestReconcileStalled_ForceReapsCtxIgnoringWorker` (pkg/dispatcher) failed on
  56a5680f1, 5× green locally with -race — known concurrency-flake family.
- Engine hardening: `36fb786b5` (+ chart 0.33.0, umbrella `3a20e24`),
  `1d076a618`, `56a5680f1`; stale factory comment `afbdd6be3`.
- Lessons for next run: consult the RESOLVED sandbox mode everywhere run
  inputs depend on it; self-triggering a webhook from the connection identity
  hits the loop-guard (use a fresh head sha + close/reopen, or another actor);
  first devbox run per pod re-downloads the Nix toolchain (~2-4 min — the PVC
  warm-store follow-on).

### The issue → Featurly → PR half of the cycle (runs 019f4582 / 019f4590, fixes 4–6)

Labeling an issue `implement` is meant to route to the implementer (Featurly)
and open a PR that then re-triggers Billy — the other half of the loop. Two
more gaps surfaced, both fixed live:

- **Routing (fix 4, `2281a2eff`)**: a 3-bot webhook with no pinned default
  routed `issues/labeled` to **review-pr** (run 019f4582), which stopped at
  `diff_precheck` — an issue has no diff to review. `resolveReviewBot`'s
  SelectBot→review-pr fallback is right for a PR delivery, wrong for an issue.
  New `selectIssueLabeledBot` (pinned default → feature-dev → fallback), the
  issue-path counterpart to `selectForgePRBot`, wired on GitHub+GitLab.
  Validated: issue #86 → run **019f4590 = feature_dev**, and Featurly shipped a
  genuine, high-quality feature (threaded a `*log.Logger` through the whole
  generic-secret resolution path so silent credential drops become greppable —
  the erreurs-explicites finding the debugging itself surfaced), build+test
  green.
- **Forge token on the board-launch path (fixes 5+6, `483e69a3f` + `9b339c999`)**:
  Featurly implemented but `forge_auth_probe` found no `forge_token`, so it
  never opened its PR (`secrets_ref` null). Root cause: an issue-labeled
  delivery makes a **board card**, and the board coordinator
  ([boarddispatch.go](../../pkg/server/boarddispatch.go)) launches via
  `runs.Launch(BotID)` — which resolves generic secrets by (tenant, bot)
  **binding**, NOT the webhook secret override (that only reaches the direct
  webhook path). Forge provisioning set only the override; the tier-3 name
  fallback misses (stored `forge_github_<conn>` ≠ workflow `forge_token`). Fix:
  the orchestrator now upserts a per-bot `forge_token` binding at provision
  (fix 5), reconciled even on an idempotent re-provision so an
  already-provisioned integration is backfilled (fix 6). This is a general
  cloud-platform fix: EVERY board-launched bot that pushes (Featurly, Billy on the
  board path) needed it, not just this cycle.
- Standing gap (deferred): fully zero-touch issue handling still needs the
  cloud dispatcher (the webhook `issues` path is labeled-only by design); and
  carrying the webhook's secret_overrides/launch_vars onto the board card
  itself would make the board path robust without a binding (belt-and-braces).

## 2026-07-08 — re-dogfood post-improvement (P1-P4), same PR #72 target, $1.80 (run 019f41af)

- Status: **validated — reliability + integration confirmed; production excellent.**
  Improvements are RIGHT and SAFE; this run's shape (1 pass, no MR) did not trigger
  the two biggest savers (see "P1/P2 not exercised live" below — an honesty note, not
  a regression).
- Versions: bot branch-improve-loop @ `b7ea4bd78` (P1 forge_auth_probe + P4
  tool_max_steps 40→20 in `db812f0dc`; P2 verify_probe + P3 verify_build effort in
  `b7ea4bd78`) · iterion static binary `v0.31.0+b7ea4bd78` (built CGO_ENABLED=0,
  invoked directly so `os.Executable()` bind-mounts the fresh binary into the sandbox).
- Method: **apples-to-apples with the pilot below.** Same target: independent clone
  `/tmp/iterion-pr72-clone` **reset to the RAW PR #72 tip** `4b2394b94` (the pilot's
  added test removed, so Billy re-finds the gap). `sandbox` inline
  (iterion-sandbox-full:edge), `worktree: auto` bases on the clone's HEAD (CWD =
  clone), `--store-dir <main>/.iterion` so the run is **visible in the operator's
  studio** while operating on the clone (setupWorktree bases on CWD's git root but
  writes the worktree under `--store-dir`). `base_ref=main`, **`open_mr=false`**,
  `--merge-into none`, `--max-cost-usd 12`.
- Result: **converged in ONE pass, 8.5 min, $1.80 (31.8k tok).** Billy reviewed
  `main…4b2394b94` (the per-node `timeout:` feature) across all layers and re-found the
  exact same gap the pilot did — the parser/AST/IR/C199 were tested but the RUNTIME
  enforcement path was not — and added `pkg/backend/model/executor_timeout_test.go`
  (+115: a `blockingBackend` that blocks on the ctx deadline + `immediateBackend`
  happy path, asserting the bounded context cuts the node off AND that a
  context-deadline error is **not** retried). Commit `f5f55d5` on storage branch
  `iterion/run/ash-pulse-starforge-1c89`. `verify_run` re-ran `go build ./... && go
  test ./pkg/dsl/… ./pkg/backend/model/…` → **green** (so the added test compiles and
  passes). Graph flow: campaign → verify_probe → verify_build → verify_run → gate
  (converged) → mr_gate (open_mr=false) → done.
- Cost story vs the pilot's $2.26 (honest breakdown): campaign ~$1.17 (18.9k tok, was
  $1.31/21.2k — run variance on a slightly different pass) + verify_build ~$0.68 (12.9k
  tok, P3 effort now `medium`) + verify_run (deterministic tool, 9.8s) + **no
  finalize_mr** (open_mr=false avoided the pilot's $0.26). Net −$0.46 (~20%), of which
  ~$0.26 is the avoided MR finalize and ~$0.20 is campaign variance; **P3's verify_build
  delta was only ~$0.01** — verify.sh authoring uses little thinking, so `high→medium`
  barely moves it here (it will matter more on repos with a heavier build-capture step).
- **P1/P2 not exercised live (by design of this run's shape) — structurally proven:**
  - P2 (verify_probe skips verify_build) only fires on **pass 2+**; this run converged
    in 1 pass, so verify_probe correctly ran at `iteration=0` → `fresh=false` → routed
    to verify_build (the new node integrates without breaking the happy path). The
    **skip** path rests on `TestVerifyProbeLoopIterationWiring` (proves the loop
    iteration reaches the tool via the campaign→verify_probe edge with-mapping into node
    input — {{loop.*}} does NOT resolve inside a tool command) + the python-logic cases
    (fresh=true only when iteration>0 AND verify.sh valid) + `iterion validate`.
  - P1 (forge_auth_probe short-circuits finalize_mr when no push credential) only fires
    when `open_mr=true`; this run used `open_mr=false` (mr_gate → done), so the probe
    wasn't reached. Its logic is deterministic + unit-covered.
- Reliability: **HIGH.** Same target → same high-value conclusion (add the runtime
  enforcement test), reproduced independently. The two new deterministic nodes
  (verify_probe, forge_auth_probe) slotted into the graph with zero runtime surprise.
- Engine hardening: none needed. The e2e scenario sweeps gained stubs for the two new
  nodes (`aad7bddce`, `test(e2e): stub verify_probe + forge_auth_probe`) so
  `go test ./e2e/` stays green.
- Lessons for next run: to **measure P2's $0.69/pass saving live**, point Billy at a
  ≥2-pass target (a branch with 2+ real issues, or a synthetic branch with a planted
  build-affecting change so pass 2 re-runs). To **measure P1's skip live**, run
  `open_mr=true` with no forge_token and no host `gh` auth → forge_auth_probe → done.
  For a pure production/cost demonstration this 1-pass/no-MR run is the right shape and
  $1.80/8.5min is a good point; the structural savers show on longer/MR runs.

## 2026-07-08 — pilot on a real contributor PR (#72), converged in 1 pass (run 019f415c)

- Status: **validated — high-quality single-commit improvement.**
- Versions: bot branch-improve-loop (feat/agent-node-timeout tree) · iterion 1f5082f (static, F7 skills fix in the engine)
- Method: local run, `sandbox: auto` (iterion-sandbox-full:edge, devbox toolchain
  so the build/test gate has Go — the cloud runner does NOT, cf. F6). Independent
  clone checked out on the PR branch `feat/agent-node-timeout` so `worktree: auto`
  bases on the PR (NOT main — a linked git worktree bases on the shared repo HEAD,
  which is main; an independent clone was required). `base_ref=main`,
  `max_passes=2`, `open_mr=true`, `mr_branch=iterion/billy/pr-72-pilot`.
- Result: **converged in ONE pass.** Billy reviewed `main...feat/agent-node-timeout`
  (the per-node `timeout:` feature), found the parser + C199 validation were tested
  but the RUNTIME enforcement path (`executeBackend` deriving the bounded context —
  the feature's behavioral heart) was not, and added
  `pkg/backend/model/executor_timeout_test.go` (+108: `TestNodeTimeout_Enforced` with
  a ctx-blocking backend + 20ms timeout, plus a happy-path immediate backend). The
  deterministic build/test gate (`verify_run`) passed green. Exactly the test a good
  human reviewer would add. Pushed by hand to `iterion/billy/pr-72-pilot` → draft PR
  #82 (base = the PR branch, so the diff is just Billy's addition).
- Cost: campaign $1.31 (21.2k tok) · verify_build $0.69 (13.1k tok) · verify_run
  (deterministic tool, 9.7s) · finalize_mr $0.26 (6.8k tok) = **~$2.26, ~9 min**.
- F7 (engine) re-validated here too: `finalize_mr` successfully `Launching skill:
  forge-mr-create` — the directory-form skills fix works on the local sandboxed path,
  not just cloud.
- Efficiency/reliability misses to fix (operator asked to bring Billy to a Willy-grade
  production/cost ratio):
  - **finalize_mr burns budget discovering there are no credentials**: with no
    forge_token mounted and no `gh auth`, the agent ran 4 probe commands before
    abandoning the push. A deterministic pre-check should skip finalize_mr (or
    short-circuit it) when no push credential is present.
  - **verify_build ($0.69)** re-authors the build/test script each run — candidate for
    caching / lower effort.
- Lessons for next run: base the worktree on the PR via an INDEPENDENT clone (a linked
  worktree bases on main). Provide a `forge_token` (or run in a `gh`-authed shell) if
  you want Billy to self-push; otherwise recover the commit from the `iterion/run/<name>`
  storage branch the engine always creates and push by hand.

## 2026-06-14 — re-validation on a clean clone + good dead-code judgment (run 019ec5bc)

- Status: **validated.** Re-ran in the C082 worktree studio (non-watchexec) on a
  clean iterion clone with a synthetic `billy-test` branch: one added file
  `pkg/log/billy_demo.go` exporting `WriteMarker`, which had no godoc AND
  swallowed the `os.Create` error. `base_ref=main`, `merge_into=none`.
- Result: cross-family review loop **converged** (review → fix → re-review →
  streak), `Run finished`, committed to storage branch
  `iterion/run/feral-crash-duskvane-127c` (`final_commit 9c5a5891`), not merged
  (merge_into=none respected).
- Value — **correct judgment, not a cosmetic fix.** Rather than just adding godoc
  + handling the error, Billy recognized `WriteMarker` as *unreferenced dead code
  that swallows an error* and **removed it** (`refactor(log): remove unused
  WriteMarker demo helper`, "No code in the tree calls it, so remove the dead
  code."). That's the right call — exactly what a demanding reviewer should do.
- Finding (minor, non-fatal): `fix_claude` (claude_code) emitted one
  `Tool error: StructuredOutput — No such tool available: StructuredOutput`
  before recovering and producing its output normally. The agent appears to try a
  `StructuredOutput` tool that isn't registered in the claude_code delegate — a
  wasted step, same broad family as the Devy claude_code-structured-output gap.
  Worth wiring/​silencing, but it did NOT block convergence.
- Convergence machinery (shared with Willy) is reference-correct; this run
  re-confirms it + the asymptote (no oscillation) on a fresh target.

---

**Status:** validated end-to-end (2026-06). **Scope of this report:** the
capability and the engineering hardening it drove. The target here is
iterion's own repository (the `feat/cloud-control-plane` epic), so target
details are included.

## Summary

Billy (the `branch-improve-loop` bot) was exercised against a real, large
branch — a ~7000-line / 42-chunk epic — and demonstrably:

1. **converges** a cross-family review/fix loop on a big diff (monotonic
   decrease to zero blockers + a two-family approval streak), rather than
   oscillating or stalling;
2. **finds real, high-value issues** the human author missed, and
   **authors complete fixes** for them — including an ADR for a design-level
   change;
3. **drives the full pipeline** plan_chunks → alternating cross-family review
   (Claude ↔ GPT) → same-family fix → `streak_check` → `prepare_commit` →
   semantic `commit_changes`, and stops at the asymptote.

The exercise also hardened the engine: **one significant runtime bug was
root-caused and fixed** while driving real runs — without it the GPT family
could not review a diff this size at all.

## What was validated

- The loop: `plan_chunks (deterministic diff measure + chunking) →
  round_robin reviewer (claude-opus-4-8 ↔ openai/gpt-5.5) → family-matched
  fixer → streak_check (2 consecutive opposite-family approvals) →
  prepare_commit → commit_changes`.
- **Chunked review at scale:** a 42-chunk diff, each reviewer reading chunks
  one at a time then merging into one whole-diff verdict (cross-family
  approval is on the whole diff, never chunk-by-chunk).
- **Convergence to an asymptote**, not oscillation — the bot settled into a
  stable approved state and committed.
- **Both backends working through the whole run**, including the GPT family on
  the local ChatGPT-forfait path.

## Method

A single end-to-end run (`019eb168`) on `feat/cloud-control-plane`, ~7000
lines of diff. Reviewers/fixers: `claude-opus-4-8` (Claude Code) and
`openai/gpt-5.5` (claw, ChatGPT-forfait). Sandboxed (per-run container),
`--merge-into none` so the result lands on a storage branch for review.
Budget raised to 8h/250$ for this run (the 2h/60$ default is too small for a
diff this size); convergence took ~2h of effective run time.

## Result

Converged: status `finished`, commit `1ffc4bc` —
`fix(secrets,memory): enforce bot-secret binding egress and isolate bot
memory by id`, 745 insertions / 84 deletions across 30 files, build + tests
green. Fast-forward-merged into the epic, then merged to `main`.

Convergence trajectory (blockers per verdict):

```
claude 2 → gpt 2 → claude 1 → gpt 1 → claude 1 → gpt 1
→ claude 0 (approved) → cross-family streak → commit
```

A slight oscillation near the end (GPT re-raised one blocker before settling)
is within the accepted asymptote behaviour; the `prior_pushback` /
`previous_scanned_areas` feedback kept verdicts from re-litigating resolved
items in a loop.

## What Billy found and fixed

Genuine issues in the reviewed epic, fixed with tests:

- `cloudpublisher` did not persist `RepoURL`/`RepoSHA`/`BotID`, breaking
  cloud/webhook **resume** and bot-bound secret resolution. Fixed across the
  publisher, `queue/types`, and the run store.
- `secretguard` did not intersect a bot-secret binding's egress hosts. Fixed,
  with ADR 018.
- Bot memory was not isolated by bot id. Fixed (`fsstore`, `scope`).
- Binding-route validation tightened; new tests added.

## Engineering hardening (the enabler)

Before the fix, `gpt-5.5` on the ChatGPT forfait died on the 42-chunk review
with `context_length_exceeded` — not a fundamental limit but a bug: the
forfait's effective context window is smaller than the model's advertised
1.05M, so claw's preemptive compaction (sized to the advertised window) never
triggered in time, and nothing reacted to the backend's rejection.

Fix: **reactive force-compaction** — on a context-window rejection the tool
loop force-compacts the running history to a shrinking target
(256k→128k→64k→32k, independent of the advertised window) and retries.
Surfaced as an `llm_retry`, reusing claw's existing pure compactor. With it,
Billy ran for hours with both GPT nodes (reviewer **and** fixer) and zero
context-overflow deaths.

## Operational resilience

The run absorbed several transient infrastructure interruptions — network
drop, ChatGPT-forfait cap, and an intermittent sandbox bootstrap flake — via
delegate-level network retries and an auto-resume loop that relaunches from
the checkpoint (no progress lost) until convergence. None were the
context-overflow bug; all were absorbed without operator intervention.
