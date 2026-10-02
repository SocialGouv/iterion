# Agent instructions — the tree

Doctrine specific to this repository: what to know before touching iterion,
and the traps each page exists to spare you. [AGENTS.md](../../AGENTS.md) is
the root every session loads (Claude Code through
[CLAUDE.md](../../CLAUDE.md)); these pages are read **on demand**, and product
references stay in [`docs/`](../README.md) proper.

Seven domains, each an index of one-line-per-leaf:

| Domain | Read it when |
|---|---|
| [workflow/](workflow/README.md) | How work reaches `main`: the review loop, the gate, releases, dogfood. |
| [engine/](engine/README.md) | Which package owns a behaviour; the invariants; the DSL and runtime. |
| [backends/](backends/README.md) | Who runs the prompt: selection, fallbacks, parity, plugins, sandbox, dials. |
| [bots/](bots/README.md) | Writing or amending a catalog bot; keeping the engine bot-agnostic. |
| [testing/](testing/README.md) | Adding a test, choosing the proof layer, a leaking test. |
| [ops/](ops/README.md) | "How do I configure / operate / debug X" — runbooks, security self-audit. |
| [orientation/](orientation/README.md) | Finding your way: the knowledge graph, the deterministic map. |

Two pages live one level up, still on-demand:
[repo-map-and-graph.md](../repo-map-and-graph.md) (where/impact/path — or just
`iterion map`) and [state-of-the-art.md](../state-of-the-art.md) (how proven a
surface is).

## Adding to the tree

A discovery that cost real time lands here: the content in its page (or a new
`docs/` runbook indexed from [ops/runbooks.md](ops/runbooks.md)), one line in
that page's index, at most a line in AGENTS.md. `pkg/repomap/agenttree_test.go`
enforces the byte budgets and fails on a page its index does not link.
