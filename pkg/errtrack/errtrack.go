// Package errtrack is iterion's optional error-tracking seam. It wraps
// the Sentry Go SDK, which speaks the Sentry DSN ingestion protocol —
// so the same DSN drives a hosted Sentry project or a self-hosted
// GlitchTip with no code change.
//
// The package is OFF unless SENTRY_DSN is set at runtime: with the var
// unset Init does nothing at all (no client, no background worker, no
// network) and every capture helper returns immediately, so a binary
// built with errtrack behaves exactly like one without it.
//
// Failures are loud but never fatal — a malformed DSN is reported
// through the caller's *log.Logger at error level and the process
// carries on. Observability must not be able to take a run down.
package errtrack

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/log"
)

// Environment variables read by Init. They are the SDK's own standard
// names so an operator can reuse the deployment recipes (and the
// GlitchTip docs) verbatim.
const (
	// EnvDSN is the master switch: unset or empty ⇒ tracking is off.
	EnvDSN = "SENTRY_DSN"
	// EnvEnvironment tags events with the deployment (prod, staging…).
	EnvEnvironment = "SENTRY_ENVIRONMENT"
)

// flushTimeout bounds every flush the package performs. Short enough
// that a shutdown or a fatal exit is never noticeably delayed by an
// unreachable ingest host.
const flushTimeout = 2 * time.Second

// contextKey is the Sentry "contexts" bucket the structured fields of
// a log record land in.
const contextKey = "iterion"

// enabled is the package's single piece of global state, mirroring
// "a Sentry client is installed on the current hub". Kept separate
// from the hub so the hot path (a capture with tracking off) costs one
// atomic load and no SDK call.
var enabled atomic.Bool

// initOnce guards against a second Init replacing a live client — the
// CLI root and a long-running subcommand may both reach for it.
var initOnce sync.Once

// Config parameterises Init. Every field is optional: the zero Config
// resolves the DSN and environment from the standard env vars and the
// release from the build info.
type Config struct {
	// DSN overrides the SENTRY_DSN env var. Empty ⇒ read the env.
	DSN string
	// Environment overrides SENTRY_ENVIRONMENT. Empty ⇒ read the env.
	Environment string
	// Release overrides the derived "iterion@<version>+<commit>".
	Release string
	// Logger receives the loud-but-not-fatal init failure. nil is safe
	// (pkg/log's methods are nil-receiver tolerant) but then an invalid
	// DSN is silent — always pass the surface's logger.
	Logger *log.Logger
	// Transport replaces the HTTP transport. Test-only seam: unit tests
	// install an in-memory transport so no event ever leaves the process.
	Transport sentry.Transport
	// ServerName tags events with the process identity (a CLI command
	// name, a runner pod). Empty ⇒ the SDK's default (the hostname).
	ServerName string
}

// Init installs the Sentry client when a DSN is configured and reports
// whether tracking ended up enabled. It is idempotent: the first call
// wins, later ones return the current state.
//
// With no DSN this is a no-op — that is the whole off-switch. Callers
// therefore wire it unconditionally at their entry point.
func Init(cfg Config) bool {
	initOnce.Do(func() { enabled.Store(doInit(cfg)) })
	return enabled.Load()
}

// doInit holds the body of Init so the sync.Once closure stays a
// one-liner.
func doInit(cfg Config) bool {
	dsn := strings.TrimSpace(cfg.DSN)
	if dsn == "" {
		dsn = strings.TrimSpace(os.Getenv(EnvDSN))
	}
	if dsn == "" {
		// Off. Not even a debug line: "unset ⇒ zero behaviour change".
		return false
	}

	env := strings.TrimSpace(cfg.Environment)
	if env == "" {
		env = strings.TrimSpace(os.Getenv(EnvEnvironment))
	}

	release := strings.TrimSpace(cfg.Release)
	if release == "" {
		release = defaultRelease()
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:              dsn,
		Environment:      env,
		Release:          release,
		ServerName:       cfg.ServerName,
		Transport:        cfg.Transport,
		AttachStacktrace: true,
		BeforeSend:       scrubEvent,
		BeforeBreadcrumb: scrubBreadcrumb,
	})
	if err != nil {
		// Loud, never fatal: a broken DSN costs error tracking, not the
		// process. The DSN itself never reaches the log line — the SDK's
		// error text quotes the *scheme*, not the secret key, and we
		// scrub defensively anyway.
		cfg.Logger.Error("errtrack: error tracking disabled — %s", Redact(err.Error()))
		return false
	}
	cfg.Logger.Debug("errtrack: error tracking enabled (release=%s environment=%s)", release, env)
	return true
}

// Enabled reports whether a Sentry client is installed. Call sites use
// it to skip building expensive context when tracking is off.
func Enabled() bool { return enabled.Load() }

// defaultRelease derives the Sentry release from the build stamp:
// "iterion@v3.48.3+abc1234", falling back to "iterion@dev" on a
// non-injected build. Without a release, events cannot be attributed
// to a version and regression triage is guesswork.
func defaultRelease() string {
	v := strings.TrimSpace(appinfo.Version)
	if v == "" {
		v = "dev"
	}
	rel := appinfo.Name + "@" + v
	if c := strings.TrimSpace(appinfo.Commit); c != "" {
		if len(c) > 12 {
			c = c[:12]
		}
		rel += "+" + c
	}
	return rel
}

// Flush waits for buffered events to reach the ingest host, bounded by
// flushTimeout. No-op when tracking is off. Call it on every shutdown
// path — captures are asynchronous, so an event not flushed dies with
// the process.
func Flush() bool {
	if !enabled.Load() {
		return true
	}
	return sentry.Flush(flushTimeout)
}

// CaptureError reports err as an event, with fields attached as the
// "iterion" context. No-op when tracking is off or err is nil.
func CaptureError(err error, fields map[string]any) {
	if !enabled.Load() || err == nil {
		return
	}
	sentry.WithScope(func(scope *sentry.Scope) {
		applyFields(scope, fields)
		sentry.CaptureException(err)
	})
}

// CaptureMessage reports a message at the given level with fields as
// context. No-op when tracking is off.
func CaptureMessage(level sentry.Level, msg string, fields map[string]any) {
	if !enabled.Load() || msg == "" {
		return
	}
	sentry.WithScope(func(scope *sentry.Scope) {
		scope.SetLevel(level)
		applyFields(scope, fields)
		sentry.CaptureMessage(msg)
	})
}

// CapturePanic reports a recovered panic value and flushes, because
// the caller is usually about to re-panic or exit. No-op when tracking
// is off.
//
// Use it INSIDE an existing recover() block; it never recovers on its
// own, so a caller's panic semantics are unchanged.
func CapturePanic(r any) { CapturePanicFields(r, nil) }

// CapturePanicFields is CapturePanic with structured context — the
// label of the goroutine that died, the run it belonged to.
func CapturePanicFields(r any, fields map[string]any) {
	if !enabled.Load() || r == nil {
		return
	}
	sentry.WithScope(func(scope *sentry.Scope) {
		applyFields(scope, fields)
		sentry.CurrentHub().Recover(r)
	})
	sentry.Flush(flushTimeout)
}

// AddBreadcrumb records a trail entry shown alongside the next event.
// No-op when tracking is off.
func AddBreadcrumb(level sentry.Level, msg string, fields map[string]any) {
	if !enabled.Load() || msg == "" {
		return
	}
	sentry.AddBreadcrumb(&sentry.Breadcrumb{
		Category:  contextKey,
		Message:   msg,
		Level:     level,
		Data:      scrubFields(fields),
		Timestamp: time.Now(),
	})
}

// applyFields attaches the (scrubbed) structured fields of a log
// record to a scope as a single context bucket.
func applyFields(scope *sentry.Scope, fields map[string]any) {
	scrubbed := scrubFields(fields)
	if len(scrubbed) == 0 {
		return
	}
	scope.SetContext(contextKey, scrubbed)
}

// reset returns the package to its off state. Test-only: production
// code initialises once and never tears down.
func reset() {
	enabled.Store(false)
	initOnce = sync.Once{}
}
