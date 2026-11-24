# ADR-033: Launch gate fails open on metering error

- **Status**: Accepted
- **Date**: 2026-06-22
- **Authors**: Adry
- **Code**: [pkg/server/launch_gate.go](../../pkg/server/launch_gate.go)

## Context

Run admission combines several gates: org suspend/read-only status, active-run concurrency, in-memory launch rate, monthly run quota, monthly cost cap, and the Mongo-backed metering increment. Some gates are cheap and local; others depend on Mongo and can transiently fail.

The system treats quotas as operator policy rather than the hard security perimeter. A transient storage outage should not wedge all launches for active orgs.

## Decision

The launch gate in [`pkg/server/launch_gate.go`](../../pkg/server/launch_gate.go) orders admission checks from cheap to expensive: load/reuse the team and check `CanLaunch`, then active concurrency, then in-memory launch rate, then Mongo-backed monthly caps and metering.

Store errors fail open. If fetching the team or active-run count fails, launch proceeds. If the org-usage `AllowRun` call fails, launch also proceeds, and the server logs that the launch is unmetered with a fail-open warning.

> **Amended 2026-11-05 ([ADR-162](162-the-fork-review-lane-is-a-gesture-on-floors-that-land-first.md), decision 5; design, lands with its U6).** An untrusted (fork-lane) launch fails closed on every arm that would admit it unmetered — an unreadable team or org, no counter, a metering error — whichever surface enters the gate, because its per-author bound is the perimeter against an outside party, not operator policy. A trusted launch keeps failing open.

Actual policy denials still fail closed: suspended/read-only orgs, exceeded concurrency, rate-limit buckets, and returned quota denials produce explicit launch denial responses. The fail-open path is for storage/metering errors, not for known policy violations.

## Trade-offs

| Dimension | Fail open on metering/store errors | Fail closed on metering/store errors |
|---|---|---|
| Availability | Transient Mongo outages do not stop all launches. | Storage blips can wedge launch traffic. |
| Billing/quota precision | Some launches may be unmetered. | No launch bypasses metering when storage is down. |
| Operator visibility | Logs `fail-open, launch unmetered`. | Errors are visible as failed launches. |
| User impact | Active orgs keep working during transient failures. | Users see outages caused by metering dependencies. |

The honest concession is that fail-open launches can escape monthly counters during metering outages.

## Alternatives considered

### 1. Fail closed on any Mongo metering error

The gate could deny launches whenever team lookup, active-run counting, or monthly counter updates fail.

**Rejected because**: transient Mongo errors would become a global launch outage even when the org is otherwise allowed to run.

### 2. Meter before cheap in-memory gates

The gate could charge quota before rate/concurrency checks and roll back later if a cheap gate denies.

**Rejected because**: it would add Mongo load and compensation paths to requests that can be rejected locally.

## Consequences

- **Launch availability is prioritised.** Temporary metering failures do not block otherwise valid runs.
- **Known denials remain enforced.** The gate still returns denials for explicit policy breaches.
- **Some launches can be unmetered.** Operators must rely on logs to notice and investigate storage-backed metering failures.
- **Gate order is intentional.** Cheap in-memory checks reduce unnecessary Mongo work.
- **Rechallenge if metering becomes billing-critical.** If metering becomes a strict billing boundary, fail-open behaviour should be replaced with a stronger admission design.
