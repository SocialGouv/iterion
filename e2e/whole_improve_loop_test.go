package e2e

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// wilToInt coerces an edge-relayed numeric (which template substitution may
// deliver as an int, float64, or stringified number) to an int, defaulting
// to 0 for absent/unparseable values — matching how the real next_item python
// seeds its cursor from an empty/literal STATE_CURSOR.
func wilToInt(v interface{}) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	case string:
		s := strings.TrimSpace(x)
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return int(f)
		}
	}
	return 0
}

func wilEqualInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sweepState models the ADR-057 work-list + cursor the real bot persists to
// its out-of-tree scratch dir (scratch_dir/worklist.json + scratch_dir/state). The
// e2e executor cannot run next_item's embedded-python reader or the adaptive
// enumerate agent, so this in-memory model stands in for them and drives the
// one property the sweep's control-flow depends on: the deterministic
// next_item routing (needs_enumerate → capped → exhausted → has_item) keyed on
// the edge-fed incoming_cursor, exactly as the real tool seeds it from the
// state file. Tests mutate items/maxItems and override individual node stubs.
type sweepState struct {
	items      []string // work-item titles; empty until enumerate "writes" them
	maxItems   int
	enumerated bool  // flips true once enumerate runs (the work-list now exists)
	diskCursor int   // models scratch_dir/state (the crash-safe disk cursor)
	cursors    []int // the cursor each next_item pass USED (assertions)
}

// stubSweep registers the baseline stubs for a green sweep: next_item (the
// deterministic cursor/work-list reader), enumerate (writes the list),
// re_enumerate (done-oracle: no more sites by default), transform, the
// verify_build→verify_run gate (green), both reviewers (approve) and
// commit_item. Individual tests override a node afterward (later .on wins) to
// exercise a red verify, a review reject, or an appending re_enumerate.
func stubSweep(exec *scenarioExecutor, st *sweepState) {
	exec.on("next_item", func(in map[string]interface{}) (map[string]interface{}, error) {
		// Mirror the real next_item: the edge-fed incoming_cursor (the advance
		// compute's carry_next) wins when present; otherwise seed from the disk
		// cursor (enumerate/re-enum returns carry nothing). Then persist it.
		cur := st.diskCursor
		if raw, ok := in["incoming_cursor"]; ok {
			if s := strings.TrimSpace(fmt.Sprint(raw)); s != "" && s != "<nil>" {
				cur = wilToInt(raw)
			}
		}
		st.diskCursor = cur
		st.cursors = append(st.cursors, cur)
		out := map[string]interface{}{
			"needs_enumerate": false, "capped": false, "exhausted": false, "has_item": false,
			"item_id": "", "item_title": "", "item_targets": "", "item_change_spec": "",
			"cursor": cur, "total_items": len(st.items),
			"max_items": st.maxItems, "sweep_max": 2*st.maxItems + 30, "transform_max": 2*st.maxItems + 20,
			"_tokens": 1,
		}
		switch {
		case !st.enumerated:
			out["needs_enumerate"] = true
		case cur >= st.maxItems:
			out["capped"] = true
		case cur >= len(st.items):
			out["exhausted"] = true
		default:
			out["has_item"] = true
			out["item_id"] = fmt.Sprintf("item-%d", cur)
			out["item_title"] = st.items[cur]
			out["item_targets"] = st.items[cur] + ".src"
			out["item_change_spec"] = "apply the axis at this site"
		}
		return out, nil
	})
	exec.on("enumerate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		st.enumerated = true // the work-list now exists on "disk"
		return map[string]interface{}{"total_items": len(st.items), "summary": "planned", "_tokens": 1}, nil
	})
	exec.on("re_enumerate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		// Done-oracle default: a fresh scan finds nothing left → converge.
		return map[string]interface{}{"found_more": false, "appended_count": 0, "summary": "done", "_tokens": 1}, nil
	})
	exec.on("transform", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"applied": true, "summary": "applied", "_tokens": 10}, nil
	})
	exec.on("verify_build", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"prepared": true, "summary": "verify.sh written", "_tokens": 1}, nil
	})
	exec.on("verify_run", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"passed": true, "skipped": false, "exit_code": 0, "log_tail": "", "_tokens": 1}, nil
	})
	approve := func(fam string) func(map[string]interface{}) (map[string]interface{}, error) {
		return func(_ map[string]interface{}) (map[string]interface{}, error) {
			return map[string]interface{}{
				"approved": true, "family": fam, "blockers": []string{}, "fix_plan": "",
				"confidence": "high", "_tokens": 10,
			}, nil
		}
	}
	exec.on("reviewer_claude", approve("claude"))
	exec.on("reviewer_gpt", approve("gpt"))
	exec.on("commit_item", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"success": true, "output": "committed", "_tokens": 1}, nil
	})
}

// TestWholeImproveLoop_SweepHappyPath is the canonical ADR-057 flow: enumerate
// writes a 3-item work-list, and each item sweeps transform → verify (green) →
// review (approve) → commit, then re_enumerate finds nothing and the run
// finishes. Asserts:
//   - enumerate runs exactly once (fresh run), re_enumerate once (done-oracle);
//   - each item transforms and commits exactly once (transform == commit == 3);
//   - the cursor advances 0,1,2 across the has-item passes (one commit per site,
//     not the old advance-every-review or stuck-at-0 shapes);
//   - the run finishes.
func TestWholeImproveLoop_SweepHappyPath(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &sweepState{items: []string{"split-foo", "extract-helper", "converge-bar"}, maxItems: 50}
	stubSweep(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-sweep-happy", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-sweep-happy")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("enumerate"); got != 1 {
		t.Errorf("enumerate called %d times, want 1 (fresh run builds the work-list once)", got)
	}
	if got := exec.callCount("re_enumerate"); got != 1 {
		t.Errorf("re_enumerate called %d times, want 1 (the done-oracle runs once, finds nothing)", got)
	}
	if got := exec.callCount("transform"); got != 3 {
		t.Errorf("transform called %d times, want 3 (one per work-item)", got)
	}
	if got := exec.callCount("commit_item"); got != 3 {
		t.Errorf("commit_item called %d times, want 3 (one incremental commit per item)", got)
	}
	// The cursor must advance through 0,1,2 (one commit per site) and reach 3
	// (exhausted → re_enumerate). Collect the distinct in-order cursors the
	// next_item passes saw; a stuck-at-0 cursor (never advances) or one that
	// skips a value both fail this.
	var distinct []int
	seen := map[int]bool{}
	for _, c := range st.cursors {
		if !seen[c] {
			seen[c] = true
			distinct = append(distinct, c)
		}
	}
	if !wilEqualInts(distinct, []int{0, 1, 2, 3}) {
		t.Errorf("distinct incoming_cursor sequence = %v, want [0 1 2 3] (advance one site per commit, then exhausted)", distinct)
	}
}

// TestWholeImproveLoop_RedVerifySkipsWithoutCommit pins the "never land broken
// code" rule: a single-item sweep whose deterministic build/test gate is ALWAYS
// red must SKIP the item without committing (after the bounded verify-fix
// retries), advance the cursor, and still converge + finish. Asserts:
//   - commit_item NEVER fires (a red verify must not commit);
//   - verify_run retried (called ≥ 4: initial + verify_loop(3) retries) before
//     the skip;
//   - the run finishes (skip → advance → re_enumerate empty → done).
func TestWholeImproveLoop_RedVerifySkipsWithoutCommit(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &sweepState{items: []string{"unbuildable-site"}, maxItems: 50}
	stubSweep(exec, st)
	// Override the gate: always red. verify_build "tries" but can't fix it.
	exec.on("verify_run", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"passed": false, "skipped": false, "exit_code": 1,
			"log_tail": "stub build failure", "_tokens": 1,
		}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-sweep-redverify", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-sweep-redverify")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if exec.wasCalled("commit_item") {
		t.Errorf("commit_item fired on an always-red verify — a broken change must be skipped uncommitted, never landed")
	}
	if got := exec.callCount("verify_run"); got < 4 {
		t.Errorf("verify_run called %d times, want ≥4 (initial + verify_loop(3) retries before the skip)", got)
	}
	// The reviewer must never see an unverified change: alt/review is only
	// reached `when passed`, which never happens here.
	if exec.wasCalled("reviewer_claude") || exec.wasCalled("reviewer_gpt") {
		t.Errorf("a reviewer ran on a never-green verify — review must be gated behind a green build")
	}
}

// TestWholeImproveLoop_ReviewRejectRetransforms pins the per-item review→fix
// loop: on a single item the reviewer rejects once with a concrete blocker,
// then approves. The transform must re-run to fix exactly that blocker, then
// the item commits. Asserts transform ran twice, commit_item once, run finishes.
func TestWholeImproveLoop_ReviewRejectRetransforms(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &sweepState{items: []string{"site-needs-two-tries"}, maxItems: 50}
	stubSweep(exec, st)
	// The claude reviewer (first family in DUAL parity here) rejects once with a
	// concrete blocker, then approves. The gpt reviewer always approves.
	reviewCalls := 0
	reject := func(fam string) func(map[string]interface{}) (map[string]interface{}, error) {
		return func(_ map[string]interface{}) (map[string]interface{}, error) {
			reviewCalls++
			approved := reviewCalls > 1
			blockers := []string{"axis applied inconsistently at this site"}
			fixPlan := "use the shared helper like the other sites"
			if approved {
				blockers = []string{}
				fixPlan = ""
			}
			return map[string]interface{}{
				"approved": approved, "family": fam, "blockers": blockers,
				"fix_plan": fixPlan, "confidence": "high", "_tokens": 10,
			}, nil
		}
	}
	exec.on("reviewer_claude", reject("claude"))
	exec.on("reviewer_gpt", reject("gpt"))

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-sweep-reject", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-sweep-reject")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("transform"); got != 2 {
		t.Errorf("transform called %d times, want 2 (initial + one re-transform after the concrete-blocker reject)", got)
	}
	if got := exec.callCount("commit_item"); got != 1 {
		t.Errorf("commit_item called %d times, want 1 (the item commits once, after it is approved)", got)
	}
}

// TestWholeImproveLoop_ReEnumerateAppendsThenConverges pins the done-oracle's
// continuation: enumerate writes 1 item; after it lands, the first re_enumerate
// finds a NEW site and appends it (found_more=true), the sweep processes that
// item too, and only the SECOND re_enumerate reports nothing left → done.
// Asserts both items commit (commit_item == 2), re_enumerate ran twice, and the
// run finishes — the axis is not declared done until a fresh scan finds nothing.
func TestWholeImproveLoop_ReEnumerateAppendsThenConverges(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &sweepState{items: []string{"first-site"}, maxItems: 50}
	stubSweep(exec, st)
	reEnumCalls := 0
	exec.on("re_enumerate", func(_ map[string]interface{}) (map[string]interface{}, error) {
		reEnumCalls++
		if reEnumCalls == 1 {
			// A fresh scan finds one more site → append it to the work-list.
			st.items = append(st.items, "second-site-found-on-rescan")
			return map[string]interface{}{"found_more": true, "appended_count": 1, "summary": "found 1 more", "_tokens": 1}, nil
		}
		return map[string]interface{}{"found_more": false, "appended_count": 0, "summary": "done", "_tokens": 1}, nil
	})

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-sweep-reenum", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	run, err := s.LoadRun(context.Background(), "run-sweep-reenum")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if run.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want %s", run.Status, store.RunStatusFinished)
	}
	if got := exec.callCount("commit_item"); got != 2 {
		t.Errorf("commit_item called %d times, want 2 (the appended site is swept + committed too)", got)
	}
	if reEnumCalls != 2 {
		t.Errorf("re_enumerate called %d times, want 2 (append round + the converge round that finds nothing)", reEnumCalls)
	}
}

// TestWholeImproveLoop_EventTrace establishes the event-coherence baseline for
// the sweep: a happy-path run persists node lifecycle + edge-selection events
// covering the core sweep nodes. This is the regression net for engine event
// emission — a missing event type surfaces here first.
func TestWholeImproveLoop_EventTrace(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")
	exec := newScenarioExecutor()
	st := &sweepState{items: []string{"only-site"}, maxItems: 50}
	stubSweep(exec, st)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-sweep-events", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := s.LoadEvents(context.Background(), "run-sweep-events")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if !hasEvent(events, store.EventRunStarted) {
		t.Errorf("missing run_started event")
	}
	if !hasEvent(events, store.EventRunFinished) {
		t.Errorf("missing run_finished event")
	}
	if countEventType(events, store.EventNodeStarted) < 4 {
		t.Errorf("expected ≥4 node_started events (enumerate + next_item + transform + a reviewer), got %d",
			countEventType(events, store.EventNodeStarted))
	}
	finishedIDs := eventNodeIDs(events, store.EventNodeFinished)
	finishedSet := make(map[string]bool, len(finishedIDs))
	for _, id := range finishedIDs {
		finishedSet[id] = true
	}
	for _, want := range []string{"enumerate", "next_item", "transform", "commit_item"} {
		if !finishedSet[want] {
			t.Errorf("expected node_finished event for %q, got %v", want, finishedIDs)
		}
	}
}

// TestWholeImproveLoop_SweepStructural is a structural assertion on the bot's
// IR: it confirms the ADR-057 sweep spine exists with the expected node kinds —
// the deterministic next_item entry, the adaptive enumerate/transform agents,
// the deterministic verify/commit tool nodes, and the enum-constrained review
// verdict. Drift here (e.g. reverting a deterministic gate to an LLM) breaks the
// mechanism silently, so we pin it.
func TestWholeImproveLoop_SweepStructural(t *testing.T) {
	wf := compileFixtureStubSafe(t, "whole-improve-loop/main.bot")

	if wf.Entry != "next_item" {
		t.Errorf("workflow entry = %q, want %q (the deterministic work-list/cursor reader)", wf.Entry, "next_item")
	}
	// Deterministic (no-LLM) nodes: next_item, verify_run, commit_item are tool
	// nodes; review_gate/mr_gate are compute nodes.
	for _, id := range []string{"next_item", "verify_run", "commit_item"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected tool node %q", id)
		}
		if _, ok := node.(*ir.ToolNode); !ok {
			t.Errorf("node %q is %T, want *ir.ToolNode (deterministic, no LLM)", id, node)
		}
	}
	// Adaptive agents: enumerate/re_enumerate/transform must be agent nodes.
	for _, id := range []string{"enumerate", "re_enumerate", "transform"} {
		node, ok := wf.Nodes[id]
		if !ok {
			t.Fatalf("workflow missing expected agent node %q", id)
		}
		if _, ok := node.(*ir.AgentNode); !ok {
			t.Errorf("node %q is %T, want *ir.AgentNode (adaptive)", id, node)
		}
	}
	// The chunked-review machinery must be gone.
	for _, id := range []string{"snapshot_chunk", "streak_check", "fix_claude", "fix_gpt", "commit_unit"} {
		if _, ok := wf.Nodes[id]; ok {
			t.Errorf("retired chunked-review node %q is still present — ADR-057 replaces the chunker with the axis sweep", id)
		}
	}
}
