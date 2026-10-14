# ADR-038: GitHub team gating uses a materialized reverse-lookup index

- **Status**: Accepted
- **Date**: 2026-06-22
- **Authors**: Adry
- **Code**: [pkg/auth/orgsso/types.go](../../pkg/auth/orgsso/types.go), [pkg/auth/orgsso/mongo.go](../../pkg/auth/orgsso/mongo.go)

## Context

GitHub logins can grant access to iterion orgs based on allow-listed GitHub org/team memberships. At login time, iterion may have a set of GitHub team memberships for the user and must find the tenant SSO rows that grant access.

The lookup must be efficient and tenant-isolated. Scanning all org SSO rows or issuing one query per team would make login cost grow with tenant count or membership count and would increase the chance of cross-tenant mistakes.

## Decision

Org SSO provider rows materialize GitHub grants into lowercased `github_team_keys` in [`pkg/auth/orgsso/types.go`](../../pkg/auth/orgsso/types.go). The keys include exact team grants such as `<org>/<team_slug>` and wildcard org grants such as `<org>/*`.

The Mongo store in [`pkg/auth/orgsso/mongo.go`](../../pkg/auth/orgsso/mongo.go) creates a partial multikey index on `github_team_keys` for enabled GitHub provider rows. Login resolution uses one `$in` query over the user's candidate keys to find granting org rows.

The derived keys are internal and maintained on write; API clients see the grants, not the materialized lookup representation.

## Trade-offs

| Dimension | Materialized `github_team_keys` + one `$in` query | Scan rows or query per team |
|---|---|---|
| Login cost | One indexed reverse lookup. | O(orgs) scan or N team queries. |
| Tenant isolation | Mongo filter returns only rows with matching materialized grants. | Application-side filtering over many tenant rows is riskier. |
| Write cost | Grant writes must update derived keys. | Writes are simpler. |
| Normalization | Lowercasing and wildcard expansion happen once on write. | Normalization repeats at login. |

The honest concession is that writes carry denormalization logic that must stay in sync with grant semantics.

## Alternatives considered

### 1. Scan all org SSO provider rows at login

Login could have loaded enabled GitHub rows and tested each grant against the user's teams in application code.

**Rejected because**: that is O(orgs) per login and weakens tenant isolation by bringing unrelated tenant grants into the login path.

### 2. Query once per GitHub team

Login could have issued a separate database query for each org/team membership key.

**Rejected because**: users can belong to many teams, making login an N-query path with avoidable latency and load.

## Consequences

- **GitHub team-gated login is indexed.** A user's team set resolves to granting org rows with one `$in` query.
- **Grant semantics are materialized at write time.** Lowercasing and wildcard expansion are centralized in the provider row validation/flattening path.
- **The storage schema includes internal lookup state.** `github_team_keys` is not part of the public API but is critical for login performance.
- **Tenant isolation is easier to reason about.** The query returns only matching enabled GitHub rows rather than scanning all tenant configs.
- **Rechallenge if GitHub identifiers change.** If GitHub team identifiers become opaque non-normalizable tokens, the key shape and index should be revisited.

## Status note (2026-10-14)

`FlattenGitHubTeamKeys`
([pkg/auth/orgsso/types.go](../../pkg/auth/orgsso/types.go)) materialises only
grants whose `Verified` flag is set. An unverified grant is stored and returned
by the API but never enters `github_team_keys`, so it matches no login until
the org has proven it controls that GitHub org.
