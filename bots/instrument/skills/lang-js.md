---
name: lang-js
description: Node.js / TypeScript reference for the instrument campaign — @sentry/node for error tracking (init order, process handlers, express/fastify seams, testTransport), pino as the default structured logger when the repo has none, hook patterns for existing loggers. Read when the target repo is Node/JS/TS.
---

# lang-js — instrumenting a Node.js / TypeScript repository

## Error tracking: `@sentry/node`

The official Node SDK (v8+ line) speaks the Sentry DSN protocol —
GlitchTip accepts it unchanged. Core wiring:

```ts
import * as Sentry from "@sentry/node";

export function initErrTrack() {
  const dsn = process.env[DSN_ENV_VAR];
  if (!dsn) return false;                 // opt-in: unset ⇒ zero behaviour change
  Sentry.init({
    dsn,
    release: RELEASE,                     // repo's own version/commit source
    environment: process.env.SENTRY_ENVIRONMENT,
    beforeSend: scrub,                    // drop secrets/PII
  });
  return true;
}
```

- **Init order matters** (v8): `Sentry.init` must run BEFORE the
  frameworks/handlers it auto-instruments are loaded — put the init in
  a module imported first from the entry point (the SDK docs call this
  the `instrument.js` pattern). For a plain error-tracking setup
  without tracing this is less strict, but keep the convention anyway.
- **Process seams**: `Sentry.init` installs `uncaughtException` /
  `unhandledRejection` handlers via its default integrations — verify
  they are active rather than re-adding your own. For servers:
  `Sentry.setupExpressErrorHandler(app)` (v8) / the fastify
  equivalent, AFTER the routes. Workers/queue consumers: capture in
  their existing catch blocks (`Sentry.captureException(err)`).
- **Flush on shutdown**: `await Sentry.flush(2000)` in the graceful
  shutdown path and before `process.exit` on the fatal path.
- Package manager: use the repo's own (respect its lockfile — npm /
  yarn / pnpm), pin per house convention, commit the lockfile change
  with the slice.

### Testing without a network

Two proven options; pick what fits the repo's test stack:
- `Sentry.init({ dsn: "https://k@example.invalid/1", transport: makeTestTransport() })`
  — a transport whose `send` pushes envelopes onto an array; assert on
  the captured events.
- Or spy on `Sentry.captureException`/`captureMessage` with the repo's
  mocking tool (vitest/jest `vi.spyOn`). Prefer the transport form —
  it exercises the real client pipeline (beforeSend, scrubbing).
Assert the off-state too: DSN unset ⇒ `initErrTrack() === false`, no
handlers installed by you, no client (`Sentry.getClient()` undefined).

## Logging

- **Repo already has a central logger** (winston, pino, a wrapper):
  EXTEND it — add JSON-in-prod default, level/fields if missing, and
  the tracker coupling at ITS seam (winston: a custom Transport;
  pino: a second stream via `pino.multistream` or a lightweight
  wrapper around the level methods).
- **No central logger**: use **pino** — the Node standard for
  structured JSON logs (JSON by default, one object per line,
  `level`/`time`/`msg` + fields). Human-readable dev output via
  `pino-pretty` ONLY on TTY/dev (a dev dependency or opt-in
  transport), never in prod. Wrap pino in a tiny module the repo owns
  (`lib/log.ts`), exposing leveled methods + child-logger fields, so
  the tracker coupling lives in one place: error → `captureException`
  / `captureMessage` (fields as context), warn → `addBreadcrumb`.
  Non-blocking, try/catch'd: logging must never throw.
- **Prod default**: JSON when not a TTY or when `NODE_ENV=production`
  — follow the repo's own env conventions, keep an explicit log-format
  env override.

## Stray sweep targets (JS/TS)

`console.log/warn/error` on server/worker paths (route handlers,
services, jobs). Leave alone: tests, build scripts, CLIs whose stdout
is the product, and browser bundles (frontend logging is a different
scope — flag it as a finding instead of wiring it drive-by).

## Metrics family: `prom-client`

- **SDK**: `prom-client`. Expose `register.metrics()` on the production
  HTTP server under `/metrics` (Express: `app.get('/metrics', async (_req, res) => res.set('Content-Type', register.contentType).end(await register.metrics()))`);
  for Fastify use `fastify-metrics`; for NestJS `@willsoto/nestjs-prometheus`
  (it installs the /metrics controller and default collectors only — HTTP
  request metrics need an interceptor of your own).
- **HTTP request count + latency by route and status**: express →
  `prom-express` or a hand middleware over `new Counter(...)` and
  `new Histogram(...)` from prom-client, registered in the registry you
  expose, with a ROUTE TEMPLATE label — `req.route.path` COMPOSED with
  mounted-router prefixes (`req.baseUrl` alone re-introduces the ids:
  nested routers answer the matched child path), never `req.originalUrl`. NestJS/fastify
  libraries default to route templates — keep it that way.
- **Process metrics**: `prom-client`'s default collect list already
  carries CPU, memory, FDs and event-loop lag (`collectDefaultMetrics()`)
  — present unless a custom registry was chosen deliberately.
- **Scrape wiring**: `ServiceMonitor` or scrape annotations per the repo's
  manifests (see the metrics family skill).
- **Env gate**: the house config convention, beside the DSN toggle.
