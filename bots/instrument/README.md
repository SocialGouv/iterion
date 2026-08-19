# instrument (Obsy)

Observability instrumentation campaign — **ADR-058 minimal-framing**
(the proven feature-dev / branch-improve-loop chassis). ONE adaptive
`campaign` agent surveys the target repo's production entry points and
wires the observability families requested in `scope`, one verified
semantic commit at a time — tests included, ADRs authored for the
SDK/library choices, docs updated where the repo documents
configuration. A deterministic build/test gate + an in-loop adversarial
review + a bounded continuation loop re-poke it until every family is
fully wired and the tree is green; an opt-in tail opens the pull
request. git is the durable state.

## Families (`scope` — open comma list)

| Family | Meaning (definition of done: `skills/instrumentation.md`) |
|---|---|
| `errors` | Error tracking via a **Sentry-DSN-protocol** SDK (Sentry AND GlitchTip). Enabled only when the DSN env var is set (default `SENTRY_DSN`); loud non-fatal init failure; release/environment tags; capture at process seams; flush on shutdown; secret/PII scrubbing. |
| `logs` | ONE central logging seam — **extend the repo's existing logger, never replace it**. Leveled + structured fields; **JSON by default on production surfaces**, human on interactive CLI; stray-print sweep; error→event / warn→breadcrumb coupling into the tracker. |
| `tracing` | **Opt-in** (add it explicitly). Sentry-first transactions/spans, env-tunable sampling, conservative prod default. See `skills/tracing.md`. |

Stack knowledge lives in `skills/lang-go.md` / `lang-js.md` /
`lang-python.md` — adding a stack or a family is a skill file, never a
DSL edit.

## Inputs

| Var | Required | Description |
|---|---|---|
| `scope` | no | Families to wire (default `errors,logs`; `tracing` is opt-in) |
| `dsn_env_var` | no | Runtime DSN env var name (default `SENTRY_DSN`; unset at runtime = tracking off) |
| `mission_notes` | no | Repo-specific arbitrations for this run (which module to extend, seams, exclusions) |
| `workspace_dir` | no | Defaults to `${PROJECT_DIR}` (the run's worktree — do not override) |
| `baseline` | no | Known pre-existing failures to SKIP (empty = cheap stash-check once) |
| `max_passes` | no | Continuation-loop cap (default 6) |
| `open_mr` | no | Push the series + open a PR on convergence (default false) |
| `mr_branch` / `mr_base` / `source_issue_ref` | no | PR wiring — see main.bot |

## Shape (v2 — one agent, minimal framing)

```
campaign → verify_probe → verify_build → verify_run → review → gate
gate → mr_gate           when converged (green AND instrumentation_complete AND review.clean)
gate → campaign          as continuation_loop(max_passes), carrying the failure log
mr_gate → forge_auth_probe → finalize_mr → surface_pr_link → done   (opt-in PR tail)
```

## Run it

```sh
iterion run bots/instrument \
  --var mission_notes="extend pkg/log; SENTRY_DSN standard envs" \
  --var scope=errors,logs
```

Wire the DSN afterwards by setting the env var (e.g. `SENTRY_DSN`,
plus `SENTRY_ENVIRONMENT`) on the instrumented deployment — the run's
docs slice records the exact envs it introduced.
