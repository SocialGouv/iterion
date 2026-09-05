# e2e-coverage (Endy)

Autonomous **end-to-end coverage completion** — ADR-058 v2 minimal-framing,
sibling of test-coverage (Testy). ONE adaptive `campaign` agent inventories
the application's FEATURES from the outside, maintains a committed
**feature×coverage matrix**, and closes each gap with a real, deterministic
e2e test written in the repo's OWN harness — one `test(e2e):` commit per
feature, the matrix row flipped in the same commit. A deterministic gate
re-runs the repo's own suite AND enforces the matrix contract; a bounded
continuation loop re-pokes the campaign until every feature row in scope is
terminal. git is the durable state.

**Anti-façade by design:** the metric is features whose regression would
fail a test, never a green table. The campaign's contract enforces the
feature-level mutation test (break the feature ⇒ the test must fail) and
forbids stub-echo / harness-only / no-invariant tests and borrowed claims;
the gate's **claims check** makes "feature covered" unfakeable at the
existence level — every `covered-*` row must cite a test that grep-resolves
in the tree (an *orphan claim* is a red gate). See
[skills/e2e-coverage.md](skills/e2e-coverage.md) for the doctrine and
[skills/coverage-matrix.md](skills/coverage-matrix.md) for the contract.

**Deterministic-first:** a CI-runnable, credential-free e2e test beats an
opt-in/live one. Features that genuinely need a live external (real LLM,
third-party OAuth, cloud service) end `covered-live` (a test in the repo's
opt-in layer) or `excluded` — always **with the reason**, never silently
skipped.

## Inputs

| Var | Required | Default | Description |
|---|---|---|---|
| `target` | no | `""` | Scope: a feature family, area, or free description. Empty ⇒ the WHOLE application (the run only converges when no `uncovered` row remains). |
| `matrix_path` | no | `docs/e2e-coverage-matrix.md` | The committed feature×coverage matrix, relative to the repo root. |
| `baseline` | no | `""` | Known pre-existing failures to SKIP (empty = cheap stash-check once). |
| `max_passes` | no | `8` | Continuation-loop cap. |
| `workspace_dir` | no | `${PROJECT_DIR}` | Workspace root (resolves to the run worktree under `worktree: auto` — do not override). |
| `scratch_dir` | no | `${PROJECT_SCRATCH_DIR}/e2e-coverage` | Out-of-tree scratch for the gate's verify script/log. |

## Shape (v2 — one agent, minimal framing)

```
workspace_probe → workspace_not_a_repo            when not ok (WORKSPACE_NOT_A_REPO, no LLM spent)
workspace_probe → plan_topology   when ok
plan_topology → plan → plan_review_topology → (plan_review → plan_gate → plan_revise)
campaign → verify_build → verify_run → gate
gate → done            when converged (suite green AND matrix_ok AND
                       coverage_complete AND (scoped OR 0 uncovered rows))
gate → campaign        as continuation_loop(max_passes), carrying fail_log
gate → done            (loop exhausted — ship what is banked)
```

- `workspace_probe` — deterministic entry precondition (~100ms, no LLM):
  a launch whose `workspace_dir` is absent or not a git repository fails
  typed (`WORKSPACE_NOT_A_REPO` on the run's own `failure_code`, through
  the `workspace_not_a_repo` fail node) before any LLM node spends.
- plan phase (ADR-091) — the plan is AUTHORED by default (`plan_phase:
  off` opts out); `plan_review: auto` gates ONLY the cross-model peer
  review (on iff a second model family is credentialed at launch),
  otherwise the campaign gets the author's plan stamped as unreviewed
  (`plan_provenance`).
- `campaign` — one adaptive claude_code agent: feature inventory → matrix →
  per gap: observable contract → deterministic e2e test in the repo's own
  idiom → see it pass → flip the row → commit test+row together.
- `verify_build` + `verify_run` — the stack-agnostic deterministic gate: an
  agent writes `<scratch>/verify.sh` from the repo's own toolchain (see
  `skills/verify-build.md` + `skills/verify-tests.md`); a tool node re-runs
  it on the REAL exit code **and** enforces the matrix contract (parse,
  statuses, justifications, claims grep — orphan claims are red).
- `gate` — deterministic compute; `new_test_code` is observability only:
  the claims check is the stronger floor, and a legitimate pure-mapping
  pass (citing pre-existing tests during inventory) must be able to
  converge without new code.
- `supervisor persy:` — the perseverance coach watching `campaign`
  (docs/supervisors.md); `--supervisors off` disables it per run.

## Run

```bash
# whole application (converges only when the matrix has no uncovered row):
iterion run bots/e2e-coverage/main.bot

# one feature family this run:
iterion run bots/e2e-coverage/main.bot --var target='persistence & resume lifecycle'
```

Stack-agnostic: how to enumerate features, find the repo's e2e harness, and
write each test lives in [skills/](skills/) — adding a language needs no DSL
edit. See [main.bot](main.bot) for the full DSL.
