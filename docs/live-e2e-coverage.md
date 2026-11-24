# Live e2e coverage + quality/value snapshots

> This page is the reference for **running** the opt-in live layer (harness,
> judge panel, env knobs, cost discipline). The authoritative
> feature×coverage inventory — including which rows are `covered-live` and why
> — is [docs/e2e-coverage-matrix.md](e2e-coverage-matrix.md).

iterion ships an **optional, real-LLM** end-to-end layer that exercises
every first-class bot and every engine feature against real models (real
cost, real budget). It has two distinct signals:

1. **Reliability invariants** (semi-deterministic) — a real run, asserted
   on invariants that survive LLM non-determinism: schema validity,
   required-non-empty fields, no-hallucinated-assignees, which nodes
   finished, commit / board-issue counts, convergence/streak, and an
   acceptable-error boundary. These DO fail the test.
2. **Subjective quality + value-for-money** — a cross-family LLM judge
   **panel** grades the *real work product* and its cost, snapshots the
   verdict into a committed per-target history, and compares against the
   previous snapshot to attest improvement/regression. This is
   **report-only by default** (it never fails on a subjective dip); an
   opt-in gate can turn a clear regression into a failure.

All live tests are gated behind the `live` build tag and skip cleanly when
credentials are absent, so they never run in normal CI.

## Running

```bash
# one bot / one feature (real cost — see the matrix below)
devbox run -- task test:live:bot:review-pr
devbox run -- task test:live:feat:permission

# the whole moved-bot set (slow + costly)
devbox run -- task test:live:bots

# free: the deterministic snapshot-store unit tests (no LLM)
devbox run -- task test:live:quality:unit
```

Credentials: `claude` CLI (Claude Code OAuth) and/or `ANTHROPIC_API_KEY`,
`OPENAI_API_KEY`. Docker is required for sandboxed bots (sec-audit-*,
secured-renovacy). Each test `t.Skip`s when its prerequisites are missing.

## The last-green ledger — "do they still pass?" without spending

Every live run writes one row to the committed ledger
(`e2e/testdata/live/ledger.json`) — **pass or fail, panel or no panel** —
keyed by the `task test:live:*` target the operator invoked. The writer is
the harness itself (`pkg/liveledger.Track`, hooked from `runBotLive`'s
entry and from the tests that bypass it), registered at `t.Cleanup`, so a
failure is recorded as a failure and the run's cost travels with the row.
A **skipped** run claims neither pass nor fail, but it is no longer
silent: every prerequisite guard refuses through one chokepoint
(`skipLive`), which records a `verdict: skipped` row naming the
prerequisite in `skip_reason`. That is what stops `never` from meaning
both "nobody has paid for this target" and "nobody could run it here" —
two different asks of the operator. `TestEveryLiveSkipNamesItsPrerequisite`
holds the class for free: inside a `live`-tagged test a `t.Skip` is legal
only as the `testing.Short()` arm or with a reason-naming call
(`liveledger.RecordSkip`, `<tracker>.SetSkipReason`) on the line before it,
so the next guard cannot quietly skip without a row. **A skip is a WRITE
to a committed file**: recording one leaves the tree dirty and the free
gate red until `pkg/backend/backendcaps/live_seed.json` is re-copied from
the ledger and the golden re-derived
(`go test ./pkg/backend/backendcaps/ -args -update-backendcaps`) — the
cost of attempting a target on a machine that cannot run it, which the
writer logs as it happens (and only when the COMMITTED ledger moved: a
scratch `ITERION_LIVE_LEDGER_PATH` moves nothing the gate compares, and
an unset `ITERION_LIVE_LEDGER_TARGET` writes nothing). A skip NEVER erases what a run
proved: over a recorded `pass`/`fail` the verdict, date, cost and
provenance stand and only `skipped_at`/`skip_reason` are stamped, so a
half-skipped target says which half — a `pass` carries the skipped half's
missing prerequisite next to it, and a standing `fail` carries the reasons
it could not be re-run since.
Aggregate targets (`test:live`, `test:live:bots:new`, …) run many test
functions under one row: the verdicts **merge fail-sticky**, then pass
over skipped, then skipped over `never`; duration and cost accumulate and
the provenance folds to a sorted set — the fold is commutative in every
field, so a row never depends on the order the members finished in.
The judge panel's snapshot store is a different, quality-only record and
does not replace the ledger.

Each row also names **what served it and who paid**: `served_model` is
read from the run's persisted `llm_request` events — what ACTUALLY
answered, not what the fixture asked for — and `funded_by` names the
credential ROUTE, never a key. Without them a `pass` obtained on an
overridden route is indistinguishable from a `pass` on the fixture's
pinned model, which makes the row a false receipt. The pair arrived with
**schema 2**: `liveledger.Load` refuses a ledger newer than the binary
rather than dropping fields it cannot see, precisely so a stale reader
cannot report an overridden run as a pinned one.

The ledger carries one row per **recording** target — a target whose run
reaches a Track call. The exceptions, classified in the enumeration
itself (`LiveTarget.Records`): the delegating targets (`test:live:bots`,
`test:live:bots-real` — each sub-target records its own row, and
`{{.TASK}}` re-resolves per sub-task so the parent's name never reaches a
Track call) and the non-test targets (`test:live:status`,
`test:live:compile`, `test:live:quality:unit`). A row that no run could
ever write would stay `never` forever and dilute the signal.

```bash
# free: no credential, no network — prints every recording test:live:*
# target with its last verdict (or an explicit `never`) sorted by staleness
devbox run -- task test:live:status
```

`never` is written, never elided: a blank row reads as "fine" to every
future reader, which is the one thing it never means. The status command
is a **read**: a target the Taskfile gains later shows up as `never` in
the table immediately, but the committed file is only seeded when the
operator passes `-write-back` (e.g. `go run ./cmd/iterion-live-status
-write-back`) or when the target's first real run writes its row. The
class guard (`TestEveryLiveTestRecordsToTheLedger` in `pkg/liveledger`)
fails a new live test that reaches no recording hook. See #1422.

Two companion guards keep the row set and the target set honest, because
a row only means something if a run can be attributed to it:

- `TestEveryLiveTestIsInvocableFromItsOwnTarget` — every live test needs
  a `test:live:*` target that runs it on its own (a target whose `-run`
  pattern reaches nothing outside the test's own source file; the
  `openai-compatible` pair shares one target and one row on purpose).
  A test reachable only from `test:live` or `test:live:feats:all` runs
  under THAT row, which merges its members fail-sticky — so no verdict,
  cost or date is ever attributable to the test, and its own row reads
  `never` forever while the coverage matrix cites it as live proof.
- `TestEveryRecordingTargetHasALedgerRow` — the committed file carries a
  row for every recording target. The status table lists a missing
  target from the Taskfile at read time, so an absent row is invisible
  in the file itself and makes every "N of M read `never`" count wrong
  on M. Seed with `-write-back`, then reconcile
  `pkg/backend/backendcaps/live_seed.json` (compared cell-for-cell by
  `TestLiveLedgerZeroMove`) and the reviewed golden beside it
  (`go test ./pkg/backend/backendcaps/ -update-backendcaps`).
- `TestNoTwoRecordingTargetsSelectTheSameLiveTest` — a target's `-run`
  pattern selects exactly the tests its name claims: among targets whose
  selection sits in ONE source file, no two may match the same test.
  `test:live:feature-dev` and `test:live:whole-improve-loop` once carried
  unanchored patterns that silently ran (and timed out, and billed, and
  fail-marked) their `_Real` sibling — whose own target exists — under
  the cheap target's row (#2784). The `openai-compatible` pair stays
  legal: no second target selects either function.

## Quality layer — how it works

- **Evidence** = the *real artifact*, never the bot's self-report: the git
  diff for code bots, created/moved board issues for board bots, doc diffs
  for docs bots, findings for audit bots — plus the run's price metrics
  (cost USD, tokens, duration, iterations).
- **Panel** = two judges from different model families (default
  `openai/gpt-5.5` + `anthropic/claude-sonnet-4-6`), both ideally
  different from the assessed bot's primary family; a same-family judge is
  flagged. Each scores the stable rubric and, when a prior snapshot
  exists, a relative (better/same/worse) verdict.
  - **Backends, validated live:** the OpenAI judge runs through claw
    (API key / ChatGPT-OAuth). The Anthropic judge runs through claw when
    `ANTHROPIC_API_KEY` is set, otherwise through the **claude_code OAuth
    delegate** — so the panel is genuinely **cross-family with NO API key**
    (validated: two real verdicts, gpt-5.5 + claude-sonnet, disagreement
    surfaced). Two gotchas were the whole battle, both fixed: pass the
    **bare** model id to claude_code (it rejects a `provider/` prefix), and
    ask the judge for a fenced ```json block (parseSDKOutput extracts it
    even amid Claude Code preamble). Set `ITERION_LIVE_JUDGE_CLAUDE_CODE=off`
    to force OpenAI-only; override the pair with `ITERION_LIVE_JUDGE_MODELS`.
    A judge that still returns non-conforming output is dropped (note) so
    the panel stays clean.
- **Rubric** (0.0–1.0, multi-dimensional so no single number is gameable):
  `efficacy`, `completeness`, `output_quality`, `restraint`,
  `reliability`, `value_for_money`, `overall` (holistic, not an average).
- **Snapshots** are committed, append-only per target under
  `e2e/testdata/live/quality/<name>/<UTC-ts>__<runid>.json`. The test
  **writes** a snapshot every run; **commit** the representative ones — the
  directory is the history of a bot's quality evolution. The newest prior
  file is the baseline the panel compares against.
- **Anti-Goodhart**: the bot never sees the rubric/judges; the assessment
  is external + post-hoc and is **never** fed back into any bot loop;
  judges grade the artifact, not claims; cross-family panel; relative
  comparison; the prompt rewards genuine improvement and penalises
  façade / verbosity-padding. See
  [docs/workflow_authoring_pitfalls.md](workflow_authoring_pitfalls.md).

### Environment knobs

| Var | Effect |
|---|---|
| `ITERION_LIVE_QUALITY=off` | Skip the judge panel entirely (iterating on reliability only). |
| `ITERION_LIVE_QUALITY_GATE=1` | Turn a clear regression vs the last snapshot into a test failure. |
| `ITERION_LIVE_JUDGE_MODELS` | Comma-separated judge model specs (override the default cross-family pair). |
| `ITERION_LIVE_QUALITY_DIR` | Override the snapshot history root (default `e2e/testdata/live/quality`). |
| `ITERION_TEST_STORE_DIR` | Run store: default `~/.iterion` (visible in studio); `workspace` to isolate per-test. |
| `ITERION_LIVE_MODEL` | Re-target EVERY LLM node of the run at one model spec (`openai_compatible/gpt-oss-120b`). |
| `ITERION_LIVE_BACKEND` | Re-target every LLM node's backend the same way. |
| `ITERION_LIVE_FUNDED_BY` | The credential ROUTE name the ledger's `funded_by` records (never a key — a length bound, the known key prefixes and a route-name charset refuse the obvious paste; a short name-shaped secret would pass, so the operator still owes a NAME). |
| `ITERION_LIVE_MAX_COST_USD` | Per-target spend ceiling, applied at the knob chokepoint to the workflow of every run a live target launches. It REPLACES a bot's own `budget: max_cost_usd` in both directions — lower or higher — the precedence `iterion run --max-cost-usd` already has. A target that cannot honour it refuses with the reason (see below); the judge panel is not capped by it. |

### Re-targeting a live run — the override knobs

Most live fixtures pin a vendor model, so a target can only be re-proven
on the credential that vendor needs. `ITERION_LIVE_MODEL` /
`ITERION_LIVE_BACKEND` lift that: the harness folds them into a
`model.ModelOverrides` rule under the **`"*"` selector** — every LLM node
— documented to outrank the node's DSL `backend:`/`model:`
([model_override.go](../pkg/backend/model/model_override.go)): an operator
re-targeting a bot at launch is an explicit choice, and the engine never
silently replaces it. A re-prove under an override re-targets the whole
bot, never one node of it — a child a fixture pins to a cheap vendor
default follows the override too (`liveSubagentDefault`).

**One chokepoint carries every knob to every run**
([`e2e/live_knobs_test.go`](../e2e/live_knobs_test.go)). `liveOverride` is
the one reader of the knobs. `newLiveClawExecutor` is the one constructor
of a live executor, and the `armLiveRun` it calls is the one function that
applies a knob to a run: the re-target as the executor's `"*"` rule, the
ceiling on the workflow the engine runs, the refusal of a re-target this
machine cannot fund. `newLiveEngine` is the one constructor of a live
engine, and refuses an executor that was not armed for the same workflow.
`recordLiveProvenance` is the one place a row is told what served and who
paid. That holds for `runBotLive` and for every test that wires its own
registries, tool sets, MCP managers or second engine — the class the
review rounds of #1423 found twice was a knob applied on the `runBotLive`
path while the direct targets bypassed it.
`TestEveryLiveRunIsArmedAtTheKnobChokepoint` (`pkg/liveledger`) sweeps the
live harness source, build tag included, and refuses any other site that
builds an executor or an engine, applies a budget, reads the knobs or feeds
provenance. `TestEveryLaunchKnobReachesEveryLiveTargetShape` (e2e, free:
the chokepoint file carries no build tag) drives each knob through each
shape a target obtains its run in, on a tool-only run for $0, and reads the
result back from the persisted run and the written row.

Four targets cannot honour every knob, and refuse with the reason rather
than spend without it. `feat:schedule` launches through
`cli.RunScheduleRun` and `claude-bg` drives `delegate.ClaudeCodeBackend`
directly: neither path has a seam for the re-target or the ceiling, so
both refuse those (`refuseRetargetWithoutSeam`; the funder still reaches
their row). `feat:budget` and `feat:budget-resume` assert on their
fixture's own budget, which the ceiling would replace
(`refuseCapOnBudgetSubject`; a re-target stays legal there). The sweep
refuses a new launch outside the engine, or a new test asserting on
`budget_exceeded`, that does not refuse the same way.

The knobs are the operator's **shell** gesture: they are operator-grade
names (`internal/envtrust`), refused from a project file by the harness's
`.env` loader as by the engine's. Under `task` that loader is not enough:
Task's root `dotenv: [".env"]` plants the repo-root `.env` into every
`task test:live:*` process before the harness starts, where a value in the
environment no longer says whether the shell or the file put it there. So
the harness refuses to run while that file assigns a launch knob at all
(`liveenv.DotEnvKnobs` — by name, no value of the file reaches the reason):
remove the line and export the knob in the shell.

These rules keep that honest, and they are the reason the knobs are safe
to use on a committed receipt:

- **the row says what served.** `served_model` comes from the run's
  `llm_request` events, so an override that failed to reach the executor
  is recorded as the pinned model that actually answered — not as the
  override that was asked for. Honest scope: only the claw path emits
  `llm_request` (in-process, and relayed from a claw inside a sandbox), so
  a `claude_code` / `codex` / `pi` delegation names no model. The column is
  a POSITIVE receipt and never a negative one, and it says which case a
  reader is in (`liveledger.ServedModelFor`): empty means **no LLM node
  ran** (a tool-only run — the `node_started` events carry each node's
  kind, and a router's mode); `unobserved` means an agent, a judge or an
  `llm` router ran and the backend named nothing — or that the run's events
  could not be read at all, which fails the test too; `unobserved:<spec>`
  means the same under a re-target that asked for `<spec>`, so an
  overridden pass can never read as a pass on the pinned model.
  `iterion-live-status` prints the sentinel in words. In a run that mixes a
  claw node with a delegated one, the column names the claw spec alone: a
  positive receipt of what was observed, never a claim that every node was.
  That reading holds for every row the harness writes because every
  recording path feeds the column: `runBotLive` reads the events BEFORE
  it takes a verdict (a `fail` row says what answered too), and each test
  that tracks its own row feeds it through `feedLedgerCost` right after
  the run — or `recordLiveProvenance` from the backend's own report where
  there is no event stream (`claude-bg`). `TestEveryDirectTrackerFeedsProvenance`
  refuses a `Track` call with no feed beside it.
- **`funded_by` names a credential the environment CARRIES.** The
  operator's `ITERION_LIVE_FUNDED_BY` wins; otherwise the name is derived
  only for a route served by a single credential variable AND only when
  that variable is set. A gateway reached by network auth, or served
  through the DB credential cascade, records nothing rather than naming an
  unset variable as the funder of a committed receipt. The pair reaches
  every row: `recordLiveProvenance` writes it on the engine paths and on
  `claude-bg` alike.
- **a re-targeted run does not skip on a key it no longer reaches.** The
  credential guards (`requireEnv`, `requireOpenAI`, `requireOpus48`) ask
  ONE helper — `overrideCoversProvider` over
  `internal/liveenv` — whether the override routes every LLM node away
  from the vendor they protect. A gateway run therefore needs no
  `ANTHROPIC_API_KEY`; an override ONTO `anthropic` still does, and a
  bare model id — or a slash prefix the model registry does not route on,
  `scaleway/gpt-oss-120b` being a model id — waves nothing through (which
  provider would serve it is a guess). Waving a guard moves the
  prerequisite onto the re-target: a re-target whose own route this
  machine cannot fund — read from the engine's availability report
  (`detect.Detect`, the probe iterion resolves its routes with) — is a
  `skipped` row naming that route, decided where the guards ask and again
  where the run is armed (with the fixture's vendor key present, no guard
  is asked at all). Waved anyway, the run would reach a provider it cannot
  pay and record a `fail`, with a ref and a `served_model`, for a run that
  never had a chance. The rule reads the override, not each node, and is
  conservative where the report is: an `anthropic` route only the claude
  CLI's forfait could serve reads as unfunded — set
  `ITERION_LIVE_BACKEND=claude_code` and the CLI's own login decides. Tool guards (`requireCLI`, `requireDockerImage`) are
  untouched: a missing binary is not a credential a run can be routed
  around.
- **the subject decides whether the verdict means anything.** Re-proving
  an engine invariant (a budget cap firing, a fork, a schedule trigger,
  skill mirroring) on another route proves the engine. Re-targeting a
  test whose SUBJECT is the backend (`feat:backend-autodetect`,
  `claude-bg`, `codex-web-search`, `permission-kimi`, `claw-*`,
  `workspace-commands`, `ultracode`) proves the wrong thing — those stay
  `never`, with the reason written down. The ceiling has its own subjects:
  `feat:budget` and `feat:budget-resume` prove their fixture's own cap, so
  they refuse it.

The ceiling reaches the run through `ir.ApplyBudgetOverrides`, applied by
`armLiveRun` to the workflow the engine then runs — the engine builds the
run's budget from it at `Run`, and the persisted run's `budget` shows it.
That path is per-field, non-zero wins, in BOTH directions. It
can lower a shipped cap — the raise-only steering path
(`SharedBudget.RaiseCaps`) cannot, so an authorised $20 would silently
keep a bot's $100 — and it can RAISE one, which is what re-proving a
target on a metered route the bot never budgeted for needs. It is not a
clamp, and `TestTheEnvCeilingRaisesTheShippedBudgetToo` keeps it from
becoming one quietly: clamping the operator's $120 back to a bot's
shipped $5 would silently replace an explicit launch choice.

## Authoring a new bot/feature test

The shared harness lives in `e2e/live_support_test.go` + `e2e/live_quality_test.go`.
A new test is one file `e2e/live_bot_<name>_test.go` (or `live_feat_<name>_test.go`):

1. Skip-guard the prerequisites: `requireCLI(t, "claude")`, `requireOpenAI(t)`,
   `requireDockerImage(t, ref)` (sec/sandbox bots), `requireOpus48(t)` (ultracode).
2. Seed a realistic fixture in an `os.MkdirTemp` dir — `seedGoModuleFixture`,
   `seedBranchDiffFixture`, or write files + `gitCommitAll`. **Never** mutate
   the real repo.
3. Call `runBotLive(t, liveSpec{...})`:
   - `botFile: "<bot>/main.bot"` for a plain bot, or `bundleDir: "../bots/<bot>"`
     to also mirror the bot's skills.
   - `vars`/`inputs`: the bot's required vars; disable side-effects
     (`post_to_board=false`, `open_mr=false`) where a toggle exists.
   - `autoResume: true` for bots with `human` nodes (drives gates to a
     terminal node by synthesizing schema-shaped answers).
   - For board-writing bots without a disable toggle, `t.Setenv(
     "ITERION_TEST_STORE_DIR", "workspace")` to isolate the board.
   - `withWorkDir: true` for sandbox / `worktree: auto` bots.
4. Assert reliability invariants on the returned `liveResult`:
   `assertNodesFinished`, `assertSchemaValid`, `assertOutputFieldsNonEmpty`,
   `assertNoHallucinatedAssignees`, `assertCommitsBeyond`, `countFinished`
   (loop/convergence).
5. Grade quality: `assessQuality(t, res, qualityInput{kind, name, persona,
   primaryFamily, task, workProduct})`. Gather `workProduct` with
   `gitArtifactEvidence(t, ws)` (code bots) or render the emitted
   issues/findings (board/audit bots).

### Registering it — four files, not one

A `TestLive_*` function needs a `task test:live:*` target of its own
(`TestEveryLiveTestIsInvocableFromItsOwnTarget`), and that target needs a
row in **three** committed files that the gate compares cell-for-cell:

| file | guard that reads it |
|---|---|
| `Taskfile.yml` — a target anchored on the function name | `TestEveryLiveTestIsInvocableFromItsOwnTarget` |
| `e2e/testdata/live/ledger.json` — the row, `verdict: never` | `TestEveryRecordingTargetHasALedgerRow` |
| `pkg/backend/backendcaps/live_seed.json` — the embedded twin | `TestLiveLedgerZeroMove` (cell-for-cell) |
| `pkg/backend/backendcaps/testdata/registry.golden.json` | `TestRegistryGolden` — the golden embeds `LiveSeed()`; regenerate with `go test ./pkg/backend/backendcaps/ -args -update-backendcaps` and review the diff |

The last two are the ones a new target forgets: the seed is hand-maintained,
no task generates it, and the golden carries it inside a larger document.
Then add the row to [docs/e2e-coverage-matrix.md](e2e-coverage-matrix.md) —
whose per-surface counts [docs/e2e-stabilisation-plan.md](e2e-stabilisation-plan.md)
derives, and whose guard prints the table the plan should carry.

## Coverage matrix

Status legend: ✅ implemented · 🚧 planned.

### Bots

| Bot | Persona | Test | Status |
|---|---|---|---|
| feature-dev | Featurly | `TestLive_FeatureDev[_Real]` | ✅ |
| whole-improve-loop | Willy | `TestLive_VibeReviewAlternating[_Real]` | ✅ |
| secured-renovacy | Renovacy | `TestLive_SecuredRenovacy[_Real/_Protestware]` | ✅ |
| review-pr | Revi | `TestLive_Bot_ReviewPR` | ✅ |
| whats-next | Nexie | `TestLive_Bot_WhatsNext` | ✅ |
| docs-refresh | Doki | `TestLive_Bot_DocsRefresh` | ✅ |
| adr-cartograph | Adry | `TestLive_Bot_AdrCartograph` | ✅ |
| evolve | Evoly | `TestLive_Bot_Evolve` | ✅ |
| revi-converse | Revi | `TestLive_Bot_ReviConverse` | ✅ |
| rgaa-audit | Acci | `TestLive_Bot_RgaaAudit` | ✅ |
| branch-improve-loop | Billy | `TestLive_Bot_BranchImproveLoop` | ✅ |
| feature-gap-fill | Fini | `TestLive_Bot_FeatureGapFill` | ✅ |
| test-coverage | Testy | `TestLive_Bot_TestCoverage` | ✅ |
| bmady | Bmady | `TestLive_Bot_Bmady` | ✅ |
| devbox-setup | Devy | `TestLive_Bot_DevboxSetup` | ✅ |
| adr-rechallenge | ReArchi | `TestLive_Bot_AdrRechallenge` | ✅ |
| dep-update-guard | Vetty | `TestLive_Bot_DepUpdateGuard` | ✅ |
| sec-audit-source | Seki | `TestLive_Bot_SecAuditSource` | ✅ |
| sec-audit-deps | Depsy | `TestLive_Bot_SecAuditDeps` | ✅ |

### Features

| Feature | Test | Status |
|---|---|---|
| Node types / routers / await / session modes | `TestLive_Full_ExhaustiveDSLCoverage` | ✅ |
| claw backend + tools + MCP + vision + long-context | `TestLive_Lite_Claw*`, `TestLive_ClawToolCoverage` | ✅ |
| Router (llm mode) | `TestLive_Feat_RouterLLM` | ✅ |
| Permission gate (deny + ask) | `TestLive_Feat_Permission_Deny`, `_Ask` | ✅ |
| Ultracode | `TestLive_Feat_Ultracode` | ✅ |
| Supervisors | `TestLive_Feat_Supervisor` | ✅ |
| Cursors | `TestLive_Feat_Cursors` | ✅ |
| Board capabilities | `TestLive_Feat_BoardCaps` | ✅ |
| rtk compression | `TestLive_Feat_Compress` (rtk binary) | ✅ |
| Verified Action recovery | `TestLive_Feat_VerifiedAction` | ✅ |
| Budget + resume + fork | `TestLive_Feat_Budget`, `_BudgetResume`, `_Fork` | ✅ |
| Worktree finalization | `TestLive_Feat_Worktree` | ✅ |
| Human llm modes | `TestLive_Feat_HumanLLM` | 🚧 |
| Skills mirroring | `TestLive_Feat_Skills` | ✅ |
| Backend auto-detect | `TestLive_Feat_BackendAutodetect` | ✅ |
| Sandbox network allowlist (host_state/build: follow-up) | `TestLive_Feat_Sandbox_Network` (docker) | ✅ |
| Dispatcher | `e2e/dispatcher_test.go`, `board_dispatcher_test.go` (deterministic) | ✅ |
| Webhooks | `pkg/webhooks/*_test.go` (parsers) + server stubs (deterministic) | ✅ |
| Schedule (trigger → run) | `TestLive_Feat_Schedule` | ✅ |
| claude_code background work (session waits for its async subagent; lifecycle-off witness) | `TestLive_Feat_ClaudeCodeBackgroundWork` (`task test:live:claude-bg`) | ✅ |
| claw named workspace subagents (`.claude/agents/<name>.md` resolved by the `agent` tool's subagent_type; tools: narrowing; tools: [] counter-example) | `TestLive_Feat_ClawWorkspaceAgents` (`task test:live:claw-workspace-agents`) | ✅ |
| Bundles / Expr-Compute / Codex | skills+bundle bots / `ExhaustiveDSLCoverage` / dual-model (covered) | ✅ |

## Cost discipline

Full real-scenario runs are **expensive** ($ and hours). Never wire the
`*:all` aggregates into blocking CI. Run targets piecemeal; the judge
panel adds a small, bounded cost (one structured call per judge) on top of
each run. Per-test cost/time estimates live in each test's doc comment and
in the Taskfile target `desc`.

**The bill is the ledger, not this page.** `task test:live:status` prints
every target with its verdict, date, duration, cost, `served_model`,
`funded_by` and — for a target that could not run — the prerequisite it
lacked. A second, hand-kept table of runs here would rot into a louder
claim than the file it paraphrases, which is the one thing a cost receipt
must not be.

A **free tier** exists and is worth running before anything is spent:
`test:live:feat:worktree` and `test:live:feat:rtk` are tool-only fixtures
(git, `rtk`) with no LLM node — seconds and $0, no credential — and
`test:live:feat:sandbox-net` / `test:live:feat:sandbox-image-env` need a
docker image rather than a key. `task test:clilive` is a free real-CLI
layer beside them: each proof skips when its CLI is absent and spends
nothing. Set `ITERION_TEST_STORE_DIR=<writable abs path>` for any of them
in a sandboxed shell: the store defaults to `$HOME/.iterion`, and a store
the process cannot create is a fatal AFTER the ledger hook is registered,
which stamps a `fail` on a target that is perfectly healthy.
