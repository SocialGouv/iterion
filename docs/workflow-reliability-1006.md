# Workflow reliability rollout (#1006)

This document is the compatibility ledger for the reliability work. Each
tranche is independently deployable and is stacked on the previous one; an
operator can stop at any boundary without rewriting existing run records.

## Context contract (tranche A)

Every newly created in-process run carries `execution_context.version = 1`.
The record identifies:

- the Iterion run store (`run_store`), separately from business stores;
- the declared workspace isolation mode and logical workspace id;
- the compiled workflow revision, source root and optional bundle revision;
- root/parent run and parent-node lineage;
- the policy (`legacy`, `report`, or the future `enforce` gate).

The record contains identities and revisions only. It never stores credentials,
prompts, model output or business payloads. Older run documents omit the field
and remain readable; resume keeps the legacy behaviour until an admission
policy is explicitly enabled.

## Launch-surface matrix

| Surface | Context persisted | Admission owner | Planned tranche |
| --- | --- | --- | --- |
| local/API in-process launch | yes | runview service | A/B |
| local resume | existing context preserved | runview + runtime | B/C |
| detached launch | compatibility stamp | detached runner | B |
| cloud queued launch | queue/document propagation | publisher/runner | B |
| subbot child | parent/root reconciliation | runview subbot runner | B |
| watcher/Copi observation | diagnostic projection | watcher authority | F/G |

This matrix is intentionally explicit: a green unit test in one launch path is
not evidence that a queued or nested launch has the same context.

## Bounded invalid-output correction (tranche D)

Schema-invalid output is corrected only when the executor implements the
optional `runtime.OutputCorrector` capability. The engine allows at most the
configured budget for each node episode, resolved
`WithOutputCorrectionBudget` → `ITERION_OUTPUT_CORRECTION_BUDGET` → `2`.
`0` (or `off`) disables correction entirely. No CLI flag or DSL field is
wired yet: the env var is the operator-facing escape hatch until a launch
surface needs a per-run one.

**What the correction boundary actually covers.** The *artifact*, the
`node_finished` event and the outgoing *edge* are written only after a payload
validates — those are the downstream effects a correction must not replay. It
is not a boundary around all events: the node's own lifecycle events
(`node_started`, `llm_request`, tool calls) necessarily precede validation and
describe the original attempt, and so do `emitVerifiedActionIfPresent` and the
`session: persist` slot commit, which run on the pre-correction payload by
design — a session slot records the CLI session that produced the output, not
the repaired copy of it.

Correction is **trunk-only**. Fan-out branch nodes validate directly and are
not corrected; the ledger key already carries a branch slot, so extending it
later needs no migration.

The episode ledger is persisted on the run document, keyed by node
*execution* — node id plus loop-iteration path and branch id, sanitized to a
Mongo-safe field name. It records the attempt count, output/violation
fingerprints and a status. `unchanged` and `exhausted` are terminal: a
corrector returning the same invalid payload stops immediately, and a resume
continues the existing budget rather than starting a new loop — only a genuine
budget *raise* reopens an exhausted episode, and never an `unchanged` one.
Fingerprints cover the schema payload only, so the engine's own `_`-prefixed
metadata (`_duration_ms` above all, which changes every execution) cannot make
one episode look like another.

The engine merges that metadata onto whatever the corrector returns, so
`_tokens`/`_cost_usd` still reach budget accounting and `_backend`/`_model`/
`_fallback_used`/`_served_by` still reach a downstream gate. A key the
corrector sets wins, which is how a model-backed corrector reports its own
spend — it has no other channel today, and a correction round trip emits no
event of its own. Executors must keep correction side-effect free; publish and
external-effect nodes remain behind the validation boundary.
