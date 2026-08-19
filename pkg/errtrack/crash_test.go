package errtrack

import (
	"strings"
	"testing"
	"time"
)

// A detached goroutine dying is the incident an operator most needs to
// see, and the one Go gives no other way to observe: a panic there is
// unrecoverable from anywhere else in the process.
func TestTrackPanicCapturesAndRepanics(t *testing.T) {
	tr := enable(t, Config{})

	var escaped any
	func() {
		// Stands in for the process dying: the guard must let the panic
		// through, so this outer recover is what catches it.
		defer func() { escaped = recover() }()
		func() {
			defer TrackPanic("server.hub")
			panic("lease refresh nil deref")
		}()
	}()

	if escaped != "lease refresh nil deref" {
		t.Fatalf("the guard swallowed the panic instead of re-panicking: %v", escaped)
	}

	events := tr.all()
	if len(events) != 1 {
		t.Fatalf("want 1 captured panic, got %d", len(events))
	}
	ev := events[0]
	if !strings.Contains(ev.Message, "lease refresh nil deref") &&
		(len(ev.Exception) == 0 || !strings.Contains(ev.Exception[0].Value, "lease refresh nil deref")) {
		t.Fatalf("captured event does not name the panic: %+v", ev)
	}
	if ev.Contexts[contextKey]["surface"] != "server.hub" {
		t.Fatalf("captured event lost the surface label: %+v", ev.Contexts)
	}
}

// With tracking off the guard is still transparent: it must not turn a
// crash into a swallowed panic just because no DSN is configured.
func TestTrackPanicRepanicsWhenDisabled(t *testing.T) {
	reset()
	t.Cleanup(reset)

	var escaped any
	func() {
		defer func() { escaped = recover() }()
		func() {
			defer TrackPanic("cli.root")
			panic("boom")
		}()
	}()
	if escaped != "boom" {
		t.Fatalf("disabled guard changed how the process dies: %v", escaped)
	}
}

// A guard that only fires on panics must be invisible otherwise.
func TestGoRunsTheFunction(t *testing.T) {
	done := make(chan struct{})
	Go("test.surface", func() { close(done) })
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Go never ran fn")
	}
}
