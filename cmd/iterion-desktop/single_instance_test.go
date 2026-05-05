//go:build desktop

package main

import "testing"

// TestSingleInstance_ReleaseTwice_NoPanic locks in the documented "Safe to
// call multiple times" contract. The previous implementation called
// close(s.stop) unconditionally and panicked with "close of closed
// channel" on a second invocation — perfectly ordinary code like a
// `defer instLock.Release()` paired with onShutdown's explicit call
// would have crashed shutdown. The fix uses sync.Once; this test asserts
// that contract.
func TestSingleInstance_ReleaseTwice_NoPanic(t *testing.T) {
	// We deliberately construct a SingleInstance with nil flock + nil
	// listener so the test does not require an actual lock file or TCP
	// bind. The only invariant we care about is that Release is
	// idempotent — flock.Unlock() and listener.Close() are both
	// best-effort branches we skip via the nil checks.
	s := &SingleInstance{stop: make(chan struct{})}

	if err := s.Release(); err != nil {
		t.Fatalf("first Release returned error: %v", err)
	}
	// The second call must not panic and must not error.
	if err := s.Release(); err != nil {
		t.Fatalf("second Release returned error: %v", err)
	}
	// And a third for good measure (e.g. defer + explicit + lifecycle hook).
	if err := s.Release(); err != nil {
		t.Fatalf("third Release returned error: %v", err)
	}
}

// TestSingleInstance_ReleaseNil is a defensive check on the nil-receiver
// branch, which the desktop App may rely on if AcquireSingleInstanceLock
// failed and the App still calls Release in onShutdown.
func TestSingleInstance_ReleaseNil(t *testing.T) {
	var s *SingleInstance
	if err := s.Release(); err != nil {
		t.Fatalf("nil-receiver Release returned error: %v", err)
	}
}
