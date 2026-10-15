# The shipped-workflow corpus

Every guard that holds this repository's `.bot` files to something — they
parse and compile, their acting prompts carry the
[UNTRUSTED INPUT BOUNDARY](untrusted-input.md), their templates quote no
author — walks a corpus. This page is what that corpus IS, so no guard has to
spell it a second way.

The walk is every workflow this repository ships, and the package has ONE
definition of that — `catalogWorkflowFiles`, the table of roots in
`bots/catalog_parse_compile_test.go` the parse/compile gate already read —
with nothing the engine routes left outside it unnamed:

- every `*.bot` of every bundle under `bots/`, not just each `main.bot`,
  because a bundle may ship sibling entrypoints (golden-master) or none at
  all (smoke);
- every `*.bot` of `examples/`, the declaration fragments aside: an example
  is routable, not decoration — `botregistry.DefaultPaths` is
  `[bots examples .botz]`, the roots the zero-config dispatcher, the studio
  and the server discover, so an issue whose assignee names one runs it;
- the dispatcher's zero-config fallback
  (`pkg/cli/templates/dispatch_bots_default.bot`), compiled into every binary
  and run against a raw issue body;
- the operator scripts under `scripts/adhoc/`.

Each root carries its own floor, because a total cannot see a root: a layout
rename that stopped matching `examples/*.bot` would leave 63 of 70 files, a
count every total floor downstream reads as healthy — each sits below the
catalogue's own size, so a bot may be retired. Measured with that root narrowed
to nothing and its floor dropped to 0: fourteen of the sixteen guards over the
corpus stay green; the two that redden are the `bounded` census and the frozen
class list, which names the five prompts that left. The floor is asserted where
a root's files are produced (`catalogRootFiles`), so neither enumerator can
hand a narrowed root on — a `-run` that selects a single guard included.

That table of globs is not the same thing as what the engine ROUTES:
`botregistry.discoverBots` walks each root of `DefaultPaths` with
`filepath.WalkDir` at unbounded depth, pruning only a bot's `lib/` fragments
and the inside of a bundle. So `examples/a/b/c.bot` is routable and matches no
root, and the twelve parser fixtures under `bots/testdata/` are routable
entries. `TestEveryRoutableBotIsAShippedWorkflow` asks the registry itself
which bots this workspace routes and requires each one to be in the corpus or
named in `routableNonWorkflows` with the reason it ships no workflow — it calls
the discovery rather than re-modelling its walk, so a change to the depth, the
pruning or the roots moves the guard with it.

What the walk REACHED is the subject there, not what it returned. A `.bot`
whose name another source already claimed yields no entry: discovery routes
that name to the claimer and records the loser as a `shadowed` diagnostic, so
a check reading the entries alone passes over the very same acting prompt under
the basename `main.bot` that it reddens under any unclaimed one. The guard
reads those diagnostics beside the entries, and
`TestNoShippedBotNameIsShadowed` holds the repository to one source per name —
four shipped examples answered to `main` or `sample` and were reachable by no
name at all, including their own. The hatch is a distinct name (a bundle's
manifest `name:`, a loose file's `## ---` frontmatter `name:`), which puts the
file back among the entries rather than hiding it.
