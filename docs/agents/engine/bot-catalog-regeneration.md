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
