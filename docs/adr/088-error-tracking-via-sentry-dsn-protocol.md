# ADR-088: Error tracking through the Sentry DSN protocol, opt-in by env

- **Status**: Accepted
- **Date**: 2026-08-19
- **Code**: [pkg/errtrack/errtrack.go](../../pkg/errtrack/errtrack.go)
  (`Init`, `Enabled`, `Capture*`, `Flush`),
  [pkg/errtrack/scrub.go](../../pkg/errtrack/scrub.go) (`scrubEvent`,
  `Redact`),
  [pkg/errtrack/hook.go](../../pkg/errtrack/hook.go)
  (`LogHook`, `AttachLogHook`),
  [pkg/errtrack/tracing.go](../../pkg/errtrack/tracing.go) +
  [http.go](../../pkg/errtrack/http.go) +
  [span.go](../../pkg/errtrack/span.go) (`TracingEnabled`,
  `HTTPMiddleware`, `StartSpan` — see the 2026-08-20 addendum),
  [pkg/log/log.go](../../pkg/log/log.go) (`Hook`, `SetHook`),
  [pkg/alert/errtrack.go](../../pkg/alert/errtrack.go) (`TrackerSink`)
- **Runbook**: [docs/observability.md](../observability.md)

## Context

iterion's observability was, until now, entirely *run-scoped*: every
run writes `events.jsonl`, the studio replays it, `iterion report`
renders it. That is excellent for "what did this run do" and useless
for "is the fleet healthy" — a panic in a runner pod, a dispatcher that
starts logging errors at 03:00, a `nil` map write in a fire-and-forget
server goroutine leave no trace anyone will find. The store belongs to
the run; a crash belongs to the deployment.

The gap is felt only in the deployed shapes (cloud server, runner pods,
a dispatcher daemon) — an operator running `iterion run` on a laptop
reads the error on their terminal. So whatever we add must be
**invisible** to the laptop case and must not become a dependency the
single-user path pays for.

## Decision

Use **`github.com/getsentry/sentry-go`**, wrapped in a thin
`pkg/errtrack`, activated **only when `SENTRY_DSN` is set at runtime**.

The DSN protocol — not the vendor — is the thing being chosen. The same
DSN, the same SDK and the same events work against a hosted Sentry
project and against a self-hosted **GlitchTip**, which implements the
Sentry ingestion API. A deployment that refuses SaaS runs GlitchTip and
changes nothing but the DSN's host.

Concretely:

- **Off by default, off completely.** With `SENTRY_DSN` unset, `Init`
  returns before touching the SDK: no client, no background worker, no
  network, not even a log line. Every helper is one atomic load and a
  return. This is what makes the wrapper acceptable on the CLI path.
- **Loud but never fatal.** A malformed DSN is reported at error level
  through the caller's `*log.Logger` and the process carries on.
  Observability must not be able to take a run down.
- **`release` + `environment` always.** Release is
  `iterion@<version>+<commit>` from `pkg/internal/appinfo`; environment
  comes from the SDK's own `SENTRY_ENVIRONMENT`. Events without a
  release cannot be attributed to a version, which makes regression
  triage guesswork.
- **Capture at the process seams that already exist** — the CLI top
  level (recover, capture, *re-panic*), the fatal-error path before
  `os.Exit`, `server.goSafe`'s recovery block, and the alert manager —
  never scattered at call sites.
- **Coupled to `pkg/log`, not bolted beside it.** The central logger
  grew a warn+ `Hook`; `errtrack.AttachLogHook` is the only consumer.
  An error line becomes an event carrying the record's structured
  fields, a warn line becomes a breadcrumb. There is no second logging
  path and no `errtrack.Log*` API.
- **Scrubbing is on the SDK's own `BeforeSend`**, so nothing bypasses
  it: sensitive field names dropped, URL userinfo / provider and
  iterion token prefixes / `__ITERION_SECRET_*__` placeholders / emails
  redacted, the user record cleared.

## Alternatives considered

**A hand-rolled sender (POST the envelope ourselves).** iterion already
declines dependencies for small jobs — `loadDotEnvFromCwd` exists so
godotenv doesn't. But the Sentry envelope format, the rate-limit
headers (`X-Sentry-Rate-Limits`), the retry/backoff, the client reports
and the stack-trace serialisation are not a small job, and getting the
rate-limit half wrong turns an incident into a self-inflicted DoS
against the operator's own ingest. Rejected: the dependency is pure Go,
CGO-free (so the static `CGO_ENABLED=0` build is unaffected) and
vendored like every other.

**OpenTelemetry (`otel` logs/error events).** Tempting, since
`pkg/cloud/tracing` already speaks OTLP. But OTel's error story is
spans and log records — there is no issue grouping, no regression
detection, no release health, and no self-hostable receiver that gives
an operator the "this crashed 41 times since v3.48" view that motivated
this work. It would have meant shipping a *collector* to get half the
answer. Tracing stays OTel's job; incidents are Sentry-protocol's. The
two are complementary and are wired independently.

**Reusing `pkg/alert` + a webhook.** The alerting manager already fans
run-health conditions to a generic webhook, and one could point it at
an incident tool. It observes the *run event stream* only: it cannot
see a panic in a server goroutine, a boot failure, or an error log from
the dispatcher — precisely the blind spot. It is now a *producer* for
the tracker (`alert.TrackerSink`), which is the right relationship.

## Consequences

- One new vendored dependency (`sentry-go`), pure Go, ~0 cost when
  disabled.
- The `SENTRY_DSN` / `SENTRY_ENVIRONMENT` names are the SDK's
  standards rather than the repo's `ITERION_*` convention. Deliberate:
  an operator pastes the DSN from the Sentry/GlitchTip project page
  into the deployment recipe every other service already uses.
- A panic at an explicitly-captured seam (`goSafe`) produces both the
  exception event *and* the event derived from its error log line. The
  duplication buys a stack trace on one and the record's fields on the
  other; it is documented in the runbook rather than papered over with
  a suppression mechanism.
- `pkg/log` now has a hook slot. It is single-consumer by design — if a
  second consumer ever appears, the slot becomes a slice, not a second
  logging abstraction.

## Addendum — 2026-08-20: tracing, opt-in on the same client

The decision above closed with *"Tracing stays OTel's job; incidents
are Sentry-protocol's"*. That still holds for the OTLP path
([pkg/cloud/tracing](../../pkg/cloud/tracing/tracing.go)), which is
untouched — but it read as "iterion will never emit a Sentry
transaction", and a deployment already paying for a Sentry/GlitchTip
project should not need a collector to answer *"which route is slow"*.
Tracing is therefore wired **on the same client**, and stays off unless
asked for. The two paths are independent and complementary: point one,
the other, or both.

- **A second opt-in, not a consequence of the first.**
  `SENTRY_TRACES_SAMPLE_RATE` in `[0, 1]` enables it; unset, `0`,
  unparsable or out of range ⇒ off even with a DSN set. The SDK does
  not read that variable natively, so `Init` resolves it and sets
  `EnableTracing` / `TracesSampleRate`. A value that is set but unusable
  is reported loudly and never costs error tracking — same rule as a
  malformed DSN. Off is the safe default even though the family was
  requested at build time: a transaction per request is quota an
  operator turns on deliberately.
- **Auto-instrumentation for HTTP, exactly one hand-made span.** The
  server's root handler is wrapped with the SDK's own `sentryhttp`
  (behind `errtrack.HTTPMiddleware`, so `pkg/server` does not import
  the SDK); the one hand seam is the in-process provider call in
  `pkg/backend/model.callAndAggregate`. The engine's node loop, the
  store and the dispatcher are deliberately NOT instrumented — one
  seam done well beats a span tree nobody reads.
- **A run is not a transaction.** Runs last minutes to hours.
  Transactions are request-scoped or call-scoped; a run's story is
  already `events.jsonl`, which is the layer built for it. This is the
  non-goal most likely to be "fixed" by a later pass, hence recorded.
- **Zero overhead when off, provably.** `HTTPMiddleware` returns the
  identity function and `StartSpan` returns a nil handle plus the
  caller's own context, so an un-traced deployment runs the same
  handler chain and allocates nothing. Both are asserted, not asserted
  *about*.
- **Transaction naming is a design constraint, not a detail.** The SDK
  names a transaction from `http.Request.Pattern`, which the mux stamps
  during routing — but the auth layer forwards a `WithContext` copy, so
  the outermost middleware always sees it empty and every run id would
  become its own transaction. The server resolves the registered
  pattern up front and hands it to the middleware.
- **Transactions need their own scrubbing hook.** `BeforeSend` never
  sees them; `BeforeSendTransaction` gets the same scrubber, extended
  to walk spans. Wiring one without the other would have shipped an
  unscrubbed wire path.
- **Consequence — the key filter gained a numeric exemption.** Span
  measurements exposed that `token` in `sensitiveKeys` was redacting
  `input_tokens` as readily as `access_token`. A credential is text, so
  a numeric value is now exempt: over-redaction destroys an event as
  surely as a leak.
- **GlitchTip renders transactions but not Sentry's performance
  product** (no span metrics, no profiling, no replay). The
  instrumentation is identical; the caveat is about what the operator
  sees, and lives in the runbook.
