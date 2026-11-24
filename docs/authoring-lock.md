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
in between).

The bot tree write lock (`holdBotTreeWrites`, `pkg/server/bot_tree_lock.go`)
orders these writers of one server process, and no other — each named by a
`botTreeWriter*` constant there:

- sixteen handlers that write the bot tree, or copy a bot's files into what
  they write: the assistant's commit, git commit and git publication, the
  editor save, the run file save, the bot metadata edit, the workspace
  toggle, the trigger from an invocation (its subscription), bot create,
  import, install and upload, the marketplace install and uninstall, the
  dependency update and the dependency localization;
- the server's run merge routes: the merge, the conflict resolution — by
  hand and by the agent — the finalize and the abort;
- the project switch, which holds the root it leaves until the new one is
  in place.

A handler takes the lock BEFORE it resolves anything and holds it through
its write; the run merge routes take it around their git writes, after the
repository's lock. The per-file locks come after it. The key is a
directory — the project's root — not a bot: no per-bot key exists before the
resolution a rename re-points.

The boundary: whatever reads or writes the bot files of a working tree —
resolution, validation, the write, and the hooks git runs there — runs under
the lock; only git objects and the network run outside it. An install clones
before it takes the lock; a dependency update fetches a remote source between
a snapshot taken under the lock and a commit that verifies it under the lock
again (409 when a writer changed it). A git writer takes the lock on the
repository's whole working tree, since a hook reads any file of it. An
authoring git commit holds it from its resolution through its hooks and its
publication (reference-transaction hooks, the index, post-commit) and lets
it go around `commit-tree -S` alone: the signature reads the tree object it
built and may wait on a pinentry. A publication pins its commit under the
lock and reaches the remote with it let go. On Git 2.40 or newer it runs the
pre-push hook under the lock again — with git's own arguments and stdin,
through `git hook run --to-stdin` — and pushes `--no-verify`. An older git
has no such command: with no pre-push hook at the effective hooks path
(`core.hooksPath` honoured) it pushes plainly, as before; with one, it holds
the lock through its own push, the network included, so git runs the hook
under it. The dependency actions keep the tree from their install to their
publication, signature included: the tree is provisional in between. A
request body is read before any lock.

The repository's lock (`holdRepositoryIndex`), keyed by the common git
directory and always taken before the tree's, orders the server's writers of
the index: the authoring git commit, the dependency actions and the server's
run merge routes, which write the operator's files and so hold the tree too
(a model call or a push runs outside both). Git refuses a second writer of
the index (`index.lock`); here the second waits. The agent resolution reads
the conflict before its model call and stages the answer under both locks,
on the conflict read again there: a file resolved or edited meanwhile, an
abort, or a run no longer conflicted answers 409, nothing staged.

Writers queue in arrival order and wait as long as their request: one that
ends first answers 503 `bot_tree_busy`, nothing written. Reads take nothing.
Both locks are the process's, shared by every server it runs — `iterion
studio --workspace` serves one per project — and two directories exclude
each other when one contains the other, so nested project roots are ordered
too.

## What the locks leave open

Each of these writes or reads the bot tree, or the operator's checkout,
outside both locks, unordered against the writers above:

- The engine's own merges into the operator's checkout — the review-gate merge
  (`performGateMerge`) and the run-end auto-merge (`finalizeWorktree` →
  `trySquashMerge`): beside an authoring git commit the merge fails at once
  (`index.lock`, uncommitted changes) and is marked failed, its branch kept,
  or the git commit answers 400 on `index.lock`.
- The rewind of a run that executed in place (`handleRewindRun`,
  `pkg/server/runs_control.go`): it rewrites and deletes files of the
  operator's checkout, bots included, so beside an assistant commit it can
  leave a published `main.bot` whose manifest it deleted, both answering
  success.
- Bot discovery paths outside the project root (`iterion studio --workspace`,
  a project's registered bots paths): the lock is keyed by the root, so two
  projects sharing a bots directory outside their roots are not ordered, and a
  write in one reads the other's half-published bot — a 404 for a bot that is
  there.
- The pipeline card create and the run launch resolve a bot with no lock:
  across a publication's window they read it missing and answer a request
  error; nothing persists.
- The run-start catalogue refresh (`RegenerateCatalogForBundle`) reads
  discovery with no tree lock: across a publication's window it writes a
  catalogue without that bot's row, missing until the next refresh.
- Writers in another process — the CLI, a detached run, another server — take
  neither lock: only the per-file locks order them, and their resolutions can
  read a tree mid-write.

Two narrower windows stay with the publication: the reference-transaction
hook git runs when a push updates the remote-tracking ref runs outside the
lock, and on a git older than 2.40 a pre-push hook installed between the
publication's look at the hooks path and its push runs inside that push,
outside the lock — run, never skipped. A publication that swaps a bot file
in one rename would close the readers' windows above without a lock.

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
