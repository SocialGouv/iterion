package errtrack

import (
	sentry "github.com/getsentry/sentry-go"

	"github.com/SocialGouv/iterion/pkg/log"
)

// LogHook returns the pkg/log Hook that couples the central logger to
// the tracker: an error line becomes an EVENT carrying the record's
// structured fields as context, a warn line becomes a BREADCRUMB that
// rides along with the next event.
//
// The hook is only meaningful once Init has enabled tracking; with
// tracking off every capture helper it calls returns immediately, so
// installing it unconditionally is harmless — but AttachLogHook is the
// call site to prefer, since it skips the plumbing entirely.
func LogHook() log.Hook {
	return func(level log.Level, msg string, fields map[string]any) {
		switch level {
		case log.LevelError:
			CaptureMessage(sentry.LevelError, msg, fields)
		case log.LevelWarn:
			AddBreadcrumb(sentry.LevelWarning, msg, fields)
		}
	}
}

// AttachLogHook installs LogHook on logger when — and only when —
// tracking is enabled. Every production surface calls it right after
// Init, so an error log anywhere in the process reaches the tracker
// without a single extra call site.
//
// Returns true when the hook was installed.
func AttachLogHook(logger *log.Logger) bool {
	if !Enabled() || logger == nil {
		return false
	}
	logger.SetHook(LogHook())
	return true
}
