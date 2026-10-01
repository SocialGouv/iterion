# Engine — the domain

Which package owns a behaviour, and the invariants each one carries. Exact
questions go to `iterion map find|neighbours|impact|path` and the generated
[map-packages.md](../../references/map-packages.md); these leaves carry the
invariants. The product view is [docs/architecture.md](../../architecture.md).


| Leaf | Read it when |
|---|---|
| [tenancy-credentials-spend.md](tenancy-credentials-spend.md) | Orgs, teams, keys, caps, platform settings. |
| [workspace-and-review-scope.md](workspace-and-review-scope.md) | Workspace capture/restore, review scope, sandbox wiring. |
| [events-leases-and-schedules.md](events-leases-and-schedules.md) | The trigger spine, elections, queues, schedules, retries. |
| [extensions-and-bot-sources.md](extensions-and-bot-sources.md) | Plugins, skills, memory, marketplace, bot sources. |
| [dsl-backend-execution.md](dsl-backend-execution.md) | The DSL pipeline, backends, runtime, store, server, forge. |
| [dsl-and-runtime.md](dsl-and-runtime.md) | The compiled graph and how a workflow executes. |
| [dsl-quick-reference.md](dsl-quick-reference.md) | The exact syntax of the cross-cutting fields, one line each. |
| [dsl-errors-resume.md](dsl-errors-resume.md) | A run fails, resumes or rewinds; a checkpoint looks torn. |
| [worktree-finalization.md](worktree-finalization.md) | A run's workspace finalization misbehaves; touching `finalizeWorktree`. |
| [automation-surfaces.md](automation-surfaces.md) | Something launched a run and you need to know what. |
