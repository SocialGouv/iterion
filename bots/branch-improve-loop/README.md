# branch_improve_loop (Billy) — companion notes

Companion to the `branch_improve_loop` workflow ([`main.bot`](main.bot)).
This is a **design journal**: what the mechanism is, why it is shaped this
way, and what is still open.

## What it does — one agent reviews+improves the branch diff, minimal framing

`branch_improve_loop` runs a **REVIEW-AND-IMPROVE campaign scoped to a branch's
diff**. The scope is **the changes THIS branch introduces over `base_ref`**
(default `main`) — the diff `git diff $(git merge-base base_ref HEAD)` measured
against the **WORKING TREE** (so a prior pass's uncommitted fixes stay visible;
the commit-only `base_ref...HEAD` form would hide them and the loop could never
converge). One capable agent reads that diff, finds the **real issues the
change introduces or leaves** — bugs, regressions, missing/weak tests,
unhandled errors, quality problems **in the diff** — and improves them,
**committing each fix in stride** with a semantic message. It does **not**
re-litigate code the branch didn't touch.

The mechanism is deliberately **minimal**: give ONE capable agent a mission +
standing autonomy and let it work in its natural flow — the way a productive
human-driven Claude Code session actually looks
([docs/references/productive-session-patterns.md](../../docs/references/productive-session-patterns.md)):
a **living todo list** born from reading the diff (never frozen upfront
phases), and for each issue the repeated unit **locate → smallest fix → build →
test → COMMIT**, a few edits per commit, validation *before* the commit,
committing each fix **as it finishes** (never batch).

### Sibling of whole_improve_loop v2

Same v2 shape as [`whole_improve_loop`](../whole-improve-loop/main.bot)
(ADR-058): one adaptive `campaign` agent + a deterministic build/test gate + a
bounded continuation loop + git-as-state + an opt-in PR path. The **only**
difference is scope — `whole_improve_loop` applies ONE determined **axis**
across the whole codebase; `branch_improve_loop` reviews+improves the **diff of
one branch**. `whole_improve_loop`'s AXIS is replaced here by the branch diff
itself: `base_ref` + the diff define the work.

### Why v2 replaced the v1 chunked review loop

v1 was a chunked cross-family review loop: ~11 nodes (`plan_chunks → alt →
reviewer_claude/reviewer_gpt → streak_check → fix_claude/fix_gpt →
prepare_commit → commit_changes`) plus the chunking dials and the mono/dual
topology vars. It **over-framed the work** — the same lesson
`whole_improve_loop` learned (ADR-058): *"the deficit is framing, not
capability"*, *"once framed, a campaign runs itself"*. A capable agent reviews
a branch diff and fixes what it finds better as **one flow** — a living todo
list, one verified commit per fix — than as an assembly line of
chunk/review/fix/commit nodes. v2 keeps what the data says matters — the
verified per-fix commit cadence, the deterministic build/test gate, the
baseline, the termination contract — and drops the graph machinery around it.

## The graph

```
campaign ──▶ verify_build ──▶ verify_run ──▶ review ──▶ gate
   ▲   (one adaptive agent:   (writes        (in-loop  (deterministic
   │    reviews+improves       <scratch>/     adversarial build/test gate
   │    the branch diff,        verify.sh)     re-review)  + review verdict)
   │    commits each fix                                      │
   │    in stride)                                            │
   │                                                          │
   │  (not converged: RED → fix / green but more              │
   └────────────── issues → next pass) ◀──────────────────────┤
                                                               │  (converged:
                                                               ▼   green ∧ branch_clean
                                              mr_gate ──▶ (…) ──▶ done   ∧ review.clean)
```

(`verify_probe` reuses a valid `verify.sh` on passes 2+, skipping the LLM
`verify_build`; the mr tail forks to `finalize_mr` (open PR) or the PR
push-back lane — see below.)

- **`campaign`** (adaptive, claude_code, full tools) is the whole engine: it
  runs `git add -N .` then reads the branch diff, builds a living todo list of
  the real issues in the diff, and fixes them one at a time — locate → smallest
  fix → build → test → **commit** (`git add -A` incl. untracked, semantic
  message) — until a fresh re-review finds no real issue left. It emits a
  **termination contract** (`branch_clean`, `commits_this_pass`,
  `issues_remaining`, …). It may pause for the operator on a genuine mid-flight
  decision (kept rare).
- **`verify_build` → `verify_run`** is the **deterministic, stack-agnostic**
  build/test gate: an adaptive agent reads the `verify-build` skill and writes
  the repo's real build+test into `<scratch_dir>/verify.sh`; a tool node
  re-runs it and gates on the **real exit code** (no LLM judgment). This is
  both the tight real-feedback loop AND the anti-Goodhart truth oracle — the
  agent can't self-certify. `verify_build` does **not** fix code.
- **`review`** (adaptive, claude_code, readonly) is an **in-loop adversarial
  self-review** of the branch diff, run after the deterministic build gate. It
  reads the code-review-invariants skill and blocks convergence ONLY on a
  high-confidence defect in the six invariant classes (emitting `clean` +
  `findings`) — the downstream reviewer's job, moved into the loop so the
  pushed-back branch ships clean.
- **`gate`** (deterministic compute) decides continuation: `converged =
  verify_run.passed && campaign.branch_clean && review.clean` — the gate is
  **green** AND the campaign reported **`branch_clean`** AND the in-loop
  `review` came back **clean**. Not converged → back to `campaign`; a RED gate
  carries the failure log, a green-but-review-dirty pass carries the review
  findings so the agent fixes what `review` flagged, and a green-but-more-work
  pass carries an empty log so the agent simply keeps reviewing.
- **`mr_gate`** routes the post-convergence tail into exactly one of three
  mutually-exclusive lanes:
  - **`forge_auth_probe` → `finalize_mr`** — the opt-in PR path (`open_mr`)
    shipping the series of per-pass commits.
  - **`push_auth_probe` → `push_back_tool` → `publish_verdict`** — the
    **PR push-back** path (`push_branch` set, `open_mr=false`): a ~100ms
    credential probe, then `push_back_tool` pushes the run's HEAD onto the
    PR's source branch (no-op via rev-list when nothing is new), then
    `publish_verdict` posts Billy's review verdict as a comment on the PR
    (`pr_url`) **and**, when `gate_enabled`, a `gate_context` commit
    status on the head the push just produced — a status posted on the
    old head leaves a required check absent, which blocks the PR with
    nothing pointing at why. See [merge-gate.md](../../docs/merge-gate.md).
  - **`done`** — finish (commits stay on the storage branch).

## Convergence & bounding

- **Done-oracle:** the run converges when the campaign reports `branch_clean`
  (a fresh re-review of the branch diff finds no remaining real issue), the
  deterministic gate is green, **and** the in-loop `review` came back
  `clean`.
- **`max_passes` cap:** the single declared continuation loop
  (`campaign → verify_build → verify_run → review → gate → campaign`) is capped
  by `max_passes` (default 8); on exhaustion it ships what is banked.
- `iterion validate` reports **no undeclared cycle** (one declared loop).

## git is the state (crash-safe / resumable)

There is **no chunk/worklist scratch file** any more — **git is the durable
state**. The campaign commits each fix in stride, so an interrupted /
budget-capped run keeps every committed fix, and a re-dispatch simply re-runs
`campaign`, which reads `git log`, re-diffs the branch, and continues from those
commits. The only out-of-tree scratch is the deterministic gate's
`<scratch_dir>/verify.sh` + `verify.log` (default
`${PROJECT_SCRATCH_DIR}/branch-improve-loop`, engine-resolved off the repo —
never inside the target worktree).

## Right artifact (anti-Goodhart)

`git diff` omits **untracked** files, so a branch that ADDS files would be
reviewed incomplete. The campaign runs `git add -N .` (intent-to-add) BEFORE
diffing so new files show in the branch diff, and commits the uncommitted
working tree after its own build+test passes, staging untracked files
(`git add -A`) so a fix that adds a test/helper actually lands. The
deterministic `verify_build`/`verify_run` gate then re-checks the committed
tree. See
[docs/workflow_authoring_pitfalls.md](../../docs/workflow_authoring_pitfalls.md).

## Base-ref diff semantics (preserved from v1)

The scope is the **merge-base-vs-working-tree** diff, not the commit-only
range: `git diff $(git merge-base base_ref HEAD)`. This captures the branch's
committed changes AND any uncommitted fixes a prior pass of this loop applied
(fixes are committed in stride, but the re-review still measures the working
tree). Reviewing the commit-only `base_ref...HEAD` would hide prior fixes and
the loop could never converge. Default `base_ref` is `main`; pass
`--var base_ref=develop` (or any local ref) for a different integration base.

## Stack- & repo-agnostic

The **base_ref + the diff** define the work. No language / package-manager
literal and no iterion-specific target path appears in any var default, command
body, or schema — `campaign` and `verify_build` are adaptive agents that read
whatever repo they are pointed at (CLAUDE.md "Catalog bots are repo-agnostic" +
"Universal code bots"). The `verify_build`/`verify_run` gate stays deterministic
while remaining universal: the agent writes the repo's own build/test into
`<scratch_dir>/verify.sh`, the tool node runs it and gates on the exit code.

## Vars

| Var | Default | Description |
|---|---|---|
| `workspace_dir` | `${PROJECT_DIR}` | Repo to review (the run's worktree). |
| `base_ref` | `main` | Branch/ref to diff against; scope is `git diff $(git merge-base base_ref HEAD)` (merge-base vs working tree). |
| `pilot` | `end` | Where the human sits in the loop. `end` runs to completion and the human reviews the result; `middle` presents the plan — which findings it intends to fix, which to contest and why — and **waits for the operator to arbitrate before any invasive edit**. Both are first-class; `middle` is how you delegate the work while keeping the arbitration. Rendered as a top-level studio launch field alongside `base_ref`. |
| `scope_notes` | `""` | Free-form extra context for the campaign agent. |
| `prior_review` | `""` | Findings to verify and fix before continuing the campaign's own review. The webhook path seeds this automatically when `/billy` is invoked on a PR that Revi already reviewed; Billy rechecks every finding against the current diff rather than trusting a stale verdict. |
| `baseline` | `""` | **G5** — known pre-existing failures / flaky tests the campaign must SKIP (empty = it establishes the baseline once cheaply against `base_ref`). |
| `max_passes` | `8` | Hard cap on continuation passes — the convergence backstop; sizes the declared loop. |
| `open_mr` / `mr_branch` / `mr_base` / `source_issue_ref` | off | Opt-in PR path shipping the series of per-pass commits (`mr_base` empty = `base_ref`). |
| `push_branch` | `""` | PR-context push-back: the existing forge branch (the PR's source branch) the run's commits belong to. Set with `open_mr=false` → `push_back_tool` pushes HEAD onto it so fixes land ON the PR. Empty = commits stay on the local storage branch. |
| `pr_url` | `""` | The PR Billy is hardening, as a forge URL. When set, `publish_verdict` posts Billy's review verdict as a comment ON that PR. Empty = skip. |
| `gate_enabled` / `gate_context` | `true` / `revi/review` | Merge gate: `publish_verdict` also posts a commit status under `gate_context` on the PR head this run produced. The context is the check NAME — a required check applies to every PR, so a repo where several bots gate different PRs gives them ONE shared context, pinned per repo on the integration `launch_vars`. |
| `forge_publish_url` / `forge_publish_token` | `""` | The deterministic forge-publish grant, **injected by the iterion server** at launch when the run's team has a forge connection covering `pr_url`'s repo. The run itself never holds a posting credential. |
| `scratch_dir` | `${PROJECT_SCRATCH_DIR}/branch-improve-loop` | Out-of-tree working files (the gate's `verify.sh` / `verify.log` only — git is the state). |

## Run

```bash
iterion run bots/branch-improve-loop/main.bot \
  --var workspace_dir=/path/to/repo \
  --var base_ref=main
```

See [main.bot](main.bot) for the full DSL.

## Plan phase (cross-model pair review, ADR-091)

`plan_review: auto` resolves at launch from the run's credentials: when a
SECOND model family is available, the diff triage is authored (claude,
read-only), critiqued by a cross-family peer (`claw` +
`openai/gpt-5.6-sol` by default), and revised by the SAME author session
before the campaign fixes; otherwise the phase is bypassed whole (the v2
shape, unchanged). `plan_review_policy` picks the mid-run
peer-unavailability behaviour: `skip` (default — the reviewer's
`action: skip` route: continue unreviewed, loudly stamped; the peer is
an optional enrichment and must never block the campaign — Anthropic
alone always suffices) or `wait` (the run parks failed_resumable, the
usage-window retry resumes it — the deliberate-spend posture).
