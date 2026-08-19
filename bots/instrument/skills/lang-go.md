---
name: lang-go
description: Go reference for the instrument campaign — sentry-go for error tracking (init, recover, flush, mock transport tests), log/slog as the default structured logger when the repo has none, hook patterns for a home-grown logger, and Go module/vendoring gotchas. Read when the target repo is Go.
---

# lang-go — instrumenting a Go repository

## Error tracking: `github.com/getsentry/sentry-go`

The official Go SDK for the Sentry DSN protocol (works unchanged
against GlitchTip). Core wiring:

```go
err := sentry.Init(sentry.ClientOptions{
    Dsn:         os.Getenv(dsnEnvVar),   // "" would disable, but prefer: skip Init entirely when unset
    Release:     release,                 // from the repo's own version/commit source
    Environment: os.Getenv("SENTRY_ENVIRONMENT"),
    BeforeSend:  scrub,                   // drop secrets/PII before the wire
})
```

- **Skip `Init` entirely when the DSN env is empty** — that is the
  cleanest "zero behaviour change" off-state, and makes the enabled
  check (`enabled bool` on your wrapper) trivial to test.
- `sentry.Init` returns an error on a malformed DSN — log it at error
  level through the repo's logger and continue (never `os.Exit`).
- **Flush**: `sentry.Flush(2 * time.Second)` on the shutdown path AND
  before a fatal exit. `CaptureException`/`CaptureMessage` are async;
  without the flush the event dies with the process.
- **Panic seams**: in each goroutine/worker recovery point the repo
  already has, add capture inside the existing `recover()` block:
  `sentry.CurrentHub().Recover(r); sentry.Flush(2 * time.Second)`.
  For the process top level, a small
  `defer func(){ if r := recover(); r != nil { capture(r); panic(r) } }()`
  in `main` (re-panic so exit semantics don't change). HTTP servers
  that already recover per-request: capture there, keep their recovery
  semantics intact.
- **Context/tags**: prefer `sentry.WithScope` + `scope.SetContext` /
  `scope.SetTag` at capture time over global mutable state.

### Testing without a network

`sentry.ClientOptions.Transport` accepts a custom transport. In tests,
install a fake that records events in memory:

```go
type memTransport struct{ events []*sentry.Event; mu sync.Mutex }
func (t *memTransport) Configure(sentry.ClientOptions) {}
func (t *memTransport) SendEvent(e *sentry.Event) { t.mu.Lock(); t.events = append(t.events, e); t.mu.Unlock() }
func (t *memTransport) Flush(time.Duration) bool { return true }
func (t *memTransport) FlushWithContext(context.Context) bool { return true }
func (t *memTransport) Close() {}
```

(Match the `sentry.Transport` interface of the pinned SDK version —
check `transport.go` in the vendored copy; older versions have no
`FlushWithContext`/`Close`.) Then assert: an error-level log produces
one event with the expected message/fields; warn produces a breadcrumb
on the scope; DSN unset produces nothing.

### Modules & vendoring

- `go get github.com/getsentry/sentry-go@latest` then, **if the repo
  vendors** (a `vendor/` dir + `-mod=vendor` builds), `go mod vendor`
  — never hand-edit `vendor/`. Commit go.mod/go.sum/vendor in the same
  slice as the first use.
- sentry-go is pure Go (no cgo) — safe for `CGO_ENABLED=0` static
  builds.

## Logging

- **Repo already has a central logger** (common in mature Go repos):
  EXTEND it. The usual gaps to fill: a JSON output mode (one object
  per line: ts/level/msg/fields), `WithField`-style context, and a
  **hook seam** — a small interface or func the wrapper calls on every
  line at/above a level. The tracker coupling then registers a hook:
  error → `CaptureEvent` (message + fields as context), warn →
  `AddBreadcrumb`. The hook must be non-blocking and panic-safe
  (`defer recover()` inside the hook dispatch): log lines never crash
  the producer.
- **No central logger**: use stdlib **`log/slog`** (Go ≥1.21) — zero
  new dependency. `slog.NewJSONHandler(os.Stderr, …)` for prod
  surfaces, `slog.NewTextHandler` (or the repo's preferred pretty
  handler) for TTY. Wrap it in a tiny package the repo owns so the
  seam is yours to hook: implement a `slog.Handler` that delegates and
  forwards error/warn records to the tracker.
- **Prod default detection**: prefer an explicit per-surface default
  (the server/daemon commands construct the logger with JSON; the CLI
  constructs human) over global TTY sniffing — explicit beats magic,
  and both stay overridable by the repo's log-format env.

## Stray sweep targets (Go)

`fmt.Print*`/`println` on server/daemon/worker paths, stdlib
`log.Printf/Fatalf` imports outside `main` bootstrap, per-package
ad-hoc loggers. Leave alone: test files, `//go:build`-tagged host
tooling, generated code, and stdout that is the command's product.
