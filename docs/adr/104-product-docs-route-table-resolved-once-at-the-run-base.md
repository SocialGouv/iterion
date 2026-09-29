# ADR-104 — The product-docs route table is resolved once, at the run base

- **Status**: Accepted
- **Date**: 2026-09-29
- **Applies to**: `bots/product-docs` (Prody) — `route_table`, `coverage_check`, `diagram_lint`
- **Neighbours**: ADR-044 (deterministic gates, never an LLM judgment), ADR-058 (one campaign agent + deterministic gates), [ADR-092](092-product-docs-editorial-sovereignty-and-git-native-source-deltas.md) (source deltas are git-native)

## Context

When a golden-master net is present, the exhaustiveness gate holds the
documentation to the application's **declared routes**: every route must be
described by a page (`ROUTE_UNDOCUMENTED`), and the map may only draw screens
the application serves (`PHANTOM_MAP`).

golden-master states those routes through its `config.json` `routes_probe`, a
command it replays at every gate (stdout and stderr merged). It commits no
artifact for them. The gates read only a committed `<net>/routes.txt`, so on a
net built the usual way both route checks were inert: degraded, zero routes.

## Options

- **(a) golden-master commits the probe output at its rite.** The file goes
  stale as soon as a change adds a route. A modernisation lot must not touch
  the net, so nobody regenerates the file, and the gates stop demanding the
  new route until the next rite. That is a false green.
- **(b) Each gate replays the probe, on every pass, in the live docs
  worktree.** The table is always fresh, but it now depends on the worktree
  that the judged agent writes to. An adversarial review reproduced 14
  defects, among them:
  - an uncommitted, git-ignored file overrode the table;
  - the probe's side effects landed in the tree the scope gate judges;
  - a timed-out probe's child wrote after the verdict;
  - the two gates could read two different tables;
  - "the net inside the workspace" was decided by location, not ownership,
    so the command of a clone left in tree was executed.
- **(D) A single deterministic node computes the table once per run, from a
  commit.**

## Decision

(D). A `route_table` node runs once, before any page is written. It hands one
frozen answer, as data, to the map step, the campaign and both route gates.
The gates no longer read a route file or run a command.

- **A route file wins, and is read at the run base commit** (`git show`),
  never from the working tree. A file that exists only in the working tree is
  named and set aside.
- **Otherwise the net's committed `routes_probe` is replayed in a throwaway
  checkout of the run base** (`git worktree add`, git hooks off):
  - it runs in its own process group, which is killed when the probe returns
    or times out;
  - stdout and stderr are read merged, as golden-master reads them, through a
    1 MiB bound and a single deadline (golden-master's 120 s);
  - the output is decoded tolerantly;
  - lines that name files inside the checkout are set aside;
  - one path under several methods counts as one route.
- **Only the workspace's own net is replayed**: `<workspace>/<oracle_dir>` in
  the workspace's own repository. The command of any other net, such as a
  source clone, is never executed. `catalog_ingest` refuses a `scratch_dir`
  inside the workspace.
- **With no table, the path checks degrade visibly** to the corpus and name
  the cause. The cause is an operator-side note, never a repair order to the
  campaign.

## Consequences

- The table depends on a commit, not on anything the judged agent wrote
  during the run. It is a stored node output, so a run resumed on a new pod
  judges by the same table.
- Four readers share one answer. The gates become pure functions of the
  pages, the net and the table.
- A route that a change adds is demanded from the next run on. A route added
  *during* a docs run is not: the run documents a commit.
- Accepted limits:
  - a probe that needs build outputs missing from a clean checkout degrades,
    and says so;
  - a probe that detaches with `setsid` escapes the group kill (the sandbox
    pod still bounds it);
  - a committed route file wins over the probe, so keeping it fresh is the
    operator's job. By default, commit none.
- Two semantic changes ship with this decision:
  - **A cited corpus entry covers the route its captured path instantiates**
    (query aside). Pages cite entries more often than paths. On a real net,
    130 of 131 routes were refused while every screen was documented.
  - **The map lint grounds a mapped path on a captured corpus path** when the
    table misses it. A source-reading probe can miss a handler, for example
    one inherited from a generic controller.
