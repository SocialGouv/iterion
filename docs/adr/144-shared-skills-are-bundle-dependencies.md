# ADR-144: A shared skill is a bundle DEPENDENCY, not a copy — `exports.skills` declares it, the install materializes it, and a miss is typed

- **Status**: Accepted (2026-10-29) — on the clauses below, each backed by a record or a measurement already in the tree. The two axes named under *What is not promised* are **not** decided here: each is owed its own decision, taken when the slice that needs it starts. Arbitrated as ARB-70 of the debt-leap lane, on the shape ADR-143 set: decide what the tree backs, name what it does not.
- **Date**: 2026-10-29
- **Authors**: devthejo (session with Claude Opus 5), on the split ARB-8 of the debt-leap lane decided on 2026-10-28: "#2357 closes on the git-native half; the `exports.skills` primitive becomes a NEW ticket — ADR first" ([#2763](https://github.com/SocialGouv/iterion/issues/2763))
- **Landed by this change**: this record, and the two prose sites that owed it — [`internal/skillshare/families.go`](../../internal/skillshare/families.go) and [skills.md](../agents/bots/skills.md), which both said "owes an ADR first". **No code changes with it.** Owed by the slices behind it, in this order, each with its own witness: the manifest key with its per-index refusals and its release floor (#2763 unit 2), the install-time resolution with its typed error (unit 3), the cloud twin (unit 4).
- **Relates to**: [ADR-059](059-skill-library.md) (the skill library, and the precedence this record does not touch) · [ADR-094](094-shared-subbot-bundles-bot-uri.md) (shared subbot bundles — the sharing contract this one mirrors, for the other artefact kind) · [skills.md](../agents/bots/skills.md) (the authoring surface) · [`internal/skillshare`](../../internal/skillshare/families.go) (the inventory of what sharing has to express)

## Context

The catalogue duplicates skills, and the duplication is now *declared*
rather than silent: `internal/skillshare` names every skill file more than
one bundle carries, `task skills:sync` writes the mirrors, and a gate
reddens on a drifted mirror, a collapsed variant, an unreviewed edit, a
vanished copy or an undeclared shared skill (#2357, the git-native half).

Measured at `057d69c17`, by the declaration and independently by a walk of
`bots/*/skills/` + `examples/*/skills/`, which agree: **20 shared file
names over 72 copies** — 20 sources, 21 mirrors, 31 declared variants.
(The scouting note that said 34 variants / 75 files is wrong; the ticket's
own 72 / 31 is right.) `verify-build.md` alone exists in 11 copies.

What the declared half deliberately does NOT do is make sharing a property
of the bundle FORMAT. A bundle cannot consume a skill it does not own, so
every consumer carries bytes, and the gate's job is to keep 72 copies
honest rather than to let there be fewer.

Four facts shape what the primitive can be:

- **The key already exists.** `Exports BundleExports`
  ([manifest.go:239](../../pkg/bundle/manifest.go)) carries `Workflows`
  only (`:506`), with `BundleDependencies` as the consumer-side allow-list
  (`:518`) — "a workflow can only resolve a shared bundle that its own
  manifest declares". `exports.skills` is therefore a FIELD on a live key,
  not a new key.
- **An older binary refuses it loudly, not silently** — measured, not
  assumed. `decodeManifest` reads with `yaml.UnmarshalStrict`
  ([manifest.go:1125](../../pkg/bundle/manifest.go)), and a probe through
  `DecodeManifest` on this very tree answers a field nested inside a key it
  knows with `line 3: field skills not found in type
  bundle.BundleExports`, exactly as it answers an unknown top-level key.
  What is missing is not safety, it is the actionable sentence: "this
  bundle needs iterion >= X", which is what a release floor says.
- **The floor table cannot express a floor a MANIFEST key asks for.**
  `syntaxFloors` ([profile.go:485](../../pkg/bundle/profile.go)) is "the
  ONE list — `RequiredRelease` reads it, `SyntaxFloors` projects it", so a
  syntax in it is asked for by `validate`'s C252, the push admission, the
  scaffold and the release test at once. Its `need` reads
  `SyntaxRequirements`: the `.bot` SOURCES. That is why `category`/`tags`
  are held by a constant in a *test*
  (`catalogTaxonomySince = "3.225.0"`, [floor_release_test.go:179](../../pkg/bundle/floor_release_test.go))
  which no product reader consults — a catalogue guard, not a contract.
- **Precedence and the typed miss are already decided.** Skills resolve
  bundle > plugin > library > hand-authored
  ([ADR-059](059-skill-library.md); the statement lives at
  [engine_run.go:825](../../pkg/runtime/engine_run.go) and
  [library_skills.go:25](../../pkg/runtime/library_skills.go)), and
  `ResolveExtraSkills` ([library_skills.go:259](../../pkg/runtime/library_skills.go))
  is the precedent for refusing a miss at launch: "deliberately stricter
  than the workflow's own `skills:` reference, which is soft", because
  dropping a list a human typed "reproduces exactly the silent failure
  this whole seam exists to avoid".

## Decision

1. **A shared skill is a dependency, declared on BOTH sides.** The
   producer exports it, the consumer declares what it is allowed to
   resolve and under which name its `skills:` lists reference it:

   ```yaml
   # producer
   exports:
     skills:
       - id: verify-build
         path: skills/verify-build.md
   # consumer
   dependencies:
     skills:
       - name: verify-build   # the name a `skills:` list references
         from: app-dev        # the bundle that exports it
         id: verify-build     # defaults to name
   ```

   This is `exports.workflows` + `dependencies.workflows` for the other
   artefact kind — same two-sided shape, same allow-list property, `.md`
   where the workflow contract says `.bot`. A producer-only key cannot
   express consumption, and the consumer is the side that must stop
   carrying bytes.

2. **Resolution is a materialization at install, into the installed
   bundle's own skills directory.** The install writes the resolved
   content where a shipped copy would have been, so every downstream
   reader keeps reading what it reads today: the run-start mirror
   ([bundle.go:460](../../pkg/runtime/bundle.go)), the install's skill
   count ([install.go:181](../../pkg/botinstall/install.go)) and
   `validate`'s skill-file lints (C231–C234). Teaching each of them about
   dependencies would be the engine `if` the modularity rule exists to
   prevent.

3. **The bundle it resolves FROM comes through the mechanisms that already
   distribute bundles** — `botinstall` / `botregistry` / `marketplace` /
   `botsource`, pinned the way [ADR-094](094-shared-subbot-bundles-bot-uri.md)
   pins a shared workflow bundle (exact version and content hash in the
   project lockfile). Git-native first: no new fetcher, no second
   distribution system.

4. **A miss is a typed error at install, naming the bundle, the skill and
   every layer that was looked in** — never a silent empty skills
   directory. `ResolveExtraSkills` is the shape; the failure it refuses is
   the one `bots/catalog_skill_refs_test.go` describes for in-bundle names
   ("the bot still answers — it just answers from the model's own priors",
   which reads as "the bot got dumber" rather than as a broken
   reference). The witness asserts on the EFFECT (the installed skills
   directory is short by one file) as well as on the error: the install's
   exit code is the screen, not the effect.

5. **Precedence is unchanged, and a dependency may not shadow an
   operator-attached name.** A materialized dependency lives in the
   BUNDLE tier, so ADR-059's order needs no amendment and no fifth layer
   appears. The consequence is a rule the catalogue already states for
   shipped skills and must now state for declared ones: a catalogue bot
   may not depend on a name the operator attaches — the allow-list at
   [catalog_skill_refs_test.go:37](../../bots/catalog_skill_refs_test.go)
   carries the reason per entry, and that deliberate choice ("a bot may
   reference a skill it does NOT ship, so the operator supplies it") must
   not turn into an install failure or a silent shadow.

6. **The release floor goes through the ONE table, which learns to read
   the manifest.** `exports.skills` / `dependencies.skills` get a pin in
   `syntaxFloors` with its `releaseNotesMarkers` entry, and the table's
   `need` widens past `SyntaxRequirements` to see the manifest — so one
   declaration is asked for by `validate` C252, the push admission, the
   scaffold and the release test at once, instead of by a fourth
   hand-rolled check. The widening is additive: `RequiredRelease(req)`
   keeps its signature for the callers that have only sources
   (`pkg/botscaffold`, `pkg/cli/dsl_migrate`), and the manifest arrives as
   a second input on the path that has one — a floor nobody can ask for is
   the defect, not the arity. The floor is owed ONLY by a manifest that carries
   the key: adopters pay it, nobody else (no shipped manifest declares
   `exports:` or `dependencies:` today — measured over the 38 `bots/` and
   3 `examples/` manifests the floor walk reads).

7. **Every falsifiable claim above is owed by a named unit**, because this
   record proves nothing by itself and saying otherwise would be the
   tautology the ticket is trying to avoid: unit 2 proves 1 and 6 (decode,
   per-index refusals, the floor), unit 3 proves 2, 4 and 5 (resolution,
   the typed miss, the allow-list extension), unit 4 proves the twin.

## Rejected alternatives

- **`contributes: skills` on `plugin.yaml`.** The plugin system already
  contributes skills — `Contributes.Skills` ([detail.go:52](../../pkg/plugin/detail.go)),
  and `SynthesizeSkillsManifest` even builds a skills-only plugin from a
  directory — so "publish the shared skill as a plugin" is a real option
  and has to be refused for a reason. A plugin is what an OPERATOR
  installs, out of band, to extend the runtime; it sits in its own tier
  BELOW the bundle in ADR-059's order. A catalogue bot's own playbook is
  not an operator choice: the bot cannot answer without it. Making it one
  turns "the bot works" into "the operator installed the right plugin",
  which is precisely the exception
  [`operatorAttachedSkills`](../../bots/catalog_skill_refs_test.go) carves
  out per entry, with its reason, against the rule that catalog bots are
  repo-agnostic. It also loses the pin: a plugin's skills are not
  addressed by the consuming bundle's version and content hash, so "which
  version of the shared logic did this run execute" — ADR-094's opening
  complaint — stays unanswerable. Same shape as ADR-094's own refusal of
  `contributes: workflows`: the runtime-extension system must not become
  a second distribution system.
- **A producer-only `exports.skills`.** It reads like the smaller change
  and cannot do the job: the consumer is the side that must stop carrying
  bytes, and with no consumer declaration there is no allow-list, so a
  bundle would resolve a name from any installed bundle that happens to
  export it. The workflow contract already settled that ("a workflow can
  only resolve a shared bundle that its own manifest declares").
- **Declaring the duplication and stopping there** (the status quo of
  #2357). It holds 72 copies honest, which is worth having and is why it
  shipped first, but every consumer still carries the bytes and a shared
  fix is still N edits. The gate measures drift; it cannot remove the
  copies.

## What is not promised

- **A per-bundle token inside a shared skill's body.** The prize is real
  and smaller than the ticket's estimate: measured by diffing the bytes,
  **7 of the 31** declared variants differ from their source by exactly one
  line on each side — the `[[bundle]]` self-reference to their own
  orchestrating playbook. They are the `lang-go.md`, `lang-generic.md` and
  `lang-py.md` families mirrored into `supply-shield` and
  `supply-shield-cve`, plus `diff-scope.md` into `supply-shield-cve`
  ([families.go](../../internal/skillshare/families.go)); one more variant
  (`package-cache.md` into `supply-shield-cve`) carries that
  self-reference alongside genuine content, so a token would shrink it
  rather than retire it.
  Substituting a token would make a skill a TEMPLATE — a new authoring
  surface: delimiters (`{{ }}` already belongs to the DSL), escaping,
  which reader expands it, what `validate` lints, what the sync gate then
  compares. Until that decision, those copies stay declared variants and
  `exports.skills` shares only documents that are shareable verbatim. A
  copy whose generated region `iterion bots regen-catalog` rebuilds
  (`iterion-bot-catalog.md` into `issue-triage`) can never be a
  byte-mirror and is out of scope either way.
- **A cloud store of skill CONTENT.** There is none today: `pkg/skilllib`
  is a local store, and no skills collection exists in `pkg/store` or
  `pkg/server`. What IS durable is the REFERENCE — `extra_skills` on the
  run record ([run.go:1088](../../pkg/store/run.go)) and the
  `skills_injected` event — so a reader must not mistake the reference for
  the twin. New durable state ships its twin in the same change
  (cloud-native rule 3), which makes the twin unit 4's to land; its shape
  (per team, per bundle, content-addressed by digest) is a decision this
  record does not make.
- **A second source of truth.** `internal/skillshare` stays the inventory
  and `task skills:sync` its generator. The server plane is a parallel
  mechanism (git-native rule 4), never the place a shared skill's bytes
  come from when the tree has them.
- **Flattening the catalogue.** 11 copies of `verify-build.md` are not 11
  adopters-in-waiting: each copy's divergence belongs to its bot, and
  migrating one is a reviewed change per bundle with its own witness,
  never a bulk rewrite. The declared variants are the record of what a
  blind flattening would destroy.

## Consequences

- The two sites that said "owes an ADR first" now cite this record. They
  are prose, so the citation is the whole change; nothing executable
  depends on this file.
- `category`/`tags` are held by a catalogue test alone, so a third-party
  bundle declaring them with a floor below 3.225.0 is admitted today and
  fails later at the strict decode. Clause 6 builds the seam that fixes
  that too; moving the taxonomy floor into the table is named here and
  owed by its own ticket, not smuggled into this lane.
- A bundle that adopts a dependency STOPS carrying the file, so the
  `internal/skillshare` family shrinks by one member as each consumer
  migrates. The gate keeps reddening on whatever is still declared, and
  the inventory shrinking is how progress is measured — not a count in a
  report.
- #2763 stays open until unit 4: this record is the ticket's first unit,
  and a record is not a capability.
