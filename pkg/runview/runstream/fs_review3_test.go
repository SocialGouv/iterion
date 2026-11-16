package runstream

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A Terminal hint's status read can be stale: a run resumed between that read
// and the final drain reopens its episode, and the stream — whose drain
// already read the resume — keeps tailing instead of ending cleanly. That the
// kept stream really comes from the status reread is pinned on the window
// path (TestFileSourceSettleWindowRechecksAResumedRun); both end through the
// same settleEnd.
func TestFileSourceHintedEndRechecksAResumedRun(t *testing.T) {
	src, st := barrierFixture(t, 5*time.Second, store.EventRunFailed)
	term, wake := make(chan struct{}), make(chan struct{})
	sub, err := src.SubscribeEventsHinted(t.Context(), "barrier", EventCursor{Seq: 0}, FileStreamHints{
		Drain:        wake,
		Terminal:     term,
		TerminalPoll: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	receiveSeq(t, sub, 0, 5*time.Second)
	setStatus(t, st, "barrier", store.RunStatusFailedResumable)
	var flipped bool
	setFileTerminalHook(func() {
		if flipped {
			return
		}
		flipped = true
		// Lands after the hint's status read and before the final drain: the
		// status file says running and run_resumed reopens the episode.
		setStatus(t, st, "barrier", store.RunStatusRunning)
		appendType(t, st, "barrier", store.EventRunResumed)
	})
	t.Cleanup(func() { setFileTerminalHook(nil) })
	term <- struct{}{}
	// The end check re-reads the status (running): the stream goes on, and
	// the resumed episode's events arrive.
	receiveSeq(t, sub, 1, 5*time.Second)
	expectOpen(t, sub, 500*time.Millisecond, "a resumed run ended at its Terminal hint")
	setFileTerminalHook(nil)
	appendType(t, st, "barrier", store.EventNodeFinished)
	receiveSeq(t, sub, 2, 5*time.Second)
	appendType(t, st, "barrier", store.EventRunFinished)
	setStatus(t, st, "barrier", store.RunStatusFinished)
	term <- struct{}{}
	receiveSeq(t, sub, 3, 5*time.Second)
	select {
	case _, ok := <-sub.Events():
		if ok {
			t.Fatal("unexpected batch after run_finished")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the finished run's stream never ended")
	}
}
