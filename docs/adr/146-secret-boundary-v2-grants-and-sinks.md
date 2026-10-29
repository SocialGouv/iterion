# ADR-146: Secret boundary v2 — a secret is granted to a sink, and what crosses a node boundary is a separate question

- **Status**: Accepted — the three axes below are decided by the operator (2026-10-29, debt-leap ARB-110, question #37 of the fleet's question file). The slices land in the order S3 → S2 → S4, each with its own witness; this record is amended last to the state as implemented.
- **Date**: 2026-10-29 (proposed and accepted the same day; the axes were measured first, then decided)
- **Authors**: devthejo (session with Claude Opus 5 for the measured draft, session with Claude Fable 5.1 for the decision), orchestrator-arbitrated — debt-leap ARB-78 of 2026-10-29: "C — ADR-146 en `Proposed`, les deux axes contestés nommés comme questions ouvertes ; zéro code S2/S3", then ARB-85 (axis 3 added), then ARB-110 (the three axes decided by the operator in session)
- **Landed by this change**: the decision. The record was first landed as `Proposed` with the measurements below and nothing implemented; this amendment fills the three axes and names what each slice delivers. The slices behind it (per-secret `sinks:` grants in the DSL, the known-value redaction at the model boundary, the sandboxed-run launch refusal) each arrive with their witness, S3 first.
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

Three measurements decide what v2 can and cannot say.

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

**No declared secret enters a sandboxed container's environment in
clear.** A declared `secrets:` value reaches a sandboxed run through Layer 1
at the moment of execution — a tool node's `command:` is materialised on the
host into the environment of the `docker exec` / pod exec that runs it
(`MaterializeAuthoredShellEnv`), a `claude_code` tool input through the
host-side `PreToolUse` hook — through Layer 2 on the wire, or through the
file channel; a sandboxed `claw` materialises nothing in-container, because
the materialiser is a host closure and closures do not cross the crossing
(`pkg/backend/delegate/io.go`). The ONLY plaintext the container's
environment carries is the PROVIDER credential the in-container runner
spends to talk to its model: `forwardableProviderEnv` for `claw`
(`pkg/backend/model/claw_backend.go`, with the separate gateway branch of
iterion#2028) and the credential env of `claude_code`
(`pkg/backend/delegate/claude_code_creds.go`). That credential is
`RedactOnly` in the guard — Layer 2 never substitutes it — and an
OAuth-forfait credential cannot be substituted at all (the CLI refreshes it
statefully; [secrets.md](../secrets.md#limitation-oauth-forfait-credentials-are-not-substituted)).

## Decision

**A secret is granted to the sinks it may reach, and the grant is part of
the secret's declaration.** The unit of the boundary is the pair
`(secret, sink)`, not the secret alone: `sinks:` beside `hosts:` in the
secret's block names the materialization kinds the secret is allowed to
reach, and a reference that reaches an ungranted sink is a typed refusal at
COMPILE time, carrying the sink it named and the grant that would allow it —
the same shape as C150/C151, one rung more precise.

The sinks, one per materialization kind:

| sink | what reaches it | enforced |
|---|---|---|
| `prompt` | a prompt body renders the reference as the placeholder: the model HOLDS the placeholder from then on | compile time, on the reference |
| `agent` | the placeholder a model emits in its own tool input is materialised (claude_code's `PreToolUse` hook, claw's in-process tool loop) | run time, by the guard |
| `command` | a tool node's authored `command:`, `postcondition:` or `script:` is materialised before exec | compile time, on the reference |
| `params` | a tool action's `params:` value is materialised before the vendor call | compile time, on the reference |
| `file` | the secret is written as a file the run mounts (`as: file`) | compile time, on the declaration |
| `egress` | the placeholder is substituted on the wire toward `hosts:` (Layer 2) | run time, by the egress rewriter |

Three consequences follow, in the order they can be built:

1. **The compiler is where an ungranted sink dies.** The refusal is a
   diagnostic on the reference, not a runtime check: a run that paid for a
   workspace and a model call to learn a secret may not reach a prompt has
   already told the model the secret exists. The two sinks the compiler
   cannot see — a placeholder a MODEL emits, a request on the wire — are
   honoured by the guard from the same declaration: one source of truth
   (`ir.Secret.Sinks`), carried into `secretguard.Secret`, never a second
   list.
2. **The known-value redaction stays one choke point per boundary.** v2
   adds no second redactor: the boundary it extends to is named by axis 1
   below, and that boundary reuses `Guard`'s known values — never a
   parallel matcher, which would drift (the lesson of `pkg/errtrack`:
   extend the tracker, never add a second one).
3. **A sandboxed run refuses, at launch and typed, a grant it cannot
   honour.** When a declared secret's grants can be served by neither the
   egress-substitution path nor the file-secret path on the run's sandbox
   — and by no other grant — the launch FAILS with a typed error naming the
   secret and the two paths, instead of running with a placeholder nothing
   resolves. Backend parity is part of the decision: the check sits in the
   runtime above the backend choice, so `claude_code`, in-process `claw`
   and sandboxed `claw` reach the same verdict, and a grant a backend
   cannot serve is typed-refused rather than silently unresolved.

## The three axes — decided by the operator (ARB-110, question #37)

**Axis 1 — which boundary redacts a known value that re-enters a model's
context? Decided: (1a) the model boundary alone.** A sentinel printed by
node A can re-enter node B's context through a tool result or an
`{{outputs.A}}` reference. The redaction happens where the model READS:
the rendered text of every prompt (`ClawExecutor.resolveTemplate`, the one
rendering path above both backends, and the user-content walker beside it)
turns known values back into their placeholders, as the far-side relays
already do (an echoing tool's result, a background task's label, the
stream). The data a downstream node consumes programmatically keeps its
value: the `node_output.go` invariant holds, the PLIAGE shape works, and
the redaction follows the Layer 1 switch — `ITERION_SECRETS_PLACEHOLDERS=off`
renders values by the operator's explicit choice, and the model boundary
renders them too. The workspace readers (`Read`, `Bash`) keep what the
workspace holds, the documented exception of v1. Rejected: (1b) the crossing
itself — strictly stronger, and it breaks the invariant above and every
workflow that passes a credential between nodes, a documented rupture with a
compat mode for a protection the model boundary already gives.

**Axis 2 — what is the default grant for a secret that declares no
`sinks:`? Decided: (2b) ALL, the restriction is opt-in.** Today's behaviour
stays the default: a secret without `sinks:` reaches every sink, exactly as
before this record, and nothing existing breaks — no bot of the catalog,
no bundle on iterion.cloud declares `sinks:` today. The protection reaches
the authors who ask for it, and the ask is short: `sinks: [prompt, egress]`
keeps a token out of every command line and every container (the model
holds the placeholder, only the proxy resolves it), `sinks: [command]` keeps
a password away from every model. Rejected: (2a) NONE — safe by
construction and BREAKING for every existing bundle, which needs a
one-release compat mode (a C1xx naming the sinks a workflow uses, grants
inferred for that release, refusal after it); the path to NONE is documented
as a possible future default, not a promise, and a leak does not come from an
authored reference site in the first place.

**Axis 3 — what channel delivers a secret to a SANDBOXED run, and may the
launch refuse? Decided: (3b) the documented status quo for the provider
credential, the refusal opt-in through `sinks:`.** The plaintext channel is
not an oversight, it is the MECHANISM by which a sandboxed node spends the
credential the engine resolved for it (the measurement above), and the
OAuth forfaits have no other channel at all. So the provider credential
keeps crossing in the container's environment, documented with its reason;
"provider API keys crossing as placeholders the proxy substitutes toward the
provider's host" is a follow-up with its own ticket, not this record. What
S4 refuses is a declared secret the run cannot serve as granted: `egress`
granted on a run whose sandbox substitutes nothing (no sandbox, or TLS
inspection off, or a driver without inspection), `file` granted without a
real driver, `agent` granted on a sandboxed `claw` (its in-container runner
materialises nothing) — each a typed launch error naming the secret and the
paths, never a run that carries a placeholder nothing resolves. Rejected:
(3a) refuse by default with an explicit opt-out — it refuses every sandboxed
node that spends a provider credential (the PLIAGE gateway lane
iterion#2005 and every sandboxed bot carrying a key), and the OAuth forfaits
would have to opt out by construction, which annuls the rule.

## How it lands

- **S3 — grants in the DSL** (`pkg/dsl/spec`, `ast`, `parser`, `ir`, the
  printers): `sinks:` is a list beside `hosts:`; an unknown sink name is a
  compile error; `as: file` with a `sinks:` list that lacks `file`, or
  `file` granted on a value secret, is a compile error; a reference in a
  prompt body, a tool `command:`/`postcondition:`/`script:` or an action
  `params:` whose secret does not grant that sink is a compile error on the
  REFERENCE, naming the sink and the grant. The guard carries the grants:
  `agent` gates `Materialize` of model-emitted tool input, `egress` gates
  the egress rewriter. A placeholder a MODEL wrote into data that a tool
  body renders (`{{outputs.x}}`, `{{input.x}}`) is the agent sink's whatever
  body it lands in: defused before the body's materialiser, so `sinks:
  [command]` keeps a secret off every model's path, not only off its
  prompts. Witness: a bundle referencing a `sinks: [agent]` secret in a
  `command:` is refused at compile time; mutating the grant check reddens
  the test on its assertion.
- **S2 — the model boundary**: the rendered prompt text goes through
  `Guard.Unmaterialize` (known values → placeholders) under the Layer 1
  switch, at the one rendering path. Witness: a sentinel a tool node prints
  reaches node B's prompt through `{{outputs.A.stdout}}` as the placeholder
  and never as the value — on `claude_code` and on `claw`, one `NodeExecutor`
  builds both tasks; mutating the boundary reddens the prompt assertion.
- **S4 — the launch refusal**: one check in the runtime, above the backend
  choice, run on every launch path (a child adopting its parent's sandbox
  included) and never on a dry run; a typed error carrying every refused
  secret and the paths. What a run serves: `command`, `params`, `file`
  always; `egress` on an inspecting sandbox; `agent` through a claude_code
  agent or judge node, or in-process claw unsandboxed. Witness: a sandboxed
  bundle with a `sinks: [egress]` secret on a sandbox that cannot inspect is
  refused before anything starts; mutating the check reddens the test on
  its typed-error assertion.

## Consequences

- The ticket (iterion#1949) keeps a design half and an implementation
  half: this record IS the design half, decided on its three axes; the
  implementation half lands as the three slices above, S3 first, and the
  record is amended to the implemented state when the last one lands.
- What landed before the axes: the engine's recorded failure text now goes
  through the same Layer-0 matcher as the event path (iterion#2037,
  `pkg/runtime/failure_record.go`), and the redactor every backend receives is
  the SAME one — one executor builds every task, so `claw` in-process,
  sandboxed `claw`, `claude_code` and `pi` cannot drift apart. The decided
  axes keep that.
- `pkg/secrets` is shared with the PLIAGE gateway lane (iterion#2005):
  the gateway env rides a separate branch of `forwardableProviderEnv` and
  is never merged into `providerCredentialEnvVars` — one source of truth
  each, and the host sets a gateway node's egress policy explicitly. Axis 1
  as decided touches neither: the crossing keeps its values and the provider
  env keeps its channel.
- The default ALL means `sinks:` is additive: a `.bot` that never writes it
  compiles and runs as before. An older binary reading a `.bot` that writes
  it refuses the unknown property, as it refuses any property it predates.
- A bot-secret binding or the cloud transport may NARROW a secret's sinks
  the way it narrows `hosts:` (never broaden); that narrowing is a follow-up
  on the binding store, not part of these slices.
- Two bounds the adversarial rounds measured and this record states: the
  clone credential of a cloud run (the stored `forge_token` / `gitlab_token`
  / `github_token` the runner writes to the workspace's git credential
  store) is the runner's channel and the grants do not govern it — a
  follow-up names it; and the model boundary leaves the ambient name
  heuristic's values (`GIT_AUTHOR_EMAIL` matches `AUTH`) alone, since a
  placeholder in a prompt there breaks the commit the agent was asked for.
- The path-rooted permission rules (iterion#1710) are the BELT, not the
  guarantee: a bot that reads untrusted input still gates its OUTPUT
  deterministically with a known-value scan, as `bots/prod-watch` does.
