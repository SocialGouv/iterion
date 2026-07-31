# Why Iterion?

Organisations are about to run **fleets of AI agents** — reviewing code, shipping
features, auditing security, triaging issues, keeping docs honest. Today that work
is driven by hand: a chat window, a one-off script, a prompt copy-pasted between
people. There is no shared way to budget it, isolate it, audit it, or reproduce it
— and no way to know whether an agent will do the job well the *next* time, not
just this once.

That is the gap Kubernetes closed for containers, and it is the gap Iterion closes
for agents: **a declarative control plane where agent work is a readable file,
every run is an operated and auditable execution graph, and quality is something
you measure and converge on — not something you hope for.**

You describe *what* the agents should do — plan, implement, review, ask a human,
gate on tests. Iterion handles *how*: scheduling branches in parallel, enforcing
budgets, isolating writes in a sandbox, checkpointing long runs, and routing
between nodes — locally, in CI, or across a multi-tenant cloud.

## Why now

Late 2025 / early 2026, frontier models crossed a threshold: structured pipelines
(plan → implement → review → fix) started producing output worth coming back to
after lunch. *"Automate this"* became a viable thought rather than a wishful one.
When a single agent can hold a real task for an hour, the bottleneck stops being
the model and becomes everything around it — budget, isolation, resumption,
review, and proof that it converged. Iterion is the engine we built to take that
seriously.

## A pattern that led to Iterion

- **Plan** — a written contract from an LLM: goals, files, constraints.
- **Implement** — a tool-using agent executes with high autonomy.
- **/simplify** — a clarity pass: dead code out, reuse in.
- **Review-fix** — reviewer critiques, fixer addresses, loop until satisfied. For
  critical work, raise the bar to consecutive approvals across alternating model
  families.
- **Light human finalization** — real tests, a diagonal read.

That shape motivated the engine, but it is not the fleet's only or even default
topology. Productive-session evidence showed that capable agents often do better
with *less* relay framing: the maintained improvement bots now wrap one adaptive
campaign agent in the deterministic verification, scope, and termination gates
that fit each bot (with scans or manifests only where the domain needs them).
Specialized multi-node graphs still earn their place when a real boundary — human
authority, parallel evidence, provider diversity, or a deterministic action —
deserves its own node. Iterion supports both, and lets you move between them
without rewrites.

## What Iterion lets you do

- **Compose pipelines** — chain agents, judges, routers, humans, tools, and
  compute nodes into one graph in a single `.bot` document. Parallel branches
  converge on a downstream node via `await: wait_all` / `best_effort` — no
  separate join node to wire.
- **Make agents work together** — multi-agent, multi-backend, multi-model inside
  one workflow. Mixing model families is a one-line change, so a plan from one
  provider can be reviewed by another.
- **Operate long autonomous work** — run on demand, on a schedule, in CI, or
  unattended for hours, with shared budgets and per-run sandboxes. A 90-minute run
  that dies at minute 80 resumes from minute 75.
- **Formalise the method that worked** — the recipe becomes a versioned, diffable,
  shareable file instead of tribal knowledge in someone's head.
- **Evolve fluidly** — add a node, swap a backend, fork a variant. The DSL is small
  enough that fluency takes an afternoon.
- **Ship it as a platform** — inspect and steer live runs, answer human gates,
  preserve or merge worktrees, dispatch from issues and pull requests, and hand the
  same workload to a multi-tenant control plane with tenancy, quotas, secrets, and
  audit.

## Measure with the asymptote

Most agent tooling shows you one run and lets you believe it. Iterion asks a harder
question: run the same task ten times against the same workflow and plot the
quality. The curve climbs, then stabilises — **the asymptote**. It tells you
whether the pipeline converges at all, what ceiling it converges to, and how much
variance a single run should carry. `iterion bench asymptote` produces it for any
workflow on any corpus, so "is this good enough to trust unattended?" becomes a
number, not a vibe.

The asymptote is detected by a judge — its verdict prompt is the load-bearing
piece. Treat every new judge as a multi-draft exercise.

## Why a dedicated engine

Shell scripts can chain commands but can't checkpoint long autonomous runs, enforce
shared budgets, serialize workspace writers, or produce a typed replay stream.
Python frameworks (LangGraph, CrewAI) fit many teams; Iterion picks differently — a
small `.bot` document anyone can read, diff, and re-run without an interpreter,
backed by worktree/sandbox adapters and local or cloud control planes. Two recipe
variants run side-by-side without touching code, and the same file that runs on
your laptop runs unchanged on the cloud runner.

See the [current as-built state](current-state.md), then [install](install.md).
