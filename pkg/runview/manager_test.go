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
