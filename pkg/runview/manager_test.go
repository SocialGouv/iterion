package runview

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
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
	// Already closed, not "closed within a second": a context's Done is shut
	// by the time its CancelFunc returns, so the only thing a timer adds here
	// is a verdict the host's scheduler can flip — the select picks at random
	// among the arms that are ready, and a second of starvation makes the
	// timer one of them.
	select {
	case <-ctx.Done():
	default:
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

// runnerTeardown is how long each engine goroutine spends between its
// cancellation and its Deregister, in the bubble's virtual time: the drain
// Stop owes its caller.
const runnerTeardown = 40 * time.Millisecond

func TestManager_StopDrains(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager()
		const N = 3
		doneCh := make(chan string, N)

		// Every goroutine spawned here blocks on a channel, so each gets an
		// escape the test owns AND a join the test DEFERS — releaseAfter needs
		// only the join, its goroutine blocking in a time.Sleep virtual time
		// always completes: a t.Fatalf below while one of these still blocks
		// exits the main bubble goroutine behind a blocked one, which synctest
		// reports as `deadlock: main bubble goroutine has exited but blocked
		// goroutines remain` — a panic that aborts the test BINARY. Measured
		// on this tree with neither: a Stop that stops cancelling takes
		// pkg/runview from 739 tests run to 247, so one regression here would
		// cost ~490 other tests their verdict. Deferred, the joins run on the
		// Goexit path too; release is closed only after the last assertion, so
		// it can never decide one.
		release := make(chan struct{})
		joins := make([]chan struct{}, 0, N)
		defer func() {
			close(release)
			for _, gone := range joins {
				<-gone
			}
		}()

		for i := 0; i < N; i++ {
			runID := "run-" + string(rune('A'+i))
			ctx, err := m.Register(context.Background(), runID)
			if err != nil {
				t.Fatalf("Register %s: %v", runID, err)
			}
			gone := make(chan struct{})
			joins = append(joins, gone)
			go func(rid string, c context.Context) {
				defer close(gone)
				select {
				case <-c.Done():
				case <-release:
					return
				}
				// Mimic the engine goroutine's normal teardown: it takes a
				// while, and Stop owes that wait to whoever called it.
				time.Sleep(runnerTeardown)
				m.Deregister(rid)
				doneCh <- rid
			}(runID, ctx)
		}

		stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		start := time.Now()
		m.Stop(stopCtx)

		// Exactly the teardown, both directions: Stop has two halves — cancel
		// every handle, then wait for each to drain — and only the equality
		// witnesses the second. A shorter wait is a Stop that cancelled and
		// walked away, leaving engines writing into a store the server has
		// already declared down; a longer one is the shutdown ctx expiring on
		// a run Stop never cancelled.
		if waited := time.Since(start); waited != runnerTeardown {
			diagnosis := "it did not wait for the drain"
			if waited > runnerTeardown {
				diagnosis = "it waited out the shutdown budget instead: a handle it never cancelled"
			}
			t.Errorf("Stop returned after %s, want the %s each run spends tearing down — %s",
				waited, runnerTeardown, diagnosis)
		}

		// Manager.Stop returns once each handle's done is closed, which the
		// teardown goroutines do BEFORE reporting themselves — so the reports
		// may still be in flight here. synctest.Wait is the join: it returns
		// once every goroutine of the bubble is durably blocked or gone, which
		// is a fact about this fixture rather than a guess at how fast three
		// goroutines are rescheduled.
		synctest.Wait()
		for i := 0; i < N; i++ {
			select {
			case <-doneCh:
			default:
				t.Fatalf("only %d/%d goroutines drained", i, N)
			}
		}
		for _, id := range []string{"run-A", "run-B", "run-C"} {
			if m.Active(id) {
				t.Errorf("%s still active after Stop", id)
			}
		}
	})
}

func TestManager_StopRejectsNewRegistrations(t *testing.T) {
	m := NewManager()
	// No handle is registered, so Stop cancels nothing and never reaches the
	// `ctx.Done()` arm of its drain: the harness context is the bound, and a
	// duration here would be a real-clock figure nothing reads.
	m.Stop(t.Context())
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
//
// Every hand-off witness below runs inside testing/synctest: the Manager is
// pure in-process state — no store, no subprocess, no background worker — so
// the bubble's durable-block contract holds, and each figure these tests read
// is the VIRTUAL time the code under test spent waiting. Host load adds none
// of it (docs/test-waits.md draws the line at real-process service fixtures,
// which these are not). What that buys, per #2362: a refusal that must not
// pay the grace is read as exactly zero rather than "under a second", a wait
// for the previous runner as exactly the teardown it waited for, and the
// production grace itself costs the suite nothing.

// previousRunnerTeardown is how long the outgoing runner takes to finish
// leaving, in the bubble's virtual time: the gap a resume lands in.
const previousRunnerTeardown = 80 * time.Millisecond

// releaseAfter runs release once after d of the bubble's virtual time, and
// returns the join its caller MUST defer.
//
// The join is not hygiene: inside a bubble, a t.Fatalf while this goroutine
// still sleeps exits the main bubble goroutine with a blocked one behind it,
// which synctest reports as `deadlock: main bubble goroutine has exited but
// blocked goroutines remain` — a panic that aborts the test BINARY: the
// failing assertion is still printed, every test after it is not. Deferred,
// the receive runs on the Goexit path too: the test goroutine blocks durably,
// virtual time reaches d, the release happens, and the failure is reported as
// its own.
func releaseAfter(t *testing.T, d time.Duration, release func()) (join func()) {
	t.Helper()
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(d)
		release()
	}()
	return func() { <-released }
}

// handoffWaitDiagnosis names the direction a hand-off wait missed its exact
// figure in. AwaitHandoff has two wake-ups — the previous runner's Deregister
// and the grace timer — so one figure guards two defects: shorter is a
// successor released before the previous runner left, longer is a successor
// the Deregister never woke, held until the grace expired. At one fault only
// the longer direction reaches here: a wait that returns early leaves the
// handle registered, so the premise above — the re-Register succeeding at all
// — fails first (measured, on a mutant that deletes AwaitHandoff's wait).
func handoffWaitDiagnosis(waited, want time.Duration) string {
	if waited > want {
		return "Deregister never woke it: it was held to the end of the hand-off grace"
	}
	return "it did not wait for Deregister"
}

func TestRegister_WaitsOutAnExitingPreviousRunner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager()
		if _, err := m.Register(context.Background(), "run-1"); err != nil {
			t.Fatalf("first Register: %v", err)
		}

		// The engine has returned, so the run already reads resumable — this is
		// the window the studio offers Resume in.
		m.MarkLeaving("run-1")

		// The previous runner finishes its teardown shortly after the resume
		// attempt begins, exactly as the real park sequence does.
		start := time.Now()
		left := releaseAfter(t, previousRunnerTeardown, func() { m.Deregister("run-1") })
		defer left()

		if _, err := m.Register(context.Background(), "run-1"); err != nil {
			t.Fatalf("re-Register during the hand-off window: %v — a resume in this window must wait, not fail", err)
		}
		// Exactly the teardown, both directions: zero is a successor released
		// before the previous runner left (its remaining defers would tear
		// down THIS registration), and the full handoffGrace is a wait that
		// timed out instead of being woken by Deregister.
		if waited := time.Since(start); waited != previousRunnerTeardown {
			t.Errorf("returned after %s, want the %s the previous runner took to leave — %s",
				waited, previousRunnerTeardown, handoffWaitDiagnosis(waited, previousRunnerTeardown))
		}
	})
}

// The guard against two genuinely concurrent runners for one run must stay
// intact: a previous runner that never leaves still yields an error, it does
// not get silently displaced.
func TestRegister_StillRefusesARunnerThatNeverLeaves(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager()
		if _, err := m.Register(context.Background(), "run-2"); err != nil {
			t.Fatalf("first Register: %v", err)
		}
		m.MarkLeaving("run-2") // marked, but the teardown never completes
		// The PRODUCTION grace, kept: virtual time elapses it the instant the
		// wait blocks durably, so the refusal costs the suite nothing and the
		// test no longer has to shorten the very figure it exercises.
		start := time.Now()
		if _, err := m.Register(context.Background(), "run-2"); err == nil {
			t.Fatal("re-Register succeeded against a previous runner that never left — two runners would share one run")
		}
		if waited := time.Since(start); waited != registerHandoffGrace {
			t.Errorf("refused after %s, want the full %s grace: a previous runner on its way out is waited for to the end",
				waited, registerHandoffGrace)
		}
	})
}

// A runner that is still WORKING is not a hand-off, and the refusal must not
// pay the grace to say so. Otherwise every duplicate registration parks its
// caller — an HTTP handler, typically — for the whole registerHandoffGrace
// before returning the error it could have returned at once.
func TestRegister_RefusesALiveRunnerImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager()
		m.handoffGrace = time.Hour // paying ANY of it is the defect; make the size unmistakable
		if _, err := m.Register(context.Background(), "run-live"); err != nil {
			t.Fatalf("first Register: %v", err)
		}

		start := time.Now()
		if _, err := m.Register(context.Background(), "run-live"); err == nil {
			t.Fatal("re-Register succeeded against a live runner")
		}
		// Zero, not "under a second": the only thing that moves the bubble's
		// clock is the code under test blocking, so a refusal that consults
		// nothing spends exactly none of the grace. The 1s bound this replaces
		// left a second of slack over a legitimate zero wait, and a loaded
		// runner closes that much (#2161 measured 1.78s of scheduling on one
		// call).
		if waited := time.Since(start); waited != 0 {
			t.Errorf("refusal paid %s of the %s hand-off grace — a live runner is not a hand-off and must fail fast",
				waited, m.handoffGrace)
		}
	})
}

// A caller that gives up (client disconnect, server draining) must not be held
// for the rest of the grace — and must be told WHY it failed. "Already
// registered" and "your deadline expired mid-hand-off" call for opposite
// reactions from an automated retry chain.
func TestRegister_HandoffWaitHonoursTheCallerContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager()
		m.handoffGrace = time.Hour // the wait the caller must NOT be held for
		if _, err := m.Register(context.Background(), "run-ctx"); err != nil {
			t.Fatalf("first Register: %v", err)
		}
		m.MarkLeaving("run-ctx") // in the hand-off window, but the teardown stalls

		const callerBudget = 30 * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), callerBudget)
		defer cancel()
		start := time.Now()
		_, err := m.Register(ctx, "run-ctx")
		if err == nil {
			t.Fatal("re-Register succeeded although the previous runner never left")
		}
		// The caller's own deadline to the millisecond: the wait ends on
		// ctx.Done, never on a share of the grace behind it.
		if waited := time.Since(start); waited != callerBudget {
			t.Errorf("waited %s, want the caller's own %s deadline and not a share of the %s grace",
				waited, callerBudget, m.handoffGrace)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want it to wrap context.DeadlineExceeded — a caller "+
				"cannot tell a transient hand-off timeout from racing a live runner otherwise", err)
		}
	})
}

// The hand-off signal must mean "that goroutine is FINISHED", not "it is
// partway through its teardown". A successor released too early races the
// outgoing goroutine's remaining defers, and those are keyed on run id alone:
// dropRunLog and unregisterRunEngine would tear down the SUCCESSOR's log buffer
// and engine registration, turning a loud "already registered" into a resume
// that reports success with a dead log tail and no steering channel.
//
// Asserted here on the Manager contract that makes it possible — Deregister,
// the only thing that closes done, must be the last teardown step in
// spawnRun's defer chain. This test pins the half the Manager owns; the defer
// ordering is pinned by its own comment in service_launch.go.
func TestDeregister_IsTheLastWordOnAHandoff(t *testing.T) {
	m := NewManager()
	if _, err := m.Register(context.Background(), "run-order"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	m.MarkLeaving("run-order")

	m.mu.Lock()
	done := m.handles["run-order"].done
	m.mu.Unlock()

	// Still held: the wait must not release on MarkLeaving alone, or a
	// successor would start while the teardown is still running.
	select {
	case <-done:
		t.Fatal("done closed by MarkLeaving — the successor would race the whole teardown")
	default:
	}

	m.Deregister("run-order")
	// Deregister closes done before it returns, so the fact is readable
	// without a clock.
	select {
	case <-done:
	default:
		t.Fatal("Deregister did not close done")
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
	default:
		t.Fatal("done not closed: the hand-off wait would block for its full grace")
	}
	m.Deregister("run-3") // must not panic on a closed channel
}

// A subbot child is resumed EXTERNALLY while its in-process handle is still
// held, and its paused status reaches the store before the engine has finished
// returning. That makes a second registration a hand-off by construction, which
// is why the child declares it at registration instead of waiting for a
// MarkLeaving moment that comes too late to help.
//
// This is the case that started the whole investigation
// (TestServiceLaunch_SubbotChildHumanGate_ParkAndResume flaking on
// "already registered"), and it is the case a leaving-only gate silently stops
// covering.
func TestExpectHandoff_MakesAChildResumeWaitRatherThanFail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager()
		if _, err := m.Register(context.Background(), "child-1"); err != nil {
			t.Fatalf("Register: %v", err)
		}
		m.ExpectHandoff("child-1")

		start := time.Now()
		left := releaseAfter(t, previousRunnerTeardown, func() { m.Deregister("child-1") })
		defer left()

		if _, err := m.Register(context.Background(), "child-1"); err != nil {
			t.Fatalf("external resume of a child mid-release: %v — it must wait for the "+
				"active pass to release, not fail", err)
		}
		if waited := time.Since(start); waited != previousRunnerTeardown {
			t.Errorf("returned after %s, want the %s the active pass took to release — %s",
				waited, previousRunnerTeardown, handoffWaitDiagnosis(waited, previousRunnerTeardown))
		}
	})
}
