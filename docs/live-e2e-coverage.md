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
| Bundles / Expr-Compute / Codex | skills+bundle bots / `ExhaustiveDSLCoverage` / dual-model (covered) | ✅ |

## Cost discipline

Full real-scenario runs are **expensive** ($ and hours). Never wire the
`*:all` aggregates into blocking CI. Run targets piecemeal; the judge
panel adds a small, bounded cost (one structured call per judge) on top of
each run. Per-test cost/time estimates live in each test's doc comment and
in the Taskfile target `desc`.
