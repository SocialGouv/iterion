# The layer map — package layers as an executable guard

The package layers are pinned by a test, not by prose:
`internal/layerguard` walks every non-test import of `./pkg/...` and
`./internal/...` — in five build contexts, one hop deep, test files aside,
the bounds the next section states — and refuses the dependencies the map
forbids.
The rules codify what the tree already does, so the guard bites on the
FIRST new violation — the cheapest moment to fix one.

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

## What the guard reads, and what it does not

- **Every non-test file, over five listing contexts.** One `go list` answers
  for one build context, so a non-test file behind a GOOS suffix or a build
  tag can cross a layer where nobody looks: a `pkg/store/lock_windows.go`
  importing `pkg/cli` is green under linux and red under `GOOS=windows`, same
  tree, same test binary. The guard unions five contexts — the host,
  `GOOS=windows`, `GOOS=darwin`, `GOOS=freebsd` (the three files that read
  `!linux && !darwin && !windows`), and then the build tags the files those
  four left uncompiled still ask for, read off those files instead of listed
  in the test (`goldens_record` today, which is where `pkg/botreplay`'s
  three edges live). That list is not the promise: a file no context
  compiles is refused by `TestEveryNonTestFileIsSeenByAListingContext`, and
  `testdata/crossbuild` replays both attacks on a module of its own.
- **Which file a build compiles is the go tool's answer, never the guard's.**
  The walk hands out every non-test `.go` file under the two trees — 1722 on
  2026-10-30, it skips nothing — and the listing the guard already reads
  sorts them, because `go list` publishes both the files a context compiled
  (`GoFiles`, `CgoFiles`) and the files it named and left out
  (`IgnoredGoFiles`): 1704 compiled, 18 the go tool names in no listing of
  any context (the `testdata/` fixtures below, a file called `_x.go` or
  `.x.go`, a directory called `_x` or `.x` — go/build drops such a name
  before it reads a constraint, and a pattern names no package in such a
  directory), 0 held out by the one name no listing may carry, 0 left over.
  That name is `ignore`: the toolchain's own generators carry it, so
  `go list -tags ignore` exits 1 on GOROOT's packages over this module
  (`TestTheReservedIgnoreTagCannotBePassedToTheListing`) and every test of
  the guard would fatal over a tree `go build ./...` compiles clean. A file
  it keeps out earns its exemption from `go/build`'s own `MatchFile` — no
  context matches the file, one does the moment that name is satisfied — so
  `//go:build ignore` is exempt and `//go:build ignore || mytag` is not:
  `mytag` is a context the guard asks for, and the crossing that file holds
  is judged. `TestTheGuardAndTheGoToolReadTheSameFiles` writes a module per
  shape and compares the guard to `go list -deps`: if the crossing ships,
  the guard names it; if no build holds it, the guard is silent.
- **Out of the listing is not out of the build.** A `_x`, `.x` or `testdata`
  DIRECTORY holds an importable package: a pattern names it nowhere, and
  `go build ./...` ships it the moment something imports it, rules or no
  rules. `TestEveryImportOfTheJudgedTreesIsAJudgedPackage` refuses that
  edge — an import under `pkg/` or `internal/` the listing does not hold is
  a package whose own imports no rule reads, and the remedy is to rename the
  directory or to list it. The importer can be anywhere, which is why the
  listing covers the whole module (`./...`, 230 packages) and not just the
  two judged trees: `cmd/` is the composition root, no pattern of `pkg/` or
  `internal/` names it, and `cmd/x` importing `pkg/store/_old` is the same
  defect one tree further out. A dropped FILE name is reached by nothing,
  since no import names a file, and `TestNoGuardIsReadAroundByTheBuild`
  holds every Go file of the repository to a package of the module, so a
  stray one is refused there. `testdata/neverbuilt` is the module that says
  all of it, under `TestTheGoToolSortsTheShapesTheGuardOwesNoContextFor`.
- **Direct imports, one hop.** A rule hit is one import edge; a banned
  family reached THROUGH another package is not one. `pkg/dsl/ir` is a leaf
  in that sense only: its direct imports name no `pkg/forge` package and, of
  the `pkg/dispatcher` family, only the accepted
  `pkg/dispatcher/native/boardops` — seven `boardops.CapBoard*` constants
  one hop away, not a weight reached through something else. What
  `go list -deps ./pkg/dsl/ir` adds on top of the one hop is
  `pkg/backend/tool/privacy/detector`, `pkg/dispatcher/native`,
  `pkg/dispatcher/tracker` and `pkg/forge`: four packages of those same
  banned families, the weight filed as iterion#2857. Neither half is prose —
  `TestTheLeafsOneHopReadingIsWhatThePageSays` reads this sentence off this
  page and faces both halves with the tree. Read "must not import" above as
  the one hop the guard checks.
- **Test files are out of scope, by decision** — `go list` runs without
  `-test`, so a coupling that lives only in a `_test.go` file is none of the
  guard's business: `pkg/backend/model`'s test variant imports
  `pkg/dispatcher/native` and `boardops`, and a green guard says nothing
  about it.

## The verdict, and what it cost to get one

The first reading of this page claimed "verified 2026-10-23 over 903
internal import pairs: zero violations". It was not a verdict: the guard
compared `go list`'s full import path
(`github.com/SocialGouv/iterion/pkg/dsl/ir`) to the bare prefixes above
(`pkg/dsl/ir`), so no rule ever bound a package, nothing was checked, and
the test passed over a tree that held twelve violations
([#2829](https://github.com/SocialGouv/iterion/issues/2829)). The module
prefix is now trimmed at one seam, `parseListing`, so no full path reaches
a comparison, and six tests keep the guard honest about itself: every
clause of every rule must bind a package of the tree — the banned side too,
where seven of the first rule's eight clauses live — every accepted
exception must still be a pair some rule names, every non-test file must be
compiled by some listing context or sorted out of it by the go tool, every
import under the two trees must name a package the listing holds, the
bounds this page states must be the bounds the guard claims, and the number
of times the rules fire is pinned, so a rule born mute over pairs already
accepted is an edit a reviewer sees.

Faced on 2026-10-29 at `e59110ed5`, over the 213 packages and 974
module-internal import edges the five contexts named there: **12 violating
pairs, all twelve accepted** with their reason in `allowExceptions`, and 19
rule hits over them. The same twelve on 2026-10-30, now read off a listing
of the whole module: 230 packages, 1061 module-internal edges, 1058 of them
landing under `pkg/` or `internal/` and every one naming a package the
listing holds. Those counts are each reading's provenance, not a standing
claim — they move with the tree, and the numbers that have to stay true are
the pinned `expectedAllowCount` and `expectedRuleHitCount`, which the guard
itself checks. The twelve are three families, not twelve decisions:

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

The count of rule hits is pinned beside it (`expectedRuleHitCount`), because
an accepted pair silences EVERY rule on that pair: a rule added later whose
only target is a pair already accepted would land green and mute, with
nothing in this file to edit and nothing in review to read. Its arrival
moves that number instead.

## The written invariants the map encodes

The rules are the existing doctrine made checkable, not a new policy:
`AGENTS.md` names `pkg/errtrack` THE tracker and `pkg/log` THE logger
(name-based rules, enforced by review and the boundary guards), the seams
in [`philosophy.md`](philosophy.md) fix the import directions above
(the DSL compiles alone; the IR is a leaf the backends read; the store
carries no executor; the server is composed in `cmd/`). A rule that
becomes false is a decision: it is edited here, in review, with its
reason — the same ratchet as the test's exception count.
