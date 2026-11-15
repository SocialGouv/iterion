# Engine — the domain

Ownership: `iterion map find|neighbours|impact|path` and
[map-packages.md](../../references/map-packages.md). Invariants below;
view in [architecture.md](../../architecture.md).

| Leaf | Read it when |
|---|---|
| [tenancy-credentials-spend.md](tenancy-credentials-spend.md) | Tenancy, credentials and spend. |
| [identity-data-lifecycle.md](identity-data-lifecycle.md) | Sign-in/launch audit rows, retention and erasure. |
| [account-erasure.md](account-erasure.md) | Account erasure, pseudonyms and store seams. |
| [http-failures.md](http-failures.md) | HTTP failures and correlation. |
| [workspace-and-review-scope.md](workspace-and-review-scope.md) | Workspace capture/restore, review scope and sandboxes. |
| [events-leases-and-schedules.md](events-leases-and-schedules.md) | Triggers, elections and queues. |
| [extensions-and-bot-sources.md](extensions-and-bot-sources.md) | Plugins, skills and bot sources. |
| [dsl-backend-execution.md](dsl-backend-execution.md) | The DSL pipeline, backends, runtime, store, server, forge. |
| [dsl-and-runtime.md](dsl-and-runtime.md) | Compiled graph, target capabilities, workflow execution. |
| [dsl-quick-reference.md](dsl-quick-reference.md) | Cross-cutting field syntax. |
| [dsl-errors-resume.md](dsl-errors-resume.md) | Failures, correction budgets, resume, checkpoints and admission refusals. |
| [worktree-finalization.md](worktree-finalization.md) | A run's workspace finalization misbehaves; touching `finalizeWorktree`. |
| [automation-surfaces.md](automation-surfaces.md) | Run launch entry points. |
| [bot-catalog-regeneration.md](bot-catalog-regeneration.md) | Bot catalogue copies and refresh failures. |
| [run-provenance.md](run-provenance.md) | Attempt snapshots, workflow history, cloud source fallback, metrics and launch audit. |
| [run-value-formats.md](run-value-formats.md) | Exact formats and bound writers. |
| [run-invocation-preparation.md](run-invocation-preparation.md) | Prepared bindings and terminal writes. |
| [run-birth.md](run-birth.md) | Birth identity, Mongo notes/streams and publication. |
| [retained-payloads.md](retained-payloads.md) | FS/Mongo grants and cleanup limits. |
| [run-streams.md](run-streams.md) | Mongo/FS selection, replay, lifetime. |
| [run-ws-origin.md](run-ws-origin.md) | Expected birth, origin refusals. |
| [run-cursors.md](run-cursors.md) | Event cursor boundaries and legacy compatibility. |
| [run-event-origins.md](run-event-origins.md) | Event origins, readers, broker, Service feeds and snapshot scans. |
