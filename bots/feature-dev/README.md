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
| `failure_context` | no | Host-attested failed-run envelope for cross-project delegation |
| `delegation_instructions` | no | Operator-approved scope for the delegated worker |
| `workspace_dir` | no | Defaults to `${PROJECT_DIR}` (the run's worktree — do not override) |
| `baseline` | no | Known pre-existing failures to SKIP (empty = cheap stash-check once) |
| `scratch_dir` | no | Out-of-tree working files — the gate's `verify.sh` / `verify.log` only; git is the durable state. Defaults to `${PROJECT_SCRATCH_DIR}/feature-dev`, engine-resolved OFF the repo |
| `max_passes` | no | Continuation-loop cap (default 8) |
| `plan_phase` | no | `on` (default) authors the plan before the campaign; `off` plans in stride — see **Plan phase** below |
| `plan_review` | no | `auto` (default) resolves at launch to on iff a second model family is credentialed; `on` forces the peer review |
| `plan_review_policy` | no | What a mid-run peer failure does: `skip` (default) or `wait` |
| `open_mr` | no | Push the series + open a PR on convergence (default false) |
| `mr_branch` / `mr_base` / `source_issue_ref` | no | PR wiring — see main.bot |

## Shape (v2 — one agent, minimal framing)

```
workspace_probe → workspace_not_a_repo                    when not ok (WORKSPACE_NOT_A_REPO, no LLM spent)
workspace_probe → plan_topology           when ok
plan_topology → plan → plan_review_topology ─┬─ plan_review → plan_gate → plan_revise ┐ (peer only when
plan_topology ──────────────── (plan_phase off) ┴──── (plan_review off: unreviewed) ──┤  plan_review
                                                                                     ▼  resolved on)
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

**Precondition.** `workspace_probe` (a tool node, ~100ms, no LLM) is the
entry: a launch whose `workspace_dir` is absent or not a git repository
fails typed (`WORKSPACE_NOT_A_REPO` — on the run's own
`failure_code`/`error` through the `workspace_not_a_repo` fail node, and on
the probe's output) before any LLM node spends — a `--bot` launch carrying only `pr_url`
attaches no repository.

**Plan phase (ADR-091).** The plan is AUTHORED by default on every
deployment (claude, read-only); `plan_phase: off` is the explicit opt-out
(plan in stride, the v2 shape). `plan_review: auto` resolves at launch
from the run's credentials and gates ONLY the peer review: when a SECOND
model family is available, the plan is critiqued by an external peer and
revised by the SAME author session before the campaign implements;
otherwise the campaign receives the author's plan stamped as unreviewed
(`plan_provenance`). feature-dev's peer is the `kimi` backend on
`kimi-code/kimi-for-coding` — a `judge` node pinned in `lib/nodes.bot`,
with no `ITERION_PLAN_REVIEW_BACKEND_GPT` / `ITERION_PLAN_REVIEW_MODEL_GPT`
override, unlike the sibling campaign bots that run the review on `claw` +
`openai/gpt-5.6-sol`.
`plan_review_policy` picks the mid-run peer-unavailability behaviour:
`skip` (default — the reviewer's `action: skip` route: continue
unreviewed, loudly stamped) or `wait` (the run parks failed_resumable,
the usage-window retry resumes it — the deliberate-spend posture).

**Persy.** A `supervisor persy:` block watches the `campaign` node
(docs/supervisors.md): the perseverance coach that pushes back on
premature "impossible" verdicts, expedient shortcuts, failure loops and
unbanked state under budget pressure. `--supervisors off` disables it per
run.

**Quota fallback.** Five nodes — `plan`, `plan_revise`, `verify_build`,
`review` and `finalize_mr` — declare a `kimi_quota` fallback: when the
primary claude_code call comes back `usage_window` or `unavailable`, the
node is re-executed on the `kimi` backend (`kimi-code/kimi-for-coding`)
rather than parking the run. `campaign` deliberately declares none — the
implementing agent stays on one family for the whole pass. Grok is
deliberately not in the lane (its CLI is installed but its entitlement is
unverified here). So on a shut forfait window the plan, the generated
`verify.sh`, the in-loop adversarial review or the PR body can come from a
different model family than the rest of this page describes.

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

See [main.bot](main.bot) and [lib/](lib/) for the full DSL.

## Layout — a bot in several files

`main.bot` holds the header, the vars, the secrets, the supervisor and the workflow; the
rest lives beside it under `lib/` and is reached through the `import` lines at the head of
the main — `lib/schemas.bot` (the schemas), `lib/prompts.bot` (the prompts), `lib/nodes.bot`
(the nodes). The four files are ONE program: `iterion validate`, `run`, the studio and a
remote launch read the unit; the manifest's `requires.iterion` names the release that reads
`import`. See docs/dsl.md, "import — a bot in several files".
