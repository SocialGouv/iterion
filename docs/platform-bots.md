# Platform bot overrides — DB-backed bots, no rollout

The bot catalog (`bots/`) is baked into the server and runner images, so
changing a native bot historically cost a build + image publish + rollout.
**Platform bot overrides** move that iteration loop into the database: a
super-admin pushes a bundle, and from the **next launch** every tenant and
every launch surface (studio, webhooks, schedules, board dispatch,
triggers, resume) runs the pushed version. Deleting the override reverts
to the baked catalog. Nothing restarts.

The same change also runs **runtime-mutable platform settings** for the
webhook role bots and the sandbox default image (see below).

## The iteration loop

```sh
# edit bots/review-pr/ in a local checkout, then:
iterion remote admin bots push bots/review-pr
# → next launch of review-pr runs the pushed bundle (prompt, skills, devbox, manifest)

iterion remote admin bots            # list overrides (slug, version, digest)
iterion remote admin bots show review-pr
iterion remote admin bots pull review-pr --out /tmp/review-pr
iterion remote admin bots fork review-pr   # seed the override from the baked bundle
iterion remote admin bots rm review-pr     # revert to the baked catalog
```

`push` compiles the bundle server-side before persisting — a bot that does
not compile is rejected with the diagnostics, never left to fail at
launch. The response carries non-fatal warnings (see *Known gaps*). The
studio surface is Admin → **Bot overrides** (list, digest, revert) plus a
`platform override` badge on the /bots gallery card; the push loop itself
is CLI-first.

## How it works

- **Storage**: an override is an ordinary `pkg/botsource` row under the
  reserved sentinel tenant `platform:` — same collection, validation and
  compile check as team-authored bots (the pattern platform LLM
  credentials established). Hard limits: 6 MiB / 512 files per bundle.
- **Resolution precedence** (most specific wins): *team botsource →
  platform botsource → baked catalog FS*, on **every launch surface** —
  the studio button, the board dispatcher, the trigger spine, a cloud
  schedule and an inbound webhook — because each launcher knows the team
  it launches for (the card's, the subscription's, the schedule's, the
  webhook token's). A team that forks a bot runs its fork on its own
  automation, and every launch records which tier served it
  (`bot_source_tier`) **and shows it** — the run API returns it on the run
  header, the studio badges a `team bot` / `platform override` run beside
  its bot chip, and *Launched with* names the tier in full. A run that
  recorded no tier shows nothing rather than defaulting to `baked`: an
  unresolved tier reading as a working one is the failure mode this whole
  section exists for. Enforced
  by the central resolver in `pkg/server/bot_resolver.go` and its static
  sweep tests — one of which fails a launch site that hardcodes an empty
  team.
- **Spelling.** All three tiers tolerate the same variants
  (`feature_dev` / `Feature-Dev` / `"feature dev"` → `feature-dev`),
  because a board card, an agent's `set_bot` and a hand-written
  subscription all carry operator-typed names. A slug may legitimately be
  stored with `_` too, so the team tier compares both sides normalized.
- **A metadata read matches the tier that will SERVE the launch**, or the
  two disagree in silence — a fork that renames a `consumes:` var gets an
  empty seed, a fork that renames its `/command` stamps the operator's
  text under a var the running bundle never declared, and neither is an
  error. Which tenant a lane passes depends on what it describes:
  - **a delivery about to launch** reads the launching team's tier —
    the webhook hand-off seeds, the gate-var defaults, the retry policy,
    the command-routing discovery and the labeled-issue route, the
    converse-bot existence probe, the config-share surface, the bot
    home's "enable this trigger", and the forge auto-provisioning
    lookups (which build the webhook's `CommandMap` and its event
    subscription). The forms are `effectiveEntriesForTeam` /
    `effectiveFindByNameForTeam` / `botManifestFor` / `botExistsForTeam`
    / `entryOriginFor`, all over the same row resolution the launch uses,
    so an operator-typed spelling reaches both;
  - **a run that already launched** reads *that run's own*
    `bot_source_tenant` — the pause-notice role and the hand-off PRODUCER
    set (`teamBotManifest`). The ambient tenant is the wrong question
    there: a team's own run may still have been served by the platform or
    baked tier, and the run records which.

  The two FS-catalog write paths (`PUT /api/v1/bots/{name}` and its
  `/overlay`) refuse a stored bot with `409` instead of editing the
  bundle every tenant shares. Deliberately tenant-free: the native
  board's comment dispatcher (one local store, no tenancy) and the
  platform + baked floor each team-aware form falls through to. A static
  sweep (`bot_resolver_sweep_test.go`) fails a new tenant-free metadata
  read that is not declared with its reason.
- **Known gap — the pipelines control center.** Its "launch now" action
  and its ticket-admission check resolve the bot on the platform + baked
  tiers and launch by *filesystem path* (`entry.MainFile()`) rather than
  through the tiered resolver, so a stored bundle has no path to launch
  from: a team's fork of a catalog slug runs its ORIGIN there, and a bot
  only the team authored cannot be carded at all. Both halves move
  together — making the check tenant-aware alone would create cards that
  can never launch — so it belongs with the launch-surface work of #871,
  not the metadata pass. Tracked as **#970**.
- **Launch**: the server resolves the override ONCE, materializes it to a
  temp dir, and compiles against it (prompts/ participate in IR and the
  workflow hash). The queue message (schema v9) carries a
  `bot_bundle {tenant_id, slug, version}` ref; the **runner** fetches the
  row from Mongo, **verifies the version still matches**, materializes it
  into ephemeral scratch and attaches it as the run's bundle *instead of*
  the stale baked one — full fidelity: manifest, skills/, prompts/,
  devbox.json.
- **Version drift**: a push racing an in-flight launch fails that attempt
  loudly (`version drift: store has vN+1, launch resolved vN`) rather than
  pairing the launch's IR with newer resources. A resume re-resolves the
  current row, so a skills-or-manifest-only push self-heals — but a push
  that changed `main.bot` or `prompts/` moves the workflow hash, so the
  resume is REFUSED until you force it (and the auto-retry sweeper, which
  never forces, re-arms then abandons). Same for a deleted row.
- **Resume re-resolves by ORIGIN, not by path**: the launch persists which
  tier served the run — `bot_source_tier` (`team` | `platform` | `baked`)
  and, for the two stored tiers, the row's owner in `bot_source_tenant`
  (the team id or the `platform:` sentinel; empty for baked, which is why
  the tier is its own field). A resume/auto-retry reloads
  the SAME row at its current version; a row deleted mid-run fails the
  resume explicitly (relaunch, or resume with inline source). A run
  launched from the BAKED catalog picks up an override pushed since — the
  "effective at the next launch" rule. The pinned `sandbox_image` is
  likewise re-resolved per resume (attempt N and N+1 can differ if the
  setting changed between them); only redelivery of one queued message is
  guaranteed identical.
- **Metadata**: listings (`/api/v1/bots`), webhook command discovery,
  hand-off producers/consumers, gate-var defaults and retry-policy
  manifest reads all consult the platform overlay (TTL-cached ≤30 s per
  replica; the mutating replica reads its own write immediately).
- **Audit**: every mutation lands on the platform audit log with a
  sha256 **content digest** over the sorted file map — the provenance
  record for "what exactly is deployed". `admin bots` list shows the same
  digest.

## Shipping a baked-catalog change — two halves, or it did not ship

A change to `bots/<slug>/` that is MERGED reaches a deployment in two halves,
and a run only sees it when both have moved:

- the **runner image** — the pod that EXECUTES the bot (its engine evaluates
  the bot's expressions, its baked `bots/` is what the by-ref rebuild reads);
  it is pinned by digest in the deployment values, so a merge changes
  nothing until the digest is bumped;
- the **server** — the process that RESOLVES the bot at launch (team →
  platform → baked, `pkg/server/bot_resolver.go`) from ITS OWN baked catalog
  and stamps the ref on the queue message; it follows `:edge`, so a
  `kubectl rollout restart deploy/iterion` is the bump.

Bump the runner and forget the server, and every launch still resolves the
OLD bot (2026-09-06: Billy 1.6.0 on the runner, 1.5.x served — no
`delivery_reserve` node in the run). Push a platform override to skip the
image rollout, and the override runs on the runner's CURRENT engine: a bot
that needs a builtin the engine does not have (1.6.0's variadic `min`/`max`,
#830, on a v3.112.7 engine) compiles, then fails at its first evaluation —
and the failure auto-resumes in a loop (#857, #858). The order that works:

1. bump the runner digest to an image built from the main that carries the
   bot AND the engine it needs (`docs/cloud-deployment.md` § pinning);
2. `kubectl rollout restart deploy/iterion` so the server resolves the new
   baked catalog;
3. only then dogfood; a platform override is for iterating on a bot the
   deployed engine already supports.

## A bundle may declare the engine it needs — `requires.iterion`

The two-halves rule above was documented in prose, and a production push broke
it anyway. A bundle can now state its own floor:

```yaml
# bots/<slug>/manifest.yaml
requires:
  iterion: ">= 3.112.14"
```

`push` then **refuses** (409) when the deployment cannot honour it, naming
what the bot asked for, what the deployment runs, and where that number came
from:

```
bot "branch-improve-loop": bot requires iterion >= 3.112.14 but this build is
v3.112.7+abc123 — upgrade the engine (or the image this bot runs on), or relax
the manifest's requires.iterion (floor from runner v3.112.7+abc123). Bump the
runner image and restart the server first (docs/platform-bots.md § Shipping a
baked-catalog change), or push anyway with --force
```

The floor is the **minimum** of this server's own build and every runner build
observed on runs in the last 7 days (`Run.runner_version`, the build each
runner stamps on what it executes — there is no other channel: the server
follows `:edge`, the runners are pinned by digest). Both halves count: the
server compiles the bot at launch, the runners evaluate it, and a queued run
lands on whichever pod takes it.

`--force` is the escape hatch — pushing ahead of a rollout is legitimate — and
is never silent: the response carries the overridden requirement as a warning.

If the push lands anyway (forced, or the guard could not decide), the **runner
refuses at launch**: the run ends `failed` — not `failed_resumable` — with
`BOT_REQUIRES_NEWER_ENGINE`, and the delivery is acked so no redelivery
repeats the same arithmetic. `iterion validate` reports the same thing locally
as **C250** (unmet) / **C251** (this build has no orderable version, so the
check could not run). Full contract: [docs/bundles.md](bundles.md#requires--the-engine-contract).

## Trust model

A platform override executes across **all tenants**, with each tenant's
own credentials and bindings for that bot slug — exactly the trust level
of the baked image it replaces. That is why the surface is super-admin
only, safe-origin-gated, and digest-audited. Treat a push like a deploy:
review the diff first (`admin bots pull` + `git diff` against the repo).

## An override outlives the release that made it necessary

The tier's whole point is that a stored bundle **outranks the baked
catalog** at every launch surface. The consequence is easy to miss: an
override pushed once keeps serving after a later release bakes a *newer*
bundle for the same slug. The image moves; the bot does not.

Measured on 2026-09-06: `review-pr`'s override, pushed 2026-09-04 for the
0.7.0 cost pass, was still serving every production review 29 hours after
#742 baked the 0.8.0 review tiers into the image. Nothing said so — the
release notes, the runner digest and the bilan all reported the tiers as
deployed, while the graph that actually ran had no `tier_expand` node.

**iterion now reports it, and still does not refuse it** (pinning an older
bundle is a legitimate choice — a rollback is exactly this):

- `GET /api/admin/bots` returns `bundle_version`, `shadowed_version` and
  `shadows_newer_version` on every row. The last is omitted unless true, so
  a healthy inventory stays quiet. This is the check to run after any
  release that touched a bot you have overridden.
  `shadowed_version` is **what would serve without that row**, which is not
  always the bake: resolution is team → platform → baked, so a team row is
  measured against the platform override when one exists. The same fields
  appear on the team listing (`GET /api/teams/{id}/bot-sources`).
  If the catalog itself cannot be read, the response carries
  `shadow_check_unavailable: true` and the per-row shadow fields are absent —
  an inventory that looks clean because the check could not run would be
  worse than one that admits it did not run.
- The resolver logs one `Warn` naming the tenant, both versions and the two
  ways out — once per `(tenant, origin, slug, stored version)`, not per
  launch. The tenant is in the key on purpose: many teams can hold a row for
  the same slug, and a slug-only key would let the first one to launch
  silence all the others.

Versions are compared as dotted numeric components, so `0.10.0` correctly
beats `0.9.0`. `Manifest.version` is free-form, so a pair that does not
parse numerically is treated as **unordered** and never flagged — a false
staleness alarm on an operator's own naming scheme would be worse than the
silence it replaces.

To clear a shadow, either re-push the current bundle
(`iterion remote admin bots push bots/<slug>`) or drop the override and let
the image serve (`DELETE /api/admin/bots/<slug>`). Prefer dropping it once
the reason for the override has shipped: it restores the normal flow where
releases carry bots, and removes the trap for the next release.

## Known gaps (v1)

- **Binary files** cannot ride an override (the store carries JSON text);
  `push` refuses them explicitly. A bot with binary attachments keeps its
  baked form. Content-addressed blob storage is the follow-up.
- **File modes are dropped**: the store carries path→content only, and
  materialization writes every file `0644` — an executable helper loses
  its `+x` bit, so a step invoking it directly fails with permission
  denied on the override while the baked bundle worked. `push` warns per
  executable file; call helpers through their interpreter
  (`python3 script.py`, not `./script.py`) in an overridable bot.
- **Provisioned webhook projections** (`CommandMap`/`BotRules` stored on a
  forge repo integration at provision time) are not rebuilt on a push; an
  override that changes `invocations:` routes correctly through live
  command discovery but the provisioned map stays stale until the repo
  integration is re-provisioned. `push` warns when it detects the drift.
- **Nexie's catalog skill** is regenerated at run time from the runner's
  filesystem manifests, so other bots' overridden metadata is not
  reflected in it (the bot's own bundle IS the override). A DB-aware
  regen seam is the follow-up.
- `push` derives the slug from the bundle DIRECTORY name; listing/launch
  key on the manifest `name:`. Keep them identical (every shipped bot
  does) — a divergent pair would append a new entry instead of overriding.
- **One-time resume hash mismatch**: stored bots now compile through the
  full bundle path, so the workflow hash folds in `prompts/` + presets. A
  run launched from a stored bot BEFORE this change was hashed
  source-only — resuming it hash-mismatches once; resume it with
  `--force`. Runs launched after this change are unaffected.
- The k8s sandbox driver drops host binds, so a bundle `devbox.json`
  staged via the host-bind path does not provision inside a k8s sandbox —
  a pre-existing gap shared with baked bundles.

## Rollout note (queue schema v9)

v9 added `bot_bundle` + `sandbox_image` to the RunMessage. Runners accept
**both v8 and v9** (the change is additive), so queued v8 messages stay
consumable by upgraded runners. The reverse direction keeps the standard
policy: a pre-bump runner rejects v9, so roll server and runner from the
same release (or runner first) as usual.

## Platform settings: bot roles + sandbox image + bot vars

Three more runtime-settings families (ADR-090 doctrine: env/const = default,
DB record = override, ≤30 s propagation, super-admin surface), stored in
the same `platform_settings` collection as the usage caps:

```sh
# Webhook role → bot-id bindings (reviewer / revi_converse / brancher / implementer)
iterion remote admin roles                        # stored + effective + origin
iterion remote admin roles set --reviewer my-reviewer
iterion remote admin roles set --clear-reviewer   # back to the built-in default

# `sandbox: auto` fallback image — resolved at publish, pinned on the RunMessage
iterion remote admin sandbox
iterion remote admin sandbox set --default-image ghcr.io/…/iterion-sandbox-slim@sha256:…
iterion remote admin sandbox set --clear-default-image

# Bot-var overrides — ${ITERION_X:-default} resolved from the DB before the pod env
iterion remote admin vars                        # stored + origin
iterion remote admin vars set ITERION_VIBE_EFFORT_CLAUDE max
iterion remote admin vars rm  ITERION_VIBE_EFFORT_CLAUDE  # back to env/default
```

Role overrides apply at every webhook lane that used to read the
hardcoded constants (auto-review fan-out, `/revi approve`, the merge-queue
auto-heal, the issue-labeled implementer lane) — a symbol-sweep test keeps
new code from re-hardcoding them. The sandbox image is pinned per message
so a redelivery or checkpoint re-claim reruns in the same environment;
prefer an `@sha256` digest ref. On a store outage both resolvers serve the
last-known value (logged) — availability over freshness, the caps posture.
