package errtrack

// TrackPanic reports a panic that is taking the process down and then
// re-panics. Use it as the FIRST statement of a goroutine's defer:
//
//	go func() {
//		defer errtrack.TrackPanic("server.hub")
//		hub.Run()
//	}()
//
// It deliberately does NOT contain the panic — that is the difference
// with CapturePanic, which a caller uses inside a recover block that
// has already decided to carry on. A goroutine reached through this
// guard dies exactly as it did before; the tracker only gains the
// event.
//
// Without it that crash leaves nothing behind: Go cannot recover
// another goroutine's panic, so neither the CLI top level nor an HTTP
// handler's own recovery ever sees a detached goroutine die.
//
// No-op — like every helper here — when tracking is off.
func TrackPanic(surface string) {
	p := recover()
	if p == nil {
		return
	}
	// CapturePanicFields flushes: the process has seconds to live.
	CapturePanicFields(p, map[string]any{"surface": surface})
	panic(p)
}

// Go runs fn in a goroutine guarded by TrackPanic.
func Go(surface string, fn func()) {
	go func() {
		defer TrackPanic(surface)
		fn()
	}()
}
