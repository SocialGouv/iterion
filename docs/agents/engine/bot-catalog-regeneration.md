# Which runs regenerate the bot catalogue

Nexie's routing table, `<bundle>/skills/iterion-bot-catalog.md`, is generated
from the enabled bots' manifests ([automation-surfaces.md](automation-surfaces.md)
says what it holds and how to change it). This page says which runs write it,
and why the scope is what it is. Code:
[pkg/botregistry/catalog.go](../../../pkg/botregistry/catalog.go) and the
run-start call in [pkg/runtime/engine_run.go](../../../pkg/runtime/engine_run.go).

Regeneration runs
automatically on every studio bot-metadata save (server, every owner
under the workspace) and at run start (engine), scoped to the catalogue
the run's OWN bundle owns — Nexie's run refreshes Nexie's copy, and that
copy alone. Why the scope: a run mirrors only its own bundle's skills, so
a write for any other owner leaves a file that run has no copy of — an
uncommitted edit to a tracked file of the workspace every time the running
generator renders differently from the checkout, the standing case for a
review run whose binary is older than the head it reviews. Three
consequences worth knowing: a run whose bundle ships no template writes
nothing; a bundle rooted OUTSIDE the workspace (a materialized `.botz`)
writes nothing either and keeps the copy its packer froze, since the
regeneration renders from the workspace's manifests and never writes
outside it; a bundle under the workspace that ships the template but is
not one of the bundles discovered there (a `manifest.yml` spelling, which
`bundle.OpenDir` honours and bot discovery does not) is reported as a
`bot catalog regen` warning rather than traded for a write into another
bot's bundle. Only a plain `.bot` run — no bot id on its run document, a
field cloud launches persist and local ones leave empty — refreshes every
owner under its workspace, because the engine cannot enumerate what a
loose `.bot` opens. A BOT run carrying no bundle (the
runner resolves none when the id does not resolve or its directory fails
to open, and none at all — silently — when `ITERION_BOTS_PATH` is unset)
mirrors no skill whatsoever, so it refreshes nothing: every owner's copy
is a file that run cannot read.

## An overlay that is not read

The workspace overlay (`.iterion/bot-overrides.yaml`) is read with its own
bounds: a size cap, and the read budget (`ITERION_DSL_READ_BUDGET`) its YAML
decode is held to. One its reader refuses holds visibility decisions nobody
read, so it never counts as no overlay: the catalogue is not regenerated,
card launch and slash-command discovery refuse while it stands, and the
studio's Bots gallery lists every bot disabled beside the refusal. Every
other door that starts a bot reads no bot visibility at all — among them a
manual Run (its resume, retry and rewind too), label, pull-request and
generic webhooks, provisioned slash commands and converse replies,
triggers, schedules, the cloud board, the push-debounce sweeper, the forge
gate's relaunch and auto-fix, the local dispatcher's static routes,
`iterion run`, the MCP `local_run` and a subbot's delegation — and the local
dispatcher of the default configuration reads its own store's overlay, not
the workspace's (#2676). A listing that finds the overlay's
bytes unchanged reuses their reading instead of decoding them again. The
Catalog manager's toggles are applied one at a time, under a lock file beside
the overlay; a toggle waits for it 30 s at most (503 after).

## Who hears about a refresh that failed

The refresh is never fatal to the gesture that triggered it — the bundle is
already in the workspace, or already deleted — but its failure reaches the
operator at every gesture that changes WHAT the catalogue advertises, instead
of being dropped. The cause travels on the result:
`botinstall.Result.CatalogError` for an install, upload or dependency
materialization, `botinstall.RemoveResult.CatalogError` for a removal, carried
on through `botdeps.SyncResult` / `UpdateResult` and
`cli.MarketplaceUninstallResult`; on the server, `Server.regenCatalog` returns
the rendered cause (and keeps its warning log). A non-nil `error` from
`Install`/`Remove` keeps its one meaning — the install or the removal did NOT
happen — so no caller can read a stale catalogue as a failed gesture.

Each surface then shows it: the CLI prints `cli.PrintCatalogWarning` (the
cause, which names the catalogue it could not write, plus `iterion bots
regen-catalog`) in human mode and the `catalog_error` field in `--json` — on
an aborted `bots sync`, the partial rows and the abort leave in ONE document
(`{"results": …, "error": …}`) marked already-reported, so a machine reader
never meets a second document after the first; the server returns that field
in the create, install, upload, metadata `PUT`, visibility overlay and
marketplace install/uninstall bodies; the studio appends
`catalogStaleSuffix` to the success toast of every import, install,
uninstall and catalogue toggle, and the builder's "is live" banner names the
stale catalogue instead of claiming the new bot is visible to the
orchestrator. A successful refresh adds nothing anywhere — the field is
omitted and nothing is printed. Two facts keep a foreign bundle from
forging a line of this output: every relayed CAUSE is rendered through
`terminalText`, and every bundle-supplied NAME is pre-empted at its own gate
— install names are held to the bot-name charset (`botlock.ValidName`), so a
manifest cannot install the newline the `iterion run` footer would have
relayed raw, while removal names — installed workspaces still carry names
outside that charset, and an installed bundle must stay uninstallable — are
held to path safety only, with every message that carries the name quoting
it (`%q`), so a newline in a store entry's name cannot write a line of the
uninstall's output either.

A sync that aborts returns the dependencies it already materialized, and
their cause with them: `bots sync` prints those rows and the warning before
failing, because an aborted sync leaves a real install behind. The warning
vouches only for its own rows — never for the workspace as a whole, which an
aborted sync leaves short of its pins.

What carries no cause, and why:

- **The workspace's two metadata routes** — the manifest `PUT` and the
  visibility overlay (`respondBot`'s only two callers) — DO change which
  bots the table routes on: the rendered table holds one row per ENABLED
  bot, so a save or a toggle whose refresh failed leaves the previous set
  routed until a regeneration succeeds. That is why their answers carry the
  cause (`catalog_error` on the entry), next to the standing server-log
  warning the engine watches; the studio shows the cause on the toggle and
  metadata-save confirmations.
- **Private dependency staging** (`botdeps.StageOne`): the refresh is skipped
  by construction, which preserves the pinned catalogue bytes the hash
  verification depends on.
- **A dependency already materialized at its pinned hash**: the sync returns
  it without installing, so nothing changed and nothing is refreshed.
- **The studio's dependency transaction** (`bots-update`): it stages privately
  and then renames the staged tree onto `.botz/<name>` — a discovery root — so
  the workspace catalogue keeps the previous pin's card until the next
  refresh. Nothing is dropped (no refresh runs), but the staleness is real and
  no surface mentions it. `bots-localize` cannot reach it: it refuses unless
  the materialized copy already hashes to the pin, and re-pins to that same
  hash, so the sync returns early. It owes no refresh for the localized
  source either, while discovery claims the consumer bundle: that source
  lands under the consumer's own subtree (`plugins/<dep>`), which discovery
  never walks once it has claimed the consumer, so the bundle set the
  catalogue renders is exactly what it was before. A consumer discovery
  does not claim — a `manifest.yml` spelling, which the assistant authoring
  surface opens, or a missing `main.bot` — has its `plugins/<dep>` walked
  instead: the localized copy claims the dependency's name ahead of the
  `.botz` materialization, the name routes to the copy, and every catalogue
  regeneration refuses on the shadowed bundle until the source is fixed or
  removed, while the localize itself reports success. Whether localize
  should refuse an unclaimed consumer, or surface the wedged regeneration,
  is an open question. A localize that landed a directory discovery can see
  would owe a refresh.

## The lock a refresh holds

A generated catalogue (`<owner>/skills/iterion-bot-catalog.md`) is a stored
bot file: the studio, the assistant and the unit editor all replace it
through the server's authoring lock. The refresh takes that same lock —
`pkg/authoringlock`, the per-file `flock` beside the file it governs — and
holds it from before it reads the owner's template until after its rename.
So a save decided on the file's bytes can no longer land in the middle of a
refresh and be replaced: it waits for the refresh and then refuses on its own
stored-file check, or the refresh waits for it.

What the grain buys is the hot path. The lock is per FILE, and a run
regenerates only the catalogue its own bundle owns, so two runs of different
bots never wait on each other, and a save of one owner's catalogue costs that
owner's refresh alone — the workspace-wide refresh joins per-owner failures
and keeps the healthy owners refreshed.

Each caller bounds its own wait, because each knows what it may spend:

- a **run** start waits `runtime.catalogLockWait` and then goes on with the
  catalogue on disk (the refresh is best effort, and the mirror falls back to
  it) — a run start is not a gate an editor gets to hold;
- the **server** waits as long as the request it serves, so a client that
  hangs up stops the wait;
- the **operator's** `iterion bots regen-catalog` waits as long as the
  command, and reports the file it left alone.

A refusal names the file and says the lock was not taken, which is the
difference that matters: the file keeps the bytes its editor decided on
instead of the ones this build renders.

The control directory the lock needs (`.iterion/authoring/` beside the file)
lands inside the bundle, and is no member of it: every walker of a bundle's
content skips `.iterion` (`bundle.IsPackSkipped`), so a refresh does not move
`bundle.ContentHashDir` and cannot make a materialized dependency drift from
the hash its `bots.lock` pins. The repository ignores the directory too.

An open question, measured and not yet decided (ARB-109 of the leap): a
dependency materialized under `.botz/` that ships the template is an owner
like any other, so a workspace-wide refresh rewrites ITS generated catalogue
with the calling workspace's routing table — and that write does move
`ContentHashDir`, which is the hash `bots.lock` pins and `botdeps.Sync`
compares. Either the dependency keeps the table its packer froze, or the
generated copy becomes local state excluded from the hash.
