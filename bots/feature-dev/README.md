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
campaign → verify_probe → verify_build → verify_run → review → gate
gate → mr_gate         when converged (green AND feature_complete AND review.clean)
gate → campaign        as continuation_loop(max_passes), carrying fail_log
gate → mr_gate         (loop exhausted — ship what is banked)
mr_gate → forge_auth_probe → finalize_mr  when open_mr   → done
mr_gate → done         when not open_mr
```

(`verify_probe` reuses a valid `verify.sh` on passes 2+, skipping the LLM
`verify_build`; `forge_auth_probe` is a ~100ms credential pre-flight before the
`finalize_mr` agent.)

- `campaign` — one adaptive claude_code agent: brief exploration, living
  todo of slices, one verified commit per slice, ADR obligation and
  findings→board handoff in the contract.
- `verify_probe` + `verify_build` + `verify_run` — the stack-agnostic
  deterministic gate: `verify_probe` (tool) decides whether the existing
  `<scratch>/verify.sh` can be reused (passes 2+) or must be regenerated; on a
  miss `verify_build` (agent) writes it from the repo's own toolchain (see
  `skills/verify-build.md`); `verify_run` (tool) re-runs it and gates on the
  real exit code.
- `review` — an **in-loop adversarial self-review** (adaptive claude_code,
  readonly) run after the build gate. It reads the code-review-invariants skill
  and the run's own diff and blocks convergence ONLY on a high-confidence
  defect in the six invariant classes (emitting `clean` + `findings`) — the
  refinement a downstream reviewer would do, moved INTO the loop so the PR
  ships clean. There is no cross-family reviewer relay; this one in-loop
  adversarial `review` node remains.
- `gate` — deterministic compute:
  `converged = passed && feature_complete && review.clean`. `fail_log` carries
  the build failure (RED build) or the review findings (green but review-dirty)
  back to the campaign.
- `finalize_mr` — opt-in: a `forge_auth_probe` credential pre-flight gates it,
  then it pushes the series, opens the PR (`skills/forge-mr-create.md`), and
  back-links the source issue.

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
