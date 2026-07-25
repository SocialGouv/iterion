package runview

import (
	"context"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// mkChild creates a child run linked to parentID via ParentRunID/ParentNodeID
// and drives it to the given terminal status, stamping a node_finished event
// carrying `output` so subbotTerminalOutput can reconstruct it.
func mkChild(t *testing.T, s store.RunStore, parentID, nodeID, childID string, status store.RunStatus, output map[string]any) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, childID, "child", nil); err != nil {
		t.Fatalf("create child %s: %v", childID, err)
	}
	r, err := s.LoadRun(ctx, childID)
	if err != nil {
		t.Fatalf("load child %s: %v", childID, err)
	}
	r.ParentRunID = parentID
	r.ParentNodeID = nodeID
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatalf("save child %s: %v", childID, err)
	}
	if output != nil {
		if _, err := s.AppendEvent(ctx, childID, store.Event{
			Type:   store.EventNodeFinished,
			NodeID: "terminal",
			Data:   map[string]any{"output": output},
		}); err != nil {
			t.Fatalf("append child event %s: %v", childID, err)
		}
	}
	if err := s.UpdateRunStatus(ctx, childID, status, ""); err != nil {
		t.Fatalf("status child %s: %v", childID, err)
	}
}

// TestReattachSubbotChild exercises the re-attach decision oracle directly:
// the recorded child's status decides reuse (finished → its output) vs.
// spawn-fresh (failed / cancelled / vanished / no record), and a consumed or
// discarded record is cleared so it can't be re-consumed.
func TestReattachSubbotChild(t *testing.T) {
	ctx := context.Background()
	newReq := func(parentID, key string) runtime.SubbotRequest {
		return runtime.SubbotRequest{ParentRunID: parentID, NodeID: "run_child", ReattachKey: key}
	}

	t.Run("no record → spawn fresh", func(t *testing.T) {
		s := mustStore(t)
		if _, err := s.CreateRun(ctx, "parent", "p", nil); err != nil {
			t.Fatal(err)
		}
		_, _, handled := ReattachSubbotChild(ctx, s, newReq("parent", "run_child"), iterlog.Nop())
		if handled {
			t.Fatal("handled=true with no recorded child; want spawn-fresh")
		}
	})

	t.Run("empty key → spawn fresh", func(t *testing.T) {
		s := mustStore(t)
		if _, err := s.CreateRun(ctx, "parent", "p", nil); err != nil {
			t.Fatal(err)
		}
		_, _, handled := ReattachSubbotChild(ctx, s, newReq("parent", ""), iterlog.Nop())
		if handled {
			t.Fatal("handled=true with empty key; want spawn-fresh")
		}
	})

	t.Run("finished child → reuse output + clear record", func(t *testing.T) {
		s := mustStore(t)
		if _, err := s.CreateRun(ctx, "parent", "p", nil); err != nil {
			t.Fatal(err)
		}
		mkChild(t, s, "parent", "run_child", "child-fin", store.RunStatusFinished, map[string]any{"verdict": "ship it"})
		if err := s.SetSubbotChild(ctx, "parent", "run_child", "child-fin"); err != nil {
			t.Fatal(err)
		}
		out, err, handled := ReattachSubbotChild(ctx, s, newReq("parent", "run_child"), iterlog.Nop())
		if !handled || err != nil {
			t.Fatalf("handled=%v err=%v; want handled=true err=nil", handled, err)
		}
		if out["verdict"] != "ship it" {
			t.Errorf("reused output = %v; want verdict=ship it", out)
		}
		p, _ := s.LoadRun(ctx, "parent")
		if _, ok := p.SubbotChildren["run_child"]; ok {
			t.Errorf("record not cleared after consuming finished child: %v", p.SubbotChildren)
		}
	})

	t.Run("failed child → spawn fresh + clear stale record", func(t *testing.T) {
		s := mustStore(t)
		if _, err := s.CreateRun(ctx, "parent", "p", nil); err != nil {
			t.Fatal(err)
		}
		mkChild(t, s, "parent", "run_child", "child-bad", store.RunStatusFailed, nil)
		if err := s.SetSubbotChild(ctx, "parent", "run_child", "child-bad"); err != nil {
			t.Fatal(err)
		}
		_, _, handled := ReattachSubbotChild(ctx, s, newReq("parent", "run_child"), iterlog.Nop())
		if handled {
			t.Fatal("handled=true on a failed child; want spawn-fresh")
		}
		p, _ := s.LoadRun(ctx, "parent")
		if _, ok := p.SubbotChildren["run_child"]; ok {
			t.Errorf("stale record for failed child not cleared: %v", p.SubbotChildren)
		}
	})

	t.Run("in-flight child + shutdown mid-park → error, record PRESERVED", func(t *testing.T) {
		// A resumed parent re-parks on a still-paused child; the process is then
		// shut down (ctx cancelled) before the child is answered. AwaitSubbotTerminal
		// returns ctx.Err(); the record MUST survive so the next resume re-attaches
		// rather than spawning a fresh child (ADR-083 invariant, regression guard).
		s := mustStore(t)
		if _, err := s.CreateRun(ctx, "parent", "p", nil); err != nil {
			t.Fatal(err)
		}
		mkChild(t, s, "parent", "run_child", "child-paused", store.RunStatusPausedWaitingHuman, nil)
		if err := s.SetSubbotChild(ctx, "parent", "run_child", "child-paused"); err != nil {
			t.Fatal(err)
		}
		cctx, cancel := context.WithCancel(ctx)
		cancel() // simulate the process shutting down while re-parked
		_, err, handled := ReattachSubbotChild(cctx, s, newReq("parent", "run_child"), iterlog.Nop())
		if !handled {
			t.Fatal("handled=false for an in-flight child; want handled=true (parked)")
		}
		if err == nil {
			t.Fatal("err=nil on a cancelled-ctx park; want ctx error")
		}
		p, _ := s.LoadRun(ctx, "parent")
		if p.SubbotChildren["run_child"] != "child-paused" {
			t.Errorf("record cleared on shutdown mid-park (lost-work bug): %v", p.SubbotChildren)
		}
	})

	t.Run("vanished child → spawn fresh + clear stale record", func(t *testing.T) {
		s := mustStore(t)
		if _, err := s.CreateRun(ctx, "parent", "p", nil); err != nil {
			t.Fatal(err)
		}
		// Record points at a child that was never created (pruned).
		if err := s.SetSubbotChild(ctx, "parent", "run_child", "child-gone"); err != nil {
			t.Fatal(err)
		}
		_, _, handled := ReattachSubbotChild(ctx, s, newReq("parent", "run_child"), iterlog.Nop())
		if handled {
			t.Fatal("handled=true on a vanished child; want spawn-fresh")
		}
		p, _ := s.LoadRun(ctx, "parent")
		if _, ok := p.SubbotChildren["run_child"]; ok {
			t.Errorf("stale record for vanished child not cleared: %v", p.SubbotChildren)
		}
	})
}

func mustStore(t *testing.T) store.RunStore {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return s
}
