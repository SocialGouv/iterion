package trigger

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
)

// The claim watchdog reuses the EXISTING event vocabulary rather than
// minting new audit types, and that choice has a load-bearing
// consequence for the trigger spine: a claim reclaim (EvtIssueClaimed)
// must be INERT — a reaper pass that touched N stuck cards must not fire
// N subscriptions — while a Reopen (EvtIssueState) must be SEEN, because
// it is a real state transition tailers and subscriptions must observe
// (F23). IsCardEvent is the shared filter both the local tail and the
// cloud poll-tail use, so pinning it here pins both.
func TestWatchdogEventsSpineVisibility(t *testing.T) {
	// A reclaim / renewal never reaches the spine.
	if IsCardEvent(native.EvtIssueClaimed) {
		t.Error("EvtIssueClaimed (the reaper's reclaim) must be inert to the trigger spine — a reap pass would otherwise fire a subscription per stuck card")
	}
	if IsCardEvent(native.EvtIssueReleased) {
		t.Error("EvtIssueReleased must be inert to the trigger spine")
	}
	// A Reopen is a genuine state transition the spine must see.
	if !IsCardEvent(native.EvtIssueState) {
		t.Error("EvtIssueState (a Reopen carries it) must reach the trigger spine — a reopened card is a real transition")
	}
}
