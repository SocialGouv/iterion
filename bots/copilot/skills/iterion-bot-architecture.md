---
name: iterion-bot-architecture
description: Complete embedded victor/iterion-bot-authoring/v1 standard for designing .bot graphs — responsibilities, contracts, convergence, advisory review, retries, subbots, effects, idempotency, parallelism, routing, context budgets, proofs, resume, and validation. Load in design posture alongside iterion-dsl-authoring.
---

# Bot authoring standard

**Identifier:** `victor/iterion-bot-authoring/v1`

Personal method for designing Iterion `.bot` graphs — not an official
standard of the Iterion engine. It talks about properties — risk, effect,
contract, capability, proof — not model names, vendors, disciplines, or
project files.

It does not replace the Iterion DSL. A consuming project applies it through
a **local profile** (models, retry caps, budgets, entry points, verifiers)
and SHOULD declare:

```text
authoring_standard: victor/iterion-bot-authoring/v1
```

This complete versioned standard is embedded in the
`iterion-bot-architecture` skill. An `authoring_standard:` declaration is
an identifier for these embedded rules, not a file reference. Copi MUST NOT
search the workspace, the operator's home, or an environment variable for an
external copy. Future additions and good practices are made directly in this
skill so every Copi installation receives the same standard.

A project MAY add a visible local profile for models, retry caps, budgets,
entry points, verifiers, and documented exceptions. That profile supplements
this standard; it does not cause a second authoring-standard document to be
loaded. The standard MUST NOT name or link to a particular project profile.

## RFC 2119 keywords

| Keyword | Meaning |
|---|---|
| **DSL** | Enforced by `iterion validate` (and the compiler). |
| **MUST** | Architectural requirement of this standard. Tools other than the DSL may audit it; a graph can still compile if it violates it. |
| **SHOULD** | Default; a documented, reviewable exception is allowed. |
| **MAY** | Optional. |

---

## 1. Scope and vocabulary

| Term | Meaning |
|---|---|
| Producer | Node that creates or transforms an artifact (`agent`, sometimes `tool`) |
| Judge | Node that evaluates an already produced artifact and gives non-binding semantic advice (`judge`) |
| Human gate | `human` node for a durable decision or external contribution |
| Work unit | Deliverable the workflow commits to finish (task, job, item) |
| Shared mutable resource | File, scene, store, or system only one writer may mutate at a time |
| Idempotency key | Stable identity of a work unit, including freshness (see §6) |
| Execution profile | Model-routing class (capability, context, modalities, cost) |
| Subbot | Independently executable and reusable nested capability with a declared input and a clear output or deliverable |
| Projected peak context | Maximum tokens a node can accumulate before completing one invocation or its bounded resumed session, including prompts, injected inputs, tool results, deltas, and reserved output |

Keep these layers out of the same paragraph:

1. **DSL** — true because the language/runtime says so.
2. **MUST/SHOULD of this standard** — architecture, not the parser.
3. **Project profile** — pins, enums, tools, entry points; out of this file.

---

## 2. Node kinds

**DSL.** `agent` and `judge` accept the same property surface. The compiler
does not stop an agent from evaluating, or a judge from using write tools.

**MUST.** Treat kinds as responsibilities:

| Kind | Role | MUST NOT |
|---|---|---|
| `agent` | Produce | Be the sole proof that its own work is correct |
| `judge` | Evaluate an already produced artifact and advise its producer | Mutate that artifact; trigger new production; hard-filter the artifact on a semantic opinion |
| `human` | Durable decision or external input | Hide a technical target inside a form that is meant to stay binary |
| `tool` | Deterministic effect (CLI, hash, seal, I/O) | Shell out to an agent CLI or `iterion run` |
| `compute` | Pure expressions | I/O or model calls |
| `router` | Fan-out / condition | `await` (**DSL**) |
| `subbot` | Nested run with its own contract | Hide a second graph inside a prompt |

**SHOULD.** One named responsibility per node. If an agent also needs to
judge or call a vendor, split the graph.

**SHOULD.** A `tool` MAY call an **explicitly authorized** external service
when that call is a single responsibility, declares its effects, uses an
idempotency key when possible, and emits a redacted proof. How the project
annotates the provider is a profile convention, not a DSL rule.

Hiding an agent CLI inside a `tool` is an architectural **MUST NOT**,
typically enforced by project audit, not by `iterion validate`.

---

## 3. Input and output contracts

**MUST.** Any output **consumed** by a condition, router, writer, or another
node has a closed schema.

**MAY.** A platform-native opaque handle MAY cross one graph edge as `json`
only when the DSL has no producer type for that handle (for example, Iterion
reserves `file` for human uploads while a deterministic tool emits a native
run-attachment preview descriptor). The producing deterministic node MUST
construct and validate the descriptor from sealed paths, hashes, MIME and
size; the consumer MUST treat it as opaque display/input plumbing. No
condition, router, model prompt, repair scope, or business writer may inspect
fields inside that open value. This is not permission to carry ordinary
domain objects or fan-out collections as `json`.

**MAY.** Output meant only for a human MAY stay free text if the workflow
never interprets it (conversational, exploratory, documentary bots).

**MUST.** A node with a **side effect and no textual output** MUST be
followed by a deterministic post-check that attests the expected effect
(file present, hash, dimensions, exit code).

**SHOULD.** Machine-checkable decisions (hash, schema, equality, allowlist)
live in `compute` or `tool`. The model chooses among already-valid options.

**DSL / MUST.** Every guarded edge has an exhaustive complement or `else`.

### 3.1 Subbot boundary and contract

A `subbot` is a reusable capability boundary, not a way to move a node out of
its parent graph.

**MUST.** A subbot contains at least **two executable nodes**. For this rule,
`agent`, `judge`, `human`, `tool`, `compute`, `router`, and nested `subbot`
declarations count as executable nodes; schemas, prompts, the workflow
declaration, `entry`, `done`, and `fail` do not. A capability that requires
only one executable node stays inline in its caller. A project audit MUST
reject a single-node subbot even when `iterion validate` accepts it.

**MUST.** A subbot reads all variable data through a declared input contract
and, on success, emits a clear deliverable or a closed structured output. A
file or external artifact is a clear deliverable only when the output identifies
it unambiguously and carries the receipt, status, hash, or other proof required
by its contract. Unstructured completion prose alone is not a subbot output.

**MUST.** A subbot is fully independent of its caller. Given its declared
inputs and authorized external capabilities, it can be executed and validated
without access to the parent workflow's node outputs, conversation, implicit
working state, or control-flow history. It MUST NOT depend on a caller node
name, a sibling output, an undeclared parent path, or knowledge of which node
will run before or after it.

**MUST.** A subbot can be reused unchanged by different workflows. Callers MAY
map different values into its input contract, but MUST NOT require edits to the
subbot's prompts, graph, schemas, or internal paths. Having only one current
caller is allowed; caller-specific assumptions are not. The capability MUST
remain independently testable through its public input/output contract.

**SHOULD.** Name a subbot after the capability and deliverable it owns, not the
parent phase or the first workflow that happens to call it.

---

## 4. Production, evaluation, and convergence

**SHOULD.** The graph makes responsibilities and every model invocation
visible.

**SHOULD.** Costly or external effects follow:

```text
deterministic prepare (immutable job, idempotency key)
  -> effect node (one piece per invocation)
  -> deterministic post-check / seal
```

A judge **MUST**:

- not modify the artifact under review;
- not trigger new production;
- emit a structured review if the graph consumes it;
- advise rather than act as a hard semantic gate (see §5.2);
- avoid unnecessary external effects.

`readonly: true` (**DSL** intent) protects the **checkout** and classifies
parallelism safety. It does **not** by itself block an MCP or API with
external effects. A judge **SHOULD** add a read-only tool allowlist, no
production APIs, no writes to external systems, no mutation permissions.

A unique writer is required **only** when several branches converge on the
**same shared mutable resource**. An Iterion node output does not by itself
need a parent-persisted file.

`isolated: true` is a **contractual assertion** (declared store, worktree,
or namespace), not an OS sandbox provided by Iterion.

---

## 5. Deterministic verdicts, advisory review, and human decisions

### 5.1 Deterministic evaluation verdict

`PASS`, `RETRY`, and `BLOCKED` belong to deterministic contract validation
(`tool` or `compute`) and to validated durable decisions. A probabilistic
semantic `judge` does not issue one of these verdicts as routing authority;
its protocol is defined in §5.2.

Enum names below are **examples**. The workflow sets allowed values.

```text
verdict:         PASS | RETRY | BLOCKED
rework_target:   <allowed producer> | none
blocker_kind:    <workflow-defined enum>
recovery_target: <allowed acquisition capability> | none
```

| Verdict | `rework_target` | Next |
|---|---|---|
| PASS | `none` | continue |
| RETRY | the inspected producer, never another | bounded loop to that producer (§5.3) |
| BLOCKED | `none` | node named by `recovery_target`, or `fail` if `none` |

**MUST.** The graph MUST NOT infer the next node from `findings` prose.

**SHOULD.** A `RETRY` names an observable defect tied to a criterion. A
`BLOCKED` means information or an external capability is missing: do not
invent scope to work around it.

**MUST.** Every graph cycle has a loop name and a cap. Retry counts are a
**project variable**, not a universal default.

### 5.2 Semantic judges advise; they do not filter

A model judge detects risks and proposes advice. It is not the owner of the
artifact and it does not have veto power over the producer. Example review
status names are:

```text
review_status: CLEAR | ADVICE
artifact_hash: <hash of the reviewed snapshot>
findings:      <structured findings>
```

| Review status | Meaning | Next |
|---|---|---|
| `CLEAR` | The judge has no advice on the reviewed snapshot | continue |
| `ADVICE` | The judge found a semantic risk worth reconsidering | prepare one review delta, then re-enter the same producer |

**MUST.** An `ADVICE` is non-binding. The producer reads it critically and
MAY accept it, accept it partially, or reject it. The producer emits the new
artifact (which MAY be unchanged) and a separate structured response annex.
For every `finding_id`, that annex records at least:

```text
decision:         ACCEPTED | PARTIALLY_ACCEPTED | REJECTED
rationale:        <why>
changed_refs:     <artifact locations or stable ids>
evidence_refs:    <optional supporting contract or evidence>
reviewed_hash:    <hash before the response>
resulting_hash:   <hash after the response>
```

**MUST.** Findings carry a stable id, the reviewed artifact hash, and a
criterion from a project allowlist. Free prose MUST NOT choose the next node,
the producer, or the permitted repair scope. A deterministic aggregator
validates finding codes, merges duplicates, and derives routing and repair
scope from the current contract. The judge MAY describe a possible repair,
but that suggestion is never an instruction.

**MUST.** Every finding is answered exactly once. An `ACCEPTED` response must
identify a corresponding observable change. A `REJECTED` response MAY preserve
the artifact byte-for-byte, but must explain the disagreement. This is not an
identical failed `RETRY`: the semantic advice never refused the artifact, and
the response annex is a new part of the review work unit. The identical-output
rule in §5.3 still applies after a deterministic refusal.

On the next visit, the judge reviews both the new snapshot and the producer's
responses. It MAY return `CLEAR` because the artifact changed or because the
reasoned response resolves its concern. If it repeats a finding, it **MUST**
keep the same `finding_id` and explain why the prior response is insufficient.
A new finding **SHOULD** concern changed material or a regression introduced by
the revision, rather than reopening settled preferences.

**MUST.** One semantic review phase has at most **three collective advice
cycles**. A project MAY choose a lower cap, never a higher one. When several
judges review the same artifact, they SHOULD inspect the same immutable
snapshot in parallel, then have their findings aggregated into one producer
response; they do not each receive a separate three-revision budget.

After the third advice cycle, the producer **MUST** emit its final artifact and
response annex, then the graph continues without another semantic review turn.
The workflow **MUST NOT** mislabel this as `CLEAR` or judge approval. It records
an explicit state such as `ADVISORY_EXHAUSTED`, preserves non-cleared findings
and producer responses, and surfaces them to the next durable consumer or
human gate.

**SHOULD.** In a hierarchical map/reduce workflow, review the decision artifact
at the level that owns it, rather than adding one late judge after every level
has been flattened:

- experience or journey judges review coarse boundaries and functional
  fidelity, then return advice to the coarse producer;
- split judges inspect the complete reduced sibling set when overlap, grain,
  priority, or dependency quality is comparative; content judges MAY inspect
  bounded groups in parallel;
- item or epic judges review the complete fixed item without reopening
  reducer-owned boundaries, IDs, ordering, or dependencies.

The graph MAY use one correction cycle by default and expose a project setting
up to the three-cycle maximum. Reducing the advice cap is the preferred cost
control; removing a review whose false CLEAR would waste the downstream work is
not automatically an economy. A cycle means judge snapshot A, producer
response/revision B, and a fresh judgement of B. If B still receives advice at
the cap, continue as `ADVISORY_EXHAUSTED`; do not call the unreviewed revision
`CLEAR`.

**SHOULD.** When an item's meaning controls generated visual evidence, keep the
visual-intent artifact in that item's responsibility. First converge the
textual/structural contract, then run a separate bounded visual realization
loop: deterministic image job -> media generator -> vision-capable semantic
judge -> feedback to the same visual-intent responsibility -> targeted
regeneration. Regenerate only affected views when the contract can identify
them. Image advice is non-filtering and exhausts into the human gate with its
open findings; generation and vision review remain explicit nodes with their
own capabilities, idempotency keys, effects, and budgets.

**MUST.** Deterministic contracts remain filtering. Schema, hashes, signatures,
allowlists, measured bounds, required evidence, safety rules, and other
machine-checkable invariants MAY yield `PASS`, `RETRY`, or `BLOCKED` under
§5.1. A semantic judge cannot waive such a failure, and a semantic opinion
cannot promote itself into one.

### 5.3 Retry and advice re-enter the producer, not a twin

**MUST.** A `RETRY` edge and an `ADVICE` response edge return to the **same
node** that produced the artifact. Declaring a second node with the same system
prompt, tools, and contract to serve the correction path — a *correction twin*
— duplicates a responsibility; it is not a routing decision. The twin exists
because the delta differs, but the delta is built by the deterministic node in
front of the producer, which is where that difference belongs.

A twin costs three things the graph cannot get back:

- **identity** — two declarations can diverge in prompts, tools, routing, and
  fixes even though they own the same artifact responsibility;
- **input** — the correction path tends to re-render material already frozen
  in the work packet, with the actual delta buried at the end;
- **fidelity** — a correction node that cannot reload the exact artifact and
  its frozen inputs cannot honour a "change only this" instruction. Every
  retry becomes a rewrite, and the judge reviews unnecessary drift.

**MAY.** The re-entered producer keeps its conversation across visits
(**DSL**: a session mode that resumes the node's own last conversation) only
when its projected peak context across the maximum number of visits satisfies
the no-compaction budget in §9.1. Session continuity is an optimization, never
an authority or a substitute for frozen artifacts. If the bounded resumed
session could reach that budget, the same producer responsibility uses a fresh
session and reconstructs its state from those artifacts. The first visit
receives the frozen work packet. Every later visit receives a **delta** — the
deterministic verdict, aggregated advice and prior producer responses, the
human notes, or the contract refusal — plus the paths and hashes needed to
reload the frozen inputs and current artifact, not a rendered copy of the full
brief.

**MUST.** A delta stays self-sufficient in **paths**. Session resumption can
fail silently (lost state, unavailable backend) and the runtime may fall back
to a fresh conversation without telling the graph. The delta therefore names
where the artifact and its frozen inputs live, so a context-less producer
re-reads instead of correcting blind.

**MUST.** When the producer re-emits an artifact **identical** to the refused
one, the receiving contract treats it as a **failed retry**, not as an
idempotent success. A contract that answers "already applied" on a retry path
re-presents the refused artifact to the judge and to the human gate, and the
refusal is silently lost. See §6.

Consequences on the node's declaration (**DSL**):

- session continuity has no meaning across backends, so a node that keeps its
  conversation MUST NOT declare a fallback that changes backend; its safety
  net stays inside the same backend. This is an authorized exception to the
  cross-family preference of §8 and MUST be recorded where the routing
  contract is audited — otherwise the next routing review "fixes" it back and
  the graph stops compiling.
- session persistence may be restricted to trunk nodes. A producer inside a
  fan-out body may be unable to keep its conversation; verify against the
  engine before designing a per-item retry around it.

**SHOULD.** Distinct re-entry sources — deterministic contract refusal,
semantic advice, human gate, a late deterministic refusal from downstream —
converge on a **single** deterministic node that merges their feedback in a
documented priority order, then re-enters the producer. **MUST.** Deterministic
retry, advisory review, and human revision keep separate named cycles and caps:
an advisory conversation must not consume the human's budget. Check how the
engine resets those counters — a counter re-entered through a sibling cycle's
edge may not reset, in which case its cap covers the whole run and must be
sized for that.

### 5.4 Judge vs producer capability

**SHOULD.** Choose producer and judge capability from the cost of bad
production, of a **false CLEAR**, and of unhelpful **ADVICE**. The judge MAY be
less, equally, or more capable than the producer depending on risk.

Economic heuristic (not a MUST): if a retry re-runs an expensive producer, a
stronger judge often costs more than it saves. Security, compliance, critical
migrations, irreversible decisions — a missed risk is costlier than extra
advice, so the judge MAY be the most capable node. Hard enforcement still
belongs to deterministic contracts and durable human decisions, not to the
model's confidence.

**SHOULD.** A judge of an artifact uses the **same routing class** as its
creator (same artifact kind). Do not invent a `high_judge` profile.

Common cases where the judge is the only quality LLM: the creator is not a
model (vendor, compiler, engine); the review is as wide as the production.

### 5.5 Human gates

A human refusal **MUST NOT** be assumed to mean “rerun the producer”. The
workflow chooses which states it allows, for example:

| Decision | Next |
|---|---|
| APPROVE | continue |
| REVISE | producer responsible for the defect |
| BLOCKED | acquire / wait (`recovery_target`) |
| REJECT | controlled terminal failure |
| CANCEL | cancel |

The technical target **MUST** come from a structured field, a deterministic
rule, or — only if free-text interpretation is required — an explicit model
node **followed by a deterministic validation**. A model MUST NOT be the
default classifier when a field or rule would suffice.

---

## 6. External effects, idempotency, and atomicity

**MUST.** An external effect (API, generation, remote write) is a single
graph responsibility and leaves a redacted proof.

The idempotency key **SHOULD** include:

- contract version;
- input hashes;
- tool/model version when it changes the effect;
- freshness window, when the result can go stale (search, periodic scans,
  time-varying data, deliberately renewed generation);
- an explicit regeneration nonce, when a new run is requested on purpose.

**MUST.** The same idempotency key MUST NOT produce two effects. An
intentional new execution requires a **new key**.

**MUST.** A writer (receipt, bundle, canonical copy) is **atomic**: a crash
MUST NOT leave a half-written artifact as current state.

**SHOULD.** Record the attempt **before** the first effect node of a
cacheable unit. A crash before a candidate retries; it MUST NOT reuse a
prior success.

On cancel / timeout, the valid current state remains authoritative; a failed
candidate MUST NOT be promoted.

---

## 7. Parallelism, isolation, and shared resources

**MUST.** Run in parallel only when writes are disjoint **and** declared.

| Marker | Guarantees | Does not guarantee |
|---|---|---|
| `readonly: true` | no checkout mutation | no MCP / API / external-system effect |
| `isolated: true` | contract promises a private store/namespace | an OS sandbox |
| `parallel_safe: true` | `fan_out_each` with disjoint item-keyed outputs | safety if keys overlap |
| `await: wait_all` | convergence barrier | a unique writer |

`fan_out_all`: known independent branches.
`fan_out_each`: one piece per item. Object items use a stable `key`. Scalar
content-addressed items omit `key` because Iterion resolves named keys only on
objects; their producer MUST emit a deterministic order and the scalar value
itself MUST carry the stable content identity.

**MUST.** Two runs with the **same idempotency key** MUST NOT mutate the same
work unit at the same time. A shared mutable resource is acquired through a
semaphore / lease / `resources:`; a retry that does not need to mutate it
MUST NOT re-acquire it for nothing.

**SHOULD NOT.** Raise `max_parallel_branches` to hide a write conflict.

---

## 8. Model routing and fallbacks

An execution profile **SHOULD** be chosen from:

- required capabilities (tools, vision, output shape);
- context size;
- modalities (text, image, code);
- cost and latency;
- confidentiality;
- failure tolerance;
- functional compatibility of fallbacks.

**SHOULD.** Primary + **one** named fallback, optionally a second safety net.
Not a long chain.

A fallback from **another family** reduces correlated outages; that is a
SHOULD, not a MUST. A fallback **MUST** meet every **minimum** capability of
the contract and MUST NOT receive more permissions than needed. It MAY have
fewer tools or permissions if the contract remains satisfiable. A text-only
backend MUST NOT silently replace a capability the contract requires
(native image generation, a given tool, a context floor).

A node that keeps its conversation across retries or advisory revisions
constrains this: see the same-backend fallback rule in §5.3.

Concrete model and backend pins belong in the **project profile**.

---

## 9. Budgets, timeouts, and stop conditions

**MUST.** Every workflow declares numeric caps. Do not add a dollar cost, a
branch count, and a retry count into one sum.

```text
max cost =
  Σ (max invocations of node × estimated unit cost)

max duration =
  critical-path duration
  + bounded waits
  + retry slack

max parallelism =
  max concurrently live branches
```

Fallbacks count as extra invocations in the worst case.

Each paid loop **SHOULD** declare separately:

- number of passes;
- max external effects;
- cost or duration limit;
- behavior when exhausted (fail, human, documented skip).

A child subbot has its own budget. `unbounded` only when the case is named
and documented.

### 9.1 Context-window budget and compaction avoidance

Compaction is a recovery mechanism for an oversized conversation, not a normal
planning primitive. An authoritative producer MUST NOT depend on a compacted
conversation to remember scope, ownership, prior decisions, or current state.

**MUST.** Before execution, every model node has a conservative projected peak
context budget expressed in tokens. It includes, for the maximum bounded path
through that node or resumed session:

- system and user prompts;
- injected schemas, packets, references, and review deltas;
- the maximum tool results the node is allowed to read;
- prior turns retained by a resumed session;
- reasoning/output headroom required by the execution profile.

The local profile declares the model context window, any runtime compaction
threshold, and a safety margin. It MUST select an execution profile whose
context window can contain the bounded work unit after that margin is reserved.
The projected peak MUST also stay below the compaction threshold when one is
configured. Estimate tokens, not file bytes. A path contributes only the
bounded content the node is authorized to read from it.

```text
projected_peak_tokens
  <= min(usable_context_window, configured_compaction_threshold_or_window)
     - safety_margin_tokens
```

**MUST.** When the bound does not fit, the author restructures the graph before
raising runtime limits or relying on compaction. Applicable reductions include:

- shorten the node's responsibility and role-specific prompt;
- project a smaller immutable work packet;
- split semantic levels into explicit producer stages;
- fan out disjoint bounded work units in parallel;
- converge them through a deterministic ownership, coverage, and dependency
  reducer;
- re-enter a fresh session from content-addressed artifacts instead of
  accumulating prior passes.

A larger-context model and hierarchical partitioning are complementary. A
large window provides safety headroom; it MUST NOT justify giving one node
several independently reviewable responsibilities or an unbounded packet.

**MUST.** State that must survive a pass lives in closed, immutable or
content-addressed artifacts with stable IDs, paths, and hashes. Conversation
history MAY improve local continuity while it remains inside the declared
budget, but downstream routing, retries, resume, and correctness MUST remain
reconstructible from artifacts alone.

**SHOULD.** Work that crosses several semantic granularities uses a staged
map/reduce shape: a coarse producer maps the experience or objective into
bounded units; independent producers refine those units in parallel; a
deterministic reducer fixes ownership, coverage, dependencies, and order before
the next level. Later producers may report new dependency claims, but MUST NOT
silently rewrite reducer-owned global state.

**SHOULD.** Runtime receipts record estimated and actual context tokens per
invocation, resumed-session depth, and every compaction event. If the runtime
cannot attest compaction, the graph still obeys the static no-compaction bound;
absence of telemetry is never evidence that the budget is safe.

---

## 10. Proofs and acceptance

A deterministic PASS **MUST** carry the proof **categories its contract
requires**. They are not interchangeable:

| Proof | What it attests |
|---|---|
| Provenance / integrity | identity, origin, lack of undetected mutation (hash, signature, content id) |
| Execution | a command or service actually ran (log, attested API response, exit) |
| Acceptance | functional or domain criteria hold (test, postcondition, durable human observation) |

A hash does **not** prove the artifact is semantically correct.

The **project** names which verifiers are authoritative per artifact type
(engine, browser, compiler, linter, human gate). An interactive aid (MCP,
manual recording) MUST NOT replace the declared verifier.

---

## 11. Resume after crash

Current state MAY be reused only if schema, revision, and transitive hashes
still hold. A missing file, a hash mismatch, or explicit feedback invalidates
**that** unit and its consumers, not independent branches.

Resume targets a responsibility, not the whole run, unless the contract
requires otherwise.

---

## 12. Validation and checklist

`iterion validate` checks syntax and a subset of **DSL** invariants, not this
standard. The project profile adds semantic audits and tests.

Checklist before merging a `.bot`:

- [ ] `iterion validate` is clean
- [ ] each node has a named responsibility; no agent CLI inside a `tool`
      (unless an authorized single-responsibility external service with
      redacted proof)
- [ ] graph-consumed output ⇒ closed schema; effect without text ⇒
      deterministic post-check
- [ ] any open `json` exception is an engine-native opaque handle, built from
      sealed proofs and never inspected for routing or domain semantics
- [ ] every subbot has at least two executable nodes, consumes only declared
      inputs, emits a clear deliverable/output, runs independently, and is
      reusable unchanged from another workflow
- [ ] judge: no mutation of the artifact, no new production, no unnecessary
      external effect, no hard semantic veto
- [ ] semantic review uses `CLEAR|ADVICE` (or closed equivalents), a frozen
      artifact hash, stable finding ids, and allowlisted criterion codes
- [ ] every advice finding receives one structured
      `ACCEPTED|PARTIALLY_ACCEPTED|REJECTED` producer response
- [ ] at most three collective advice cycles; exhaustion continues under an
      explicit non-approval state and preserves unresolved findings
- [ ] only deterministic contracts and durable decisions emit filtering
      `PASS|RETRY|BLOCKED`; semantic findings never choose routing from prose
- [ ] `RETRY` → inspected producer; `BLOCKED` → structured `recovery_target`
      or `fail`; never from `findings`
- [ ] no correction twin: retry and advice re-enter the same producer
      responsibility from frozen artifact paths and a delta; conversation
      continuity is used only when its bounded session satisfies §9.1
- [ ] after deterministic refusal, a re-emitted identical artifact is a failed
      retry; after advice, it is allowed only with a structured rejection annex
- [ ] re-entry sources merge in one deterministic node; deterministic retry,
      advice, and human revision keep separate named cycles and caps
- [ ] named, capped cycles; exhaustive guarded edges
- [ ] parallelism declared; unique writer only for a shared mutable resource
- [ ] fallback meets the contract’s minimum capabilities; documented
      exception if the capability is single-backend
- [ ] numeric budgets; idempotent jobs (key includes freshness); atomic
      writers; attempt recorded before the first cacheable effect
- [ ] every model node has a token-based projected peak context below the
      usable window and compaction threshold with a declared safety margin
- [ ] no authoritative state depends on conversation history or compaction;
      oversized work is partitioned into bounded stages/fan-outs with
      deterministic reducers and content-addressed handoffs
- [ ] persistent sessions are bounded across their maximum visits; otherwise
      the same producer re-enters fresh from artifact paths, hashes, and delta
- [ ] proofs of the categories the contract requires, from the project’s
      verifiers
