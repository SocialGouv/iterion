# ADR-153: Ports are a semantics the engine RUNS, not a declaration — the contract surface holds, the fan-out owes four slices, and a contract's verifiable clauses are checked by nothing

- **Status**: Accepted (2026-11-03) — on the clauses below, each read from the tree at `70b5b1972` and cited by file and line. The axes named under *What is not promised* are **not** decided here. **No code changes with it** beyond the five doc texts in `pkg/dsl/ir` and `pkg/dsl/spec` that stated or implied the opposite of clause 4; every slice under *Consequences* is owed, each with its own witness and its owner package. The two decisions this record first published as the operator's, each under a default, were taken on 2026-11-03 (*Consequences*): #1165 closes by absorption — its lots on main, six slices filed under #1402, the rest left to a future record (*Consequences*) — as its default read; and clause 4 ships as a positioned warning on each unverified clause plus the sweep that pins the evaluator's absence, not as run-time evaluation — its default's choice, in a per-clause form where the default read one warning naming every family.
- **Date**: 2026-11-03
- **Authors**: devthejo (session with Claude Opus 5), on the design shape ADR-143/144 set: decide what the tree backs, name what it does not.
- **Decides for**: [#1165](https://github.com/SocialGouv/iterion/issues/1165) ("design: contrats publics composables et exécution par ports"), under the epic [#1402](https://github.com/SocialGouv/iterion/issues/1402). PR [#1216](https://github.com/SocialGouv/iterion/pull/1216) proposed the whole of it and was closed unmerged — not by its review, but because the DSL programme had shipped the property registry, `dsl: 2` and `import` underneath it; the concept was harvested as lot 4bis of #1010, whose record is [099-public-contracts.md](099-public-contracts.md).
- **Relates to**: [099-public-contracts.md](099-public-contracts.md) (the surface this record starts from and does not re-propose) · [098-dsl-versioning-and-authoring-surface.md](098-dsl-versioning-and-authoring-surface.md) § 16 (the reserved `runtime_semantics` axis — cited by file, 098 being a number used twice) · [#1006](https://github.com/SocialGouv/iterion/issues/1006) (reliability: the boundary sentence is in *Consequences*) · [dsl.md](../dsl.md) and [routers.md](../routers.md) (the fan-out's documented behaviour) · [diagnostics.md](../references/diagnostics.md) (C246, C300-C304, C310)

## Context

The ticket asks for four reading levels — a workflow's public contract, a
graph, a per-node contract, the technical configuration — and for an engine
that schedules on **data availability** rather than control flow. Its two
worked examples are the diamond (`A`, `B` independent, `C` waiting on both)
and the map:

```text
A ── items:T[n] ──► B(item:T) → U ──► C(results:U[n])
                    B[0] … B[n−1]
```

[099-public-contracts.md](099-public-contracts.md) already decided the first
level and refused the second until something ran it: `contract <name>:` is a
top-level declaration bound to the program by C300-C304 and C310 — the C305
row that sits between them in `diagnostics.md` bears a subbot `source:`
refusal, not a contract clause — and `graph`, `port_policy` and
`runtime_semantics` are deliberately absent, "its property ships with the
semantics that runs under it" (§ 3, amending
[098-dsl-versioning-and-authoring-surface.md](098-dsl-versioning-and-authoring-surface.md)
§ 16). That decision holds and is not re-opened here.

What is left is narrower and sharper than a graph. Read against the tree, the
ticket's own invariants land on the six clauses below, none of which needs a
port graph to repair, and one of which turns ADR-099's decisive finding —
**"a contract that is not checked against the program it describes is worse
than none"** ([099-public-contracts.md](099-public-contracts.md) lines 24-26)
— back on the contract itself.

## Decision

1. **Schema identity is the fingerprint the tree already computes, extracted
   to one chokepoint — not a new rule.** ADR-099 § 2 already demands structure
   for C301 ("the field's declared type matches the port's type structurally").
   Inside one compilation unit a schema name resolves to exactly one schema, so
   the three comparisons that read a declared type as a string
   ([compile_contracts.go:298](../../pkg/dsl/ir/compile_contracts.go), `:501`,
   `:521`) ARE identity comparisons, and a fingerprint there would be a no-op.
   The ticket's invariant — *un nom de schéma identique ne suffit pas* — bites
   across units, where today nothing compares at all: bundlelint's C255 output
   arm holds a parent's `output:` schema to a child's ports but compares types
   "only when BOTH sides spell a scalar builtin", because a schema-named port
   projects the producer's whole object and a literal comparison "would warn on
   a correct parent" ([contracts.go:128-134](../../pkg/bundlelint/contracts.go)).
   The mechanism that resolves this exists, unexported and used by one package:
   `schemaFingerprint` ([artifact_contract.go:93](../../pkg/runtime/artifact_contract.go)),
   an order-insensitive sha256 over each field's (name, type, sorted enum),
   whose own doc states the reason — editing a schema's fields keeps its name
   while renaming an unchanged schema changes no shape, "so a name comparison
   misses the first and refuses the second". **That digest is the one
   chokepoint**, and it belongs in `pkg/dsl/ir`: `pkg/runtime` already imports
   `pkg/dsl/ir`, so one helper serves the compiler, bundlelint and the artifact
   contract, and no second digest can drift from it. This record amends ADR-099
   § 2 by that much and no more: where both sides name a **declared schema and
   the reader could read both**, C255's comparison is by identity; a child the
   caller could not read is still held to nothing (the C253 precedent), and
   builtins and `[]` cardinality keep name equality.

2. **The ordered per-element collection does not exist, and the tree's own
   statement of the limit is C310 — not a sentence in dsl.md.** The join
   computes `ordered := byBranchID(results)` precisely "so that what the
   aggregate names, quotes and carries does not depend on the order the
   branches' goroutines finished in"
   ([convergence.go:35-38](../../pkg/runtime/convergence.go)) and then merges
   the outputs from the **raw** `results` slice, `rs.outputs[nodeID] = output`
   (`:167-174`). Every branch of a `fan_out_each` writes the same template node
   ids, so the surviving output is whichever branch finished last. The tree
   says so, at compile time, as a warning: **C310**
   ([diagnostics.md](../references/diagnostics.md) C310 row;
   [compile_contracts.go:473](../../pkg/dsl/ir/compile_contracts.go)) — "the
   port projects whichever execution finished last, one execution's output,
   not the collection". So the capture is documented, not undiscovered, and
   making it deterministic is an improvement that **falsifies four shipped
   texts**, which move with it. `dsl.md`'s "It does not synthesize per-item
   outputs" is **not** that statement: its subject is the `best_effort`
   collector's fallback floor of `with:` mappings. Two slices follow, in this
   order: the capture reads `ordered` (S), and the input-ordered complete
   collection the ticket's second example needs (L, its own ticket) — one
   declared property on the primitive that already replays a subgraph per
   item, with no `graph:`, no node kind and no scheduler.
   `byBranchID` sorts branch ids **lexicographically** (`:411`), so for n ≥ 10
   `branch_r_10` precedes `branch_r_2`: deterministic, and not input order.
   The remedy is a sort on the **parsed index**. Zero-padding the id is
   refused: it is formatted at five sites
   ([fan_out_each.go:185](../../pkg/runtime/fan_out_each.go), `:202`, `:246`,
   `:270`, `:322`), persisted (`BranchCheckpoint.BranchID`,
   [run.go:2231](../../pkg/store/run.go), json **and** bson, so a resumed run
   matches against it) and publicly documented —
   `_failed_branches[].branch_id` is read by "a `with` mapping or a downstream
   gate [that] can fail closed on a missing branch"
   ([routers.md](../routers.md)).

3. **An expansion ceiling, refused before the instances exist — and the word
   is not `max_items`.** Nothing bounds the number of elements:
   `maxParallel := len(items)`
   ([fan_out_each.go:162](../../pkg/runtime/fan_out_each.go)), narrowed only by
   the budget's `max_parallel_branches`, which the registry itself calls
   "Concurrent branches" ([spec.go:605](../../pkg/dsl/spec/spec.go)). A
   model-produced array of ten thousand items therefore creates ten thousand
   branches in one invocation and discovers the problem by spending the
   budget. The ceiling
   ships as **`max_expansion:`** on the `fan_out_each` router, refused at
   resolve time **before** `ensureParallelInvocation`, naming the count it saw
   and the ceiling it was given. The word matters: `max_items` is already a
   `contract.port` property — "Maximum array cardinality"
   ([contracts.go:35](../../pkg/dsl/spec/contracts.go)) — and it is one of the
   inert clauses of clause 4, so re-using it would put one word on two
   surfaces with two enforcement points and one of them doing nothing.
   **No default ceiling**: an absent property means unbounded, exactly as
   today, and the escape hatch is omitting it — greppable, and the operator's
   explicit choice is never silently replaced.

4. **A contract's verifiable clauses are declared, compiled, rendered,
   projected and persisted, and nothing checks any of them.** One class, three
   families, read as a gesture rather than a name:
   - **`criteria:`** — `spec.CompilePublicCriterion`
     ([criteria.go:107](../../pkg/dsl/spec/criteria.go)) has exactly **one**
     non-test caller,
     [compile_contracts.go:568](../../pkg/dsl/ir/compile_contracts.go), and it
     **discards the evaluator** (`if _, err := …`). This family the tree
     already says out loud, and the record credits it rather than
     rediscovering it: [dsl.md](../dsl.md)'s C303 bullet reads "**no criterion
     is evaluated at run time, registered or not** — a criterion is a declared
     check for readers (and for the evaluators a future release will run)".
     What is missing is a **diagnostic**: C303 fires only on an unregistered
     kind, so an author who reads `iterion validate` and not that paragraph
     gets no signal on `min_length` — registered, rendered, never run.
   - **`min_items` / `max_items`** — parsed
     ([parser_contract.go:224-230](../../pkg/dsl/parser/parser_contract.go)),
     compiled ([compile_contracts.go:182](../../pkg/dsl/ir/compile_contracts.go)),
     carried on the wire ([contracts.go:35-36](../../pkg/dsl/ir/contracts.go)),
     printed ([validate.go:1024-1030](../../pkg/cli/validate.go)), written back
     ([write.go:791-795](../../pkg/dsl/author/write.go)) and persisted on the
     run doc. The only checks are **structural**: the port must be an array,
     and min ≤ max (`compile_contracts.go:197-201`). No site compares a
     length — and no page says so, while the generated registry rows read
     "Minimum / Maximum array cardinality (C300 on an input, C301 on an
     output)", which a reader takes for a diagnosed bound.
   - **`file:`** — the registry calls the block "Verifiable properties of a
     produced or consumed file", with "Expected media type" and "Minimum file
     size" ([contracts.go:39-43](../../pkg/dsl/spec/contracts.go)). They are
     carried ([contracts.go:56-58](../../pkg/dsl/ir/contracts.go)) and printed
     ([validate.go:1042-1049](../../pkg/cli/validate.go)); no site reads
     either. `schema:` gets one check — it must name a **declared** schema
     (`compile_contracts.go:187-191`) — which is a declaration check, not a
     conformance check.

   The contrast that makes this a decision rather than a complaint: `enum` and
   `matching` **are** enforced, on the **var**, by one gate —
   `runtime.ValidateVarConstraints`
   ([engine_resolve.go:1277](../../pkg/runtime/engine_resolve.go)), called by
   the engine's resolve and by runview's launch and fork admission — and the
   port mirrors them only so "a reader of the contract is never told a wider
   one" ([contracts.go:37-45](../../pkg/dsl/ir/contracts.go)); `required:` and
   `default:` are the var's facts the compiler holds the port to (C300). The
   unchecked half is exactly the half the port declares **on its own**.

   One precision, so the record does not accuse the wrong text. **ADR-099 § 5
   asserts no evaluation**: it says that `PublicCriteria` ships the evaluators
   with a machine-readable parameter declaration and that
   `CompilePublicCriterion` validates the parameters and returns the
   evaluator, which is true. The sentence that promised an evaluation was a
   **code comment**, `PublicContract`'s doc in
   [contracts.go](../../pkg/dsl/ir/contracts.go): "so the runtime compiles a
   criterion's evaluator when it runs." Nothing does, and it contradicted
   `dsl.md` one directory away. That sentence and its peers — `PublicCriterion`'s
   doc, `PublicCriteria`'s, `compilePublicCriterion`'s and C303's
   catalogue hint, each of which scoped "not evaluated" to an unregistered
   kind — not ADR-099, are what change with this record. The verdict on the
   class is the operator's, decided on 2026-11-03: a positioned warning on
   each unverified clause plus a sweep pinning the evaluator's absence
   ([#2941](https://github.com/SocialGouv/iterion/issues/2941)), not enforcement at the executor seam (see
   *Consequences*).

5. **Per-element identity is POSITIONAL, and main refuses rather than isolates
   per element.** Lot 5 asks to persist invocation and element identities and
   to "isoler les fichiers par invocation et élément". What the tree does: a
   branch id is `fmt.Sprintf("branch_%s_%d", routerNodeID, i)` over the index
   in the resolved array
   ([fan_out_each.go:185](../../pkg/runtime/fan_out_each.go)), the checkpoint
   stores that id, and a resumed invocation is matched by (router id,
   invocation key, branch-id **set**) —
   `parallelExecutionState.matches`
   ([parallel_checkpoint.go:524-539](../../pkg/runtime/parallel_checkpoint.go)).
   A **cardinality** change across a restart is caught and typed ("branch set
   changed across restart; rewind the router before resuming", `:140`). A
   same-length change in **content or order** is not: each restored branch
   re-attaches to whatever element now sits at its index. `key:` — the item
   field that names each branch ([spec.go:475](../../pkg/dsl/spec/spec.go)) —
   exists for the dependency DAG and is **not** the branch identity. On
   isolation, main's answer to concurrent mutation is a refusal: "at most 1
   mutating branch is allowed in parallel on the same workspace"
   ([workspace_safety.go:1428-1434](../../pkg/runtime/workspace_safety.go)),
   escapable by `readonly:` on an agent/judge, `isolated:` on a **subbot**, or
   `parallel_safe:` on a `fan_out_each` tool. `isolated:` is subbot-scoped
   ([spec.go:547](../../pkg/dsl/spec/spec.go)), never per element. So lot 5's
   reuse rule — "réutilisable seulement si identités et artefacts restent
   compatibles" — has no identity to key on today. An element identity derived
   from `key:` (or from the element's digest where no `key:` is declared) is
   owed with the collection of clause 2; **per-element file isolation is a
   decision, not a slice**, because it reverses the doctrine that refusal
   encodes.

6. **The empty collection's surprise is a compile-time warning, and the
   election it keys on is already in the compiler.** An empty `over:` skips to
   the convergence node with an empty aggregate and **no** validation against
   the consumer; with no convergence node the template runs **once with no
   item binding**, announced by a run-time `logger.Warn`
   ([fan_out_each.go:123-142](../../pkg/runtime/fan_out_each.go)) that no
   author and no CI gate sees. #1165 refuses carrying that forward. The
   warning can live at compile time at the **same** election the engine uses:
   `(*Engine).findConvergencePoint`
   ([convergence.go:320](../../pkg/runtime/convergence.go)) delegates to
   `ir.ExecBranchConvergencePoint`
   ([fan_out_region.go:70](../../pkg/dsl/ir/fan_out_region.go)), which the
   compiler already calls for C243/C244
   ([validate_edges.go:672](../../pkg/dsl/ir/validate_edges.go), `:690`,
   `:758`) and whose `""` already means "the branches never reconverge". So
   the diagnostic is a `pkg/dsl/ir` warning on that election, in the family
   **C246** opened — a compile-time finding about collector election
   ([diagnostics_codes.go:246](../../pkg/dsl/ir/diagnostics_codes.go)) — and
   **run-time behaviour does not move**: a warning, not a refusal, because
   that control flow was preserved deliberately "to avoid regressing bots
   whose `fan_out_each` legitimately feeds a terminal". Validating the empty
   aggregate against the consumer's schema is the ports-graph half and stays
   owed.

7. **ADR-099 § 3 holds: no `graph`, no `port_policy`, no `runtime_semantics`.**
   None of the six clauses above needs one, and each is deliverable on its own
   — which is the test § 3 set. A data-flow semantics, when one runs, composes
   these ports; it never redefines what a contract is.

## Rejected alternatives

- **Shipping the graph and `port_policy` now** (#1216 as proposed, 368 files).
  Refused by ADR-099 § 3 and not re-litigated. The measurement #1216 harvested
  is on the record and points the same way: "frozen thresholds, native 26–60 %
  slower measured before any recommendation". A pilot claim before a
  reproducible baseline is the thing lot 7 itself forbids.
- **A second fingerprint in `pkg/dsl/ir`, next to `pkg/runtime`'s.** Two
  digests over the same shape drift, and the first divergence is silent: a
  contract would call two schemas identical that the artifact contract calls
  different. The extraction IS the decision.
- **Upgrading the three intra-program string comparisons to fingerprints.**
  Inside one unit a name already resolves to one schema, so the change is a
  no-op that reads as a fix — and the cross-unit arm, the one that is blind,
  would stay blind.
- **Zero-padded branch ids** for input order. They are a documented output
  surface and a resume key; padding changes what a gate reads and what a
  restored run matches.
- **`max_items:` on the router.** One word, two surfaces, two enforcement
  points, one of them inert: the shape a reader cannot distinguish.
- **A default expansion ceiling** with warn-and-override. It changes the
  behaviour of every shipped data-driven fan-out, which is a coexistence
  decision this record does not take.
- **Refusing the n = 0 single run at compile.** It makes the ticket's clause
  literally true and breaks bots whose `fan_out_each` feeds a terminal; the
  warning is the form that keeps a shipped bot green.
- **Documenting the unchecked clauses and shipping nothing.** It leaves every
  reader of a contract — the catalogue, the parent bot, the studio — believing
  a gate runs, which is the failure mode ADR-099 was written against.

## What is not promised

- **The port graph, the data-availability scheduler, `port_policy` and
  `runtime_semantics`.** ADR-099 § 3 stands; `pkg/runtime` holds none of
  #1216's `ports*.go` family.
- **Validating an empty collection against the consumer's schema.** It needs
  the consumer-side typing a port graph supplies; clause 6 ships the author's
  warning, not the check.
- **Per-element file isolation.** Clause 5 names why: main refuses concurrent
  mutation instead of isolating, and the escape hatches are node-scoped. The
  isolation primitive is a decision — it reverses that doctrine — and it is
  owed before any slice claims it.
- **A studio reader of the compiled projection.** There is none: no file under
  `studio/src` reads `public_contract` and no contract component exists; the
  studio's whole knowledge of contracts is the generated property registry,
  which gives the generic inspector the fields and nothing more. The CLI and
  the MCP return do carry the projection
  ([validate.go:59-62](../../pkg/cli/validate.go)).
- **A verified public adapter letting an unchanged legacy workflow serve as a
  node** (lot 6), and the pilot fixtures, baseline and conversion report
  (lot 7). Absent, and out of this record's scope.
- **Exactly-once external effects.** #1165 says so itself: idempotence or
  verification, never a general guarantee.
- **Run-time evaluation of clause 4's families.** The operator decided on
  2026-11-03 for the warning and the absence sweep; enforcement at the
  executor seam can fail runs that pass today and owes its own coexistence
  decision before any slice claims it.

## Consequences

- **#1165 closes by absorption, and the six slices below are filed under the
  public-contracts epic #1402 as #2939-#2944** — decision (a) below.
  Lots 1, 2 and the subbot half of lot 6 are on main under other names, with
  their markers: the contract surface and its eight registry kinds, C300-C304
  and C310, the bundlelint pair C254/C255, the AST/JSON/unparse round trip, the `ContractSince` release floor
  in the single `syntaxFloors` table, the contract pinned on the run doc
  (`store.Run.PublicContract`, [run.go:1047-1054](../../pkg/store/run.go)),
  the deployment-compatibility probe `iterion contracts` (build, queue and
  store schema versions, read from the executable alone — `build_skew` stays
  unknown by design, and it reads no bot's contract), and the runtime
  projection of a child's ports. The six slices below carry part of lot 5 —
  its ceiling, its stable order, its empty-collection signal and its element
  identity — plus clause 1's schema identity and clause 4. They do not carry
  lots 3 and 4 (the port graph and the data-availability scheduler, which
  ADR-099 § 3 refuses), lot 6's legacy adapter, lot 7 or lot 5's per-element
  isolation: #1165's closure leaves those to a future record, and *What is not
  promised* lists them. The epic #1402 is the container; a second one next to it is the
  confusion the 2026-09-18 `epic:` → `design:` retitle was meant to end.
- **The boundary with #1006**, agreed before either record is written so
  neither re-decides the other: **reliability (#1006) owns the bounded
  correction, the context and artifact interfaces, and the resume semantics;
  this record owns the public face and the port typing.** Lot 4's dependency
  on #1006 is read that way.
- **The slices this record owes**, in order, each with its witness and its
  owner package. Every witness is built in **both** completion orders where a
  fact depends on one, and each must be seen red on its own assertion with the
  defect put back:
  1. **The schema-identity chokepoint** ([#2939](https://github.com/SocialGouv/iterion/issues/2939)) — `pkg/dsl/ir` (the digest),
     `pkg/bundlelint` (the comparison). Witness: a parent and a child each
     declaring `schema Dossier` with different fields draw C255's warning,
     while a child the caller could not read still draws nothing. M. Two
     consequences to carry in the slice, not discover: `ir.PublicPort` is
     serialised into `store.Run.PublicContract` (json **and** bson), so a new
     identity field lands in a persisted run doc; and `SameProgram` compares
     `contracts` by `reflect.DeepEqual`
     ([equiv.go:104-105](../../pkg/dsl/ir/equiv.go)), so contract equivalence
     starts depending on a schema's field set.
  2. **The deterministic capture** ([#2940](https://github.com/SocialGouv/iterion/issues/2940)) — `pkg/runtime`. Witness: twelve branches
     fed reversed then forward, the surviving `rs.outputs` and the
     `_failed_branches` sequence identical, once under `-race` with a mutex in
     the observer. S. The four texts that describe the old capture — C310's
     row, its catalogue entry, its code comment and its `warnf` — move with
     it, and the slice names that re-sorting changes which error
     `quotedBranchError` / `rootCauseBranchErr` quote for n ≥ 10.
  3. **The unenforced clause families** ([#2941](https://github.com/SocialGouv/iterion/issues/2941)) — `pkg/dsl/ir`,
     the warning the operator decided on 2026-11-03, not enforcement in
     `pkg/runtime`. Witness: a registered criterion, a `min_items` and a
     `file.min_bytes` each draw a positioned warning that names that clause
     unchecked, plus a reachability sweep over every symbol that reaches an
     evaluator rather than one name: no reference outside `pkg/dsl` to `CompilePublicCriterion`,
     `LookupPublicCriterion` or `PublicCriteria`, each of which reaches an
     evaluator through `PublicCriterionSpec.Compile` — so the day one of those
     symbols is named outside `pkg/dsl`, the sweep reddens and the criteria
     warning is removed deliberately. An evaluator carried out of `pkg/dsl`
     by a `pkg/dsl` type or helper — a `func(any) error` field on
     `ir.PublicCriterion`, an exported `pkg/dsl/ir` function that runs one —
     names none of the three and escapes the sweep, so the slice that adds
     such a carrier extends the sweep to it in the same change. The sweep pins the `criteria:`
     family only: enforcing `min_items` / `max_items` or `file:` needs no
     criterion symbol, so their warnings have no absence pin, and the slice
     that enforces either family removes its warning with it. S.
  4. **The expansion ceiling `max_expansion:`** ([#2942](https://github.com/SocialGouv/iterion/issues/2942)) — `pkg/dsl/spec`, `pkg/dsl/ir`,
     `pkg/runtime`. Witness: an over-long array draws the typed refusal AND a
     counting `NodeExecutor` records **zero** branch starts — the effect, not
     the message; a second case with no property behaves exactly as main does
     today. M.
  5. **The empty-collection warning** ([#2943](https://github.com/SocialGouv/iterion/issues/2943)) — `pkg/dsl/ir`. Witness: a
     `fan_out_each` whose template reaches no convergence point draws a
     positioned warning naming the single-run behaviour, one with a
     convergence node draws nothing, and `TestFanOutEach_EmptyArray` and
     `TestFanOutEach_EmptyArrayClearsRestoredParallelCheckpoint` stay green —
     they pin the run-time behaviour this slice must not move. S.
  6. **The input-ordered complete collection, and the element identity it
     keys on** ([#2944](https://github.com/SocialGouv/iterion/issues/2944)) — `pkg/runtime`, with `pkg/store` for the persisted identity.
     Witness: twelve elements whose executors finish in reversed order arrive
     input-ordered and complete; `wait_all` refuses an incomplete collection
     and nothing partial reaches the collector; under `await: best_effort` the
     survivors keep their indices; and a resumed invocation whose array
     changed content at equal length is refused instead of re-attached. L, its
     own ticket, after slice 2. It must refuse a second array axis **by name**
     — #1165 forbids an implicit zip and an implicit cartesian product, so the
     next variant must not need an engine `if`.
- **Decisions the operator took on 2026-11-03.** This record first published
  both as the operator's, each under the default it would publish under if
  nothing was said; the operator took (a) as its default and (b) as its
  default's choice in a per-clause form, so the record states them as
  decided: **(a)** #1165 closes by absorption and the six slices are
  filed under #1402 as #2939-#2944 — keeping #1165 open as the living design
  was the alternative, at the cost of a second container; **(b)** clause 4
  ships as **a positioned warning on each unverified clause plus the sweep
  that pins the evaluator's absence** (#2941) — the default's choice in its
  per-clause form, where the published default read one warning naming every
  unenforced family. Enforcement at the executor seam was the alternative: it
  makes the promise true, is sized L, and can fail runs that pass today, so it
  owes its own coexistence decision and is not taken here.
- **No new diagnostic code is claimed by this record.** The slices that need
  one pick it at their final rebase on `main`, by symbol and not by digit; the
  codes this record cites — C243, C244, C246, C253, C254, C255, C300-C305,
  C310 — all exist on main.
- **A record is not a capability.** Nothing executable depends on this file;
  what lands with it is the comment class that stated the opposite of clause 4
  — four doc comments and C303's catalogue hint in `pkg/dsl/ir` and
  `pkg/dsl/spec`. The generated
  surfaces that describe C303's scope (the registry's property doc, and the
  grammar, properties, SKILL and quickref pages derived from it) are accurate
  about C303 and are slice 3's to widen, not a comment fix's.
