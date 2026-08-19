---
name: tracing
description: Opt-in tracing family for the instrument campaign — Sentry-first transactions/spans, env-tunable sampling with a conservative prod default, per-stack entry points, and what GlitchTip does/doesn't support. Read ONLY when the run's scope includes "tracing".
---

# tracing — opt-in family (Sentry-first)

Only wire this when `scope` explicitly includes `tracing`. It rides the
SAME SDK and DSN as the errors family (never a second init), so errors
must be wired first or in the same slice.

## The model

Sentry-first: **transactions** (one per unit of work — an HTTP request,
a job, a worker cycle) containing **spans** (the timed steps inside —
DB call, outbound HTTP, subprocess). Most value comes from the SDK's
auto-instrumentation of the repo's frameworks; hand-made spans are for
the repo's own hot seams only.

## Definition of done

- **Sampling is env-tunable with a conservative prod default.** Expose
  the standard `SENTRY_TRACES_SAMPLE_RATE` env (the SDKs read it
  natively or via one option line); default it LOW (0 unless the env is
  set — tracing off is the safe default even when the family is
  requested at build time, the operator turns the dial at runtime).
  Never hardcode 1.0 outside dev/docs examples.
- **Auto-instrumentation first**: enable the SDK's integrations
  matching the survey's entry points (HTTP server/client, DB) —
  Go: `sentry-go`'s `sentryhttp`/`sentryecho`/… handler wrappers plus
  `EnableTracing: true, TracesSampleRate: rate` in ClientOptions;
  Node: `Sentry.init({ tracesSampleRate })` + the framework integration
  already required by the errors family (v8 auto-instruments express/
  fastify/pg/redis when init runs first — the `instrument.js` order
  rule is HARD here);
  Python: `traces_sample_rate=rate` in `sentry_sdk.init` (django/
  flask/fastapi/celery integrations pick it up).
- **Hand spans sparingly**: one or two of the repo's own hot seams
  (the survey tells you which), via the SDK's span API, behind the
  same "enabled" check as everything else.
- **Zero overhead when off**: rate 0 / DSN unset ⇒ no transaction
  objects on hot paths (guard hand-made spans with the enabled check).
- **Docs**: the sample-rate env + what gets traced land next to the
  DSN docs.

## GlitchTip caveat (state it in the docs you write)

GlitchTip accepts transaction envelopes and renders basic performance
data, but does NOT implement Sentry's full performance product
(no span metrics/dashboards, no profiling, no session replay). The
instrumentation is identical either way — the caveat is about what the
operator will SEE, so write it where you document the DSN wiring.

## Verification

Same discipline as errors: a unit test with the mock/recording
transport asserts that (rate=1, DSN set) one transaction envelope is
produced for a sample unit of work, and that rate=0/DSN-unset produces
none. No live DSN in tests, ever.
