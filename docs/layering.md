# The layer map — package layers as an executable guard

The package layers are pinned by a test, not by prose:
`internal/layerguard` walks every non-test import of `./pkg/...` and
`./internal/...` and refuses the dependencies the map forbids. The rules
codify what the tree already does, so the guard bites on the FIRST new
violation — the cheapest moment to fix one.

| the layer | must not import |
|---|---|
| `pkg/dsl/ir` (leaf) | `pkg/runtime`, `pkg/server`, `pkg/dispatcher`, `pkg/cli`, `pkg/backend`, `pkg/forge`, `pkg/runner` |
| `pkg/dsl` (compiles alone) | `pkg/runtime`, `pkg/server`, `pkg/dispatcher`, `pkg/cli`, `pkg/backend` |
| any `pkg/*` | `pkg/server` (the composition root is reached through `cmd/` and the seams) |
| `pkg/runtime` | `pkg/server`, `pkg/dispatcher`, `pkg/cli` |
| `pkg/backend/*` | `pkg/server`, `pkg/dispatcher`, `pkg/cli` |
| `pkg/dispatcher` | `pkg/server`, `pkg/cli` |
| `internal/*` | `pkg/server` |
| `pkg/store` (leaf) | `pkg/server`, `pkg/runtime`, `pkg/dispatcher`, `pkg/backend`, `pkg/cli` |

A rule forbids *crossing into* a family: a package of a banned family
importing its own family is cohesion, not a layer crossing, so
`pkg/server` reading `pkg/server/projects` is not a violation of the third
rule.

## The verdict, and what it cost to get one

The first reading of this page claimed "verified 2026-10-23 over 903
internal import pairs: zero violations". It was not a verdict: the guard
compared `go list`'s full import path
(`github.com/SocialGouv/iterion/pkg/dsl/ir`) to the bare prefixes above
(`pkg/dsl/ir`), so no rule ever bound a package, nothing was checked, and
the test passed over a tree that held twelve violations
([#2829](https://github.com/SocialGouv/iterion/issues/2829)). The module
prefix is now trimmed at one seam, `parseImportPairs`, so no full path
reaches a comparison, and two tests keep the guard honest about itself:
every rule must bind a package of the tree, and every accepted exception
must still be a pair some rule names.

Faced on 2026-10-29 at `69b46b587`, over the 212 packages and 964
module-internal import edges `go list` named there: **12 violating pairs,
all twelve accepted** with their reason in `allowExceptions`. Those two
counts are that reading's provenance, not a standing claim — they move
with the tree, and the number that has to stay true is the pinned
`expectedAllowCount`, which the guard itself checks. The twelve are three
families, not twelve decisions:

- `pkg/dsl/ir` → six `pkg/backend/*` packages and
  `pkg/dispatcher/native/boardops`: the IR validates a `.bot` against what
  the chosen backend can do, and reads the capability vocabulary where it
  is defined rather than keeping a second copy. Five of the seven are
  leaves (1 to 3 module packages); `automemory` and `boardops` drag 10 and
  43 — filed as iterion#2857.
- `pkg/cli` → `pkg/server`, `pkg/server/projects`: `iterion studio` and
  `iterion dispatch` compose an in-process server, and `pkg/cli` is
  imported by `cmd/iterion` and `cmd/iterion-desktop` only.
- `pkg/backend/tool`, `pkg/backend/delegate` →
  `pkg/dispatcher/native{,/boardops}`: the claw board tools and the MCP
  board bridge run every operation through the one `boardops` entry the
  stdio and HTTP transports use.

## The ratchet

The guard's exception list is pinned by its count (`expectedAllowCount` in
the test): a violation that review accepts goes in with its reason, and
both the entry and the count are edits of the test file — visible in
review, never silent rot. It can only shrink: an entry whose pair no rule
names on today's tree is refused, so a resolved violation cannot keep
buying silence.

## The written invariants the map encodes

The rules are the existing doctrine made checkable, not a new policy:
`AGENTS.md` names `pkg/errtrack` THE tracker and `pkg/log` THE logger
(name-based rules, enforced by review and the boundary guards), the seams
in [`philosophy.md`](philosophy.md) fix the import directions above
(the DSL compiles alone; the IR is a leaf the backends read; the store
carries no executor; the server is composed in `cmd/`). A rule that
becomes false is a decision: it is edited here, in review, with its
reason — the same ratchet as the test's exception count.
