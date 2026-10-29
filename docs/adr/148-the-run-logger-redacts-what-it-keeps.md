# ADR-148: The run logger redacts every record it keeps — one chokepoint, not N sink passes

- **Status**: Accepted — the operator picked this option on [#2458](https://github.com/SocialGouv/iterion/issues/2458) on 2026-10-22 ("option A — an ADR first, then redaction at the run logger"); this record is the ADR, and the change that carries it is the implementation.
- **Date**: 2026-10-29
- **Authors**: devthejo (session with Kimi), for #2458
- **Landed by this change**: `iterlog.Logger.WithRedactor` ([`pkg/log/log.go`](../../pkg/log/log.go)), the wiring of the run's secret guard onto the run's logger at its one construction site (`runview.BuildExecutor`), and the removal of the log-only per-site passes (`h.red` on formatted lines, `farSideForLog`). Witnesses: `pkg/log/redactor_test.go`, `pkg/backend/model/hooks_runlog_chokepoint_test.go`, and the rewired `claw_*_redaction_test.go` / `claw_relay_hook_lazy_pass_test.go`.
- **Relates to**: [secrets.md](../secrets.md) (Layer 0, the guard this composes with) · [ADR-088](088-error-tracking-via-sentry-dsn-protocol.md) (the hook the redaction precedes) · [#2385](https://github.com/SocialGouv/iterion/issues/2385) (the crossing redaction whose adversarial rounds counted the remaining class) · [ADR-146](146-secret-boundary-v2-grants-and-sinks.md) (secret boundary v2 — this record changes no boundary, only where the sink pass runs)

## Context

A run's log sinks printed the errors of non-crossing producers raw: four sinks
in `hooks.go` and six in `executor_*` wrote `info.Error` / `err` to the
operator's log with no redaction, and six far-side arguments of `hooks.go`
lines sat out of the sandbox relay's reach. #2385 had redacted the crossing's
own sinks; the rest of the class stayed open, one `h.red(...)` /
`redactWith(...)` / `farSideForLog{...}` at a time. Ten more guards would leave
the eleventh to drift: the sinks are written by different people at different
times, and a sink that forgets the pass leaks exactly once, into the one
artefact every debugging session reads.

## Decision

**The run's logger carries the redactor.** `Logger.WithRedactor(fn)` forks a
logger that passes every record it KEEPS through `fn` — the formatted line, a
`LogBlock`'s header and body, and the string values of its inherited fields —
and `runview.BuildExecutor` wires the run's `secretguard.Guard.Redact` there,
at the one construction every plane (CLI, launch API, resume, subbot,
dispatcher, cloud runner) already shares. Producers hand the log third-party
text raw; the per-site passes whose text reached nothing but a log line are
removed.

The clauses the operator asked this record to fix:

**Which records are scanned, and when.** Lines, block headers and bodies, and
JSON field values — every shape a record takes. The scan runs **after the
level test**, so a record the logger drops is never scanned and a quiet run
pays nothing, and **before `dispatchHook`**, so the error tracker (errtrack's
warn+ hook) receives the redacted text — Sentry gets known-value redaction
too, not only the console and the run.log tee.

**Composition.** The redactor IS Layer 0's `Guard.Redact` — the same
known-values-any-encoding + heuristic + recursive-decode pass the event
emitter runs — so the log and the events answer identically. It composes with
`errtrack.Redact` (the tracker's own placeholder recognition) and with the
placeholders by **idempotence**: a placeholder matches no known value and the
heuristic never redacts one, so a record already scrubbed eagerly crosses the
chokepoint unchanged. That is what lets the eager passes that must stay —
text that also reaches an error, an event or a checkpoint, and any text a
caller TRUNCATES before logging (the chokepoint can only recognise a value
the cut kept whole; redact-then-bound is the only safe order) — coexist with
it without double-rewriting.

**The kill switch** is the existing `ITERION_SECRETS_REDACT=off` and no new
knob: it builds a guard whose `Redact` passes text through (or no guard at
all, in which case no redactor is wired). The logger adds no configuration
surface of its own.

**The non-run process logger does nothing new.** It is never given a
redactor: its lines are iterion-authored, and the far-side text that reaches
them has already crossed a boundary that scrubbed it (the node's error at
`Execute`, the event payloads at the emitter). Only the fork
`BuildExecutor` builds — and its own forks: the run.log tee, the per-node
field forks, the delegate registry's backends — carries the pass. A
`WithRedactor` fork's parent is untouched.

**The measured cost.** `BenchmarkRunLoggerRedactedLine`
(`pkg/backend/model/hooks_runlog_chokepoint_test.go`), on a guard holding 32
known values (a run's provider keys, declared secrets and minted grants),
linux/amd64, i9-14900HX: a typical tool-loop line (~90 bytes) costs **427 ns
unredacted vs 25.1 µs redacted**; a 4 KiB block body, **8.4 µs vs 1.69 ms**.
The heuristic detector dominates the pass, not the literal matcher. On a
chatty `--log-level debug` run (order 10⁴ lines) this is ~0.25 s of CPU
against minutes of wall time and provider latency — paid only on records the
log keeps, and nothing at all on a run with no guard (no known values and
the heuristic off wires no redactor).

## Consequences

- A new log sink — a ` Warn` in a backend, a `LogBlock` in a hook — inherits
  the redaction instead of re-deriving it. The class #2458 counted closes by
  construction, not by audit.
- Kept eager: `persistToolPayload`, the turn/plan/checkpoint sinks (their
  stores bypass the redacting emitter), `RedactHead`/`BlockPreview*` call
  sites (cut-then-log ordering), and the crossing's error paths
  (`redactedFarSide` / `redactedThrough`), whose text reaches errors and
  events whatever the log level. `farSideForLog`, the `h.red` on formatted
  lines, and the gates that existed only to defer those passes are gone.
- The scan is per record and synchronous on the logging goroutine, like the
  write itself; the benchmark above is the budget witness.
