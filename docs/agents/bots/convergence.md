# Improvement loops converge to an asymptote

Read it when writing or reviewing a loop, campaign or reviewer bot — the asymptote rule, the stop criteria, and the shipped examples.

## Authoring `.bot` workflows that touch real code

**Before writing or amending any `.bot` workflow that has the power to
commit code, read [docs/workflow_authoring_pitfalls.md](../../workflow_authoring_pitfalls.md).**
It captures hard-won lessons about Goodhart's law in workflow design,
the façade pattern that LLM agents reach for when goals are
under-specified, and concrete rules for prompts, scanners, and judges
that resist metric-gaming. Skipping it has a real cost — the
goai → claw-code-go migration ran for 3 hours and produced a
96%-parity-reported façade because these lessons weren't yet codified.
Its "what works" companion is
[docs/references/productive-session-patterns.md](../../references/productive-session-patterns.md) —
the measured shape of productive operator sessions (commit cadence,
work-list discipline, termination contracts) distilled into authoring
rules; ADR-055/ADR-057 encode its core finding. External cross-check:
[docs/references/external-methodologies.md](../../references/external-methodologies.md)
maps two independent 2026 methodology papers (IACDM, AI-DLC) onto
iterion — what they validate, the imported rules (teach-back, cost-tier
switch, scope inventory, …, folded into the pitfalls doc), and what was
deliberately rejected.

### Improvement loops must converge to an asymptote

Every improvement/review loop must **converge to an asymptote** — settle
into a stable approved state and stop — not oscillate. A slight, very
occasional oscillation is acceptable; it must be the rare exception.
**The rule is the asymptote.** (`iterion bench asymptote` measures
exactly this — see [docs/asymptote-bench.md](../../asymptote-bench.md).)

**The default mechanism (ADR-058 v2, the whole shipped fleet).** The
flagship loop bots (whole-improve-loop, branch-improve-loop,
feature-dev, feature-gap-fill, test-coverage, e2e-coverage, docs-refresh,
adr-cartograph, secured-renovacy Phase 2) converge through ONE
`campaign` agent + a deterministic gate + a bounded continuation loop:
- the **deterministic verify gate** (`verify_build` writes the repo's
  real build+test into an out-of-tree `verify.sh`; the `verify_run`
  tool re-runs it on the REAL exit code — never an LLM judgment,
  ADR-044) is the truth oracle (docs-refresh is the exception: a
  docs-only campaign can't break the build, so it dropped the verify
  gate and converges on `scope_ok ∧ docs_aligned` alone);
- the **termination contract** (a machine-checkable flag —
  `axis_complete` / `feature_complete` / `docs_aligned` / … — plus
  `commits_this_pass` and a remaining-work note) is the done-oracle,
  with the honesty clause "under-reporting only costs a pass,
  over-reporting lands you right back here";
- **`gate.converged = <flag> ∧ gates green`** closes the single
  declared `continuation_loop(max_passes)`; exhaustion ships what is
  banked (the campaign commits each unit in stride — git is the state);
- oscillation is structurally absent: one context, fresh each pass,
  re-reads `git log` — there is no reviewer/fixer relay left to
  re-litigate.

**If you author a NEW cross-family reviewer loop** (an optional
amplification per ADR-058 — no catalog bot ships one any more),
preserve the historical convergence mechanisms: a `streak_check` gating
exit on N consecutive cross-family approvals with low-confidence
rejections non-blocking; `prior_pushback` / `previous_scanned_areas`
fed back with "do NOT re-raise without new evidence";
`loop.<name>.previous_output` for monotonic verdicts; bounded
`max_iterations` as the backstop, not the design goal.

**Mono/dual review topology (ADR-052) — MONO IS THE DEFAULT.**
[pkg/reviewtopology](../../../pkg/reviewtopology/resolve.go) resolves
`review_mode` (`auto|mono|dual`) + `mono_family` at LAUNCH and injects
them on every surface (CLI `iterion run --review-mode`, studio/API,
dispatcher bot_arg) — but ONLY into bots that declare a `review_mode`
var (`InjectIfDeclared`). **`auto` resolves to `mono`**, even when both
families are available: dual costs a full reviewer pass per family on
EVERY run, and with the merge gate wired every push re-reviews, so
cross-family confirmation is a deliberate spend (`--var review_mode=dual`)
rather than something a host opts into by having two providers configured.
The catalog bots that still run family reviewers — `review-pr` (Revi) and
`evolve` — declare the vars and gate their fan-out behind a `condition`
router (never `round_robin`, and never `when` guards on a `fan_out_all`
router's own edges: both collect every edge without evaluating the
condition). Any new reviewer-loop bot adopts the topology the same way.
The machinery stays guarded non-vacuously by
`e2e/review_topology_test.go` + `e2e/testdata/review_topology_mini.bot`.

**Right-artifact discipline** (now encoded in the campaign contracts,
still binding for anything that diffs code): judge the WORKING TREE
(`git diff HEAD`, or `git diff <base>` for branch/run scopes), never
`HEAD^...HEAD`; and make untracked files visible before diffing (`git
add -N -- ':/' $ITERION_TREE_NOISE`, or `git add -A -- ':/' $ITERION_TREE_NOISE` before each in-stride commit — a change that
ADDS files is otherwise invisible to the diff). Both failure modes were
observed live in the v1 reviewer loops (a reviewer concluding "the
feature isn't implemented" and looping forever — see
[docs/bot-runs/feature-dev.md](../../bot-runs/feature-dev.md)); the v2
contracts bake the `git add -A -- ':/' $ITERION_TREE_NOISE`-then-commit unit in (iterion's
skills mirror is never the run's work; a file under `.claude/` that IS the
deliverable is staged by name, `git add -- .claude/<path>`), and any new
reviewer you author must anchor the same way.

