package runner

import "github.com/SocialGouv/iterion/pkg/errtrack"

// trackPanic reports a panic that is taking the runner pod down and
// then re-panics. Use it as `defer trackPanic("runner.<surface>")` at
// the top of a goroutine the pod spawns.
//
// It deliberately does NOT contain the panic. A background loop of the
// runner has a job the pod cannot do without — refreshing the run's
// NATS lease, reaping orphaned sandbox pods — so swallowing its death
// would trade a loud crash for a run that keeps going without its
// safety net. The tracker observes the crash; it does not change it.
//
// Without this, the crash leaves nothing behind: Go cannot recover
// another goroutine's panic, so the CLI top level in cmd/iterion/main.go
// never sees it and the pod dies with no event at all.
func trackPanic(surface string) {
	p := recover()
	if p == nil {
		return
	}
	// CapturePanicFields flushes: the process has seconds to live.
	errtrack.CapturePanicFields(p, map[string]any{"surface": surface})
	panic(p)
}

// goTracked runs fn in a goroutine guarded by trackPanic.
func goTracked(surface string, fn func()) {
	go func() {
		defer trackPanic(surface)
		fn()
	}()
}
