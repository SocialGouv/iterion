# ADR-146: Secret boundary v2 — a secret is granted to a sink, and what crosses a node boundary is a separate question

- **Status**: Proposed — two axes are open questions for the operator, named below. Nothing in this record is implemented yet.
- **Date**: 2026-10-29
- **Authors**: devthejo (session with Claude Opus 5), orchestrator-arbitrated — debt-leap ARB-78 of 2026-10-29: "C — ADR-146 en `Proposed`, les deux axes contestés nommés comme questions ouvertes ; zéro code S2/S3"
- **Landed by this change**: the record only. The slices behind it (outbound redaction at the model boundary, per-secret materialization sinks in the DSL, the sandboxed-run default) stay unwritten until the two open axes are answered — each then arrives with its own witness.
- **Relates to**: [018-bot-secret-binding-egress-enforcement.md](018-bot-secret-binding-egress-enforcement.md) (the egress half of v1) · [069-mid-run-file-secret-refresh-for-sandboxed-runs.md](069-mid-run-file-secret-refresh-for-sandboxed-runs.md) (file secrets, refreshed mid-run) · [../secrets.md](../secrets.md) (the three layers this extends) · [../permissions.md](../permissions.md) (the path-rooted gate rules of iterion#1710, the belt this is not)

## Context — what v1 already guarantees, measured

Three layers are live and documented in [secrets.md](../secrets.md):

- **Layer 0 — sink redaction.** `Guard` carries the known values and
  redacts them out of what the run EMITS: `Redact`, `RedactBytes`,
  `RedactValue`, `RedactMap`, plus a heuristic pass
  (`pkg/backend/secretguard/secretguard.go`). The engine applies it at one
  choke point on the event path (`Engine.emitBranch`, which scrubs through
  the executor's `SecretScrubber` before persisting and before logging),
  and a node's output is scrubbed for the `node_finished` event by
  `sanitizeOutputForEvent`.
- **Layer 1 — placeholders and materialization.** A `{{secrets.X}}`
  reference renders a placeholder everywhere except the execution sinks
  the runtime materialises at, and file secrets land in a directory the
  run owns.
- **Layer 2 — TLS-inspection egress.** A sandboxed run's outbound traffic
  is substituted/inspected so a value the agent never sees can still
  authenticate a request it is allowed to make.

Two measurements decide what v2 can and cannot say.

**The live output map keeps the real values, by design.** The contract is
written at the scrubber's own declaration
(`pkg/runtime/node_output.go`): *"ScrubOutput MUST return a redacted deep
copy and never mutate its input: the live output map feeds downstream
nodes (`{{outputs.X}}` / `{{artifacts.X}}`) and the resume checkpoint,
both of which must keep the real values."* Redaction is therefore
EVENT-ONLY on purpose. A workflow that obtains a credential in node A and
uses it in node B's tool call — the shape the PLIAGE gateway lane
(iterion#2005) relies on — works precisely because the crossing does not
redact.

**There is no `sinks:` concept in the DSL.** What exists is a documented
SET of places the runtime materialises at, enforced as a refusal rather
than a grant: a secret reference that travels through a data mapping, a
`with:` value, a fail `message:` or a compute expression is refused with
C150/C151, naming "the execution sink that uses the secret (a tool's
`command:`/`script:`/`postcondition:`, a tool action's `params:` value,
or a prompt body)" (`pkg/dsl/ir/validate_refs.go`). That set is global and
identical for every secret: no secret can say "materialise me in a file,
never in a prompt".

## Decision

**A secret is granted to the sinks it may reach, and the grant is part of
the secret's declaration.** The unit of the boundary is the pair
`(secret, sink)`, not the secret alone: `sinks:` beside `hosts:` in the
secret's block names the materialization kinds the secret is allowed to
reach (a prompt body, a tool `command:`/`script:`, a tool action's
`params:`, a file the run materialises, an egress substitution), and a
reference that reaches an ungranted sink is a typed refusal at COMPILE
time, carrying the sink it named and the grant that would allow it —
the same shape as C150/C151, one rung more precise.

Three consequences follow, in the order they can be built:

1. **The compiler is where an ungranted sink dies.** The refusal is a
   diagnostic on the reference, not a runtime check: a run that paid for a
   workspace and a model call to learn a secret may not reach a prompt has
   already told the model the secret exists.
2. **The known-value redaction stays one choke point per boundary.** v2
   adds no second redactor: the boundary it extends to is named by axis 1
   below, and whichever boundary wins reuses `Guard`'s known values —
   never a parallel matcher, which would drift (the lesson of
   `pkg/errtrack`: extend the tracker, never add a second one).
3. **A sandboxed run keeps plaintext out of the container by default.**
   When neither the egress-substitution path nor the file-secret path can
   serve a declared secret, the launch FAILS with a typed error naming the
   secret and the two paths, instead of falling back to a plaintext
   environment variable. Backend parity is part of the decision:
   `claude_code`, in-process `claw` and sandboxed `claw` reach the same
   verdict, or the unsupported one is typed-refused.

## Open questions — the operator decides, this record does not

**Axis 1 — which boundary redacts a known value that re-enters a model's
context?** A sentinel printed by node A can re-enter node B's context
through a tool result or an `{{outputs.A}}` reference.

- **(1a) The model boundary alone.** Redact at prompt rendering: what the
  model READS is scrubbed, while the data a downstream node consumes
  programmatically keeps its value. Preserves the `node_output.go`
  invariant and the PLIAGE shape; a tool node can still write the value
  somewhere an attacker reads.
- **(1b) The crossing itself.** Redact when a value leaves the node that
  produced it. Strictly stronger, and it BREAKS the invariant above and
  every workflow that passes a credential between nodes — which makes it a
  documented rupture with a compat mode, not a bug fix.

**Axis 2 — what is the default grant for a secret that declares no
`sinks:`?**

- **(2a) NONE.** A secret reaches nothing until its declaration says so.
  Safe by construction, BREAKING for every existing bundle; it needs the
  one-release compat mode (a C1xx warning naming the sinks the workflow
  uses, grants inferred for that release, refusal after it).
- **(2b) ALL.** Today's behaviour stays the default and the restriction is
  opt-in. Nothing breaks; the protection only reaches the authors who ask
  for it, which is not where a secret leak comes from.

Until both are answered, no slice of this ADR is implemented: the axes
decide the witness, and a witness written against the wrong axis would
have to be thrown away. The measurement above is the part that was owed
today.

## Consequences

- The ticket (iterion#1949) keeps a design half and an implementation
  half: this record IS the design half, and it is deliberately
  under-determined on two axes rather than silently resolved.
- `pkg/secrets` is shared with the PLIAGE gateway lane (iterion#2005):
  the gateway env rides a separate branch of `forwardableProviderEnv` and
  is never merged into `providerCredentialEnvVars` — one source of truth
  each, and the host sets a gateway node's egress policy explicitly.
  Whatever axis 1 settles on must not fold those two together.
- The path-rooted permission rules (iterion#1710) are the BELT, not the
  guarantee: a bot that reads untrusted input still gates its OUTPUT
  deterministically with a known-value scan, as `bots/prod-watch` does.
