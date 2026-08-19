---
name: lang-python
description: Python reference for the instrument campaign — sentry-sdk for error tracking (init, LoggingIntegration for the error=event/warn=breadcrumb coupling, transport tests), structlog or logging+JSON formatter for standardized logs. Read when the target repo is Python.
---

# lang-python — instrumenting a Python repository

## Error tracking: `sentry-sdk`

The official Python SDK; GlitchTip accepts it unchanged. Core wiring:

```python
import os, sentry_sdk
from sentry_sdk.integrations.logging import LoggingIntegration

def init_errtrack() -> bool:
    dsn = os.environ.get(DSN_ENV_VAR, "")
    if not dsn:
        return False                      # opt-in: unset ⇒ zero behaviour change
    sentry_sdk.init(
        dsn=dsn,
        release=RELEASE,                  # repo's own version/commit source
        environment=os.environ.get("SENTRY_ENVIRONMENT"),
        before_send=scrub,                # drop secrets/PII
        integrations=[LoggingIntegration(level=logging.WARNING,      # breadcrumbs from warn+
                                         event_level=logging.ERROR)], # events from error+
    )
    return True
```

- **`LoggingIntegration` IS the coupling.** The error→event /
  warn→breadcrumb rule is native here: `event_level=ERROR` turns every
  `logging.error(...)` into a tracker event, `level=WARNING` records
  warn+ as breadcrumbs. If the repo logs through stdlib `logging` (or
  structlog bound to it), you get the coupling without touching call
  sites. Tune `level`/`event_level` to exactly warn/error.
- **Process seams**: `sentry_sdk.init` hooks `sys.excepthook` by
  default; framework integrations (django/flask/fastapi/celery) are
  auto-enabled when the package is importable — verify which apply to
  this repo's entry points rather than assuming. Long-running workers
  with their own try/except loops: `sentry_sdk.capture_exception(e)`
  inside the existing handler.
- **Flush/atexit**: the SDK flushes on interpreter exit via its atexit
  integration; for daemons with custom shutdown, call
  `sentry_sdk.flush(timeout=2)` explicitly.
- Dependency: add `sentry-sdk` via the repo's own dependency manager
  (pyproject/poetry/uv/requirements) with the house pinning style.

### Testing without a network

`sentry_sdk.init(dsn="https://k@example.invalid/1", transport=RecordingTransport())`
— a transport subclassing `sentry_sdk.transport.Transport` whose
`capture_envelope` appends to a list; assert an error log produces one
event with the expected message/extra, a warning only a breadcrumb.
Assert the off-state: DSN unset ⇒ `init_errtrack() is False` and
`sentry_sdk.get_client().is_active()` is false (or Hub client None on
older SDK lines — match the pinned version).

## Logging

- **Repo already has a central setup** (a `logging` config module,
  loguru, structlog): EXTEND it — add the JSON formatter/renderer and
  keep its API. With stdlib `logging`, the JSON side is a formatter
  (e.g. `python-json-logger`'s `JsonFormatter`, or a small hand-rolled
  formatter if the repo avoids new deps) applied to the prod handler.
- **No central setup**: prefer **structlog** for new structured
  logging (bind fields, `structlog.processors.JSONRenderer()` in prod,
  `ConsoleRenderer()` on TTY/dev), wired through stdlib `logging` so
  `LoggingIntegration` still sees every record. A repo that wants zero
  deps: stdlib `logging` + a JSON formatter module of its own.
- **Prod default**: JSON on server/daemon/worker entry points,
  human/console on interactive CLI; both switchable by the repo's
  log-format env convention.

## Stray sweep targets (Python)

`print(...)` on server/worker/job paths, per-module ad-hoc
`logging.basicConfig` calls (basicConfig belongs in ONE entry-point
setup, not scattered), bare `except: pass` that swallows what should
be captured (flag as finding if fixing is out of scope). Leave alone:
tests, scripts/, notebooks, CLI stdout that is the product.
