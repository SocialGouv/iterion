<!-- DRAFT — status: proposal, awaiting Jo's arbitrage. Refs #1424 -->

# E2E stabilisation plan — DRAFT, per surface

**Status**: proposal only. This document does not decide anything. It
enumerates, per core surface, what is already free, what must become
free, what must stay paid and why, and the budget one stabilisation
pass costs. The order in which the surfaces are attacked is Jo's to
arbitrate; the writing here is what a stabilisation pass would find, not
a commitment to a schedule.

Read [docs/e2e-coverage-matrix.md](e2e-coverage-matrix.md) and
[docs/live-e2e-coverage.md](live-e2e-coverage.md) first — the counts
below are derived from the matrix, and the same discipline (a citation
is the test that would FAIL if the promise broke) applies here.

## What "stabilised" means for a surface

Repeated from #1424 verbatim so the plan cannot drift from its own
definition:

1. Its rows in the coverage matrix are `covered-deterministic` — **not**
   `covered-live`, wherever a credential-free test can reach the same
   seam. Free coverage is what gets run every time; paid coverage is
   what gets skipped when money is short.
2. What genuinely needs a real model has a **live** target, and that
   target has a green ledger row less than one release old (the ledger
   is #1422; without it, "less than one release old" is unmeasurable).
3. The tests **fail for the right reason** — each names what its
   mutation reddens; a test no mutation separates from a simpler one
   should *be* the simpler one.
4. A row that stays `covered-live`, `unit-only` or `excluded` carries a
   written **why**, enforced by the matrix gate for terminal statuses.

## What we already have (measured 2026-09-19)

| Surface | rows | det | live | uncov | unit-only | excluded |
|---|--:|--:|--:|--:|--:|--:|
| dsl | 51 | 49 | 0 | 0 | 2 | 0 |
| runtime | 56 | 55 | 1 | 0 | 0 | 0 |
| backends | 16 | 13 | 3 | 0 | 0 | 2 |
| bots | 25 | 23 | 1 | 1 | 0 | 0 |
| dispatcher | 14 | 14 | 0 | 0 | 0 | 0 |
| cli | 55 | 55 | 0 | 0 | 0 | 0 |
| cloud | 39 | 39 | 0 | 0 | 0 | 1 |
| forge | 7 | 7 | 0 | 0 | 0 | 1 |
| sandbox | 7 | 4 | 1 | 0 | 0 | 2 |
| triggers | 12 | 12 | 0 | 0 | 0 | 1 |
| observability | 8 | 6 | 0 | 0 | 2 | 0 |
| persistence | 9 | 9 | 0 | 0 | 0 | 0 |
| security | 8 | 8 | 0 | 0 | 0 | 0 |
| plugins | 7 | 7 | 0 | 0 | 0 | 0 |
| integrations | 5 | 1 | 0 | 0 | 0 | 4 |
| webhooks | 9 | 9 | 0 | 0 | 0 | 0 |
| desktop | 1 | 0 | 0 | 0 | 1 | 1 |

Total: 329 rows in tables, 360 `covered-deterministic`, 5
`covered-live`, 1 `uncovered`, 7 `unit-only`, 7 `excluded` (the
difference between the sum-per-surface and the matrix's 380 total is
that some rows belong to sub-family sections outside the top-level
prefixes counted above).

Seven surfaces (`dispatcher`, `cli`, `cloud`, `persistence`, `security`,
`plugins`, `webhooks`) are 100 % `covered-deterministic` today. They
are not the target of this plan — the returns are in the surfaces
that still carry live rows or uncovered gaps.

## Candidate order (to arbitrate, not to assume)

Ranked by *what breaks the most other things when it moves*, matching
the language of #1424:

### 1. DSL / compiler — the highest-leverage surface

- **Already free**: 49 / 51 rows `covered-deterministic`. 2 rows
  `unit-only`, no `covered-live`, no `uncovered`.
- **What must become free**: the two `unit-only` rows should be
  read on their own terms — is the promise really only unit-testable,
  or is there a compiler-level e2e that would fail on the same
  regression? If yes, promote to `covered-deterministic`.
- **What must stay paid**: nothing on the DSL surface today. The
  compiler runs on stubs by design.
- **Budget for a pass**: two rows to read, at most one to promote —
  compiler unit tests, minutes.

### 2. Runtime — fan-out, checkpoints, resume, rewind

- **Already free**: 55 / 56 rows deterministic.
- **What must become free**: the one live row (`runtime.supervisor`,
  supervisor steers a watched node via the inbox) — does it need a real
  model to demonstrate the mechanism, or is the mechanism separable
  from the LLM? If separable, extract a deterministic seam that drives
  the inbox with a stub. If not, keep it live and require a ledger row
  under one release old (#1422).
- **What must stay paid**: whichever fraction of `runtime.supervisor`
  is genuinely about "a real model handled the inbox message correctly"
  — a stub cannot judge that.
- **Budget for a pass**: read one row, possibly split it in two.

### 3. Backends — the free/paid split bites hardest

- **Already free**: 13 / 16 rows deterministic.
- **What must become free**: none obvious — backends is where "real
  model behaviour is the thing" and the deterministic seams for
  `claude_code` and `claw` already exist. The 3 live rows
  (`backends.claw` in-process client, `backends.codex` CLI delegate,
  `backends.model-quality` quality/value grading) each observe
  behaviour that a stub cannot forge.
- **What must stay paid**: all 3 live rows. Their `covered-live` line
  in the matrix already carries the WHY.
- **Excluded**: 2 rows — the untested paths (bedrock/vertex/foundry per
  claw-code-go) are `excluded` because they are wired but never
  validated on this repo.
- **Budget for a pass**: no change proposed here — read the three live
  rows, confirm each one's "why paid" holds, ensure each has a ledger
  entry once #1422 lands.

### 4. Bots / catalogue — mid-migration to `dsl: 2`

- **Already free**: 23 / 25 rows deterministic (parse + compile +
  golden replays for the LLM-node schemas via `pkg/botreplay`).
- **What must become free**: the 1 `uncovered` row (`bots.uncovered-catalog`
  — the catalog bots with no test beyond parse + compile) is the
  single specific gap; a stabilisation pass extracts a deterministic
  seam for each bot in that group, or promotes the ones whose behaviour
  the golden replay already covers, or moves the honest ones to
  `unit-only` with a WHY.
- **What must stay paid**: the 1 live row (`bots.live-covered-catalog`)
  — the bots whose observable behaviour needs a real model in a real
  workspace. This is where the ledger (#1422) earns its keep.
- **Budget for a pass**: 37 bundles to walk, most only need a
  reference to an existing golden. The `uncovered` row is one paragraph
  of work per bot, not a new bot.

### 5. Sandbox — a low-count surface with real gaps

- **Already free**: 4 / 7 rows deterministic.
- **What must become free**: the 1 live row (`sandbox.*`) and the 2
  `excluded` rows should be re-read: is any of them reachable by a
  stub driver (`sandbox/noop` already exists) with fault injection?
  If yes, promote.
- **What must stay paid**: the paths that genuinely exercise a real
  docker / k8s driver against a real image — those cannot be forged.
- **Budget for a pass**: hours, not days — the surface has 7 rows total.

### 6. Observability, integrations, desktop, forge — the tail

- **Observability** (6 / 8 det, 2 unit-only): read the two unit-only
  rows; if the promise is truly a library invariant, keep. Else
  promote.
- **Integrations** (1 / 5 det, 4 excluded): the excluded rows are
  third-party integrations kept out of the matrix for reasons in each
  row's WHY — read each once for staleness.
- **Desktop** (0 / 1 det, 1 unit-only, 1 excluded): a single-row
  surface, low priority.
- **Forge** (7 / 7 det, 1 excluded): 100 % free, one excluded row —
  read once.

## What this plan does NOT do

Repeated from #1424 verbatim:

> Do not confuse *more tests* with *more proof*. The repo has already
> paid for these: a test exercises the **site**, not the guarantee; a
> stub that accepts anything certifies nothing; and a guard that
> enumerates spellings never converges.

No engine change is proposed. No new test-for-tests-sake is proposed. A
stabilisation pass that adds rows to the matrix without making any
mutation redden costs money and buys nothing.

## Prerequisite: the ledger (#1422)

Every "one release old" claim above is unmeasurable until #1422 is
committed. The plan therefore proposes a strict order: **#1422 lands
first**, then a stabilisation pass reads live rows against fresh
ledger entries; a live row whose ledger reading is `never` is an honest
gap the plan must own, not a green line.

## What is Jo's to decide

The five arbitrations this document does NOT make for you:

1. **Order**: the DSL → runtime → backends → bots ranking is the
   default from #1424. A surface Jo would rather do first (dispatcher
   parity? bots first because they are the highest-visible?) overrides.
2. **Scope of promotion**: `unit-only` rows can be honest terminal
   states or premature capitulations. A one-off read per row is
   cheap; a per-surface decision is you.
3. **Budget cap per pass**: the plan describes the shape of one pass
   per surface. What total spend (hours + $ if the pass includes a
   live re-run) is acceptable is you.
4. **Living with `excluded`**: 7 rows currently carry `excluded` with
   a WHY. Whether any is a candidate for promotion is you.
5. **Definition of `stabilised`**: the four-clause definition at the
   top of this document is copied verbatim from #1424 — if any clause
   should be tightened or dropped, that decision belongs on #1424, not
   here.

## Prior art — surfaces already at "stabilised" today

`dispatcher`, `cli`, `cloud`, `persistence`, `security`, `plugins`,
`webhooks` — the pattern is a per-surface commit history that walked
every row over months. What made the difference each time was NOT the
count of tests but the discipline of `TestE2ECoverageMatrixGate`:
every citation must be a test that would fail if the promise broke.
The gate is what prevented the drift the plan exists to close in the
remaining surfaces.
