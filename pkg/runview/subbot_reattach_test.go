package runview

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// pinContract stamps the wire-form contract onto a child run doc, the way the
// engine stamps it at launch (pkg/runtime/engine_run.go).
func pinContract(t *testing.T, s store.RunStore, childID string, contract *ir.PublicContract) {
	t.Helper()
	raw, err := json.Marshal(contract)
	if err != nil {
		t.Fatalf("marshal contract: %v", err)
	}
	r, err := s.LoadRun(context.Background(), childID)
	if err != nil {
		t.Fatalf("load child %s: %v", childID, err)
	}
	r.PublicContract = raw
	if err := s.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("save child %s: %v", childID, err)
	}
}

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

	t.Run("finished contracted child → projected from the PINNED contract + clear record", func(t *testing.T) {
		// A reused child that keeps a contract hands the parent the contract's
		// ports projected from the child's per-node outputs (read from the
		// run's events) — not its terminal-node output (#1280). The contract
		// read is the one PINNED on the child's run doc at launch: the source
		// is never touched (there is none in this subtest at all).
		s := mustStore(t)
		if _, err := s.CreateRun(ctx, "parent", "p", nil); err != nil {
			t.Fatal(err)
		}
		mkChild(t, s, "parent", "run_child", "child-con", store.RunStatusFinished, map[string]any{"url": "https://forge/pr/9", "log": "noise"})
		if err := s.SetSubbotChild(ctx, "parent", "run_child", "child-con"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendEvent(ctx, "child-con", store.Event{
			Type:   store.EventNodeFinished,
			NodeID: "verify",
			Data:   map[string]any{"output": map[string]any{"passed": true}},
		}); err != nil {
			t.Fatal(err)
		}
		pinContract(t, s, "child-con", &ir.PublicContract{Name: "kid", Outputs: []*ir.PublicPort{
			{Name: "url", Type: "string", FromNode: "terminal", FromField: "url"},
			{Name: "passed", Type: "bool", FromNode: "verify", FromField: "passed"},
			{Name: "verify_out", Type: "json", FromNode: "verify"},
			{Name: "ghost", Type: "string", FromNode: "never_ran", FromField: "x"},
		}})
		out, err, handled := ReattachSubbotChild(ctx, s, newReq("parent", "run_child"), iterlog.Nop())
		if !handled || err != nil {
			t.Fatalf("handled=%v err=%v; want handled=true err=nil", handled, err)
		}
		want := map[string]any{
			"url":        "https://forge/pr/9",
			"passed":     true,
			"verify_out": map[string]any{"passed": true},
		}
		if !reflect.DeepEqual(out, want) {
			t.Fatalf("projected =\n%v\nwant\n%v", out, want)
		}
		if _, ok := out["ghost"]; ok {
			t.Error("a port whose producer never ran must be an absent key")
		}
		p, _ := s.LoadRun(ctx, "parent")
		if _, ok := p.SubbotChildren["run_child"]; ok {
			t.Errorf("record not cleared after consuming the projected output: %v", p.SubbotChildren)
		}
	})

	t.Run("finished child whose source has since vanished → still projected", func(t *testing.T) {
		// The wedge the adversarial round executed (RVA1): with the contract
		// read from a recompiled source, a parent whose child source changed
		// or vanished while it was down failed EVERY future resume. The pin
		// reads the child's own doc — the source is never touched, and this
		// subtest ships no source file at all.
		s := mustStore(t)
		if _, err := s.CreateRun(ctx, "parent", "p", nil); err != nil {
			t.Fatal(err)
		}
		mkChild(t, s, "parent", "run_child", "child-gone-src", store.RunStatusFinished, map[string]any{"url": "https://forge/pr/10", "log": "noise"})
		if err := s.SetSubbotChild(ctx, "parent", "run_child", "child-gone-src"); err != nil {
			t.Fatal(err)
		}
		pinContract(t, s, "child-gone-src", &ir.PublicContract{Name: "kid", Outputs: []*ir.PublicPort{
			{Name: "url", Type: "string", FromNode: "terminal", FromField: "url"},
		}})
		out, err, handled := ReattachSubbotChild(ctx, s, newReq("parent", "run_child"), iterlog.Nop())
		if !handled || err != nil {
			t.Fatalf("handled=%v err=%v; want handled=true err=nil", handled, err)
		}
		if out["url"] != "https://forge/pr/10" {
			t.Fatalf("projected = %v, want url read from the pinned contract", out)
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
