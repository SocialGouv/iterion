# Observability — logs and error tracking

iterion has two observability layers, and they answer different
questions:

| Layer | Question it answers | Where it lives |
|---|---|---|
| Run events (`events.jsonl`) | *What did this run do?* | the run store, the studio, `iterion report` |
| **Process logs** | *What is this process doing right now?* | stdout/stderr of the server / runner / dispatcher |
| **Error tracking** | *Is the deployment healthy — what crashed, how often, since which release?* | a Sentry or GlitchTip project |

Run events are documented in
[persisted-formats.md](persisted-formats.md). This page covers the
other two: how to configure them, and what an operator gets.

## Environment variables

| Variable | Default | Effect |
|---|---|---|
| `SENTRY_DSN` | *unset* | **The master switch for error tracking.** Unset ⇒ nothing is initialised and iterion behaves exactly as it did. Set ⇒ panics, fatal errors, error logs and run alerts are reported. |
| `SENTRY_ENVIRONMENT` | *unset* | Tags every event with the deployment (`production`, `staging`, …). Set it — untagged events from several deployments land in one undifferentiated stream. |
| `ITERION_LOG_FORMAT` | `human` on the CLI, **`json`** on server / runner / dispatcher | `human` (emoji console lines) or `json` (one object per line). |
| `ITERION_LOG_LEVEL` | `info` | `error`, `warn`, `info`, `debug`, `trace`. |

`SENTRY_DSN` and `SENTRY_ENVIRONMENT` are the SDK's standard names, not
`ITERION_*` ones, so the DSN you copy from the project page drops into
the deployment recipe unchanged. They are read from the process
environment, and — like every other iterion env var — from a `.env`
next to the project (see `loadDotEnvFromCwd`).

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
`iterion studio`, and every other CLI command an operator watches.

Both are a default, never a cage: `ITERION_LOG_FORMAT=human` on a
runner pod and `ITERION_LOG_FORMAT=json` on a CLI invocation both work.

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
| CLI top level ([cmd/iterion/main.go](../cmd/iterion/main.go)) | any panic escaping a command — captured, flushed, then **re-panicked** so the process still dies the way it did |
| CLI fatal path | the error that ends the process with exit 1, before `os.Exit` skips every defer. A user-input error (exit 2 — bad flag, missing file) is a typo, not an incident, and is never reported |
| [pkg/server/gosafe.go](../pkg/server/gosafe.go) | a panic in a fire-and-forget server goroutine (audit insert, `MarkUsed`, invitation mail), with the task label |
| The central logger, on the daemons | every **error** line becomes an event with the record's fields as context; every **warn** line becomes a breadcrumb attached to the next event |
| [pkg/alert](../pkg/alert/errtrack.go) | run health: `run_failed` and `budget_exceeded` as errors, `stall` and `budget_warning` as warnings, `stall_recovered` as a breadcrumb |

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
  (`authorization`, `cookie`, `token`, `secret`, `password`, `api_key`,
  `credential`, `private_key`, `dsn`, `session`, `bearer`, `auth`) are
  replaced with `[redacted]` whatever their value;
- credential-shaped **substrings** are redacted inside otherwise-useful
  text: URL userinfo (`https://key:secret@host` — the shape of a DSN),
  the `sk-` / `xai-` / `ghp_` / `glpat-` / `iap_` / `iwh_` token
  prefixes, `Bearer`/`Basic` header values, iterion's own
  `__ITERION_SECRET_*__` placeholders, and email addresses;
- the **user record** (id, email, ip) is cleared unconditionally.

Scrubbing is on the SDK's own send hook rather than at the call sites,
so nothing can bypass it — including events the SDK generates itself.

### Smoke test

With the DSN exported, the cheapest end-to-end check is a command that
fails:

```sh
export SENTRY_DSN='https://<key>@<host>/<project>'
export SENTRY_ENVIRONMENT=smoke-test

iterion run /nonexistent.bot    # a fatal error -> one event
```

An event named after the error should appear in the project within a
few seconds, tagged with the release and `smoke-test`. Unset
`SENTRY_DSN` and run the same command: nothing is sent, and the
terminal output is byte-for-byte what it was.

To verify the log coupling instead, run a daemon with a DSN set and
make it log an error (for instance point `iterion dispatch` at an
unreadable config): the line appears both on stderr and as an event.
