---
name: iterion-bot-architecture
description: How to DESIGN a .bot, as opposed to how to spell one — node responsibilities, closed contracts, the PASS/RETRY/BLOCKED verdict shape, why a retry must re-enter the producing node, subbot boundaries, idempotency, parallelism markers and what they do not guarantee, budgets, and the proof categories a PASS must carry. Load in design posture alongside iterion-dsl-authoring, before proposing any graph.
---

# Designing a `.bot`

`iterion-dsl-authoring` gets a workflow to compile. This gets it to be
*right*. They are different failures: a graph that passes `iterion
validate` can still route from prose, retry into the wrong node, or
present a hash as proof that the work is correct.

## Say which layer you are speaking from

Three layers, and conflating them is the most damaging thing you can do
in this posture, because the operator acts on what you say:

| Layer | Means |
|---|---|
| **DSL** | The compiler or `iterion validate` enforces it. You can promise this. |
| **Architecture** | A design requirement. The graph still compiles if it is violated. Say so. |
| **Project profile** | Models, retry caps, budgets, verifiers. The operator's repo decides; you do not. |

Never tell an operator "validate will catch that" unless it will. When
a rule is architectural, name it as a rule you are applying and say
that nothing in the toolchain enforces it.

### The operator's own standard wins

A repo may carry its own authoring standard, and it outranks this
skill. Look for it before designing:

- a `authoring_standard: <publisher>/<name>/<version>` declaration in
  the workflow or the project's docs;
- the `ITERION_AUTHORING_STANDARD_PATH` environment variable, which
  points at the standard's file — read it with `Read`, it is usually
  installed **outside** the repo;
- otherwise, the repo's own `CLAUDE.md` / `AGENTS.md` conventions.

When you find one, read it and follow it, and say which one you applied.
When you do not, this skill is the floor. Never hardcode a path to a
standard into a workflow you write — a project resolves it, it does not
commit it.

## Node kinds are responsibilities, not just types

`agent` and `judge` accept the same property surface, so the compiler
will happily let an agent grade itself or a judge rewrite the file it is
reviewing. Treat them as roles:

| Kind | Owns | Must not |
|---|---|---|
| `agent` | Producing or transforming an artifact | Be the only proof its own work is correct |
| `judge` | Evaluating an artifact that already exists | Mutate it, or trigger new production |
| `human` | A durable decision or outside input | Hide a technical routing target inside a yes/no form |
| `tool` | One deterministic effect | Shell out to an agent CLI or to `iterion run` |
| `compute` | Pure expressions | Do I/O or call a model |
| `router` | Fan-out and conditions | Carry `await` (the DSL refuses it) |
| `subbot` | A nested run with its own contract | Hide a second graph inside a prompt |

One named responsibility per node. If an agent also has to judge, or to
call a vendor API, that is two nodes.

**An agent CLI inside a `tool` node is the anti-pattern to watch for.**
It reads as deterministic in the graph and is not. `iterion validate`
does not catch it.

## Contracts

- Any output a condition, a router, a writer, or another node
  **consumes** needs a closed schema. Free text is fine only when the
  workflow never interprets it — a conversational or documentary bot.
- A node with a **side effect and no textual output** needs a
  deterministic post-check behind it: file present, hash, exit code,
  dimensions. Otherwise the graph believes an effect it never observed.
- Machine-checkable decisions — hash, schema, equality, allowlist —
  belong in `compute` or `tool`. Let the model choose among options that
  are already valid.
- Every guarded edge needs an exhaustive complement or an `else`.

## First: which loop shape are you in

Two regimes, and picking the wrong one is the most expensive mistake in
this posture because everything downstream follows from it.

**Campaign + deterministic gate.** For an improvement or review loop over
a repo — the whole shipped iterion fleet since ADR-058 v2. ONE agent per
pass, a deterministic verify gate (a real exit code, never an LLM
judgment), a machine-checkable termination flag, and a single bounded
`continuation_loop(N)`. The agent commits each unit in stride, so
exhausting the loop ships what is banked. There is **no LLM judge in the
loop at all**: oscillation is structurally absent because each pass is
one fresh context re-reading `git log`, with no reviewer/fixer relay to
re-litigate. Recommend this by default for "keep improving X until it is
good".

**Producer + judge, with a retry edge.** For evaluating a *specific
artifact* against criteria a command cannot express — a design, a
document, a plan, a piece of prose. Here the rest of this section
applies, and applies strictly: the relay is exactly what oscillates when
it is built carelessly.

Both regimes diagnose the same failure — a fresh-context corrector
re-derives instead of amending — and answer it differently: the campaign
removes the corrector, the producer/judge pair keeps it and makes the
retry re-enter the producer's own conversation. What neither tolerates is
a *second* agent correcting the first one's work from a blank context.

## Verdicts, and the node a retry goes back to

Give an evaluation a structured verdict, and route from the **field**,
never from the findings prose:

```
verdict:         PASS | RETRY | BLOCKED
rework_target:   <the producer inspected> | none
blocker_kind:    <workflow-defined enum>
recovery_target: <an acquisition capability> | none
```

| Verdict | Next |
|---|---|
| PASS | continue |
| RETRY | a bounded loop back to the producer that was inspected |
| BLOCKED | the node named by `recovery_target`, or `fail` |

RETRY names an observable defect. BLOCKED means something is missing —
information, a capability — and the answer is to acquire it, not to
invent scope around it.

### No correction twin

**A RETRY edge returns to the same node that produced the artifact.**
Declaring a second node with the same prompt, tools and contract to
serve the retry path duplicates a responsibility; it is not a routing
decision. It costs three things:

- **context** — a twin opens a fresh conversation, so the producer's own
  reasoning is discarded and it re-derives the artifact instead of
  amending it;
- **input** — the retry brief re-renders material the producer already
  had, with the verdict buried at the end;
- **fidelity** — a producer that cannot see what it wrote cannot honour
  "change only this". Every retry becomes a rewrite, and the next pass
  judges a different artifact.

The twin usually exists because the retry brief differs. Build that
brief in the deterministic node in front of the producer, which is where
the difference belongs.

The re-entered producer should keep its conversation across visits
(`session: inherit_if_available`). First visit gets the full brief;
every later visit gets a **delta** — the verdict, the human note, the
contract refusal — and nothing else.

**A delta still names its paths.** Session resumption can fail silently
and the runtime may fall back to a fresh conversation without telling
the graph, so the delta says where the artifact and its inputs live. A
context-less producer must be able to re-read rather than correct blind.

Two consequences, and in iterion both are enforced by the compiler
rather than left to discipline — say so, it changes the advice:

- A node that keeps its conversation **cannot** declare a fallback that
  changes backend. `C176` refuses it outright (session continuity has no
  cross-backend meaning). So a node that must survive a fall-through
  without losing its thread builds its ladder from different
  **providers inside one backend** — not from different backends. Read
  "prefer a decorrelated fallback" as *another provider*, which is what
  actually decorrelates an outage; "another backend" is the reading that
  will not compile.
- A retry loop **inside** a `fan_out_each` / `fan_out_all` body is
  `C244`, because a parallel branch has no local loop counter. Per-item
  retry is a `subbot`, or a loop wrapped around the router from the
  join. Design the per-item retry that way from the start.

**A re-emitted identical artifact is a failed retry, not an idempotent
success.** A contract that answers "already applied" on a retry path
re-presents the refused artifact to the judge and the human gate, and
the refusal is silently lost.

Where several sources can trigger a retry — a contract refusal, a judge,
a human gate — converge them on **one** deterministic node that merges
them in a documented priority order. But give each source its **own**
named cycle and cap: a shared counter lets a chatty judge eat the
human's budget.

## How capable the judge should be

Choose from the cost of bad production, of a **false PASS**, and of a
**false RETRY** — not from a habit. Default the judge to the same
routing class as the artifact's creator; there is no `high_judge`
profile to invent.

Two places to depart from that default, both deliberate:

- **A false PASS is the expensive one** — security, compliance, an
  irreversible migration. Then the judge may be the most capable node in
  the graph, because a retry is cheap next to what a wrong approval
  costs.
- **The mechanical tail.** Once the expensive phases have externalised
  the context — plan approved, work-list enumerated, verify script
  written — the remaining units are bounded and a cheaper model performs
  equivalently. Spend the strong model on discovery, design and
  judgment; pin `model:` per node and downgrade the tail.

These pull in opposite directions on purpose. The question they both
answer is which node's mistake is unaffordable, and that is a property of
the artifact, not of the node's kind.

## Human gates

A refusal does not automatically mean "rerun the producer". Let the
workflow name the states it allows — approve, revise, blocked, reject,
cancel — and take the technical target from a **structured field** or a
deterministic rule. A model is the classifier of last resort, and only
with a deterministic validation behind it.

## Subbot boundaries

A `subbot` is a reusable capability, not a way to move a node out of its
parent graph.

- **At least two executable nodes.** `agent`, `judge`, `human`, `tool`,
  `compute`, `router` and nested `subbot` count; schemas, prompts, the
  workflow declaration, `done` and `fail` do not. One executable node
  stays inline in the caller. `iterion validate` does **not** enforce
  this.
- **Declared inputs, and a clear deliverable.** A file counts only when
  the output identifies it unambiguously and carries the receipt, hash
  or status its contract requires. Completion prose is not an output.
- **Independent of the caller.** No sibling output, no caller node name,
  no undeclared parent path, no knowledge of what runs before or after.
- **Reusable unchanged.** A caller may map different values in; it may
  not require edits to the subbot's prompts, graph or paths. Having one
  caller today is fine; caller-specific assumptions are not.

Name it after the capability it owns, not the phase that first called it.

## Parallelism markers, and what they do not promise

| Marker | Guarantees | Does **not** guarantee |
|---|---|---|
| `readonly: true` | the checkout is not mutated | no MCP / API / external effect |
| `isolated: true` | the contract promises a private store or namespace | an OS sandbox |
| `parallel_safe: true` | `fan_out_each` replays write disjoint item-keyed outputs | anything if the keys overlap |
| `await: wait_all` | a convergence barrier | a unique writer |

Run branches in parallel only when the writes are disjoint **and**
declared. A unique writer is required only where branches converge on
the *same shared mutable resource* — acquire it through `resources:`, a
lease or a semaphore. Never raise `max_parallel_branches` to paper over
a write conflict.

## External effects

One graph responsibility per external effect, and it leaves a redacted
proof. The idempotency key should fold in the contract version, the
input hashes, the tool or model version when it changes the result, a
freshness window when the answer can go stale, and an explicit nonce
when a rerun is wanted on purpose.

The same key must not produce two effects — a deliberate rerun needs a
new key. Record the attempt **before** the first effect of a cacheable
unit, so a crash retries a candidate instead of promoting a stale
success. Writers are atomic: a crash must not leave a half-written
artifact as the current state.

## Budgets

Every workflow declares numeric caps, and they are dimensions, not a
sum — never add a dollar figure, a branch count and a retry count
together:

```
max cost      = Σ (max invocations of a node × its estimated unit cost)
max duration  = critical path + bounded waits + retry slack
max parallel  = branches concurrently live
```

Fallback routes count as extra invocations in the worst case. Each paid
loop declares its passes, its ceiling, and what happens when it is
exhausted — fail, human gate, or a documented skip. A subbot has its own
budget. `unbounded` only where the case is named in a comment.

Remember from `iterion-dsl-authoring`: the budget is **cumulative over
the run's whole life**, so on a looping or conversational bot these are
session caps, not per-turn caps.

## Proofs

A PASS carries the proof categories its contract asks for, and they are
not interchangeable:

| Proof | Attests |
|---|---|
| Provenance / integrity | identity and origin; nothing changed undetected (hash, signature, content id) |
| Execution | something actually ran (exit code, log, attested response) |
| Acceptance | the functional criteria hold (test, postcondition, a durable human observation) |

**A hash does not prove the artifact is semantically correct.** Which
verifier is authoritative for which artifact is the project's call — an
interactive aid never replaces the declared verifier.

## Before you say a draft is ready

- [ ] `iterion validate` clean — and say when you have not been able to run it
- [ ] one named responsibility per node; no agent CLI inside a `tool`
- [ ] graph-consumed output has a closed schema; an effect with no text has a post-check
- [ ] guarded edges exhaustive or `else`
- [ ] RETRY re-enters the inspected producer; no correction twin; the producer keeps its conversation and gets a delta that names its paths
- [ ] an identical re-emission counts as a failed retry
- [ ] retry sources merge in one deterministic node, each keeping its own named cycle and cap
- [ ] BLOCKED routes through a structured `recovery_target`, never from prose
- [ ] subbots: ≥2 executable nodes, declared inputs, real deliverable, caller-independent
- [ ] parallelism declared; unique writer only where a shared mutable resource converges
- [ ] a fallback meets the contract's minimum capabilities, and no session-keeping node changes backend on fall-through
- [ ] numeric budgets in every dimension; idempotency keys include freshness; writers atomic
- [ ] the PASS carries the proof categories its contract requires
