# ADR-141: Board issues join the by-id class — an issue is served from its team

**Status:** Accepted (2026-10-28, the #1849 epic's lane; the operator's arbitration of 2026-10-27 — "lane dédiée maintenant" — commissioned it)
**Relates to:** [103-run-addressed-by-id-across-teams.md](103-run-addressed-by-id-across-teams.md) (this amends its "What is not promised": board issues no longer keep their active-team rules)

## Context

ADR-103 deferred the board to this epic's lane: `/api/v1/native/issues/{id}` and `/api/v1/pipeline-board/tasks/{id}` — some 42 routes over three families sharing one id space (native REST, the pipeline-board projection, the forge per-card routes) — resolved their store from the JWT's ACTIVE team, so an issue id answered 404 whenever the credential was pinned to another team, even for the issue's own members. The isolation was emergent (per-request store instances + tenant-predicated queries), never an HTTP-layer contract.

## Decision

1. **The id suffices; the issue's team is the team.** The choke point classifies the two id-wildcard chains by the route the mux matched (the runs class' own rule) and resolves the issue's team team-blind through a NEW capability, `IssueTenantResolver` — in cloud, a cross-tenant `_id`-prefix read over the board collection's existing index. The caller's standing in THAT team authorizes: reads take a viewer, mutations a member — the run ladder's rule; no board GET hands out live control. The re-scoped stamp is what the handlers already read: every store resolution (`cloudBoardResolve` and its siblings) serves the issue's own board with zero handler changes.
2. **The resolver capability is required where the class exists.** Cloud mode wires the resolver beside `CloudBoardFor` and PANICS at startup without it (the runs class' own rule); self-hosted's single store has no tenancy to decouple — the class simply does not exist there, and nothing changes.
3. **A missing issue and an invisible issue answer the same 404.** The resolve is the existence check: a deleted issue stops resolving at once, no cache. An AMBIGUOUS id prefix is not an address: the ambient stamp decides, and the handler's own prefix resolution renders the verdict for the caller's team.
4. **Super-admin cross-team reads are audited**, under `issue.cross_team_read` — the named-predicate rule the runs class set (A19).

## What is not promised

- **Existence confidentiality** — as ADR-103 states: the 404 equality is for the by-id routes; timing is not equalized.
- **The board MCP surface** stays single-tenant and unmounted in cloud (the C082 invariant comment stands: wire a shared multi-tenant store there and the grant must carry the tenant).
- **The run-issue watch edges** (`POST /api/runs/{id}/watch/{issueID}`) stay run-scoped: the issueID rides verbatim as today; validating it against a board is a separate contract.

## Behavior changes this lands (documented breaks, no `!`)

- A member of the issue's team whose credential is pinned elsewhere reads the issue by id instead of 404 (the epic's point).
- A viewer of the issue's team reads by id; mutations still take a member (the mounts' own rung unchanged — the choke point's ladder matches it).
- A super-admin reading an issue across teams leaves an audit row (previously: a silent active-team read).
