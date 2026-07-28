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

	// The previous runner is on its way out: it deregisters shortly after the
	// resume attempt begins, exactly as the real park sequence does.
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
	// The handle is alive (done open), so the registration check must fire.
	if _, err := m.Register(context.Background(), "run-2"); err == nil {
		t.Fatal("re-Register succeeded against a LIVE previous runner — two runners would share one run")
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
