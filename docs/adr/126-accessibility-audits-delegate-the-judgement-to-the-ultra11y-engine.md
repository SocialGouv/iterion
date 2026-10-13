# ADR-126 — Accessibility audits delegate every judgement to the ultra11y engine; iterion orchestrates

- **Status**: Accepted
- **Date**: 2026-10-12 (decided), 2026-10-13 (first implementation, PR #2387)
- **Applies to**: `bots/rgaa-audit` (Acci); the retired `bots/ultra11y` (Ally); `bots/whole-improve-loop`'s `rgaa` preset; `scripts/ultra11y-engine.sh`, `scripts/ultra11y-engine/` and `bots/rgaa-audit/ultra11y.pin`; the engine contract tests under `bots/`
- **Neighbours**: [ADR-058](058-minimal-framing-lean-on-the-agent.md) (the one-agent auditor this replaces), [ADR-098](098-connector-catalog.md) (deterministic nodes for what no model should decide), [ADR-119](119-ambient-context-policy.md) (an auditor judges a checkout it does not control)

## Context

Two accessibility bots shipped in the catalogue, and neither could stand
behind its number.

Acci v2 (ADR-058) asked one model to find the non-conformities of a UI
codebase against the 106 RGAA criteria, with deterministic gates around it.
Its bilan (`docs/bot-runs/rgaa-audit.md`, 2026-07-07) records four real
defects found by the agent and erased between the agent and the report,
because they were emitted without the field the gates counted by: a model as
the detector means a dropped field reads exactly like a clean repository. Its
scores were not reproducible either (93 % then 72 % on the same scope), and
its reference grid was a set of `rgaa-criteria-*` skills copied from
etalab-ia/skills that contradict the referential on at least six points.

Ally (`bots/ultra11y`) moved the detection into the ultra11y engine, a
deterministic, offline CLI by a colleague (SocialGouv/ultra11y, MIT): static
rules anchored `file:line`, a worklist of the criteria a static pass cannot
decide, a fold that refuses an unjustified verdict by name, the French RGAA
report and its integrity checks. It was pinned to 2.32.0 by version only,
called `npx` at every node, relied on a subcommand the engine removed in 3.0,
let its single adjudicator write the file the fold reads, and in three dogfood
runs never walked a real codebase to `done` — the two runs on the studio app
ended on the duration budget and on a refused fold; the one that reached
`done` audited a 13-line page (its bilans, `docs/bot-runs/ultra11y.md` until
this decision, are folded into `docs/bot-runs/rgaa-audit.md`).

Meanwhile the engine's author wrote an orchestrator contract
(`skills/ultra11y/references/orchestrators.md`) for exactly this use, and the
engine reached 5.42.1 with a verdict contract (`C` must cite the evidence it
clears), a verdict ledger, a trial of the report's claims and per-criterion
briefs rendered from the referential at run time.

## Decision

**ultra11y owns the judgement; iterion owns the orchestration.** One bot,
`rgaa-audit` (Acci), rebuilt on the pinned engine; `bots/ultra11y` retired.

- The engine finds, gates and reports: the static rules, the worklist and the
  evidence it harvests, the verdict contract its fold enforces, the French
  report and its integrity checks, the issue set. **No normative text lives in
  the bundle**: the engine writes one brief per criterion at run time, the
  agents read that, and the paraphrased `rgaa-criteria-*` / `rgaa-dsfr` /
  `rgaa-audit` methodology skills are removed from the bots that audit and
  fix (Acci, Willy); `bots/app-dev` keeps its DSFR markup note, which is not
  a criteria grid, and no longer points at the removed grids.
- iterion orchestrates: criteria the engine leaves open are adjudicated by
  read-only agents in parallel theme batches; their verdicts are filtered to
  their batch and normalised before any fold; the fold is **probed on a copy**
  and its named refusals re-asked once; the real audit is folded exactly once;
  a second reader puts every stated non-conformity and every landed conformity
  on trial; a human gate is one var away; a coverage annex names every layer
  as ok, degraded or skipped; the issue plan is transcribed to the board by a
  judge with no tool but the board and held to the plan afterwards.
- The engine is **pinned twice**: at install time by a committed npm lockfile
  (`scripts/ultra11y-engine/package-lock.json`, `npm ci`), at load time by the
  size and sha256 of its entry file (`bots/rgaa-audit/ultra11y.pin`),
  fingerprinted before anything of it runs and before every call. An explicit
  `ULTRA11Y_BIN` that is not the pin is refused, never replaced.
- The engine's contract is **tested on the real binary**, offline, in CI
  (`bots/rgaa_audit_engine_contract_test.go`; the bot's own node scripts in
  `bots/rgaa_audit_bot_nodes_test.go`), and every engine defect the bot works
  around is pinned by a `known_upstream_defect_*` subtest that goes red the
  day upstream fixes it. Engine gaps go upstream as issues and pull requests
  (SocialGouv/ultra11y #41–#47), not as forks.

## Options considered

- **Absorb the knowledge** (copy the rules and the referential into skills and
  tool nodes, credit the author). Rejected: restating a referential is
  restating what drifts; the copies Acci v2 shipped were already wrong, and
  the engine's rules, packs and gates evolve upstream where one maintainer
  measures them against the W3C ACT corpus.
- **Call the CLI like deepsec in Seki** (a pure wrapper, every verdict the
  engine's, no agent). Rejected as the whole answer: the engine itself leaves
  ~99 of 106 RGAA criteria to judgement on a typical codebase, and its own
  orchestrator contract hands that judgement to an agent under its gate.
  Retained for the shape of the integration: pinned binary, real-engine
  contract tests, typed refusals, banks before any model runs (the lessons of
  `docs/agents/bots/toolchain-devbox.md`).
- **Keep two bots** (Ally for the engine, Acci for theme-by-theme reasoning).
  Rejected: the reasoning Acci v2 added was the part that could not be
  trusted, and two auditors disagreeing on the same repository is not a
  feature once one of them is reproducible.

## Consequences

- Findings are reproducible without a model in the loop; a conformance rate
  is the engine's, cross-checked line by line against the document before it
  is published; what no layer could decide is named, never silently conforming.
- An engine bump is a migration: the pin file, the lockfile, the contract
  tests, and every `known_upstream_defect_*` subtest read as a contract change
  ([engine contracts](../agents/testing/engine-contracts.md)).
- The bot inherits the engine's limits as typed refusals and degraded layers:
  no browser means a skipped rendered layer, a pull-request diff mode and the
  `review` hand-off wait for #2355, a tool node cannot read the board (#2388).
- The fixer side changes too: Willy's `rgaa` preset no longer carries a
  criteria grid; it fixes the criteria Acci's issues name, with the pinned
  engine's reproduction command as its oracle.
- The project owes the upstream a maintained relationship — the colleague's
  tool is now load-bearing, and the credits say so (`bots/rgaa-audit/CREDITS.md`).
