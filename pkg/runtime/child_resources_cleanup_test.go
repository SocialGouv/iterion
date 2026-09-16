package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestChildResourcesAbandonedReaderReturnsBeforeRestore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		work, st := t.TempDir(), tmpStore(t)
		parent := New(resourceWorkflow(""), st, newStubExecutor(), WithWorkDir(work), WithBundle(resourceBundle(t, "parent")), WithSandboxOverride("none"))
		if err := parent.Run(context.Background(), "parent", nil); err != nil {
			t.Fatal(err)
		}
		wf := resourceWorkflow("")
		wf.Entry = "router"
		wf.Nodes["router"] = &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll}
		wf.Nodes["done"] = &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}, AwaitMode: ir.AwaitWaitAll}
		wf.Edges = []*ir.Edge{{From: "router", To: "before"}, {From: "router", To: "after"}, {From: "before", To: "done"}, {From: "after", To: "done"}}
		wf.Budget = &ir.Budget{MaxParallelBranches: 2}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started, release := make(chan struct{}), make(chan struct{})
		unblock := sync.OnceFunc(func() { close(release) })
		defer unblock()
		ex := newStubExecutor()
		ex.on("before", func(map[string]any) (map[string]any, error) { <-started; cancel(); return nil, ctx.Err() })
		ex.on("after", func(map[string]any) (map[string]any, error) { close(started); <-release; return map[string]any{}, nil })
		child := New(wf, st, ex, WithWorkDir(work), WithBundle(resourceBundle(t, "child")), WithParentRunID("parent"), WithSandboxOverride("none"))
		done := make(chan error, 1)
		began := time.Now()
		go func() { done <- child.Run(ctx, "child", nil) }()
		err := <-done
		var classified *RuntimeError
		if !errors.As(err, &classified) || classified.Code != ErrCodeResourceRestore {
			t.Fatalf("cleanup code must outrank cancellation: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), "snapshot retained") {
			t.Fatalf("cleanup did not return its retained-snapshot failure: %v", err)
		}
		if elapsed := time.Since(began); elapsed > branchCancelGracePeriod+resourceDrainTimeout+time.Second {
			t.Fatalf("cancellation took %s", elapsed)
		}
		assertResourceBody(t, work, "child") // Never restore underneath the live reader.
		run, err := st.LoadRun(context.Background(), "child")
		if err != nil || run.Status != store.RunStatusFailedResumable || run.FailureCode != store.FailureResourceRestore {
			t.Fatalf("cleanup outcome=%+v,%v", run, err)
		}
		unblock()
		synctest.Wait()
		assertResourceBody(t, work, "parent")
		run, err = st.LoadRun(context.Background(), "child")
		if err != nil || run.Status != store.RunStatusFailedResumable {
			t.Fatalf("late cleanup published success: %+v,%v", run, err)
		}
	})
}

func TestChildResourcesRestoreFailureCannotBeReattachedAsSuccess(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "run", true: "resume"}[resume], func(t *testing.T) {
			work, st := t.TempDir(), tmpStore(t)
			parent := New(resourceWorkflow(""), st, newStubExecutor(), WithWorkDir(work), WithBundle(resourceBundle(t, "parent")), WithSandboxOverride("none"))
			if err := parent.Run(context.Background(), "parent", nil); err != nil {
				t.Fatal(err)
			}
			wf := resourceWorkflow("")
			if resume {
				wf.Nodes["gate"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
				wf.Edges = []*ir.Edge{{From: "before", To: "gate"}, {From: "gate", To: "after"}, {From: "after", To: "done"}}
			}
			b := resourceBundle(t, "child")
			child := func() *Engine {
				ex := newStubExecutor()
				ex.on("after", func(map[string]any) (map[string]any, error) {
					if err := os.RemoveAll(filepath.Join(work, ".claude")); err != nil {
						return nil, err
					}
					if err := os.WriteFile(filepath.Join(work, ".claude"), []byte("blocks restoration"), 0o600); err != nil {
						return nil, err
					}
					return map[string]any{}, nil
				})
				return New(wf, st, ex, WithWorkDir(work), WithBundle(b), WithParentRunID("parent"), WithSandboxOverride("none"))
			}
			err := child().Run(context.Background(), "child", nil)
			if resume {
				if !errors.Is(err, ErrRunPaused) {
					t.Fatalf("initial pause: %v", err)
				}
				assertResourceBody(t, work, "parent")
				err = child().Resume(context.Background(), "child", map[string]any{"approved": true})
			}
			if err == nil || !strings.Contains(err.Error(), "restore child resources") {
				t.Fatalf("missing restore failure: %v", err)
			}
			match := regexp.MustCompile(`backup ([^)]+)`).FindStringSubmatch(err.Error())
			if len(match) != 2 {
				t.Fatalf("backup not named: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(match[1]) })
			if _, err := os.Stat(filepath.Join(match[1], "skills", "shared.md")); err != nil {
				t.Fatalf("backup lost: %v", err)
			}
			run, err := st.LoadRun(context.Background(), "child")
			if err != nil || run.Status != store.RunStatusFailedResumable || run.FailureCode != store.FailureResourceRestore {
				t.Fatalf("restoration failure left reusable success: %+v,%v", run, err)
			}
			events, err := st.LoadEvents(context.Background(), "child")
			if err != nil {
				t.Fatal(err)
			}
			foundFailure := false
			for _, event := range events {
				if event.Type == store.EventRunFinished {
					t.Fatal("success was published before resource restoration")
				}
				if event.Type == store.EventRunFailed && event.Data["code"] == string(store.FailureResourceRestore) {
					foundFailure = true
				}
			}
			if !foundFailure {
				t.Fatal("typed cleanup failure event missing")
			}
		})
	}
}

// TestARunThatBorrowedNothingIsNotFailedByAGateItDoesNotOwn.
//
// A resource scope is opened for ANY run carrying a bundle, contributions or a
// subbot node — not only for a child that borrows the workspace's resources.
// For those, `restore` is a no-op and the gate is the shared per-workspace one,
// so draining it waits on readers the run does not own: a sibling still in
// setup, or an abandoned branch executor after a cancellation. The timeout then
// wrote FAILED_RESUMABLE over a run that had reached its end, never published
// `run_finished`, and the joined error made finalization skip the fast-forward
// and orphan the run's commits — all to hand back nothing.
//
// The reader is taken while the run EXECUTES, which is when a sibling would
// take it: taking it earlier would block this run's own setup instead, which is
// a different situation and not the one that failed.
func TestARunThatBorrowedNothingIsNotFailedByAGateItDoesNotOwn(t *testing.T) {
	work, st := t.TempDir(), tmpStore(t)
	dir, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}

	held := make(chan struct{})
	ex := newStubExecutor()
	ex.on("after", func(map[string]any) (map[string]any, error) {
		gate, releaseGate := retainResourceGate(dir)
		if err := gate.sem.Acquire(context.Background(), 1); err != nil {
			return nil, err
		}
		go func() {
			<-held
			gate.sem.Release(1)
			releaseGate()
		}()
		return map[string]any{}, nil
	})
	defer close(held)

	began := time.Now()
	e := New(resourceWorkflow(""), st, ex, WithWorkDir(work),
		WithBundle(resourceBundle(t, "solo")), WithSandboxOverride("none"))
	if err := e.Run(context.Background(), "solo", nil); err != nil {
		t.Fatalf("a run that borrowed nothing was failed by a gate it does not own: %v", err)
	}
	if elapsed := time.Since(began); elapsed >= resourceDrainTimeout {
		t.Errorf("run took %s: it waited on a drain it has no reason to perform", elapsed)
	}
	run, err := st.LoadRun(context.Background(), "solo")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusFinished {
		t.Errorf("status = %q (%q), want finished", run.Status, run.FailureCode)
	}
	// And completion was published by the exec loop, not deferred into the
	// cleanup that runs after sandbox teardown. Deferring it for every bundle
	// run left a completed run reporting `running` through container stop and
	// workspace export, a window where an eviction leaves it unfinished forever.
	if e.resourceScope != nil && e.resourceScope.reachedDone {
		t.Error("a run that borrowed nothing deferred its own completion")
	}
}
