# ADR-158: Run launches join the audit log — one choke point, one row per run, the request as the fallback principal

- **Status**: Accepted — the operator picked this option on 2026-11-03 (DA ↔ repository gap 1, "option A: sign-ins and run launches, two PRs"); this record is the ADR of the second PR, the launches, and completes ADR-157 — "Authentication events join the audit log", the sign-ins, which lands with the first PR.
- **Date**: 2026-11-03
- **Authors**: devthejo (session with Claude), for the MTSSF security qualification of iterion
- **Landed by this change**: `run.launched` written by `auditRunLaunched` at the one site every launch surface crosses, `Server.launchRun` / `launchRunWith` ([`pkg/server/runs_launch_audit.go`](../../pkg/server/runs_launch_audit.go)); `run.forked` written by `auditRunForked` from the fork handler ([`pkg/server/runs_control.go`](../../pkg/server/runs_control.go)); the `launchOrigin` value each lane hands the choke point (`requestLaunch`, `webhookLaunch`, `systemLaunch`). the cloud launch path's queue failure wrapped in `RunPersistedError` ([`pkg/runview/service_launch.go`](../../pkg/runview/service_launch.go)). Witnesses: `pkg/server/launch_audit_test.go` — one scenario per lane, each driven through its public entry (route handler, webhook intake, daemon tick), a launch that may have started, a refused one, and the sweep `TestLaunchAuditSweep_NoRawRunServiceLaunch`.
- **Relates to**: ADR-157, "Authentication events join the audit log" (the sign-ins, the scope rules this record inherits) · [cloud-admin-guide.md §1.5](../cloud-admin-guide.md) (the operator's view) · [ADR-046](046-event-driven-runs-trigger-spine.md) (the trigger spine, one of the lanes) · the security qualification note (DA cadre 4, Traçabilité 3)

## Context

A run came to exist through six lanes — `POST /api/runs`, the operator's
"launch now" on a pipeline card, an inbound webhook delivery (live or
replayed from a parked row), the cloud schedule tick, the board
dispatcher and the trigger spine — and none of them wrote an audit row.
"Who launched this run, from where, when" was answered by the run
document itself (owner, source kind, created_at), which `run.deleted`
removes and whose retention (one year) is not the audit log's (400
days). The qualification's retained scenario — a diverted agent spending
a forge token — is reconstructed from the sign-ins (ADR-157) and from the
launches; this record is the second half.

## Decision

**One choke point, and a sweep that keeps it the only one.** Every lane
calls `Server.launchRun` (or `launchRunWith` where the lane receives its
run service as a parameter); the choke point launches, and on success
writes the row. A launch that errors is audited by the same fact the
meters read off the error, `RunMayHaveStarted`: an error returned after
the run was persisted, or after a queue publish that may have landed, is a
run the runner may execute, so it writes its row with
`meta.outcome: may_have_started`, keyed by the run the error names
(`RunPersistedError.RunID`). The cloud launch path wraps a queue failure
in `RunPersistedError` with the id it minted — the rule that type already
states for a publish that may have landed — so the id the caller never
held travels with the error. A launch refused before anything could
execute writes nothing; the error itself is returned untouched.
`TestLaunchAuditSweep_NoRawRunServiceLaunch` refuses any
`.Launch(` call in `pkg/server` outside the choke point's file, whatever
the receiver is named — the gesture, not the identifier — so the seventh
lane either crosses the choke point or names itself in the allowlist with
its reason.

**The principal comes from the launch context, then the request, then the
lane's own name.** A request lane that launches on the request's context
(POST /api/runs) and a daemon that stamps its own owner on the launch
context (dispatcher, scheduler, spine) attribute by that context. The
pipeline card's "launch now" detaches its launch from the request — a
Background context, so the run outlives the HTTP call — and that context
names nobody: the `launchOrigin` carries the request, and the row is the
operator's, on their team, from their address. This fallback is a lane in
its own right, not a belt-and-braces, and has its witness
(`TestLaunchAudit_PipelineLaunchNowIsAttributedToTheOperator`). A lane
with neither — the local pipeline-admission loop — names itself
(`pipeline-admission`); it runs only outside cloud mode, where no audit
store exists, so the name is wired for the class and never recorded.

**One tenant row per run, on the run's team.** The tenant is the one the
launch was scoped to, else the principal's team. A row on an empty tenant
is a row no listing shows, which is why the request fallback above exists;
the choke point never writes a platform row — every launch has a tenant,
or it is a local-mode launch with no store.

**Meta names the bot and the trigger, never the launch vars.** `bot`,
`workflow` (the file's base name), `repo`, `trigger`, `delivery_id`,
`schedule_id`. `trigger` is the run's own provenance when the spec carries
one (`dispatcher` | `schedule` | `studio_chat` | `webhook`), else the
client of the request that served it — `studio` for a browser, told from
an `api` client as the login routes tell them (`isBrowserClient`) — else
absent, the daemon actor already naming the trigger. The launch vars are
never copied: they may carry a forge token or a secret an operator typed.

**A fork is a run of its own, with its own row.** `POST /api/runs/{id}/fork`
creates a new run without crossing the choke point; it writes
`run.forked` — the operator as actor, the child as target, the parent in
`meta.parent_run_id`. A resume continues an existing run and writes no
row: the run already has its `run.launched`.

**IP and user-agent are the request's; a webhook's address is the
intake's.** The intake resolves the delivery's source address once
(trusted proxy headers applied) and hands it to the choke point; it wins
over a re-derivation from the request, which on a replay lane is
synthetic and has no peer. A daemon's row carries neither.

## Options considered

- *A view over `runs`* (tenant, owner, source, created_at): what existed.
  Rejected — `run.deleted` erases the trace, the retention differs, and
  the deployment checklist asks for an audit row.
- *A row at each of the six call sites*: the first draft of the ticket.
  Rejected for the choke point — the seventh site would forget, and the
  sweep cannot guard N sites.
- *Carrying the request's identity on the detached launch context*
  (`context.WithoutCancel` of the request's context) instead of the
  request fallback: cleaner on paper, but it changes what the run service
  sees (owner, tenant filter) on a lane that deliberately launches with a
  bare context. Kept as the structural follow-up if a second detached
  request lane appears; the fallback stays as the net either way.
- *Auditing the resume*: rejected — not a new run, and the row would
  duplicate the run's own event stream.

## Consequences

- `GET /api/teams/{id}/audit` without `?action=` lists one more row per
  run and per fork; `?action=run.launched` is the "who launched what"
  query the qualification asks for, with `meta.trigger` telling a studio
  launch from an API client, a webhook, a schedule or the dispatcher.
- `audit.Event.Target` gains `run`; the `Event` comment lists it.
- A cloud launch whose queue publish fails now names its run on the
  error: an inbound webhook delivery that hit it records that run id
  (status `launch_error`) where it recorded `launch_uncertain` with no
  run — neither is retried, and the row now says which run may be
  executing.
- The account-erasure change (gap 3) scrubs `{ip, user_agent}` of these
  rows as of every other row whose actor is the erased account.
- A new launch lane in `pkg/server` is refused by the sweep until it
  crosses `Server.launchRun`; a `Launch` method of another type names its
  file in `launchAuditSweepAllowed` with the reason.
