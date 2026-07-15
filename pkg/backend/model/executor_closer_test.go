package model

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// countingCloser records how many times Close was called and returns a fixed
// error, so a test can assert both that Close ran and that its error surfaced.
type countingCloser struct {
	closed int
	err    error
}

func (c *countingCloser) Close() error {
	c.closed++
	return c.err
}

// TestClawExecutorCloseReleasesExtraClosers guards the fsnotify-watcher leak
// fix: WithExtraClosers-registered resources (e.g. the native board store) must
// be released by ClawExecutor.Close(), nil entries ignored, and their errors
// aggregated. Without this each per-subbot BuildExecutor call leaks a watcher.
func TestClawExecutorCloseReleasesExtraClosers(t *testing.T) {
	a := &countingCloser{}
	sentinel := errors.New("board store close failed")
	b := &countingCloser{err: sentinel}

	// Interleave a nil to prove nil entries are dropped, not stored.
	exec := NewClawExecutor(NewRegistry(), &ir.Workflow{}, WithExtraClosers(a, nil, b))

	if got := len(exec.extraClosers); got != 2 {
		t.Fatalf("extraClosers = %d, want 2 (nil entry must be ignored)", got)
	}

	err := exec.Close()
	if a.closed != 1 {
		t.Errorf("closer a closed %d times, want 1", a.closed)
	}
	if b.closed != 1 {
		t.Errorf("closer b closed %d times, want 1", b.closed)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Close() error = %v, want it to wrap the closer's error", err)
	}
}

// TestClawExecutorCloseNoClosersNoError confirms the common path (no MCP
// manager, no extra closers) still returns nil — a regression guard for the
// errors.Join refactor.
func TestClawExecutorCloseNoClosersNoError(t *testing.T) {
	exec := NewClawExecutor(NewRegistry(), &ir.Workflow{})
	if err := exec.Close(); err != nil {
		t.Fatalf("Close() with no closers = %v, want nil", err)
	}
}
