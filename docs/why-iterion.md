[← Iterion](../README.md)

# Why Iterion?

Iterion is a workflow engine for **plugging AI pipelines together** — making LLMs talk to each other, automating processes, formalising the methods you already use, and evolving all of that fluidly as the work changes.

## Origin

Late 2025 / early 2026, frontier models crossed a threshold: structured pipelines (plan → implement → review → fix) started producing output worth coming back to after lunch. *"Automate this"* became a viable thought rather than a wishful one. Iterion is the engine we built to take it seriously.

## A pattern that's worked for us (one of many)

- **Plan** — written contract from an LLM: goals, files, constraints.
- **Implement** — a tool-using agent (Claude Code, Codex, …) executes with high autonomy.
- **/simplify** — a clarity pass: dead code out, reuse in.
- **Review-fix** — reviewer critiques, fixer addresses, loop until satisfied. For critical or complex work, raise the bar to consecutive approvals across alternating model families.
- **Light human finalization** — real tests, diagonal read.

The same shape works for fixing existing code, and stretches to multi-hour autonomous sessions that produce something near-end-to-end. It's *one* pattern; Iterion runs whichever you arrive at.

## What Iterion lets you do

- **Compose pipelines** — chain agents, judges, routers, joins, humans into one graph in a single `.iter` document.
- **Make LLMs talk to each other** — multi-agent, multi-backend, multi-model, inside one workflow. Mixing families is a one-line change.
- **Automate processes** — run on demand, on schedule, in CI, or unattended for hours, with budgets and per-run sandboxes. A 90-minute run that dies at minute 80 resumes from minute 75.
- **Formalise the method that worked for you** — the recipe becomes a versioned, diffable, shareable file.
- **Evolve fluidly** — add a node, swap a backend, fork a variant. The DSL is small enough that fluency takes an afternoon.

## Measure with the asymptote

Run the same task ten times against the same workflow. Plot quality. The curve climbs, then stabilises — **the asymptote**. It tells you whether the pipeline converges, what ceiling it converges to, and how much variance to expect on a single run. `iterion bench asymptote` produces it for any workflow on any corpus.

The asymptote is detected by the judge — its verdict prompt is the load-bearing piece. Treat every new judge as a multi-draft exercise.

## Why a dedicated engine

Shell scripts can chain commands but can't checkpoint long autonomous runs, sandbox each agent, or produce a replayable log. Python frameworks (LangGraph, CrewAI) fit many teams; Iterion picks differently — a small `.iter` document anyone can read, diff, and re-run without an interpreter. Two recipe variants run side-by-side without touching code.

Get started: [install.md](install.md).
