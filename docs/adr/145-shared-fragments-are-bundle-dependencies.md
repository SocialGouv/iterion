# ADR-145: A shared fragment is a bundle DEPENDENCY, not a copy — `exports.fragments` declares it, the install materializes it under `lib/`, and the parser learns nothing

- **Status**: Accepted (2026-10-29) — on the clauses below, each backed by a record or a measurement already in the tree. What is named under *What is not promised* is **not** decided here: each axis is owed its own decision, taken when the slice that needs it starts. Arbitrated as ARB-75 of the debt-leap lane, on the shape [ADR-143](143-sandbox-read-back-contract.md) set and [ADR-144](144-shared-skills-are-bundle-dependencies.md) just applied: decide what the tree backs, name what it does not.
- **Date**: 2026-10-29
- **Authors**: devthejo (session with Claude Opus 5)
- **Landed by this change**: this record, and the one sentence of the authoring surface that owed it ([dsl.md](../dsl.md), the `import` section). **No code changes with it.** Owed by the slices behind it, in this order, each with its own witness: the manifest key with its per-index refusals and its release floor ([#1418](https://github.com/SocialGouv/iterion/issues/1418) lot 2 slice 1), the install-time materialization with its typed miss (slice 2), the `group goal(condition, cap)` exemplar (lot 3).
- **Relates to**: [ADR-094](094-shared-subbot-bundles-bot-uri.md) (shared subbot bundles — the two-sided key and the pin this record reuses) · [ADR-144](144-shared-skills-are-bundle-dependencies.md) (the same decision for skills; this one is its mirror for the other artefact kind) · [ADR-098](098-dsl-versioning-and-authoring-surface.md) §3 (the compilation unit an `import` defines) · [dsl.md](../dsl.md) (the authoring surface)

## Context

A `.bot` can already be several files. `import "lib/x.bot"` sits at the head
of a file and is held to two sets of rules: the portable ones, which need no
disk (`ImportPathError` — relative, `/`-separated, no `..`, no NUL, a `.bot`
file; [parser_import.go:59](../../pkg/dsl/parser/parser_import.go), refused as
`E045`, [diagnostic.go:47](../../pkg/dsl/parser/diagnostic.go)), and the one
only the disk settles: the resolved path stays under the bot's `lib/`
([unit.go:843](../../pkg/dsl/unit/unit.go)), where a symlink or a path that
resolves out is `ErrOutside` ([unit.go:586](../../pkg/dsl/unit/unit.go)).
The syntax has a release floor in the ONE table — `parser.ImportSince`
= `3.145.0` ([preamble.go:31](../../pkg/dsl/parser/preamble.go)), pinned into
`syntaxFloors` ([profile.go](../../pkg/bundle/profile.go)), so `validate`'s
C252 and the push admission ask for it together.

What that confinement deliberately cannot express is a brick shared BETWEEN
bundles: every path resolves under the importing bot's own `lib/`, so a second
bot that wants the same brick carries a copy. Lot 2 of #1418 said it would
wait for "a named second use".

**The named use is here, and it is not the one the ticket's own framing
expected.** Measured at `ba8b9990b`, by cutting each tracked `.bot` under
`bots/` at its top-level declarations and hashing each body:

- **Fragment FILES do not duplicate.** The 9 production fragments
  (`bots/{feature-dev,gitops-warden,review-pr}/lib/{schemas,prompts,nodes}.bot`,
  4 757 lines) hold **9 distinct contents** — not one byte-identical pair.
  Same-named files are unrelated: the largest cross-bot overlap of unique
  non-trivial lines is 42 of 695 (`nodes.bot`, gitops-warden vs review-pr),
  and half of that is Python boilerplate (`try:`, `except`, `continue`).
- **Named DECLARATIONS duplicate heavily.** Over the 51 files outside
  `bots/testdata/`: **176 names carried by more than one file, 714 copies,
  570 distinct bodies**. Of those names, **59 have a byte-identical group
  across different files, covering 210 copies** — copies a dependency can
  retire with no CONTENT reconciliation, the bytes having already agreed
  (the extraction itself stays a reviewed change per bot, below). The largest groups:
  `schema plan_revise_input`, `schema plan_output`, `schema forge_auth_state`
  and `schema plan_topology_state` in **7** identical copies each,
  `schema verify_plan` and the three `plan_review_*` schemas in 6,
  `tool forge_auth_probe` in 5; the largest identical BODY is
  `tool verify_run` (280 lines), in `bots/feature-dev/lib/nodes.bot` and
  `bots/instrument/main.bot`. The `supply-shield` / `supply-shield-cve` pair
  alone shares some twenty identical declarations.

So the brick to share is the **declaration cluster**, and sharing is a
property a bundle has to express — which three more facts constrain:

- **The key already exists, on both sides.** `BundleExports`
  ([manifest.go:506](../../pkg/bundle/manifest.go)) carries `Workflows`
  only, with `BundleDependencies` (`:519`) as the consumer-side allow-list —
  "a workflow can only resolve a shared bundle that its own manifest
  declares" — and ADR-144 has just added `Skills` to the same pair.
  `exports.fragments` is a FIELD on a live key, not a new key, and
  `decodeManifest` reads with `yaml.UnmarshalStrict`
  ([manifest.go:1125](../../pkg/bundle/manifest.go)), so an older binary
  refuses it loudly. No shipped manifest declares `exports:` or
  `dependencies:` today — measured over the 38 `bots/` and 3 `examples/`
  manifests — so a floor is owed by adopters and by nobody else.
- **The run record already carries every fragment's bytes.**
  `store.WorkflowSourceFile{Path, Text}`
  ([run.go:2393](../../pkg/store/run.go)) is filled by `sourcesOf`
  ([engine_run.go:116](../../pkg/runtime/engine_run.go)) with "every file of
  the unit the run executed — the main and the fragments its imports reach".
  The cap is **1 MiB for the whole unit** (`maxPersistedWorkflowSource`,
  `engine_run.go:32`) and past it **nothing** is recorded — `rewind --auto`
  unavailable, `fork --new-inputs` refused. The largest unit today is
  `golden-master` at **606 016 bytes**, 58 % of that cap.
- **The transport carries files, and refuses symlinks.**
  `Snapshot.AddDir` walks the bundle directory and errors on any symlink
  ("not portable", [snapshot.go:63](../../pkg/bundle/snapshot.go)); the cloud
  snapshot freezes `lib/` with the rest (dsl.md). A materialized fragment
  needs no transport of its own, and may not be a link.

## Decision

1. **A shared fragment is a dependency, declared on BOTH sides.** The
   producer exports it, the consumer declares what it may resolve and under
   which name its `import` lines reach it:

   ```yaml
   # producer
   exports:
     fragments:
       - id: forge-auth
         path: lib/forge-auth.bot
   # consumer
   dependencies:
     fragments:
       - name: forge-auth     # the name an `import "lib/<name>.bot"` reaches
         from: app-dev        # the bundle that exports it
         id: forge-auth       # defaults to name
   ```

   This is `exports.workflows` + `dependencies.workflows` (ADR-094) and
   `exports.skills` + `dependencies.skills` (ADR-144) for the third artefact
   kind — same two-sided shape, same allow-list property, `.bot` under `lib/`
   where the skill contract says `.md` under `skills/`. A producer-only key
   cannot express consumption, and the consumer is the side that must stop
   carrying bytes.

2. **Resolution is a materialization at install, into the consumer bundle's
   own `lib/`, by COPY.** The install writes the resolved fragment where a
   shipped copy would have been, so **the parser, the unit loader, `E045` and
   the `lib/` confinement learn nothing**: the materialized fragment is an
   ordinary file under `lib/`, `import "lib/<name>.bot"` keeps the meaning it
   has today, and every surface that reads the unit — `validate`, `run`,
   `resume`, `fork`, `rewind --auto`, the dispatcher, the studio, the cloud
   editor, the registry, the catalog — keeps reading what it reads now.
   Teaching each of them about dependencies would be the engine `if` the
   modularity rule exists to prevent. A symlink is not an option: the
   snapshot refuses one, and `ErrOutside` refuses it at load. A dependency
   may not SHADOW: a name that collides with a fragment the bundle itself
   ships, or with another dependency, is refused at install rather than
   overwritten — a silent overwrite would make `import "lib/x.bot"` mean one
   thing in the repository and another after an install, which is the drift
   this record exists to end.

3. **No new diagnostic code, because the refusal does not belong to the
   parser.** A dependency that cannot be resolved is a typed error at
   INSTALL, naming the bundle, the fragment and every layer looked in — the
   `ResolveExtraSkills` shape ADR-144 adopts. At parse time the file is
   either there (and `E045`/`E046`/`E047` mean exactly what they mean today)
   or absent, and `E046` would then say "no such fragment" about a file the
   author never wrote: a true sentence pointing at the wrong actor. The
   install is where the dependency exists, so it is where the miss is named.

4. **Reproducibility rides the record that exists.** `workflow_sources` is
   **unchanged**: a materialized fragment is a file of the unit, so its bytes
   are recorded by the path that records the others, and a per-fragment
   digest is a DERIVATION over that record (views are additive) rather than a
   new durable field — which is why **no cloud twin is owed by this record**.
   What the cap owes is named below, and it is a decision about the cap, not
   about sharing.

5. **The pin comes from the mechanisms that already distribute bundles** —
   `botinstall` / `botregistry` / `marketplace` / `botsource`, pinned the way
   ADR-094 pins a shared workflow bundle (exact version and content hash).
   Git-native first: no new fetcher, no second distribution system, and no
   lock file of this record's own invention.

6. **The release floor goes through the ONE table, which learns to read the
   manifest — once.** `parser.ImportSince` pins the import SYNTAX; a floor a
   manifest KEY asks for is what `syntaxFloors`' `need(SyntaxRequirements)`
   cannot express, which is ADR-144 clause 6's widening. `exports.fragments`
   asks for the same widening and must not hand-roll a second check: whichever
   of the two keys lands first builds the seam, the other declares its pin in
   it.

7. **Every falsifiable claim above is owed by a named unit**, because this
   record proves nothing by itself: slice 1 proves 1 and 6 (decode, per-index
   refusals, the floor), slice 2 proves 2, 3 and 5 (materialization, the typed
   miss, the pin), and lot 3's exemplar proves that a consumer bot compiles
   and runs with a fragment it does not carry.

## Rejected alternatives

- **A new import form resolved through the registry** —
  `import "bot://<id>/lib/x.bot"`, resolved at compile time, with a new
  diagnostic for an unresolved library and a resolved digest per fragment in
  `workflow_sources`. It is the shape the wave's own table described, and it
  is refused on four measured grounds. It is a **second sharing mechanism**
  for a third artefact kind, recorded hours after the first (ADR-144) — the
  Nth-variant test says build the seam, not the next variant. It breaks
  `E045`'s contract ("a path into `lib/`") for every reader that quotes it,
  and prose, hints and tests all quote it. It adds a durable field and so
  owes a cloud twin in the same change, for information the record already
  holds (Context). And it puts the shared library's bytes inside the
  CONSUMER's compiled unit, against the 1 MiB cap past which nothing is
  recorded at all — with the largest bot already at 58 % of it, that is a
  reproducibility regression dressed as a reproducibility feature. A
  materialized dependency, by contrast, records exactly what the copy it
  replaces recorded: the cap moves neither way.
- **A producer-only `exports.fragments`.** It reads like the smaller change
  and cannot do the job: with no consumer declaration there is no allow-list,
  so a bundle would resolve a name from any installed bundle that happens to
  export it. The workflow contract settled that already.
- **Relaxing the `lib/` confinement** — letting a path reach a sibling bot
  (`../other-bot/lib/x.bot`) or follow a symlink into a shared directory. The
  confinement is a boundary, not an inconvenience: `ErrOutside` at load, a
  symlink refused by the snapshot as unportable, and every escape case pinned
  by a test today (`TestAnImportPathIsHeldToItsRules`,
  [import_test.go](../../pkg/dsl/parser/import_test.go);
  "outside the bot's `lib/`", [unit_test.go:125](../../pkg/dsl/unit/unit_test.go)).
  A path that escapes also travels: the bundle that resolves on one disk
  fails in the snapshot, in cloud, and in any consumer that installed it.
- **Declaring the duplication and stopping there** — a `skillshare`-style
  inventory of duplicated declarations, with a gate on drift. It would keep
  714 copies honest, which is worth having and is what #2357 shipped for
  skills, but every consumer still carries the bytes and a shared fix is
  still N edits. A gate measures drift; it cannot remove a copy.

## What is not promised

- **Sharing a declaration that lives in the middle of a file.** A dependency
  shares a FILE; the 210 byte-identical copies measured above sit inside
  `main.bot`s and 1 300-line fragments. Extracting a cluster into a fragment
  of its own is a reviewed change per bot, with its own witness, never a bulk
  rewrite — the same clause ADR-144 makes about flattening eleven copies of
  one skill. The measurement is the ceiling of what sharing could retire, not
  a migration plan.
- **A per-fragment digest field, and what happens past the 1 MiB cap.** The
  digest is derivable today for every unit under the cap; past the cap the
  record holds nothing, and a digest list (tens of bytes per file) is exactly
  what would survive there. Whether `workflow_sources` gains that field — and
  whether a run that busts the cap should record digests instead of nothing —
  is one decision about `maxPersistedWorkflowSource`, owed by its own ticket,
  and deliberately not smuggled into this lane.
- **A token inside a shared fragment.** Substituting a per-bundle value would
  make a fragment a TEMPLATE, and `{{ }}` already belongs to the DSL: the same
  axis ADR-144 leaves open for skills, with the same consequences
  (delimiters, escaping, which reader expands, what `validate` lints). Until
  that decision, a dependency shares only fragments that are shareable
  verbatim.
- **A cloud store of fragment CONTENT.** What is durable is the run's own
  record of the files it executed; the server plane is a parallel mechanism,
  never the place a shared fragment's bytes come from when the tree has them.
- **Lot 3's mechanism.** The exemplar is `group goal(condition, cap)` and
  nothing more: no DSL `goal:` field, no engine API, and no stop condition
  that lets a model re-read its own transcript — its evaluator has no tool
  access, and LLM-judges-on-transcript were ruled out for high-stakes bots as
  gameable. That arbitration stands and is not relitigated here; the judge in
  the exemplar runs a deterministic check.

## Consequences

- The authoring surface now says where sharing lives, so the next author who
  wants a brick from another bot reaches for a manifest key instead of a
  `..`. That sentence is prose: nothing executable depends on this file.
- A bundle that adopts a dependency STOPS carrying the file, so the
  duplication measured here shrinks by one copy per consumer that migrates —
  and the 59 byte-identical names are where that costs no editorial judgment.
  Progress is the copies gone, not a count in a report.
- `exports.fragments` and `exports.skills` now owe the SAME floor-table
  widening. Landing it twice would be the second hand-rolled check clause 6
  refuses; the second key to land declares its pin in the seam the first
  built.
- #1418 stays open until lot 3: this record is lot 2's first slice, and a
  record is not a capability.
