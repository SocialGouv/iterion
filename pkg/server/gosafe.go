package server

import "github.com/SocialGouv/iterion/pkg/errtrack"

// goSafe runs fn in a goroutine with panic recovery, so a best-effort
// background task (audit insert, last_used_at MarkUsed, invitation email)
// can never take down the whole server process. These tasks call into
// stores (Mongo) and SMTP at every authenticated request; a single
// bad-document panic in a driver or a TLS-handshake nil deref would
// otherwise crash the server from a goroutine the caller can't recover.
// The panic is logged (nil-safe); label identifies the task in the
// recovery log line.
//
// It is also an error-tracking seam: this is the one place a
// swallowed panic would otherwise leave no trace at all when the
// server carries no logger. The capture is a no-op unless SENTRY_DSN
// is set, and it never changes the recovery semantics.
//
// Detached from the REQUEST — a client disconnect must not cut the write the
// handler already answered 200 for — but joined by the SHUTDOWN, like the
// server's loops (#1257) and the forge→board projection (#1345): these writes
// have no reconciling sweep behind them, so a process that exits mid-insert
// simply loses the audit row of a mutation it has acknowledged. fn keeps its
// own deadline rather than taking one from the shutdown: the point is to let
// the last write FINISH inside the grace period, not to cut it there.
//
// A task asked for past the join is DROPPED, so fn must acquire inside itself
// whatever it is responsible for releasing — a timer context, a lock, a
// semaphore slot taken before the call would be stranded for the remaining
// life of the process. TestNoDetachedWriteReleasesWhatItDidNotAcquire holds
// the call sites to that.
func (s *Server) goSafe(label string, fn func()) {
	_ = s.tryGoSafe(label, fn)
}

// tryGoSafe is goSafe with a return value that says whether the write was
// actually started. A caller that INSTALLED shared state before asking — the
// model-spec refresh flight every later request parks on — needs it, so it can
// answer what it installed when the write is dropped; a caller with nothing
// installed ignores it (goSafe). Same contract otherwise, the acquire-inside-fn
// rule included: the verdict says a task was dropped, it does not run anything
// on its behalf.
func (s *Server) tryGoSafe(label string, fn func()) bool {
	name := "server.goSafe:" + label
	done, registered := s.registerBackgroundWorker(name)
	if !registered {
		// Dropped, and said so: the shutdown has already joined, so this
		// write would run with nobody waiting for it and die with the
		// process a moment later. Silence here would make a lost audit row
		// indistinguishable from one that was never asked for.
		if s.logger != nil {
			s.logger.Warn("server: background task %q dropped — the shutdown already joined its background work, and this write would run with nobody waiting for it", label)
		}
		return false
	}
	go func() {
		// Closed last, after the recovery below has run: the join covers
		// the whole goroutine, capture included.
		defer close(done)
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
	return true
}
