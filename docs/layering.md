# The layer map — package layers as an executable guard

The package layers are pinned by a test, not by prose:
`internal/layerguard` walks every non-test import of `./pkg/...` and
`./internal/...` and refuses the dependencies the map forbids. The rules
codify what the tree already does (verified 2026-10-23 over 903 internal
import pairs: zero violations), so the guard bites on the FIRST new
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

## The ratchet

The guard's exception list is EMPTY and its count is pinned
(`expectedAllowCount` in the test): a violation that review accepts goes in
with its reason, and both the entry and the count are edits of the test
file — visible in review, never silent rot. The list can only shrink
without one.

## The written invariants the map encodes

The rules are the existing doctrine made checkable, not a new policy:
`AGENTS.md` names `pkg/errtrack` THE tracker and `pkg/log` THE logger
(name-based rules, enforced by review and the boundary guards), the seams
in [`philosophy.md`](philosophy.md) fix the import directions above
(the DSL compiles alone; the IR is a leaf the backends read; the store
carries no executor; the server is composed in `cmd/`). A rule that
becomes false is a decision: it is edited here, in review, with its
reason — the same ratchet as the test's exception count.
