# feature_dev

Autonomous end-to-end feature development — **v2 minimal-framing**
(ADR-058). ONE adaptive `campaign` agent takes the feature prompt,
explores briefly (fanning out read-only sub-agents on a large repo),
builds a living todo of slices, and ships the feature one verified
semantic commit at a time — tests included, ADRs authored for
non-trivial decisions, out-of-scope observations filed to the board. A
deterministic build/test gate re-checks the tree after each pass; a
bounded continuation loop re-pokes the campaign until the feature is
complete and the tree is green. git is the durable state. An opt-in
tail pushes the series and opens the pull request (PR; merge request
on GitLab — the issue-label → PR lineage).

## Inputs

| Var | Required | Description |
|---|---|---|
| `feature_prompt` | yes | High-level description of the feature, with a clear done-state |
| `workspace_dir` | no | Defaults to `${PROJECT_DIR}` (the run's worktree — do not override) |
| `baseline` | no | Known pre-existing failures to SKIP (empty = cheap stash-check once) |
| `max_passes` | no | Continuation-loop cap (default 8) |
| `open_mr` | no | Push the series + open a PR on convergence (default false) |
| `mr_branch` / `mr_base` / `source_issue_ref` | no | PR wiring — see main.bot |

## Shape (v2 — one agent, minimal framing)

```
campaign → verify_build → verify_run → gate
gate → mr_gate         when converged (tree green AND feature_complete)
gate → campaign        as continuation_loop(max_passes), carrying fail_log
gate → mr_gate         (loop exhausted — ship what is banked)
mr_gate → finalize_mr  when open_mr   → done
mr_gate → done         when not open_mr
```

- `campaign` — one adaptive claude_code agent: brief exploration, living
  todo of slices, one verified commit per slice, ADR obligation and
  findings→board handoff in the contract.
- `verify_build` + `verify_run` — the stack-agnostic deterministic gate:
  an agent writes `<scratch>/verify.sh` from the repo's own toolchain
  (see `skills/verify-build.md`), a tool node re-runs it and gates on
  the real exit code.
- `gate` — deterministic compute: `converged = passed && feature_complete`.
- `finalize_mr` — opt-in: pushes the series, opens the PR
  (`skills/forge-mr-create.md`), back-links the source issue.

The v1 staged pipeline (plan → act → simplify session chain →
alternating cross-family review/fix loop → prepare_commit →
commit_changes) and its `review_mode`/`mono_family` topology vars are
retired — see the header comment in `main.bot` and git history for the
design.

## Run

```bash
iterion run bots/feature-dev/main.bot \
  --var feature_prompt='Add a /healthz endpoint that returns build info'
```

See [main.bot](main.bot) for the full DSL.
