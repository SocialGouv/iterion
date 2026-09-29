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
- **🪤 uvicorn's own log config, and WHEN it runs.** uvicorn applies its
  dictConfig when `Config()` is constructed: `uvicorn` and
  `uvicorn.access` get their own plain-text handlers with
  `propagate=False` (`uvicorn.error` reaches `uvicorn`'s; root routing is
  untouched). What decides is whether the seam runs AFTER `Config()`. It
  does under the uvicorn CLI, `uvicorn.run("mod:app")` from a launcher
  that is not `mod` itself, and `fastapi run --entrypoint mod:app` —
  there, clearing those loggers' handlers and setting `propagate = True`
  in the seam (called at import) works. It does not under
  `uvicorn.run(app)` (pass `log_config=None`, or a dict routing through
  the seam's handlers), `fastapi run <path>` (discovery imports the
  module first), or `python mod.py` whose `__main__` calls
  `uvicorn.run("mod:app")` with a run-once seam. Re-applying the routing
  in the lifespan startup still leaves `Started server process` and
  `Waiting for application startup.` in uvicorn's format and does nothing
  under `--lifespan off`; `fastapi run` also prints a banner to stdout
  outside `logging` — for an all-JSON stdout, launch production through
  `uvicorn`. Assert over the WHOLE captured stdout+stderr of a booted
  process — every line JSON **and** no planted value in any line, searched
  raw AND decoded (feed the lines to the `leaks()` helper below, as bytes) —
  not record-by-record. Strip the query string in an access-log filter:
  `uvicorn.access` prints the request line as received
  (`GET /callback?code=…`, percent-encoded) straight into the log store.
  (Paid: on a campaign's own diff, the uvicorn lines of the production
  entry point stayed plain text while every record-level test was green.)

## Capture-endpoint E2E (the net, not just the code path)

Mock transports prove the code path. The net is proven by booting the
instrumented process with the sink pointed at a LOCAL collector:

```python
import base64, gzip, http.server, json, threading, urllib.parse
captured, arrived = [], threading.Event()
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers["Content-Length"]))
        enc = self.headers.get("Content-Encoding")
        if enc == "gzip":
            body = gzip.decompress(body)
        elif enc == "br":                  # sentry-sdk's DEFAULT when `brotli` is importable
            import brotli
            body = brotli.decompress(body)
        elif enc:                          # never assert over bytes you did not decode
            raise ValueError(f"undecoded Content-Encoding {enc!r}")
        captured.extend(l for l in body.splitlines() if l.strip())  # envelope = header + items
        arrived.set()
        self.send_response(200); self.send_header("Content-Length", "2")
        self.end_headers(); self.wfile.write(b"ok")
    def log_message(self, *a): pass

def items(captured):
    """(type, payload) of every event/transaction item. Assert arrival on THESE: a message
    also rides later events' breadcrumbs, so a substring match can report a dropped event."""
    out, i = [], 0
    while i < len(captured) - 1:
        try:
            head = json.loads(captured[i])
        except ValueError:
            head = None
        if isinstance(head, dict) and head.get("type") in ("event", "transaction"):
            try:
                out.append((head["type"], json.loads(captured[i + 1])))
                i += 2
                continue
            except ValueError:
                pass
        i += 1
    return out

def _strings(o):
    if isinstance(o, dict):
        for k, v in o.items():
            yield str(k)
            yield from _strings(v)
    elif isinstance(o, list):
        for v in o:
            yield from _strings(v)
    elif isinstance(o, str):
        yield o

def _variants(s):
    out, prev = {s}, None
    while s != prev:                       # %2540 -> %40 -> @
        prev, s = s, urllib.parse.unquote_plus(s)
        out.add(s)
    for v in list(out):                    # JWT-shaped values: base64url-decode each segment
        for seg in v.split("."):
            if len(seg) >= 16:
                try:
                    out.add(base64.urlsafe_b64decode(seg + "=" * (-len(seg) % 4)).decode("utf-8", "replace"))
                except ValueError:
                    pass
    return out

def leaks(captured, planted):
    """Planted values found anywhere in the capture, raw AND decoded: every line as raw
    text (numbers and non-JSON items included), every JSON key and string value unescaped,
    each percent-decoded until stable, JWT segments base64url-decoded."""
    texts = []
    for line in captured:
        texts.append(line.decode("utf-8", "replace"))
        try:
            texts.extend(_strings(json.loads(line)))
        except ValueError:
            pass
    return sorted({p for t in texts for v in _variants(t) for p in planted if p in v})
```

The SDK sends from a background thread: after triggering the paths, call
`sentry_sdk.flush()` in-process; for a booted process, poll with a
deadline until every event you triggered is among `items(captured)` —
match its own `event_id`, `logentry.message` or exception value;
`arrived` only says the first envelope landed. Then assert
`leaks(captured, planted) == []`. Plant distinctive values and load them
from a data file — a literal in a source file on the captured stack comes
back through the SDK's source context and fakes a hit — and plant each
identity field on its own (given name and family name separately). Over an
empty, partial or undecoded capture, "nothing leaked" is vacuously true,
which is why arrival comes first. The leak paths to trigger: emails, `sub`
uuids, OAuth codes, request query strings, cookies, stack-trace
frame-locals, process argv (`sys.argv` rides every event's `extra`),
personal names — the fields and frame vars unit tests never build.

## Stray sweep targets (Python)

`print(...)` on server/worker/job paths, per-module ad-hoc
`logging.basicConfig` calls (basicConfig belongs in ONE entry-point
setup, not scattered), bare `except: pass` that swallows what should
be captured (flag as finding if fixing is out of scope). Leave alone:
tests, scripts/, notebooks, CLI stdout that is the product.
