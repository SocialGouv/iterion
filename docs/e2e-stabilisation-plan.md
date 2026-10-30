# E2E stabilisation plan — per surface

**Status**: decided. The five arbitrations this document used to defer are
recorded below with the decision that settles each; the order is fixed and
the per-surface work is filed, one ticket per surface under its own epic.
What is NOT decided here is which surface a given session picks up — that is
the board's job ([project 203](https://github.com/orgs/SocialGouv/projects/203)).

Read [docs/e2e-coverage-matrix.md](e2e-coverage-matrix.md) and
[docs/live-e2e-coverage.md](live-e2e-coverage.md) first — the counts below are
derived from the matrix, and the same discipline (a citation is the test that
would FAIL if the promise broke) applies here.

## What "stabilised" means for a surface

Repeated from #1424 verbatim so the plan cannot drift from its own definition:

1. Its rows in the coverage matrix are `covered-deterministic` — **not**
   `covered-live`, wherever a credential-free test can reach the same seam.
   Free coverage is what gets run every time; paid coverage is what gets
   skipped when money is short.
2. What genuinely needs a real model has a **live** target, and that target
   has a green ledger row less than one release old. The ledger exists
   (#1422, `e2e/testdata/live/ledger.json`), so this clause is now measurable
   — and it measures badly: see the readings per surface below.
3. The tests **fail for the right reason** — each names what its mutation
   reddens; a test no mutation separates from a simpler one should *be* the
   simpler one.
4. A row that stays `covered-live`, `unit-only` or `excluded` carries a
   written **why**, enforced by the matrix gate for terminal statuses.

## Where we stand — measured, not quoted

| Surface | rows | det | live | uncov | unit-only | excluded |
|---|--:|--:|--:|--:|--:|--:|
| runtime | 58 | 57 | 1 | 0 | 0 | 0 |
| cli | 57 | 57 | 0 | 0 | 0 | 0 |
| dsl | 51 | 49 | 0 | 0 | 2 | 0 |
| cloud | 48 | 48 | 0 | 0 | 0 | 0 |
| server-api | 31 | 31 | 0 | 0 | 0 | 0 |
| bots | 25 | 23 | 1 | 1 | 0 | 0 |
| backends | 19 | 15 | 3 | 0 | 0 | 1 |
| studio-ui | 15 | 14 | 0 | 0 | 1 | 0 |
| dispatcher | 14 | 14 | 0 | 0 | 0 | 0 |
| tools-mcp | 14 | 13 | 0 | 0 | 1 | 0 |
| triggers | 13 | 13 | 0 | 0 | 0 | 0 |
| persistence | 9 | 9 | 0 | 0 | 0 | 0 |
| sandbox | 9 | 6 | 1 | 0 | 0 | 2 |
| webhooks | 9 | 9 | 0 | 0 | 0 | 0 |
| observability | 8 | 6 | 0 | 0 | 2 | 0 |
| plugins | 8 | 7 | 1 | 0 | 0 | 0 |
| forge | 7 | 7 | 0 | 0 | 0 | 0 |
| integrations | 6 | 2 | 0 | 0 | 0 | 4 |
| desktop | 1 | 0 | 0 | 0 | 1 | 0 |
| Total | 402 | 380 | 7 | 1 | 7 | 7 |

The surface axis is the matrix's **Family** column — the one the gate reads and
the "front door per family" section documents. The `ID` prefix is a naming
convention that disagrees on nine rows: the eight `security.*` rows are filed
under `server-api` and `cli`, because the front door of an auth guarantee is
the API or the CLI, and `cloud.run-login-return` is a studio test. So
`security` is not a surface in this table even though it is a prefix in the
matrix — and the one row whose family read `studio` now reads `studio-ui` (a
label typo; the row itself did not change).

**This table is not typed, it is pinned.**
`TestTheStabilisationPlanTableIsDerivedFromTheCommittedMatrix` (`bots/`) reads
the matrix with the contract parser's own rules and reddens on any
disagreement in either direction — a count that drifted, a surface the matrix
gained and the plan never heard about, a surface the plan kept after a rename
— and prints the table the plan should carry. It exists because the first
version of this table was typed from a 2026-09-19 reading and was still being
quoted five weeks and seventy rows later, with three whole surfaces
(`server-api`, `studio-ui`, `tools-mcp`, 60 rows between them) missing from
it. Eight surfaces (`cli`, `cloud`, `server-api`, `dispatcher`, `triggers`,
`persistence`, `webhooks`, `forge`) are 100 % `covered-deterministic`; they are
not the target of this plan.

## What the gate had never read

The matrix contract (marker, one contract table, parseable rows, allowed
statuses, a written WHY per terminal status, every citation resolving to a
real test file) is enforced by the `verify_run` tool of the `e2e-coverage`
bot. `TestE2ECoverageMatrixGate` drives that gate over **fixture** matrices
the test writes, so until this pass nothing had ever run the contract over
`docs/e2e-coverage-matrix.md` itself. The first run found eleven rows the gate
refuses:

- **Two rows with an unescaped `|` in a cell**, which shifts every cell after
  it — `cli.remote-assistant-mission` (`mission start|get|list|stop`) and
  `cloud.adaptive-routing-policy` (`refused_pinned_key` `forfait|park`). The
  gate read `list` and `cloud` as their Status; GitHub's renderer shifted the
  columns too, so no reader could see their Status either. Reworded here;
  #2800 asks the parser to honour the GFM `\|` escape and to name an over-wide
  row instead of reading it shifted.
- **Nine `ORPHAN CLAIM` rows** — citations that do not resolve: names joined
  by `+`, `;` or `and` instead of the contract's comma (one unresolvable
  citation instead of six resolvable ones), prose where a citation belongs
  (*"studio runChat/action-request tests"*), a package where a file belongs
  (`(pkg/server)`), a non-test helper (`pkg/store/storetest/conformance.go`),
  and one citation whose test had moved file (`TestConformance_FilesystemShared`
  is in `pkg/store/storetest_run_test.go`). All nine now cite tests that
  resolve.

One more mis-citation, which the gate cannot catch and a reader must:
`backends.model-quality` cited `e2e/live_quality_test.go`, a file that holds
`assessQuality` and **declares no test at all** — a citation that exists and
cannot fail. It now cites the free judge suite and the live bot test that
applies it.

`TestTheCommittedCoverageMatrixSatisfiesItsContract` runs the gate's own
contract code — sliced out of the compiled tool command, not re-implemented —
over the committed matrix, so the next occurrence of any of these is red
before it is quoted. Its floor subtests keep the slice honest: a slice that
lost a check would certify anything.

## The order, and the promotion rule

Decided (G97, signed by D7): **DSL → runtime → backends → bots**, then the
tail, ranked by what breaks the most other things when it moves. Promotion is
**asymmetric**: a `unit-only` row rises to `covered-deterministic` on proof (a
mutation seen red through the public path), while an `excluded` row rises only
once its premise has been **re-measured** in the tree — reading the WHY is not
re-measuring it.

### 1. DSL / compiler — #2792 (epic #1401)

- **Already free**: 49 / 51 rows. No live row, no gap.
- **Must become free**: possibly one of the two `unit-only` rows (`dsl.expr`,
  `dsl.parser-fuzz`) — each claims its operator-visible effect is carried by
  another row (`dsl.node-compute`, `dsl.diagnostics`). The reading is whether
  that row's test would really fail on the regression.
- **Stays paid**: nothing. The compiler runs on stubs by design.
- **Budget**: two rows to read, minutes, no money.
- **Read** (2026-10-30, #2792): both rows stay `unit-only`. `dsl.expr` — the
  evaluator is pure and exhaustively asserted in `pkg/dsl/expr`; its two
  engine effects each have a deterministic carrier that would fail on a
  broken evaluator (`dsl.node-compute` asserts the computed artifact value;
  the `when:`-expression path is driven through `edgeWhenHolds` by the
  review-topology e2e's `if(…, …, loop.review_loop.iteration % 2 == 1)`
  edge). `dsl.parser-fuzz` — the operator-visible promise (a diagnostic, not
  a panic) is carried by `dsl.diagnostics`' `TestValidate_Invalid`, where a
  parser panic would kill the test process. The matrix notes were sharpened
  to name these carriers.

### 2. Runtime — #2793 (epic #1403)

- **Already free**: 57 / 58 rows.
- **Must become free**: the free half of `runtime.supervisor` — watch →
  inject with the steer decision stubbed.
- **Stays paid**: *a real model produced a sensible steer*. A stub cannot
  judge it.
- **Ledger**: `test:live:feat:supervisor` reads `never`. Under clause 2 this
  row is an honest gap today, not a green line.
- **Budget**: one row to split; the paid half is one target at the D32 cap
  (~20 $) if the split leaves one.

### 3. Backends — #2794 (epic #1404)

- **Already free**: 15 / 19 rows.
- **The finding**: the three live rows are not the same kind of thing.
  `backends.opencode`'s live half is a conformance bench against the **real
  CLI** (`task test:clilive`) — no credential, no model turn, no money;
  `backends.model-quality`'s snapshot engine is free
  (`task test:live:quality:unit`) and only the grading of a real model's
  output is paid; `backends.codex` is genuinely paid (a real hosted
  `web_search`). `covered-live` conflates *needs money* with *needs a tool*,
  and a reader budgeting a pass cannot tell them apart.
- **Stays paid**: `backends.codex`, and the grading half of
  `backends.model-quality`.
- **Excluded**: `backends.bedrock-vertex-foundry` — real AWS/GCP/Azure
  credentials; claw's SDK paths are unit-tested with mocked clients and
  iterion adds no logic of its own.
- **Ledger**: `test:live:feat:codex-web-search` reads `never` — the target
  only became invocable on its own in this same wave.
- **Budget**: the distinction costs nothing; one paid target at the D32 cap.

### 4. Bots / catalogue — #2795 (epic #1399)

- **Already free**: 23 / 25 rows (parse + compile + golden replays).
- **Must become free, or be split honestly**: `bots.uncovered-catalog` — the
  only `uncovered` row in 402 — covers six bots with nothing beyond
  parse+compile: **modernize, wiki-gen, app-dev, supply-shield,
  supply-shield-cve, smoke**. Each belongs in one of three states: a
  deterministic seam exists (structural output), the value really is the LLM
  work product (live, with a target), or it is a scaffold nobody ships
  (`excluded`). The aggregate row should become one row per bot.
- **Stays paid**: `bots.live-covered-catalog` — nine bots whose value IS the
  work product.
- **Ledger**: one of sixteen bot targets has a verdict —
  `test:live:bot:rgaa-audit` = `pass`, 2026-10-13, $11.26, ref `0210afac6e5b`.
  Fifteen read `never`, including `test:live:bot:review-pr`, the target that
  guards the merge gate.
- **Budget**: the three-way decision is free; each bot that lands in the live
  state is one target at the D32 cap. Six bots × 20 $ is most of a D7
  ceiling — which is exactly why the free/paid decision comes first.

### 5. Sandbox — #2796 (epic #1405)

- **Already free**: 5 / 8 rows — proportionally the least free surface.
- `sandbox.docker-driver` (live) is **tool-gated, not paid**: it needs a
  container runtime, not a credential. Ledger: `test:live:feat:sandbox-net`
  reads `never`.
- **Excluded**: `sandbox.kubernetes-pod-lifecycle` (live cluster + registry,
  no fake apiserver harness in this repo), `sandbox.buildkit` (a Docker daemon
  with BuildKit; rejected by design on the k8s driver). Both premises are
  re-measurable and the container-env family is building harnesses next door.
- **Budget**: hours, no money.

### 6. The tail — #2797 observability, #2798 connectors, #2799 studio

Under their own epics (#1415, #1406, #1412):

- **Observability** (6 / 8, 2 `unit-only`): `effective-model` and
  `facade-routing` both assert through the durable record, which is how the
  rest of the surface's deterministic rows are tested — so the reading is
  whether the status understates the proof.
- **Connectors**: `plugins.workspace-commands` (live, ledger `never`, with a
  deterministic half already cited beside it — does the live half add a
  guarantee?), `tools-mcp.tool-display` (a pure formatter), and the four
  `excluded` `integrations` rows (third-party tenant, claw TUI behaviours),
  whose premises a claw bump can quietly invalidate.
- **Studio**: `studio-ui.editor-llm-meta` (is the card reachable in
  Playwright without a live model?) and `desktop.wails-app` (cgo +
  build-tagged, a GUI session; the bridge contract is the testable seam).

## What this plan does NOT do

Repeated from #1424 verbatim:

> Do not confuse *more tests* with *more proof*. The repo has already paid for
> these: a test exercises the **site**, not the guarantee; a stub that accepts
> anything certifies nothing; and a guard that enumerates spellings never
> converges.

No engine change is proposed. No new test-for-tests-sake is proposed. A
stabilisation pass that adds rows to the matrix without making any mutation
redden costs money and buys nothing. This pass added no matrix row: it made
eleven rows' citations resolve, made one citation able to fail, and put the
contract and the counts under guards that redden.

## The five arbitrations, and how they are settled

Recorded, not re-opened:

1. **Order** — G97: the DSL → runtime → backends → bots ranking of #1424 is
   signed, with the tail last. A session that wants another order argues on
   #1424, not here.
2. **Scope of promotion** — G97: asymmetric. `unit-only` rises on proof;
   `excluded` rises only once re-measured in the tree.
3. **Budget cap per pass** — D7 and D32: the live layer is a bounded, named
   burn — 8-12 targets, ~20 $ per target, a ~150-250 $ ceiling, charged to the
   PLIAGE team key (referenced by path, never printed), one ledger row per
   target as the receipt, every spend journaled. An epic may close on
   deterministic evidence; a 🟢 humming mark requires a ledger row.
4. **Living with `excluded`** — G97 again: seven rows carry `excluded` with a
   WHY (the matrix text matches `excluded` sixteen times; seven of those are
   rows, the rest are the legend and prose — count rows, not greps). None is
   promoted on reading alone.
5. **Definition of `stabilised`** — fixed by #1424 itself and copied verbatim
   at the top of this document.

## Prior art — and the honest version of it

`cli`, `cloud`, `server-api`, `dispatcher`, `triggers`, `persistence`,
`webhooks` and `forge` are 100 % `covered-deterministic` today, walked row by
row over months. The earlier version of this plan credited
`TestE2ECoverageMatrixGate` for that discipline. The credit was misplaced: the
gate ran on fixtures, never on this repository's matrix, and eleven committed
rows were in a state it refuses. The discipline that made the difference was
the per-row reading, and the gate only backs it now that
`TestTheCommittedCoverageMatrixSatisfiesItsContract` points it at the file
everyone cites.
