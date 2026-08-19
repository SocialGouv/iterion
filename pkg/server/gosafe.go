package server

import "github.com/SocialGouv/iterion/pkg/errtrack"

// goSafe runs fn in a detached goroutine with panic recovery, so a
// best-effort background task (audit insert, last_used_at MarkUsed,
// invitation email) can never take down the whole server process. These
// tasks call into stores (Mongo) and SMTP at every authenticated
// request; a single bad-document panic in a driver or a TLS-handshake
// nil deref would otherwise crash the server from a fire-and-forget
// goroutine the caller can't recover. The panic is logged (nil-safe);
// label identifies the task in the recovery log line.
//
// It is also an error-tracking seam: this is the one place a
// swallowed panic would otherwise leave no trace at all when the
// server carries no logger. The capture is a no-op unless SENTRY_DSN
// is set, and it never changes the recovery semantics.
func (s *Server) goSafe(label string, fn func()) {
	go func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			errtrack.CapturePanicFields(r, map[string]any{"task": label, "surface": "server.goSafe"})
			if s.logger != nil {
				s.logger.Error("server: background task %q panicked: %v", label, r)
			}
		}()
		fn()
	}()
}
