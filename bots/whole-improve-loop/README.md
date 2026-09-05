# whole_improve_loop (Willy) — companion notes

Companion to the `whole_improve_loop` workflow ([`main.bot`](main.bot)).
This is a **design journal**: what the mechanism is, why it is shaped this
way, and what is still open.

## What it does — one agent, its natural flow, minimal framing

`whole_improve_loop` runs a whole-codebase improvement **campaign** on one
axis. `improvement_prompt` is **THE AXIS** — one determined improvement
applied everywhere it applies, **site by site, verified and committed**.

Examples of an axis:

- `split every source file over 600 lines into cohesive smaller files`
- `converge every hand-rolled retry onto a shared backoff helper`
- `make every public function validate its inputs the same way`
- `extract a store-agnostic streaming package`

The mechanism is deliberately **minimal**: give ONE capable agent a mission +
standing autonomy and let it work in its natural flow — the way a productive
human-driven Claude Code session actually looks
([docs/references/productive-session-patterns.md](../../docs/references/productive-session-patterns.md)):
a **living todo list** born from a brief exploration (never frozen upfront
phases), and for each site the repeated unit **locate → smallest change →
build → test → COMMIT**, a few edits per commit, validation *before* the
commit, committing each site **as it finishes** (never batch).

### Why v2 replaced the v1 axis-sweep

v1 (ADR-057) was an axis-driven work-list **sweep**: a ~16-node graph
(`enumerate → next_item → transform → verify → review → commit → advance →
re_enumerate`) plus a persisted `worklist.json` + cursor. Two things broke:

- **It over-framed the work.** The rigid multi-node graph fought the agent's
  native productivity — the productive-session data is blunt: *"the deficit is
  framing, not capability"*, *"once framed, a campaign runs itself"*. A capable
  agent works better as one flow than as an assembly line of single-step nodes.
- **Its blocking upfront `enumerate` timed out on large repos.** An exhaustive
  whole-repo scan *before any work* is exactly the wrong opening; the human
  pattern explores briefly, then starts committing.

v2 keeps what the data says matters — the verified per-site commit cadence
(G8), the deterministic build/test gate (rule 8), the baseline (G5), the
termination contract (G2) — and drops the graph machinery around it.

## The graph

```
campaign ──▶ verify_build ──▶ verify_run ──▶ gate
   ▲   (one adaptive agent:   (writes         (deterministic
   │    axis + standing        <scratch>/      build/test gate)
   │    autonomy, commits       verify.sh)          │
   │    each site in stride)                        │
   │                                                │
   │  (not converged: RED → fix / green but more    │
   └────────────── work → next pass) ◀──────────────┤
                                                     │  (converged:
                                                     ▼   green ∧ axis_complete)
                                              mr_gate ──▶ (finalize_mr) ──▶ done
```

(The diagram elides three deterministic pre-flight probes and the plan
phase: `workspace_probe` is the entry — see "Precondition" below;
`verify_probe` sits between `campaign` and `verify_build` — it reuses a
valid `verify.sh` on passes 2+, skipping the LLM `verify_build`; and
`forge_auth_probe` sits between `mr_gate` and `finalize_mr` — a ~100ms
credential check so the graph only pays the `finalize_mr` agent when a
push can actually happen.)

- **`campaign`** (adaptive, claude_code, whole-repo, full tools) is the whole
  engine: it reads `git log`, builds a living todo list from a brief
  exploration, and applies the axis one site at a time — locate → smallest
  change → build → test → **commit** (`git add -A` incl. untracked, semantic
  message) — until the pass has applied the axis everywhere it can. Before
  reporting it performs two advisory quality checks: **fit** (the change solves
  the axis's real intent, not only its wording) and **rot** (no needless
  duplicate helper, one-caller abstraction, or parallel mechanism was left
  behind). The deterministic gate still decides completion. The campaign emits
  a **termination contract** (`axis_complete`, `commits_this_pass`,
  `sites_remaining`, …). On an ambiguous or self-picked axis it posts a
  non-blocking **teach-back** (`ask_user_async`: the axis restated + the
  assumptions it proceeds on) and keeps working — answers land mid-run in its
  message queue; the blocking `ask_user` stays for genuine hard stops (kept
  rare).
- **`verify_build` → `verify_run`** is the **deterministic, stack-agnostic**
  build/test gate: an adaptive agent reads the `verify-build` skill and writes
  the repo's real build+test into `<scratch_dir>/verify.sh`; a tool node
  re-runs it and gates on the **real exit code** (no LLM judgment). This is
  both the tight real-feedback loop AND the anti-Goodhart truth oracle — the
  agent can't self-certify. `verify_build` does **not** fix code.
- **`gate`** (deterministic compute) decides continuation: `converged =`
  the gate is **green** AND the campaign reported **`axis_complete`**. Not
  converged → back to `campaign`; a RED gate carries the failure log so the
  agent fixes what it broke, a green-but-more-work pass carries an empty log so
  the agent simply continues.
- **`mr_gate` → `finalize_mr`** is the opt-in PR path shipping the series of
  per-pass commits (`open_mr`).

## Convergence & bounding

- **Done-oracle:** the run converges when the campaign reports `axis_complete`
  (a fresh re-scan finds no remaining site) **and** the deterministic gate is
  green.
- **`max_passes` cap:** the single declared continuation loop
  (`campaign → verify_build → verify_run → gate → campaign`) is capped by
  `max_passes` (default 8); on exhaustion it ships what is banked.
- `iterion validate` reports **no undeclared cycle** (one declared loop).

## git is the state (crash-safe / resumable)

There is **no worklist/cursor scratch file** any more — **git is the durable
state**. The campaign commits each site in stride, so an interrupted /
budget-capped run keeps every committed site, and a re-dispatch simply re-runs
`campaign`, which reads `git log` and continues from those commits. The only
out-of-tree scratch is the deterministic gate's `<scratch_dir>/verify.sh` +
`verify.log` (default `${PROJECT_SCRATCH_DIR}/whole-improve-loop`,
engine-resolved off the repo — never inside the target worktree).

## Right artifact (anti-Goodhart)

The campaign commits the **uncommitted working tree** after its own build+test
passes, staging untracked files (`git add -A`) so a change that **adds** files
actually lands (`git diff HEAD` omits untracked). The deterministic
`verify_build`/`verify_run` gate then re-checks the committed tree. See
[docs/workflow_authoring_pitfalls.md](../../docs/workflow_authoring_pitfalls.md).

## Stack- & repo-agnostic

The **axis + the repo** define the work. No language / package-manager literal
and no iterion-specific target path appears in any var default, command body,
or schema — `campaign` and `verify_build` are adaptive agents that read
whatever repo they are pointed at (CLAUDE.md "Catalog bots are repo-agnostic" +
"Universal code bots"). The `verify_build`/`verify_run` gate stays deterministic
while remaining universal: the agent writes the repo's own build/test into
`<scratch_dir>/verify.sh`, the tool node runs it and gates on the exit code.

## Vars

| Var | Meaning |
|---|---|
| `improvement_prompt` | **THE AXIS**. Empty = the campaign picks the single highest-value cross-cutting improvement it can name for the repo. |
| `scope_globs` | Path scope (the WHERE): comma/space-separated fnmatch globs; empty = whole workspace. |
| `scope_notes` | Free-form extra context for the campaign agent. |
| `baseline` | **G5** — known pre-existing failures / flaky tests the campaign must SKIP (empty = it establishes the baseline once cheaply, then skips what predates its work). |
| `max_passes` | Hard cap on continuation passes (default 8) — the convergence backstop; sizes the declared loop. |
| `open_mr` / `mr_branch` / `mr_base` / `source_issue_ref` | Opt-in PR path shipping the series of per-pass commits. |
| `workspace_dir` | Target repo (defaults to `${PROJECT_DIR}` → the run's worktree). |
| `scratch_dir` | Out-of-tree working files (the gate's `verify.sh` / `verify.log` only — git is the state). Default `${PROJECT_SCRATCH_DIR}/whole-improve-loop`, engine-resolved off the repo — never inside the target worktree. |

## Presets

The `presets/` frame the axis: `code-quality`, `improve-quality` (SRE),
`production-ready`, `rgaa` (accessibility), `rgpd` (GDPR). Each sets
`improvement_prompt` (and skills) so the campaign works the sites that preset
targets. They compose with the campaign unchanged — a preset is just a
pre-filled axis.

## Large axes span passes (and sometimes runs)

A big axis on a large repo may exceed one run's `max_passes` / budget. Such a
run makes **bounded progress**, **commits every site it lands**, and either
hits the `max_passes` cap (ships what is banked) or exhausts the budget.
Because git is the state, a re-dispatch re-runs the campaign, which reads
`git log` and continues banking committed sites. Raise `max_passes` (with the
budget) to finish a larger axis in one run.

## Plan phase (cross-model pair review, ADR-091)

The sweep plan is AUTHORED by default on every deployment (claude,
read-only); `plan_phase: off` is the explicit opt-out (plan in stride, the
v2 shape). `plan_review: auto` resolves at launch from the run's
credentials and gates ONLY the peer review: when a SECOND model family is
available, the plan is critiqued by a cross-family peer (`claw` +
`openai/gpt-5.6-sol` by default) and revised by the SAME author session
before the campaign sweeps; otherwise the campaign receives the author's
plan stamped as unreviewed (`plan_provenance`). `plan_review_policy`
picks the mid-run peer-unavailability behaviour: `skip` (default — the
reviewer's `action: skip` route: continue unreviewed, loudly stamped) or
`wait` (the run parks failed_resumable, the usage-window retry resumes
it — the deliberate-spend posture).

## Precondition (`workspace_probe`)

The run's entry is a deterministic tool node (~100ms, no LLM): a launch
whose `workspace_dir` is absent or not a git repository fails typed
(`WORKSPACE_NOT_A_REPO` on the node's output and in the tool log) before
any LLM node spends.

## Persy (perseverance coach)

A `supervisor persy:` block watches the `campaign` node
(docs/supervisors.md): it pushes back on premature "impossible" verdicts,
expedient shortcuts, failure loops and unbanked state under budget
pressure. `--supervisors off` disables it per run.
