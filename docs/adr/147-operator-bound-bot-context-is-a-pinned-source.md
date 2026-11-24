# ADR-147: Operator-bound bot context is a PINNED SOURCE under a kinded binding — the policy record names it, the launch resolves it, the attachment contract materialises it

- **Status**: Accepted (2026-10-29) on the clauses below, each backed by a measurement in the tree at `32eb34cc9`. The two product axes the measurement opened — the NAME a bound context arrives under, and the SIZE budget that picks its transport — were raised as ARB-91 of the debt-leap lane and arbitrated the same day (D6, D7). What stays undecided is named under *What is not promised*, and nothing behind it is coded.
- **Date**: 2026-10-29
- **Authors**: devthejo (session with Claude Opus 5), for [#1908](https://github.com/SocialGouv/iterion/issues/1908) — "bind reference files to a bot, not just secrets"
- **Landed by this change**: this record, and the one half of the materialisation contract that needed no decision — a manifest default the workflow references, with no `attachments/` directory to read it from, now fails the run instead of vanishing ([`pkg/runtime/bundle.go`](../../pkg/runtime/bundle.go), two witnesses in `pkg/runtime/bundle_attach_test.go`). **No store, no route, no wire, no studio surface ships with it**: those slices start on the arbitrated clauses, additively.
- **Relates to**: [ADR-144](144-shared-skills-are-bundle-dependencies.md) (the other "who owns the artefact, and what does declaring it mean" record) · [ADR-075](075-irref-fallback-for-oversized-cloud-queue-ir.md) (the blob reference this record's transport axis inherits) · [`pkg/pluginsource`](../../pkg/pluginsource/pluginsource.go) (the pinned-source record this one reuses) · [`pkg/secrets/bindings.go`](../../pkg/secrets/bindings.go) (the policy record this one kinds)

## Context

The ticket asks for reference material an **operator** controls, materialised
into every run of one bot, schedules included, without republishing the
bundle. Its premise — "bot bundles refuse binaries" — is stale, and the
correction the stream note carries ("the mechanism exists, only the
operator-side half is missing") is itself incomplete. What the tree actually
holds, measured:

**The author-side half exists, end to end.** `DirAttachments` names
`<bundle>/attachments` as "default binary inputs referenced from the
manifest's `attachments:` map" (`pkg/bundle/bundle.go:44-47`), the manifest
declares the map (`pkg/bundle/manifest.go:82-86`, path-validated against
absolute and `..` escapes by `validateAttachmentRelPath`), and
`promoteBundleAttachmentDefaults` (`pkg/runtime/bundle.go`) opens each
declared file, sniffs its MIME and writes it as a run attachment. Its single
caller is `runPromoteAttachments` (`pkg/runtime/engine_run.go:672`), called
unconditionally by `Engine.Run` right after admission
(`pkg/runtime/engine_run.go:197`) — so it reaches **every** run the engine
executes, which is what makes a scheduled launch no different from an
interactive one — and it runs *before* the host-side promote callback,
so a Launch-modal upload overrides a bundle default by re-writing the same
name.

**And it has an installed base of exactly zero.** No `manifest.yaml` in the
tree declares an `attachments:` map — not one, at any indent. Four bundles carry an `attachments/`
directory — `docs-refresh`, `sec-audit-source`, `secured-renovacy`,
`whats-next` — and all four are empty, tracked only by a `.gitkeep` that
`pkg/botscaffold` creates from `LayoutDirs`. The mechanism is complete and
unused. Two consequences matter for this decision: the declared-name
restriction below breaks nothing *today*, whichever way it is settled; and
the first operator-side binding will be the first real user of this path,
which is why its failure modes are worth more than its ergonomics.

**What is genuinely absent is the operator's half.** The material is the
bundle's, so changing it means republishing the bundle. The only durable
per-bot record an operator can write is `secrets.BotSecretBinding`
(`pkg/secrets/bindings.go:26-45`), and it is a **reference** record by
design: `SecretID` points at a stored `GenericSecret`, `SecretNameForWorkflow`
is the name the workflow declares, `AllowedHosts` only ever tightens an
egress policy — "It does NOT store secret material". Its tenancy predicate
(`bindableGenericSecretForBotBinding`) deliberately excludes personal
credentials, because a binding is shared org automation policy. That posture
is the right one to inherit, and it rules out an answer where the operator's
bytes live in the policy record.

**The pinned-source mechanism already exists, one layer away.**
`pluginsource.PluginSource` (`pkg/pluginsource/pluginsource.go:31-67`) is a
`(GitURL, Ref)` pair whose own comment writes the doctrine this record's
title claims — "PIN A TAG OR SHA in production: a moving branch turns every
launch into a network round-trip AND makes the skill change under the
operator's feet, which is the silent-drift failure this whole area exists to
prevent" — plus a read credential by reference (`SecretID`), an `Enabled`
gate, and a `DegradedReason`/`DegradedAt` pair written only through
`MarkDegraded`/`ClearDegraded`, so a source a launch could not resolve is
skipped *loudly* instead of failing every launch of the team. It carries a
Mongo twin and a memory twin of one `Store` interface. What it is not: it is
team-scoped (`TenantID`, no `BotID`), and its contributions are
`.claude/{skills,commands,agents}` markdown.

**The cloud wire for resolved contributions is sized for markdown.**
`queue.Contributions` (`pkg/queue/types.go:398-421`) carries
`Plugin []ContributionFile` and `Library []LibrarySkillFile` **inline**,
documented as "sizes are small (markdown, a few KB each) and capped at
publish time", with a `Degraded bool` that exists precisely because "the
runner pod cannot distinguish an amputated payload from a complete one by
inspecting it". Binary reference material does not fit that envelope, and the
tree's own answer for a payload that outgrows the message is a blob
reference (`BotBundleRef.SnapshotRef *IRRef`, ADR-075).

**One hole in the materialisation contract was this ticket's complaint
already happening.** A manifest that declared defaults the workflow
references, in a bundle with no `attachments/` directory, took the same early
`return nil` as a bundle with nothing declared: every default of that bot
disappeared from every run, with no error and not even a log line. The same
fault one file at a time — a declared path missing from a directory that
exists — already failed the run through `os.Open`. One fault, two opposite
outcomes, and the silent one had the wider blast radius. There is no
authoring-time check to fall back on either: `pkg/bundlelint` says nothing
about attachments.

## Decision

**D1 — The ticket's three options are not alternatives; they name three
layers, and each layer gets one answer.** "A kinded generalisation of the
secret binding", "a bot-scoped `PluginSource` sibling" and "a bot-level
attachment set" are a policy record, a material definition and a
materialisation path. Picking one and dropping the others is what made the
question look like an arbitration between rivals.

- **Policy** — who may give a bot context, and under which name: the
  **kinded bot binding**, on the surface the operator already uses
  (`/api/teams/{id}/bots/{bot_id}/bindings`, studio `BindingsTab`).
- **Material** — what the context *is*: a **pinned source** in the
  `PluginSource` sense, a `(url, ref)` resolved to a digest.
- **Materialisation** — how it reaches the agent: the **attachment
  contract** at `promoteBundleAttachmentDefaults`, the only path that
  already reaches every run of a bot, schedules included, with MIME
  sniffing and override-by-upload.

**D2 — One kinded binding, discriminated, with the per-kind payload in its
own struct.** `Kind` is a new discriminator; the existing
`BotSecretBinding` fields stay exactly where they are, as the `secret`
kind's payload. A record whose flat fields are conditional on its kind fails
the Nth-variant test (rule 2): the third kind would add an engine `if` at
every reader. **A document with no `kind` reads as `secret`** — that is the
whole migration, and it is a no-op: every binding stored today predates the
discriminator, and a client that never sends `kind` keeps the behaviour it
has. This is what makes the slices behind this record **additive**, not the
documented breaking change the stream was tagged with.

**D3 — "Pinned" is load-bearing, and a drifted pin is degraded, never
substituted.** A context binding records the ref it was given *and* the
digest a launch resolved; a resolution that cannot reach the pin, or that
finds the content changed under a moving ref, is marked degraded with its
reason, skipped by the launch that found it so, and visible as such — the
`PluginSource` discipline, not a fresh invention. An operator-uploaded blob
is admissible only as a source pinned by its own content digest.

**D4 — Precedence is author < operator < launch.** A bundle default is the
author's suggestion; an operator binding is policy for that deployment and
overrides it; a per-launch upload is an explicit human act for one run and
overrides both. The attachment contract already implements the second half
of that order (bundle defaults are promoted *before* the upload callback, so
an upload wins); the operator binding materialises between them.

**D5 — The materialisation contract reports what it cannot deliver.** A
default the workflow references and the bundle cannot produce fails the run,
whether one file or the whole directory is missing, and the refusal names
every undeliverable default in a stable order. Shipped with this record,
because the first operator binding must not be able to deliver nothing
quietly — and because it needed no answer to the axes below.

**D6 — The name stays the workflow's; the operator fills it** (ARB-91 axis A,
decided `C`). A context binding resolves under a name the bot's workflow
declares in its `attachments:` block — that block is what gives a name its
type and makes it referenceable as `${attachments.x}`, so a binding under an
undeclared name would deliver a file no node can reach. This keeps the first
slice strictly additive: no DSL change, no new workspace contract. The other
half of the fork — a bot's context as a second channel, files **mounted**
into the workspace the way plugin contributions are mirrored into `.claude/`
— is a workspace contract of its own and is owed its own record and ticket,
not a flag on this one.

**D7 — An oversized context rides by reference, not inline** (ARB-91 axis B,
decided `E`). The transport is a blob reference in the sense of ADR-075,
which is the answer this tree already gave to "the payload outgrows the
message" (`BotBundleRef.SnapshotRef`), with the `Degraded` discipline of
`queue.Contributions`: a payload the publisher could not fully enumerate says
so, because the runner pod cannot tell an amputated one from a complete one.
An inline cap was rejected: it manufactures the limit the operator discovers
in production.

## What is not promised

**The mounted channel.** D6 settles where the first slice resolves a bound
context, not what a bot's context may ultimately BE. A context the operator
gives a bot under a name the workflow never declared — mounted into the
workspace rather than referenced as an attachment — answers the ticket more
completely and costs a new workspace contract, probably a DSL surface, and a
precedence story against the plugin mirror. It is owed its own record and its
own ticket under this epic; no flag, no half-wiring and no "TODO when the
mount lands" belongs in the slices behind this one.

**The size number.** D7 picks the transport, not the budget: what a bot's
bound context may weigh is an operator-visible limit and a storage cost, and
the figure is set where the blob store's quota is set, not here.

Also not promised, and owed to their own tickets rather than to this record:
an authoring-time diagnostic for a manifest default no workflow name
references (`pkg/bundlelint` has no attachment rule today, and a new `Cnnn`
code owes the whole catalogue's validation), and the captured-source replay
path, which rebuilds a bundle without its manifest or its attachments
directory (`pkg/runview/workflow_source.go:164` populates only `prompts/`
and `presets/`), so a replayed run promotes no default at all.

## Consequences

- The slices of #1908 — the kinded store record and its cloud twin, the
  routes, the materialisation of a bound context, the studio surface — are
  **additive**: D2 removes the migration, D6 keeps the DSL out of it, and the
  `breaking` tag the stream carried does not survive the measurement.
- The record to generalise is `BotSecretBinding`, and the record to imitate
  is `PluginSource`. A context binding that invents its own pin semantics,
  its own degraded handling or its own credential-by-value is wrong on
  arrival.
- A bot whose manifest promises reference material it cannot deliver now
  fails at run start, naming the defaults and the missing directory. The
  installed base of that promise is zero, so nothing in this repository
  changes behaviour; a bundle outside it that was silently losing its
  defaults will say so.
- `pkg/bundlelint` gains no attachment rule here, so the authoring surface
  stays as permissive as it is today: the first signal an author gets is
  still a run-time one.
