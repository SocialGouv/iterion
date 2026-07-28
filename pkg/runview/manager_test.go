package runview

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestManager_RegisterAndCancel(t *testing.T) {
	m := NewManager()
	ctx, err := m.Register(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !m.Active("run-1") {
		t.Errorf("Active(run-1) = false, want true after Register")
	}
	if err := m.Cancel("run-1"); err != nil {
		t.Errorf("Cancel: %v", err)
	}
	select {
	case <-ctx.Done():
		// expected
	case <-time.After(time.Second):
		t.Fatalf("ctx.Done not fired after Cancel")
	}
	m.Deregister("run-1")
	if m.Active("run-1") {
		t.Errorf("Active(run-1) = true after Deregister")
	}
}

func TestManager_DuplicateRegisterFails(t *testing.T) {
	m := NewManager()
	if _, err := m.Register(context.Background(), "run-1"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if _, err := m.Register(context.Background(), "run-1"); err == nil {
		t.Errorf("duplicate Register returned nil error, want error")
	}
}

func TestManager_CancelInactiveReturnsError(t *testing.T) {
	m := NewManager()
	if err := m.Cancel("ghost"); !errors.Is(err, ErrRunNotActive) {
		t.Errorf("Cancel(ghost) = %v, want ErrRunNotActive", err)
	}
}

func TestManager_StopDrains(t *testing.T) {
	m := NewManager()
	const N = 3
	doneCh := make(chan string, N)

	for i := 0; i < N; i++ {
		runID := "run-" + string(rune('A'+i))
		ctx, err := m.Register(context.Background(), runID)
		if err != nil {
			t.Fatalf("Register %s: %v", runID, err)
		}
		go func(rid string, c context.Context) {
			<-c.Done()
			// Mimic the engine goroutine's normal teardown.
			m.Deregister(rid)
			doneCh <- rid
		}(runID, ctx)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	m.Stop(stopCtx)

	// Manager.Stop returns once each goroutine exits; check the
	// completion channel saw all of them.
	for i := 0; i < N; i++ {
		select {
		case <-doneCh:
		case <-time.After(time.Second):
			t.Fatalf("only %d/%d goroutines drained", i, N)
		}
	}
	for _, id := range []string{"run-A", "run-B", "run-C"} {
		if m.Active(id) {
			t.Errorf("%s still active after Stop", id)
		}
	}
}

func TestManager_StopRejectsNewRegistrations(t *testing.T) {
	m := NewManager()
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	m.Stop(stopCtx)
	if _, err := m.Register(context.Background(), "run-1"); err == nil {
		t.Errorf("Register after Stop returned nil error, want error")
	}
}

func TestManager_WaitOnInactive(t *testing.T) {
	m := NewManager()
	if err := m.Wait(context.Background(), "ghost"); !errors.Is(err, ErrRunNotActive) {
		t.Errorf("Wait(ghost) = %v, want ErrRunNotActive", err)
	}
}

// --- the paused/deregister hand-off window -----------------------------
//
// A run parking on a human gate writes paused_waiting_human to the STORE,
// returns ErrRunPaused, and only then does the goroutine carrying it call
// Deregister on its way out. Between those, the public status says
// "resumable" while the handle is still held — and the studio and the
// pipeline-board sidebar offer Resume on exactly that status. A resume
// landing in that window used to fail with "already registered".
//
// Found by root-causing a CI flake whose message was exactly that; the test
// that surfaced it was NOT timing out (it failed in 5-7s against a 30s wait),
// so no deadline raise would have fixed it.

func TestRegister_WaitsOutAnExitingPreviousRunner(t *testing.T) {
	m := NewManager()
	if _, err := m.Register(context.Background(), "run-1"); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	// The engine has returned, so the run already reads resumable — this is
	// the window the studio offers Resume in.
	m.MarkLeaving("run-1")

	// The previous runner finishes its teardown shortly after the resume
	// attempt begins, exactly as the real park sequence does.
	go func() {
		time.Sleep(80 * time.Millisecond)
		m.Deregister("run-1")
	}()

	start := time.Now()
	if _, err := m.Register(context.Background(), "run-1"); err != nil {
		t.Fatalf("re-Register during the hand-off window: %v — a resume in this window must wait, not fail", err)
	}
	if waited := time.Since(start); waited < 50*time.Millisecond {
		t.Errorf("returned after %s: it cannot have waited for the previous runner", waited)
	}
}

// The guard against two genuinely concurrent runners for one run must stay
// intact: a previous runner that never leaves still yields an error, it does
// not get silently displaced.
func TestRegister_StillRefusesARunnerThatNeverLeaves(t *testing.T) {
	m := NewManager()
	m.handoffGrace = 20 * time.Millisecond // the refusal path, not the production wait
	if _, err := m.Register(context.Background(), "run-2"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	m.MarkLeaving("run-2") // marked, but the teardown never completes
	if _, err := m.Register(context.Background(), "run-2"); err == nil {
		t.Fatal("re-Register succeeded against a previous runner that never left — two runners would share one run")
	}
}

// A runner that is still WORKING is not a hand-off, and the refusal must not
// pay the grace to say so. Otherwise every duplicate registration parks its
// caller — an HTTP handler, typically — for the full five seconds before
// returning the error it could have returned at once.
func TestRegister_RefusesALiveRunnerImmediately(t *testing.T) {
	m := NewManager()
	m.handoffGrace = 10 * time.Second // must not be paid at all, so make it obvious
	if _, err := m.Register(context.Background(), "run-live"); err != nil {
		t.Fatalf("first Register: %v", err)
	}

	start := time.Now()
	if _, err := m.Register(context.Background(), "run-live"); err == nil {
		t.Fatal("re-Register succeeded against a live runner")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("refusal took %s — a live runner is not a hand-off and must fail fast", waited)
	}
}

// A caller that gives up (client disconnect, server draining) must not be held
// for the rest of the grace.
func TestRegister_HandoffWaitHonoursTheCallerContext(t *testing.T) {
	m := NewManager()
	m.handoffGrace = 10 * time.Second
	if _, err := m.Register(context.Background(), "run-ctx"); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	m.MarkLeaving("run-ctx") // in the hand-off window, but the teardown stalls

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := m.Register(ctx, "run-ctx"); err == nil {
		t.Fatal("re-Register succeeded although the previous runner never left")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("waited %s after the caller's ctx expired", waited)
	}
}

// Deregister closes done, which is what the hand-off wait keys on. A second
// Deregister must stay idempotent (it is called from a defer).
func TestDeregister_ClosesDoneAndIsIdempotent(t *testing.T) {
	m := NewManager()
	if _, err := m.Register(context.Background(), "run-3"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	m.mu.Lock()
	done := m.handles["run-3"].done
	m.mu.Unlock()

	m.Deregister("run-3")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("done not closed: the hand-off wait would block for its full grace")
	}
	m.Deregister("run-3") // must not panic on a closed channel
}
