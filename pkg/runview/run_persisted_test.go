package runview

import (
	"errors"
	"fmt"
	"testing"
)

// A metering surface releases its quota slot exactly when this answers false,
// so both arms are load-bearing and neither is witnessed by the surfaces' own
// tests: a mutant deleting the queue arm passed every one of them.
func TestRunMayHaveStartedReadsTheFactTheCalleeReported(t *testing.T) {
	t.Run("a plain error started nothing", func(t *testing.T) {
		if RunMayHaveStarted(errors.New("compile error")) {
			t.Fatal("a compile failure was read as a started run — a repeatable launch failure then charges one monthly unit per attempt")
		}
	})
	t.Run("nil started nothing", func(t *testing.T) {
		if RunMayHaveStarted(nil) {
			t.Fatal("no error at all was read as a started run")
		}
	})
	t.Run("a persisted run is reported, through a wrap", func(t *testing.T) {
		err := fmt.Errorf("launch: %w", &RunPersistedError{RunID: "r1", Err: errors.New("store failure")})
		if !RunMayHaveStarted(err) {
			t.Fatal("a run that exists was read as never started — refunding its slot lets the org exceed its paid quota")
		}
	})
	// The publisher's own rollback exists because a publish can report failure
	// after the message landed, and the runner then claims and executes it.
	t.Run("a queue outage may already be executing", func(t *testing.T) {
		err := fmt.Errorf("resume: %w", &QueueUnavailableError{})
		if !RunMayHaveStarted(err) {
			t.Fatal("a publish that may have landed was read as never started")
		}
	})
}
