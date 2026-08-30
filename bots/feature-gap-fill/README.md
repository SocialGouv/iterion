# feature_gap_fill

Gap-driven feature completer — **v2 minimal-framing** (ADR-058). The input
is a STRUCTURED gap spec ("here is what's implemented, here is what's
missing") rather than a feature description from zero. ONE adaptive
`campaign` agent surveys the seams the spec references, builds a living
todo from the `missing` items, and closes them one at a time — smallest
change, build, test, semantic commit in stride — preserving what already
works. A deterministic build/test gate re-checks the tree after each pass;
a bounded continuation loop re-pokes the campaign until the gap is closed
and the tree is green. git is the durable state.

## When to use

- An ADR-driven survey (Adry / `adr-cartograph`) emitted a
  `type:feature-gap` issue with a structured gap spec.
- An operator wants to FINISH a known partial implementation without
  re-architecting what already works.
- Prefer `feature_dev` (Featurly) for greenfield work where there is no
  existing partial implementation to preserve.

## Inputs

| Var | Required | Description |
|---|---|---|
| `gap_spec` | yes | Structured gap spec describing what's implemented vs what's missing |
| `workspace_dir` | no | Defaults to `${PROJECT_DIR}` (the run's worktree) |
| `scope_notes` | no | Free-form extra context (constraints, priorities) |
| `baseline` | no | Known pre-existing failures to SKIP (empty = cheap stash-check once) |
| `max_passes` | no | Continuation-loop cap (default 8) |
| `plan_review` | no | Cross-model plan phase: `auto` (default, resolved at launch from the run's credentials) / `on` / `off` — see below |
| `plan_review_policy` | no | Mid-run peer failure: `skip` (default) or `wait` — see below |

A gap spec typically lists:
- `implemented[]` — files / abstractions already in place (preserve)
- `missing[]` — the concrete deliverables Fini must add
- `evidence[]` — references (paths, line numbers) that anchor the survey

## Shape (v2 — one agent, minimal framing)

```
plan_topology → campaign                        when the plan phase is off
plan_topology → plan → plan_review → plan_gate  when it is on
plan_gate     → plan_revise → campaign          peer served
plan_gate     → campaign                        peer skipped (unreviewed plan)

campaign → verify_build → verify_run → gate
gate → done            when converged (tree green AND gap_closed)
gate → campaign        as continuation_loop(max_passes), carrying fail_log
gate → done            (loop exhausted — ship what is banked)
```

- `campaign` — one adaptive claude_code agent: brief read-only survey of
  the implemented surfaces, living todo of missing items, then locate seam
  → smallest change → build → test → commit (`Bot: feature-gap-fill`
  trailer), one item at a time. Preservation discipline and the Adry
  ADR-ownership rule live in its contract; out-of-scope observations go to
  the board `inbox` as findings.
- `verify_build` + `verify_run` — the stack-agnostic deterministic gate:
  an agent writes `<scratch>/verify.sh` from the repo's own toolchain (see
  `skills/verify-build.md`), a tool node re-runs it and gates on the real
  exit code.
- `gate` — deterministic compute: `converged = passed && gap_closed`.

## Plan phase (cross-model pair review, ADR-091)

`plan_review: auto` resolves at launch from the run's credentials: when a
SECOND model family is available, the fill plan is authored (claude,
read-only) from the gap spec, critiqued by a cross-family peer (`claw` +
`openai/gpt-5.6-sol` by default), and revised by the SAME author session
before the campaign implements; otherwise the phase is bypassed whole
(the v2 shape, unchanged — `plan_topology` routes straight to
`campaign`). The plan reaches the campaign as a MAP, not a contract.

`plan_review_policy` picks the mid-run peer-unavailability behaviour:
`skip` (default — the reviewer's `action: skip` route completes it with a
zero-value critique stamped `_skipped`, so the plan proceeds unreviewed
rather than parking the campaign on a dead peer credential) or `wait`
(the failure stays `failed_resumable` and the run-level usage-window
retry resumes it when the window reopens — the deliberate-spend
posture).

Only the first pass plans: the continuation back-edge blanks the
plan fields, so later passes read `git log` instead of re-anchoring on a
stale plan.

The v1 staged pipeline (survey_existing → plan → act → simplify →
alternating cross-family review/fix loop → prepare_commit →
commit_changes) is retired — see the header comment in `main.bot` and git
history for the design.

## Run

```bash
iterion run bots/feature-gap-fill/main.bot \
  --var gap_spec='implemented: [pkg/foo/api.go, pkg/foo/types.go]; missing: [pkg/foo/handler.go for POST /foo, tests in pkg/foo/handler_test.go]; evidence: [pkg/foo/api.go:42 declares the route surface]'
```

See [main.bot](main.bot) for the full DSL.
