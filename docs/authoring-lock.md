# The authoring lock — one lock for every writer of a stored bot file

A stored bot file has several writers — the studio, the assistant, the unit
editor, the CLI commands that rewrite a bot's files, an install that replaces
a workspace copy, and the catalogue refresh. They serialize on one per-file
lock, `pkg/authoringlock` (the `flock` beside the file it governs, its control
directory `.iterion/authoring/` beside the bundle). This page is the refresh's
side of it, moved out of
[bot-catalog-regeneration.md](agents/engine/bot-catalog-regeneration.md) so
that page keeps its byte budget; the package's own contract is in
[extensions-and-bot-sources.md](agents/engine/extensions-and-bot-sources.md).

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
