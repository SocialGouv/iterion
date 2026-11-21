package runview

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Child: a tool node that blocks (sleep) long enough for us to act on the
// CHILD run while it is still executing its first pass. Parent: prep -> child.
const subbotControlChild = `
schema child_out:
  done: bool

tool spin:
  command: ` + "`sleep 30; printf '{\"done\":true}'`" + `
  output: child_out

workflow control_child:
  entry: spin
  spin -> done
`

// A looping child: each iteration is a ~1s node boundary, so the cooperative
// operator-pause signal (checked at the top of the exec loop, never mid-tool)
// is observed within a second instead of after a 30s blocking sleep.
const subbotControlLoopChild = `
schema tick_out:
  cont: bool

tool tick:
  command: ` + "`sleep 1; printf '{\"cont\":true}'`" + `
  output: tick_out

workflow control_child:
  entry: tick
  tick -> tick as spin(60) when cont
  tick -> done else
`

const subbotControlParent = `
schema pout:
  ready: bool

schema child_out:
  done: bool

tool prep:
  command: ` + "`printf '{\"ready\":true}'`" + `
  output: pout

subbot run_child:
  source: "control_child.bot"
  output: child_out

workflow control_parent:
  entry: prep
  prep -> run_child
  run_child -> done
`

// waitForActiveChild observes the persisted child before exercising its control.
func waitForActiveChild(t *testing.T, svc *Service, parentID string) string {
	t.Helper()
	return waitForSubbotStatus(t, svc, parentID, store.RunStatusRunning)
}

// The regression for H2 (PR #193): a studio Cancel targeting the CHILD run id
// must act on it WHILE it runs its first pass, not silently no-op until the
// child pauses. Before the fix the in-process child engine ran unregistered,
// so manager.Cancel(childID) returned ErrRunNotActive.
func TestServiceLaunch_SubbotChild_CancelMidFlight(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(subbotControlChild), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}
	parentPath := filepath.Join(dir, "control_parent.bot")
	if err := os.WriteFile(parentPath, []byte(subbotControlParent), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}

	// The run gets a repository the test OWNS: without one, `worktree: auto`
	// (the IR default) takes os.Getwd() — this package inside the developer's
	// checkout — and registers the run's worktree there for good (#870).
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	t.Cleanup(func() { stopService(t, svc) })

	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: parentPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// A run goroutine that outlives the test writes into the store while
	// t.TempDir() is removing it — "directory not empty" on cleanup, a second
	// failure that hides the first.
	t.Cleanup(func() {
		_ = svc.Cancel(res.RunID)
		awaitRunCompletion(t, res.Done, "run goroutine outlived the test")
	})

	childID := waitForActiveChild(t, svc, res.RunID)

	// The child must be individually controllable: it is registered with the
	// run Manager while executing, so Cancel(childID) reaches it (pre-fix this
	// returned ErrRunNotActive).
	if !svc.manager.Active(childID) {
		t.Fatalf("child %s is not registered with the manager mid-flight", childID)
	}
	if err := svc.Cancel(childID); err != nil {
		t.Fatalf("Cancel(child) = %v, want nil (child not controllable mid-flight)", err)
	}

	// Completion is signalled by the run goroutine; the status assertion
	// below still rejects a child that ran to completion instead of cancelling.
	awaitRunCompletion(t, res.Done, "parent did not terminate after its child was cancelled")

	child, err := svc.store.LoadRun(context.Background(), childID)
	if err != nil {
		t.Fatalf("load child: %v", err)
	}
	if child.Status != store.RunStatusCancelled {
		t.Fatalf("child status = %q, want cancelled", child.Status)
	}

	// The manager handle is released once the child's pass returns, so a
	// later external resume can re-register the same id.
	if svc.manager.Active(childID) {
		t.Errorf("child %s still registered after it terminated — release missed", childID)
	}
}

// A studio Pause targeting the CHILD run id checkpoints it at the next safe
// boundary (paused_operator), leaving it resumable — the "ideally the operator
// pause signal" half of the fix. The child loops a short tool node so a pause
// boundary recurs every ~1s; the operator-paused child then PARKS the parent
// (like a human gate) until an external resume, so the parent stays running.
func TestServiceLaunch_SubbotChild_PauseMidFlight(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(subbotControlLoopChild), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}
	parentPath := filepath.Join(dir, "control_parent.bot")
	if err := os.WriteFile(parentPath, []byte(subbotControlParent), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}

	// The run gets a repository the test OWNS: without one, `worktree: auto`
	// (the IR default) takes os.Getwd() — this package inside the developer's
	// checkout — and registers the run's worktree there for good (#870).
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	t.Cleanup(func() { stopService(t, svc) })

	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: parentPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		_ = svc.Cancel(res.RunID)
		awaitRunCompletion(t, res.Done, "run goroutine outlived the test")
	})

	childID := waitForActiveChild(t, svc, res.RunID)

	// Pause must be accepted by the manager (pre-fix: ErrRunNotActive).
	if err := svc.Pause(childID); err != nil {
		t.Fatalf("Pause(child) = %v, want nil", err)
	}

	// Within a few loop boundaries the child checkpoints as paused_operator.
	waitCtx := runWaitContext(t)
	for {
		if waitCtx.Err() != nil {
			child, _ := svc.store.LoadRun(context.Background(), childID)
			t.Fatalf("child never reached paused_operator (last status %q)", child.Status)
		}
		child, err := svc.store.LoadRun(context.Background(), childID)
		if err != nil {
			t.Fatalf("load child: %v", err)
		}
		if child.Status == store.RunStatusPausedOperator {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// An operator-paused child parks the parent's subbot node (awaiting an
	// external resume), exactly like a human gate — the parent stays running,
	// not failed.
	parent, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	if parent.Status != store.RunStatusRunning {
		t.Fatalf("parent status while child paused = %q, want running", parent.Status)
	}
}

type childBirthBarrierStore struct {
	store.RunStore
	born    chan string
	release chan struct{}
}

func (s *childBirthBarrierStore) BeginRunBirth(ctx context.Context, id string) (store.RunBirthReservation, error) {
	birth, err := store.AsRunBirthStore(s.RunStore).BeginRunBirth(ctx, id)
	if err != nil {
		return nil, err
	}
	return &subbotTestBirth{RunBirthReservation: birth, after: s.afterPublish}, nil
}
func (s *childBirthBarrierStore) afterPublish(ctx context.Context, r *store.Run, e error) (*store.Run, error) {
	if e == nil && r.WorkflowName == "control_child" {
		s.born <- r.ID
		select {
		case <-s.release:
		case <-ctx.Done():
			return r, ctx.Err()
		}
	}
	return r, e
}
func TestSubbotBirthHasManagerAndCrossProcessLiveness(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "control_child.bot"), []byte(subbotControlLoopChild), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}
	parentPath := filepath.Join(dir, "control_parent.bot")
	if err := os.WriteFile(parentPath, []byte(subbotControlParent), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}

	// The run gets a repository the test OWNS: without one, `worktree: auto`
	// (the IR default) takes os.Getwd() — this package inside the developer's
	// checkout — and registers the run's worktree there for good (#870).
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(gittest.SourceRepo(t)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	barrier := &childBirthBarrierStore{RunStore: svc.store, born: make(chan string, 1), release: make(chan struct{})}
	svc.store = barrier
	released := false
	defer func() {
		if !released {
			close(barrier.release)
		}
	}()
	t.Cleanup(func() { stopService(t, svc) })

	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: parentPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		_ = svc.Cancel(res.RunID)
		awaitRunCompletion(t, res.Done, "run goroutine outlived the test")
	})

	var bornID string
	select {
	case bornID = <-barrier.born:
	case <-runWaitContext(t).Done():
		t.Fatal("child birth not reached")
	}
	born, loadErr := svc.store.LoadRun(t.Context(), bornID)
	if loadErr != nil || born.Status != store.RunStatusRunning || !svc.manager.Active(bornID) {
		t.Fatalf("published child status=%+v err=%v active=%v", born, loadErr, svc.manager.Active(bornID))
	}
	if lock, err := svc.store.LockRun(t.Context(), bornID); err == nil {
		_ = lock.Unlock()
		t.Fatal("published running child has no cross-process liveness")
	}
	close(barrier.release)
	released = true
	childID := waitForActiveChild(t, svc, res.RunID)
	if childID != bornID {
		t.Fatal("child replaced across registration")
	}

	// Pause must be accepted by the manager (pre-fix: ErrRunNotActive).
	if err := svc.Pause(childID); err != nil {
		t.Fatalf("Pause(child) = %v, want nil", err)
	}

	// Within a few loop boundaries the child checkpoints as paused_operator.
	waitCtx := runWaitContext(t)
	for {
		if waitCtx.Err() != nil {
			child, _ := svc.store.LoadRun(context.Background(), childID)
			t.Fatalf("child never reached paused_operator (last status %q)", child.Status)
		}
		child, err := svc.store.LoadRun(context.Background(), childID)
		if err != nil {
			t.Fatalf("load child: %v", err)
		}
		if child.Status == store.RunStatusPausedOperator {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// An operator-paused child parks the parent's subbot node (awaiting an
	// external resume), exactly like a human gate — the parent stays running,
	// not failed.
	parent, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("load parent: %v", err)
	}
	if parent.Status != store.RunStatusRunning {
		t.Fatalf("parent status while child paused = %q, want running", parent.Status)
	}
}

func (s *childBirthBarrierStore) Unwrap() store.RunStore { return s.RunStore }

func TestSubbotBirthDefaultAndExplicitState(t *testing.T) {
	for _, row := range []struct {
		name      string
		ask, want store.RunStatus
		refuse    bool
	}{{"default", "", store.RunStatusRunning, false}, {"queued", store.RunStatusQueued, store.RunStatusQueued, false}, {"running", store.RunStatusRunning, store.RunStatusRunning, false}, {"terminal", store.RunStatusFinished, "", true}} {
		t.Run(row.name, func(t *testing.T) {
			s, e := store.New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			ctx := t.Context()
			if _, e = s.CreateRun(ctx, "parent", "parent", nil); e != nil {
				t.Fatal(e)
			}
			ctx, e = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "parent", Format: store.LegacyValueFormat})
			if e != nil {
				t.Fatal(e)
			}
			prepared, e := PrepareSubbotInvocation(ctx, s, "parent", store.RunCreateSpec{ID: "child", WorkflowName: "child", Status: row.ask})
			if row.refuse {
				if e == nil {
					t.Fatal("terminal birth accepted")
				}
				return
			}
			if e != nil || prepared.Run.Status != row.want {
				t.Fatalf("prepared=%+v err=%v want=%s", prepared.Run, e, row.want)
			}
			actual, e := s.LoadRun(prepared.Context, "child")
			if e != nil || actual.Status != row.want {
				t.Fatalf("durable=%+v err=%v", actual, e)
			}
		})
	}
}
