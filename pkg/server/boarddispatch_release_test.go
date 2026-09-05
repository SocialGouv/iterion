package server

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dispatcher/native"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// TestBoardDispatcher_ReleaseAnnouncesItselfToTheHeartbeat: processCard's
// final ReleaseOwned lands while its claim session is still beating
// (Stop runs in the deferred teardown, after the last write). A beat in
// flight across that release comes back ErrClaimConflict — and without
// the owner announcing the release first (claimSession.Releasing, the
// local twin's rule in releaseClaimSess) the session reads its OWN
// release as a supersession: "claim lost (lease superseded) — stopping
// the worker" at WARN on an ordinary, successful finish, plus the cancel
// path fired on a card whose work is over. That line is the one signal an
// operator has to diagnose a stolen claim; a false one on every finish
// that straddles a beat buries the real ones.
func TestBoardDispatcher_ReleaseAnnouncesItselfToTheHeartbeat(t *testing.T) {
	f := newFakeBoardCoord(readyCard("native:1", "feature-dev"))
	beatInFlight := make(chan struct{})
	beatRelease := make(chan struct{})
	released := make(chan struct{})
	var once, onceRel sync.Once
	f.renewHook = func(string) {
		once.Do(func() { close(beatInFlight) })
		<-beatRelease
	}
	var buf bytes.Buffer
	d := newBoardDispatcher(f, func(context.Context, string, native.Issue) error {
		// The run finishes only once a heartbeat is parked in flight, so
		// the release below lands UNDER that beat.
		select {
		case <-beatInFlight:
		case <-time.After(5 * time.Second):
			t.Error("no heartbeat reached the coordinator — the probe cannot straddle the release")
		}
		return nil
	}, "replica-A", 1, iterlog.New(iterlog.LevelWarn, &buf))
	d.sessionInterval = time.Millisecond
	// The parked beat is let through the instant the owner's release has
	// landed: it then reads the card as no longer ours.
	f.releaseHook = func(string) { onceRel.Do(func() { close(released) }) }
	go func() {
		<-released
		close(beatRelease)
	}()

	d.tick(context.Background())
	d.wg.Wait()

	if got := f.states["native:1"]; got != native.StateDone {
		t.Fatalf("card state = %q, want done", got)
	}
	if len(f.claimed) != 0 {
		t.Fatalf("card must be released: %v", f.claimed)
	}
	if log := buf.String(); strings.Contains(log, "was lost (lease superseded)") {
		t.Fatalf("REPRODUCED: the owner's OWN release was reported as a stolen claim on an ordinary finish:\n%s", log)
	}
}
