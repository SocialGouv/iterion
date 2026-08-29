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
  platform botsource → baked catalog FS*. The team tier applies only on
  the studio launch surface (a team's experimental fork must not silently
  hijack its schedules/webhooks); the platform tier applies everywhere a
  bot id resolves, enforced by the central resolver in
  `pkg/server/bot_resolver.go` and its static sweep test.
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
  tier served the run (`bot_source_tenant` on the run doc — the team, the
  `platform:` sentinel, or empty for baked). A resume/auto-retry reloads
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

## Trust model

A platform override executes across **all tenants**, with each tenant's
own credentials and bindings for that bot slug — exactly the trust level
of the baked image it replaces. That is why the surface is super-admin
only, safe-origin-gated, and digest-audited. Treat a push like a deploy:
review the diff first (`admin bots pull` + `git diff` against the repo).

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
