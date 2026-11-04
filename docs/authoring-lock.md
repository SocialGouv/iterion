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

## The server's writers take the bot tree first

A per-file lock orders the writers of ONE file, from the moment the writer
knows which file it writes. The server's writers decide that file by
resolving: a bot name through the catalog, an editor path up to its nearest
manifest, a unit's siblings, a bundle to validate. Those reads ran outside
any lock, and a publication moves the live file aside before it moves its
successor in — so a loser of a write race read a half-written tree and
answered a request error (404, 400, 422), or acted on another bot (a rename
in between). Every server handler that writes the bot tree, or copies a
bot's files into what it writes (the trigger route's subscription), therefore
takes the server's bot tree write lock (`holdBotTreeWrites`,
`pkg/server/bot_tree_lock.go`) BEFORE it resolves anything, and holds it
through its write; the per-file locks come after it. The key is the whole
tree: no per-bot key exists before the resolution a rename re-points.

What a holder does under it is local work. The network runs outside: an
install clones before it takes the lock, a publication pins its commit under
the lock and pushes once it is released, a dependency update fetches a
remote source between a snapshot taken under the lock and a commit that
verifies that snapshot under it again (409 when a writer changed it). An
authoring git commit lets the tree go once its files are staged; its hooks,
signature and publication run under the repository's lock alone
(`holdAuthoringGitWrites`), which orders the server's writers of the
repository's index and is always taken first. The dependency actions keep the
tree from their install to their publication — the tree is provisional in
between — so their commit's hooks and signature are the one external work
under it. A request body is read before either lock.

Writers queue in arrival order and wait as long as their request: one that
ends first answers 503 `bot_tree_busy`, nothing written. Reads take nothing.
Both locks are the process's — across processes the per-file locks remain
the order.

What the lock leaves open is the readers' side: a read that crosses a
publication's window — a run launch, a card's bot check, a run start's
catalogue refresh, another process — sees the bot missing. Most answer that
read and nothing persists; the run start's refresh writes it into its
catalogue until the next refresh. A process lock cannot reach those readers;
a publication that swaps the file in one rename would.

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
