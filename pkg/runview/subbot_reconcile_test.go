package runview

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

const reconcileSlowChildBot = `
schema result:
  ok: bool

tool slow:
  command: ` + "`until [ -f \"$ITERION_RECONCILE_CHILD_RELEASE\" ]; do sleep 10ms; done; printf '{\"ok\":true}'`" + `
  output: result

workflow reconcile_slow_child:
  entry: slow
  slow -> done
`

const reconcileParentBot = `
schema result:
  ok: bool

subbot child:
  source: "reconcile_slow_child.bot"
  output: result

workflow reconcile_parent:
  entry: child
  child -> done
`

// TestServicePeriodicReconcileDoesNotFailExecutingSubbot is the end-to-end
// studio-path regression. The subbot executes synchronously in the root's
// registered goroutine without its own Manager handle, while the subbot runner
// holds its child flock. A fast reconcile ticker — including one owned by a
// second Service — must not mistake that child row for an orphan.
func TestServicePeriodicReconcileDoesNotFailExecutingSubbot(t *testing.T) {
	t.Setenv("ITERION_ORPHAN_RECONCILE_INTERVAL", "10ms")
	dir := t.TempDir()
	releasePath := filepath.Join(dir, "release-child")
	t.Setenv("ITERION_RECONCILE_CHILD_RELEASE", releasePath)
	childPath := filepath.Join(dir, "reconcile_slow_child.bot")
	if err := os.WriteFile(childPath, []byte(reconcileSlowChildBot), 0o644); err != nil {
		t.Fatalf("write child bot: %v", err)
	}
	parentPath := filepath.Join(dir, "reconcile_parent.bot")
	if err := os.WriteFile(parentPath, []byte(reconcileParentBot), 0o644); err != nil {
		t.Fatalf("write parent bot: %v", err)
	}

	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(dir))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	defer svc.Stop(context.Background())

	// Project hot-swaps can briefly leave an older Service watching the same
	// store. It has no Manager handle for svc's runs, so the persisted child
	// flock (and typed ancestor fallback) must protect the synchronous child
	// from that observer's reconcile tick.
	observer, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(dir))
	if err != nil {
		t.Fatalf("NewService(observer): %v", err)
	}
	defer observer.Stop(context.Background())

	res, err := svc.Launch(context.Background(), LaunchSpec{FilePath: parentPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// Keep the child live until the reconcile assertions have run. A fixed
	// tool sleep made child creation race the test on loaded -race runners:
	// startup alone can exceed the old five-second observation window.
	releaseChild := func() error {
		return os.WriteFile(releasePath, nil, 0o644)
	}
	defer func() { _ = releaseChild() }()

	var childID string
	deadline := time.Now().Add(30 * time.Second)
	for childID == "" && time.Now().Before(deadline) {
		runs, listErr := svc.ListRunRecordsCtx(context.Background(), ListFilter{})
		if listErr != nil {
			t.Fatalf("ListRunRecordsCtx: %v", listErr)
		}
		for _, run := range runs {
			if run.ParentRunID == res.RunID {
				childID = run.ID
				if run.Status != store.RunStatusRunning {
					t.Fatalf("child was reconciled during execution: status=%q error=%q", run.Status, run.Error)
				}
				break
			}
		}
		if childID == "" {
			time.Sleep(50 * time.Millisecond)
		}
	}
	if childID == "" {
		t.Fatal("subbot child run was never persisted")
	}

	// Cross many 10ms reconcile ticks while the gated child tool is live.
	time.Sleep(150 * time.Millisecond)
	child, err := svc.store.LoadRun(context.Background(), childID)
	if err != nil {
		t.Fatalf("LoadRun(child): %v", err)
	}
	if child.Status != store.RunStatusRunning {
		t.Fatalf("child status during live tool = %q (error %q), want running", child.Status, child.Error)
	}

	if err := releaseChild(); err != nil {
		t.Fatalf("release child: %v", err)
	}
	select {
	case <-res.Done:
	case <-time.After(10 * time.Second):
		t.Fatal("parent did not finish")
	}
	for _, id := range []string{res.RunID, childID} {
		r, loadErr := svc.store.LoadRun(context.Background(), id)
		if loadErr != nil {
			t.Fatalf("LoadRun(%s): %v", id, loadErr)
		}
		if r.Status != store.RunStatusFinished {
			t.Fatalf("%s final status = %q (error %q), want finished", id, r.Status, r.Error)
		}
	}
}
