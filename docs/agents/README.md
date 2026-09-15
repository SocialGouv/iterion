# Agent instructions — the tree

Doctrine specific to **this repository**: what an agent (or a contributor)
must know before touching iterion, and the traps each page exists because
someone already paid for.

[CLAUDE.md](../../CLAUDE.md) is the router — short, always injected, one line
per page below. [AGENTS.md](../../AGENTS.md) is the work-tracking contract
every harness reads natively. These pages are read **on demand**: that split
is the point, since pi injects both root files on every call and a 2 000-line
CLAUDE.md was paying its cost every turn.

Product and engine references live in [`docs/`](../) proper — [dsl.md](../dsl.md),
[backends.md](../backends.md), [sandbox.md](../sandbox.md), … The pages here
summarize them for someone about to edit, and add what those references do not
carry: the failure that was measured, on this repo, on a given date.

| Page | Read it when |
|---|---|
| [review-and-merge.md](review-and-merge.md) | You are about to open, merge or unblock a PR. The required loop, the merge queue, the Revi gate, the Billy pause + re-arm procedure. |
| [adversarial-review-loop.md](adversarial-review-loop.md) | Before pushing anything to the gate. The local round: the subagent prompt, verifying the fix as hard as the finding, the two blocking conditions, the three exits from a non-converging loop. |
| [engine-map.md](engine-map.md) | You need to know which package owns a behaviour, before grepping blind. |
| [dsl-and-runtime.md](dsl-and-runtime.md) | You are writing or debugging a `.bot`, or touching the compiler/runtime: node types, edges, budget, resume, worktree finalization. |
| [backends-and-execution.md](backends-and-execution.md) | A node picks the wrong model, loses its tools, or behaves "dumber" than the native harness; anything about sandboxes, plugins, supervisors or cursors. |
| [automation-surfaces.md](automation-surfaces.md) | Something launched a run and you need to know what: dispatcher poll, webhook, trigger spine, board transition. |
| [bot-authoring.md](bot-authoring.md) | You are writing or amending a catalog bot — repo-agnostic, stack-agnostic, convergent, declaring its own tools; and the mirror rule that keeps the engine bot-agnostic. |
| [testing.md](testing.md) | You are adding a test, or a test is flaky/leaking into the operator's own checkout. |
| [dogfood.md](dogfood.md) | You are about to launch a catalog bot against this repo for real. |
| [security-selfaudit.md](security-selfaudit.md) | Running the security bots on iterion itself, or triaging a `source:sec-audit-self` finding. |
| [runbooks.md](runbooks.md) | "How do I configure / operate / debug X on iterion" — the operational index, one entry per runbook with its read-me-when. |

## Adding to the tree

A session that burns real time discovering how something works owes that
discovery back to the repo. Put the **content** in the page it belongs to (or
a new `docs/` runbook, indexed from [runbooks.md](runbooks.md)), and add at
most **one line** to CLAUDE.md. The router stays short by construction: if a
change makes CLAUDE.md longer by a paragraph, the paragraph belongs here.
