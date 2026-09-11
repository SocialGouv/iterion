package runview

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A parked gate has two legitimate resumers — the operator, and the host
// delivering a watched run's outcome. The loser of that race must be able to
// tell "you were beaten to it" from "your request was malformed", or the
// studio can only show the operator a failure they did not cause.
func TestValidateResumable_MarksAStatusRefusalAsNotResumable(t *testing.T) {
	t.Parallel()
	err := validateResumable(&store.Run{ID: "run-1", Status: store.RunStatusRunning}, map[string]any{"message": "hi"})
	if err == nil {
		t.Fatal("a running run must refuse a resume")
	}
	if !errors.Is(err, ErrRunNotResumable) {
		t.Fatalf("error is not classified as ErrRunNotResumable: %v", err)
	}

	// A missing-answers refusal is the caller's own fault and must NOT be
	// re-routed as a lost race.
	missing := validateResumable(&store.Run{ID: "run-2", Status: store.RunStatusPausedWaitingHuman}, nil)
	if missing == nil {
		t.Fatal("a paused run with no answers must refuse a resume")
	}
	if errors.Is(missing, ErrRunNotResumable) {
		t.Fatal("a malformed request must not be classified as a lost race")
	}
}
