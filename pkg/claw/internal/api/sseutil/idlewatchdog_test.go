package sseutil

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type fakeCloser struct {
	closed atomic.Bool
	ch     chan struct{}
}

func newFakeCloser() *fakeCloser { return &fakeCloser{ch: make(chan struct{})} }

func (f *fakeCloser) Close() error {
	if f.closed.CompareAndSwap(false, true) {
		close(f.ch)
	}
	return nil
}

func TestIdleWatchdog_FiresOnSilence(t *testing.T) {
	fc := newFakeCloser()
	wd := NewIdleWatchdog(context.Background(), fc, 40*time.Millisecond)
	defer wd.Stop()

	select {
	case <-fc.ch: // the watchdog closed the stalled stream
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog never closed the stalled stream")
	}
	if !wd.Fired() {
		t.Fatal("Fired() = false after the watchdog closed the stream")
	}
}

func TestIdleWatchdog_TouchKeepsAlive(t *testing.T) {
	fc := newFakeCloser()
	wd := NewIdleWatchdog(context.Background(), fc, 60*time.Millisecond)
	defer wd.Stop()

	// Touch well within the idle window for several windows' worth of time.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		wd.Touch()
		time.Sleep(15 * time.Millisecond)
	}
	if wd.Fired() {
		t.Fatal("watchdog fired while the stream was actively touched")
	}
	if fc.closed.Load() {
		t.Fatal("watchdog closed the stream while it was active")
	}
}

func TestIdleWatchdog_DisabledWhenNonPositive(t *testing.T) {
	fc := newFakeCloser()
	wd := NewIdleWatchdog(context.Background(), fc, 0)
	defer wd.Stop()

	time.Sleep(80 * time.Millisecond)
	if wd.Fired() || fc.closed.Load() {
		t.Fatal("disabled watchdog (idle<=0) must never fire")
	}
}

func TestIdleWatchdog_StopPreventsFiring(t *testing.T) {
	fc := newFakeCloser()
	wd := NewIdleWatchdog(context.Background(), fc, 40*time.Millisecond)
	wd.Stop()

	time.Sleep(120 * time.Millisecond)
	if wd.Fired() || fc.closed.Load() {
		t.Fatal("stopped watchdog must not fire")
	}
	wd.Stop() // idempotent — must not panic
}

func TestIdleWatchdog_ContextCancelStops(t *testing.T) {
	fc := newFakeCloser()
	ctx, cancel := context.WithCancel(context.Background())
	wd := NewIdleWatchdog(ctx, fc, 40*time.Millisecond)
	defer wd.Stop()

	cancel()
	time.Sleep(120 * time.Millisecond)
	if wd.Fired() || fc.closed.Load() {
		t.Fatal("watchdog must stop (not fire) once ctx is cancelled")
	}
}
