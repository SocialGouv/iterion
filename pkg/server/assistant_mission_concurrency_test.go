package server

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/assistantmission"
	"github.com/SocialGouv/iterion/pkg/runwatch"
	"github.com/SocialGouv/iterion/pkg/trigger"
)

// s.assistantMission / s.assistantMissionCancel are written by
// restartAssistantMissions (boot AND every project switch, from outside
// s.stateMu) while a request goroutine reads the coordinator and Shutdown
// reads-and-clears the cancel. Under -race the unguarded version reports a
// plain data race on both fields; the interleaving it stands for drops a
// cancel and leaks a coordinator.
func TestRestartAssistantMissionsIsRaceFree(t *testing.T) {
	dir := t.TempDir()
	s := &Server{}
	missions := assistantmission.NewFSStore(dir)
	watches := runwatch.NewFSStore(dir)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(1)
	go func() { // the project-switch writer
		defer wg.Done()
		for i := 0; i < 40; i++ {
			select {
			case <-stop:
				return
			default:
			}
			s.restartAssistantMissions(nil, watches, missions)
		}
	}()

	wg.Add(1)
	go func() { // the request-goroutine reader (handleCreateAssistantMission's tail)
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.stateMu.RLock()
			c := s.assistantMission
			s.stateMu.RUnlock()
			if c != nil {
				c.nudge()
			}
			runtime.Gosched()
		}
	}()

	wg.Add(1)
	go func() { // the shutdown reader
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.stateMu.RLock()
			cancel := s.assistantMissionCancel
			s.stateMu.RUnlock()
			_ = cancel
			runtime.Gosched()
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()

	s.stateMu.Lock()
	last := s.assistantMissionCancel
	s.stateMu.Unlock()
	if last != nil {
		last()
	}
}

// handleEvent used to be `go c.sweep(context.Background())` — one unbounded
// goroutine per run event, on a matcher that filters only by SOURCE, so
// every started/finished/failed/cancelled/paused of every run spawned one,
// detached from the ctx a project switch cancels. It coalesces now: a burst
// leaves at most one pending sweep and creates no goroutine of its own.
func TestAssistantMissionHandleEventCoalescesAndSpawnsNothing(t *testing.T) {
	c := &assistantMissionCoordinator{wake: make(chan struct{}, 1)}

	before := runtime.NumGoroutine()
	for i := 0; i < 500; i++ {
		if err := c.handleEvent(context.Background(), trigger.Event{Source: trigger.SourceRun}); err != nil {
			t.Fatalf("handleEvent: %v", err)
		}
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("500 run events created %d goroutines (%d -> %d)", after-before, before, after)
	}
	if len(c.wake) != 1 {
		t.Fatalf("pending sweeps = %d, want exactly 1 coalesced request", len(c.wake))
	}
	<-c.wake
	if len(c.wake) != 0 {
		t.Fatalf("the wake channel kept a request after a drain")
	}
}

// A coordinator with no wake channel (a zero value reached through an
// unexpected path) must not panic the request goroutine that nudges it.
func TestAssistantMissionNudgeIsSafeOnAZeroCoordinator(t *testing.T) {
	var nilCoordinator *assistantMissionCoordinator
	nilCoordinator.nudge()
	(&assistantMissionCoordinator{}).nudge()
}
