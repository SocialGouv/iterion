package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// runParentOverRunner runs parentSubbotWorkflow over a hand-written runner
// that cancels the parent before returning childErr, as the operator's
// cancel of a pipeline reaches the parent and its parked child together.
func runParentOverRunner(t *testing.T, childErr error) (*store.Run, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := tmpStore(t)
	runner := func(context.Context, SubbotRequest) (map[string]any, error) {
		cancel()
		return nil, childErr
	}
	err := New(parentSubbotWorkflow(), s, newStubExecutor(), WithSubbotRunner(runner)).Run(ctx, "parent-run", nil)
	parent, lerr := s.LoadRun(context.Background(), "parent-run")
	if lerr != nil {
		t.Fatalf("load parent: %v", lerr)
	}
	return parent, err
}

// TestSubbotReconstructedChildStopIsAStop: a parked child's end comes back
// reconstructed from its record (runview.subbotChildFailure) as a
// RuntimeError carrying the child's failure code and no sentinel. A
// CANCELLED or INTERRUPTED code is the child being stopped, so with the
// parent's context ended the parent is cancelled — not failed with the
// child's CANCELLED code.
func TestSubbotReconstructedChildStopIsAStop(t *testing.T) {
	for _, code := range []ErrorCode{ErrCodeCancelled, store.FailureInterrupted} {
		t.Run(string(code), func(t *testing.T) {
			parent, err := runParentOverRunner(t, &RuntimeError{Code: code, Message: "subbot child run child-1 ended: run cancelled"})
			if parent.Status != store.RunStatusCancelled || parent.FailureCode != store.FailureCancelled || !errors.Is(err, ErrRunCancelled) {
				t.Fatalf("parent = %s/%q (error %q), err = %v, want cancelled", parent.Status, parent.FailureCode, parent.Error, err)
			}
		})
	}
}

// TestSubbotChildWordsAreBounded: the child's words carried into the
// parent's record are cut at subbotDetailLimit with an explicit marker, so
// an unbounded child stderr cannot bloat the parent's document.
func TestSubbotChildWordsAreBounded(t *testing.T) {
	words := "child stderr: " + strings.Repeat("x", 64<<10)
	parent, _ := runParentOverRunner(t, errors.New(words))
	if parent.Status != store.RunStatusCancelled {
		t.Fatalf("parent = %s/%q, want cancelled", parent.Status, parent.FailureCode)
	}
	if !strings.Contains(parent.Error, "child stderr: xxx") || !strings.HasSuffix(parent.Error, subbotDetailTruncated) {
		t.Fatalf("parent error does not carry the cut child words: %.120q … (%d bytes)", parent.Error, len(parent.Error))
	}
	if len(parent.Error) > subbotDetailLimit+256 {
		t.Fatalf("parent error is %d bytes, want at most the %d-byte bound plus the stop's own words", len(parent.Error), subbotDetailLimit)
	}
}
