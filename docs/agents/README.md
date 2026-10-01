# Agent instructions — the tree

Doctrine specific to this repository: what to know before touching iterion,
and the traps each page exists to spare you. [AGENTS.md](../../AGENTS.md) is
the root every session loads (Claude Code through
[CLAUDE.md](../../CLAUDE.md)); these pages are read **on demand**, and product
references stay in [`docs/`](../README.md) proper.

| Page | Read it when |
|---|---|
| [review-and-merge.md](review-and-merge.md) | Opening, merging or unblocking a PR: queue, Revi gate, parked gate, Billy, releases. |
| [adversarial-review-loop.md](adversarial-review-loop.md) | Before any push: why the round is required, its budget, who pays, the trailers. |
| [engine-map.md](engine-map.md) | Which package owns a behaviour, and its invariants. |
| [dsl-and-runtime.md](dsl-and-runtime.md) | Writing or debugging a `.bot`, or touching the compiler/runtime. |
| [backends-and-execution.md](backends-and-execution.md) | Wrong model, lost tools, a node "dumber" than its harness; sandbox, plugins, supervisors. |
| [automation-surfaces.md](automation-surfaces.md) | Something launched a run and you need to know what. |
| [bot-authoring.md](bot-authoring.md) | Writing or amending a catalog bot; keeping the engine bot-agnostic. |
| [testing.md](testing.md) | Adding a test, choosing the proof layer, a test leaking into the operator's checkout. |
| [dogfood.md](dogfood.md) | Launching a catalog bot against this repo for real. |
| [security-selfaudit.md](security-selfaudit.md) | Running the security bots on iterion itself. |
| [runbooks.md](runbooks.md) | "How do I configure / operate / debug X" — the operational index. |
| [graphify.md](graphify.md) | A semantic "how does X work" question for the knowledge graph. |
| [../repo-map-and-graph.md](../repo-map-and-graph.md) | "Where is…", "what uses…", "what breaks if…" — the deterministic map. |
| [../state-of-the-art.md](../state-of-the-art.md) | How *proven* a surface is. |

## Adding to the tree

A discovery that cost real time lands here: the content in its page (or a new
`docs/` runbook indexed from [runbooks.md](runbooks.md)), one line in that
page's index, at most a line in AGENTS.md. `pkg/repomap/agenttree_test.go`
enforces the byte budgets and fails on a page its index does not link.
