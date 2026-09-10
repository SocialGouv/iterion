# Observability — logs, error tracking and tracing

iterion's observability layers answer different questions:

| Layer | Question it answers | Where it lives |
|---|---|---|
| Run events (`events.jsonl`) | *What did this run do?* | the run store, the studio, `iterion report` |
| **Process logs** | *What is this process doing right now?* | stdout/stderr of the server / runner / dispatcher |
| **Error tracking** | *Is the deployment healthy — what crashed, how often, since which release?* | a Sentry or GlitchTip project |
| **Tracing** | *Where did the time go — which API routes are slow, which provider calls are expensive?* | the same Sentry/GlitchTip project (Performance) |

Run events are documented in
[persisted-formats.md](persisted-formats.md). This page covers the
other three: how to configure them, and what an operator gets.

## Environment variables

| Variable | Default | Effect |
|---|---|---|
| `SENTRY_DSN` | *unset* | **The master switch for error tracking AND tracing.** Unset ⇒ nothing is initialised and iterion behaves exactly as it did. Set ⇒ panics, fatal errors, error logs and run alerts are reported. |
| `SENTRY_ENVIRONMENT` | *unset* | Tags every event with the deployment (`production`, `staging`, …). Set it — untagged events from several deployments land in one undifferentiated stream. |
| `SENTRY_TRACES_SAMPLE_RATE` | *unset* (= **tracing off**) | A fraction in `[0, 1]`: how many units of work become a transaction. A **second** opt-in on top of the DSN. |
| `ITERION_LOG_FORMAT` | `human` on the CLI, **`json`** on server / runner / dispatcher | `human` (emoji console lines) or `json` (one object per line). |
| `ITERION_LOG_LEVEL` | `info` | `error`, `warn`, `info`, `debug`, `trace`. |

The `SENTRY_*` names are the SDK's standards, not `ITERION_*` ones, so
the DSN you copy from the project page drops into the deployment recipe
unchanged. They are read from the process environment, and — like every
other iterion env var — from a `.env` next to the project (see
`loadDotEnvFromCwd`).

## Logs

`pkg/log` is the single logging seam: leveled (`error`/`warn`/`info`/
`debug`/`trace`), structured (`WithField` / `WithFields` / `WithError`),
and rendered in one of two formats.

**JSON is the default on every long-running surface** — `iterion
server`, `iterion runner`, `iterion dispatch` — because their logs get
shipped. One object per line, with a stable field contract:

```json
{"ts":"2026-08-19T09:12:44.117Z","level":"error","msg":"dispatcher: claim failed","fields":{"issue_id":"native:3a81df64"}}
```

`ts` (RFC3339 nanoseconds, UTC), `level`, `msg`, and `fields` (omitted
when empty) are the names log shippers key on — Loki labels, ES
mappings — and do not change across versions.

**Human is the default on the interactive surfaces**: `iterion run`,
`iterion studio` (and `iterion server` in local mode, which serves the
same studio), and every other CLI command an operator watches — but
they honour the env vars too.

Both are a default, never a cage: `ITERION_LOG_FORMAT=human` on a
runner pod and `ITERION_LOG_FORMAT=json` on a CLI invocation both work.

Rollout fencing adds two bounded-cardinality Prometheus signals:

- `iterion_runner_admission_rejected_total{reason="schema"}` and
  `{reason="future_epoch"}` count deliveries delayed before any execution
  side effect because the runner cannot admit their wire schema or rollout
  generation;
- `iterion_rollout_epoch_regression_total{component="server|runner"}` counts
  process starts below the persistent epoch high-water mark.

Epoch numbers are exposed in `/healthz` and `/readyz` JSON as `epoch` and
`high_water_epoch`; they are deliberately not Prometheus labels on run
metrics, which would create unbounded historical series.

The format holds for the WHOLE process, not just its own lines. A run's
per-run logger (the one teed into `run.log` and the studio console) is a
`WithWriter` fork of the process logger, so it keeps the format, the
fields and the tracker hook; a library seam handed no logger at all
falls back to `log.NewFromEnv` / `log.NewFallback`, which read the same
two env vars. Nothing in `pkg/**` writes a diagnostic around the seam.

## Error tracking

### What it is

An SDK speaking the **Sentry DSN protocol**. The same DSN, SDK and
events work against:

- a hosted **Sentry** project (`https://<key>@o123.ingest.sentry.io/456`), and
- a self-hosted **GlitchTip** (`https://<key>@glitchtip.example.org/1`),
  which implements the Sentry ingestion API — no GlitchTip-specific
  client exists or is needed.

Nothing in iterion is Sentry-specific beyond the wire protocol. See
[ADR-088](adr/088-error-tracking-via-sentry-dsn-protocol.md) for the
choice and the rejected alternatives.

### Wiring a deployment

1. Create a project in Sentry or GlitchTip (platform: Go) and copy its
   DSN.
2. Set `SENTRY_DSN` and `SENTRY_ENVIRONMENT` on the **production
   processes**: the server, every runner pod, and the dispatcher if you
   run one. In Kubernetes that is one `env:` entry per deployment, or a
   shared `envFrom:` secret:

   ```yaml
   env:
     - name: SENTRY_DSN
       valueFrom: { secretKeyRef: { name: iterion-observability, key: sentry-dsn } }
     - name: SENTRY_ENVIRONMENT
       value: production
   ```

3. Leave it unset everywhere else. A short-lived `iterion run` with a
   DSN in scope will also report — that is fine and deliberate (the
   wiring is uniform at the CLI root) — but it is rarely what you want
   on a laptop.

Events are tagged with the **release**
`iterion@<version>+<commit>`, taken from the build stamp
(`pkg/internal/appinfo`). A binary built without `-ldflags` reports
`iterion@dev`, which makes regression triage useless — release builds
and the Taskfile already inject it; make sure your own image build
does too.

### What gets captured

| Seam | What it reports |
|---|---|
| CLI top level ([cmd/iterion/main.go](../cmd/iterion/main.go)) | a panic escaping a command on the main goroutine — captured, flushed, then **re-panicked** so the process still dies the way it did. Go cannot recover another goroutine's panic from here, which is why the worker seams below capture in their own recovery blocks |
| CLI fatal path | the error that ends the process with exit 1, before `os.Exit` skips every defer. A user-input error (exit 2 — bad flag, missing file) is a typo, not an incident, and is never reported |
| [pkg/server/gosafe.go](../pkg/server/gosafe.go) and [cloudpublisher](../pkg/server/cloudpublisher/publisher.go) `goSafeDetached` | a panic in a fire-and-forget background goroutine (audit insert, `MarkUsed`, invitation mail), with the task label. These CONTAIN the panic — the task was best-effort |
| `errtrack.Go` / `errtrack.TrackPanic` ([pkg/errtrack/crash.go](../pkg/errtrack/crash.go)) | a panic in a detached goroutine of the three daemons, tagged with the `surface` that raised it: the server's hub, file watchers, staged-upload reaper, pipeline admission loop, OIDC sweeper, board-dispatch workers and WS/PTY pumps; the runner pod's lease heartbeat, sandbox reaper, queue-depth gauge and credential refreshers; the dispatcher's actor loop and config watcher. This guard **re-panics** — those goroutines hold jobs the process cannot do without, so the crash stays a crash |
| The dispatcher actor's inner recover blocks and runtime fan-out branches | already recover and log at error level, so the log coupling reports them — no extra call site |
| The central logger, on the daemons and the studio | every **error** line becomes an event with the record's fields as context; every **warn** line becomes a breadcrumb attached to the next event. A run's own logger is a fork of the process one, so a run's error lines reach the tracker too |
| [pkg/alert](../pkg/alert/errtrack.go) | run health: `run_failed` and `budget_exceeded` as errors, `stall` and `budget_warning` as warnings, `stall_recovered` as a breadcrumb |

A goroutine added later joins the first class or the second by choosing
its helper: `goSafe` when the work is best-effort and the process should
survive it, `errtrack.Go` when its death is a crash worth reporting.
Library-internal goroutines outside the three daemons (run-stream
tailers, the event bus, supervisors) keep their own recover-and-log
path, which the log coupling reports.

Pending events are flushed (2 s bound) on shutdown and on the fatal
path.

Note: a panic at an explicitly-captured seam produces **two** grouped
issues — the exception (with the stack) and the error log line it also
emits (with the structured fields). That is deliberate; the two carry
different information.

### Failure modes

- **Malformed DSN** ⇒ one error-level log line (`errtrack: error
  tracking disabled — …`) and the process runs on without tracking. An
  observability misconfiguration never takes a run down.
- **Unreachable ingest host** ⇒ the SDK buffers and drops; flushes are
  bounded at 2 s, so a dead ingest host cannot delay a shutdown.
- **`SENTRY_DSN` unset** ⇒ zero behaviour change: no client, no
  goroutine, no network, no log line.

### Scrubbing

Everything passes the SDK's `BeforeSend` hook before it leaves the
process ([pkg/errtrack/scrub.go](../pkg/errtrack/scrub.go)):

- fields, tags and headers whose **name** looks sensitive
  (`authorization`, `cookie`, `token`, `secret`, `password`, `passwd`,
  `api_key`, `apikey`, `credential`, `private_key`, `dsn`, `session`,
  `bearer`) are replaced with `[redacted]` whatever their value —
  **unless the value is a number**, since a credential is text and the
  exemption is what keeps `input_tokens: 1200` a measurement instead of
  `[redacted]`. Bare `auth` is deliberately *not* in the list:
  substring matching would eat `author` / `pr_author`;
- credential-shaped **substrings** are redacted inside otherwise-useful
  text: URL userinfo (`https://key:secret@host` — the shape of a DSN),
  the `sk-` / `xai-` / `ghp_` / `glpat-` / `iap_` / `iwh_` token
  prefixes, `Bearer`/`Basic` header values, iterion's own
  `__ITERION_SECRET_*__` placeholders, and email addresses;
- the **user record** (id, email, ip) is cleared unconditionally.

Scrubbing is on the SDK's own send hooks rather than at the call sites,
so nothing can bypass it — including events the SDK generates itself.
Transaction envelopes take a *different* hook (`BeforeSendTransaction`),
which gets the same scrubber, walking every span's name/op/description/
tags/data.

### Smoke test

With the DSN exported, the cheapest end-to-end check is a command that
fails on the *fatal* path (exit 1):

```sh
export SENTRY_DSN='https://<key>@<host>/<project>'
export SENTRY_ENVIRONMENT=smoke-test

iterion dispatch /nonexistent-config.yaml   # exit 1 -> one event
```

Within a few seconds the project should show one event carrying the
read error, tagged `release: iterion@<version>+<commit>`, `environment:
smoke-test`, and the context `{"command": "iterion dispatch"}`.

Then unset `SENTRY_DSN` and run the same command: **nothing is sent**,
and the exit code and terminal output are byte-for-byte what they were.

Note that `iterion run /nonexistent.bot` is *not* a useful smoke test:
a missing file is a user-input error (exit 2) and those are
deliberately never reported.

## Tracing

### Turning it on

Tracing rides the **same DSN and the same client** as error tracking —
there is no second SDK and no second init. It needs a **second**
opt-in:

```sh
export SENTRY_DSN='https://<key>@<host>/<project>'
export SENTRY_TRACES_SAMPLE_RATE=0.05      # 5% of units of work
```

`SENTRY_TRACES_SAMPLE_RATE` unset, `0`, unparsable, or outside `[0, 1]`
⇒ **tracing is off even with the DSN set**. The refusal of a value that
is set but unusable is loud (one error-level line naming the variable)
and never costs you error tracking. The SDK does not read this variable
on its own; `errtrack.Init` resolves it and configures the client.

**Sampling guidance.** Keep production between **0.05 and 0.1**. Every
sampled unit of work is an envelope on the wire and a row in your
project's quota, and iterion's expensive surfaces (the studio SPA
polling, run-console requests) are chatty. `1.0` is for a smoke test or
a local session, not for a deployment.

### What gets traced

| Seam | Shape |
|---|---|
| **Every API request** ([pkg/server/server.go](../pkg/server/server.go), the root handler) | one transaction named after the **route pattern** — `GET /api/runs/{id}`, not `GET /api/runs/019f83…`, so a thousand run ids stay one entry in the performance view. Status, method and the request context ride along |
| **Every in-process LLM call** ([pkg/backend/model/generation_request.go](../pkg/backend/model/generation_request.go), `callAndAggregate`) | one `llm.generate` transaction tagged `llm.provider` / `llm.model` and carrying the call's input/output/cache-read token counts. Always **standalone, on an isolated hub** — a run's context still carries its launch request's long-finished transaction, so nesting under it would orphan the span (never exported) and pollute the global error trace context |

That is the **whole** list, and deliberately so:

- **A run gets no transaction.** Runs last minutes to hours; a span
  tree spanning a whole campaign is unreadable and its envelope is
  enormous. Use the run's own `events.jsonl` (and the studio timeline)
  for "what did this run do" — that is what it is for.
- **404s are not traced.** The SDK ignores them by default, which suits
  a server that also serves an SPA.
- The engine's node loop, the store and the dispatcher are **not**
  instrumented. One hand-made seam, done well, beats a tree of spans
  nobody reads. That includes `iterion dispatch`'s own little
  loopback mux (healthz + `/api/server/info` + the SPA): it is not the
  API server, and tracing a static-file host buys nothing.

Only the CLI-agent backends' *in-process* path is covered: a
`claude_code` / `codex` / `pi` / `kimi` / `grok` node shells out to its
own CLI, which iterion does not trace.

### Relationship to the OpenTelemetry wiring

iterion also has an **independent** OTel exporter
([pkg/cloud/tracing](../pkg/cloud/tracing/tracing.go)) driven by
`OTEL_EXPORTER_OTLP_ENDPOINT`, feeding the `otel.Tracer` spans in
`pkg/runtime`, `pkg/runner` and a couple of server handlers. The two
are complementary and share nothing: point one, the other, or both.
Sentry tracing needs no collector; OTLP needs one but reaches a
vendor-neutral backend.

### GlitchTip caveat

GlitchTip accepts transaction envelopes and renders basic performance
data, but does **not** implement Sentry's full performance product —
no span metrics or dashboards, no profiling, no session replay. The
instrumentation is identical either way; what differs is what you will
see. On GlitchTip, keep the sample rate at the low end: you are paying
the wire cost for a thinner view.

### Smoke test

```sh
export SENTRY_DSN='https://<key>@<host>/<project>'
export SENTRY_ENVIRONMENT=smoke-test
export SENTRY_TRACES_SAMPLE_RATE=1.0        # smoke only — never a deployment

iterion studio --no-browser-pane --port 4891 &
curl -s localhost:4891/api/v1/bots > /dev/null
```

Within a few seconds, **Performance** should show a transaction named
`GET /api/v1/bots`, tagged with the same `release` and `environment` as
your error events. Launch any bot on the `claw` backend and an
`llm.generate` transaction appears alongside it, tagged with the
provider and model that served it.

Then unset `SENTRY_TRACES_SAMPLE_RATE` and repeat: **no transaction is
produced**, the handler chain is the one that shipped before tracing
existed (the middleware is the identity function), and error tracking
keeps working exactly as above.
