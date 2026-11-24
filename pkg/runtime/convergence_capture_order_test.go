package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// twelveItems is the fan_out_each width the capture witnesses use: past ten,
// a lexicographic sort of branch ids puts branch_<router>_10 and _11 before
// _2 and leaves _9 last, so a byte-wise order and the item order disagree on
// both the first and the last branch.
const twelveItems = 12

// itemResults is what the branches of a fan_out_each over `each` deliver:
// item i writes the template node `work` and publishes `report`, both
// carrying i, and stops at `collect`; an item listed in failed dies by itself
// with an error naming it and produces nothing.
func itemResults(failed ...int) []*branchResult {
	dead := make(map[int]bool, len(failed))
	for _, i := range failed {
		dead[i] = true
	}
	results := make([]*branchResult, twelveItems)
	for i := range results {
		r := &branchResult{branchID: fmt.Sprintf("branch_each_%d", i), outputs: map[string]map[string]any{}}
		if dead[i] {
			r.err = itemErr(i)
		} else {
			r.outputs["work"] = map[string]any{"item": i}
			r.artifacts = map[string]map[string]any{"report": {"item": i}}
			r.joinNodeID = "collect"
		}
		results[i] = r
	}
	return results
}

// itemErr is the failure item i dies of by itself.
func itemErr(i int) error { return fmt.Errorf("item %d: tool exited 1", i) }

// arrivalOrders is two deliveries of the same branches: in index order, and
// reversed — the two orders the branches' goroutines may finish in. Each is
// built afresh, so the convergence of one cannot leave state on the other's.
func arrivalOrders(build func() []*branchResult) [][]*branchResult {
	forward := build()
	reversed := build()
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return [][]*branchResult{forward, reversed}
}

// convergenceEngine is an engine over the given collectors, each of them
// awaiting with mode.
func convergenceEngine(t *testing.T, mode ir.AwaitMode, collectors ...string) (*Engine, *runState) {
	t.Helper()
	nodes := make(map[string]ir.Node, len(collectors))
	for _, id := range collectors {
		nodes[id] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}, AwaitMode: mode}
	}
	eng := New(&ir.Workflow{Nodes: nodes}, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("capture-order", nil)
	rs.ctx = context.Background()
	return eng, rs
}

// TestConvergence_theCaptureIsTheLastItemByIndexWhateverFinishedLast: every
// branch of a fan_out_each writes the same template node ids, so the join
// keeps one value per id. That value is the last successful item's by index
// — item 11 of twelve — in whichever order the branches delivered, and so is
// the artifact they all publish; the failures listed in `_failed_branches`
// come in item order (2 before 10), not completion order and not byte order.
func TestConvergence_theCaptureIsTheLastItemByIndexWhateverFinishedLast(t *testing.T) {
	for _, results := range arrivalOrders(func() []*branchResult { return itemResults(2, 10) }) {
		first := results[0].branchID
		eng, rs := convergenceEngine(t, ir.AwaitBestEffort, "collect")
		next, err := eng.processConvergence(rs, "collect", results, nil, nil)
		if err != nil || next != "collect" {
			t.Fatalf("%s delivered first: best_effort convergence = (%q, %v), want (collect, nil)", first, next, err)
		}
		if got := rs.outputs["work"]["item"]; got != 11 {
			t.Errorf("%s delivered first: the collector reads outputs.work.item = %v, want 11 — the last successful item by index", first, got)
		}
		if got := rs.artifacts["report"]["item"]; got != 11 {
			t.Errorf("%s delivered first: artifacts.report.item = %v, want 11 — the last publishing item by index", first, got)
		}
		failed, _ := rs.outputs["collect"]["_failed_branches"].([]map[string]any)
		var ids []any
		for _, f := range failed {
			ids = append(ids, f["branch_id"])
		}
		if want := []any{"branch_each_2", "branch_each_10"}; !reflect.DeepEqual(ids, want) {
			t.Errorf("%s delivered first: _failed_branches = %v, want %v — item order", first, ids, want)
		}
	}
}

// TestConvergence_waitAllQuotesTheFirstFailedItemByIndex: re-sorting the
// branch ids on their parsed index changes which failure a wait_all
// aggregate quotes and wraps once a fan-out has ten items or more — item 2,
// not item 10, which a byte-wise sort put first.
func TestConvergence_waitAllQuotesTheFirstFailedItemByIndex(t *testing.T) {
	for _, results := range arrivalOrders(func() []*branchResult { return itemResults(2, 10) }) {
		first := results[0].branchID
		eng, rs := convergenceEngine(t, ir.AwaitWaitAll, "collect")
		_, err := eng.processConvergence(rs, "collect", results, nil, nil)
		if err == nil {
			t.Fatalf("%s delivered first: a wait_all convergence over two failed items did not fail", first)
		}
		if !strings.Contains(err.Error(), itemErr(2).Error()) || strings.Contains(err.Error(), itemErr(10).Error()) {
			t.Errorf("%s delivered first: the failure quotes %q, want item 2's error alone", first, err)
		}
		var rt *RuntimeError
		if !errors.As(err, &rt) || rt.Cause == nil || rt.Cause.Error() != itemErr(2).Error() {
			t.Errorf("%s delivered first: the aggregate's cause is %v, want item 2's failure", first, err)
		}
	}
}

// TestConvergence_waitAllCarriesTheFirstItemsLoopDecline: when the failed
// items agree on how they ended — two declines at a loop's cap — the
// aggregate carries the decline of the first by index, so the loop a chain
// walker reads is item 2's, not item 10's.
func TestConvergence_waitAllCarriesTheFirstItemsLoopDecline(t *testing.T) {
	build := func() []*branchResult {
		results := itemResults(2, 10)
		for _, i := range []int{2, 10} {
			results[i].err = fmt.Errorf("item %d: %w", i, &LoopDeclined{Loop: fmt.Sprintf("fix_%d", i), Reason: "loop_cap"})
		}
		return results
	}
	for _, results := range arrivalOrders(build) {
		first := results[0].branchID
		eng, rs := convergenceEngine(t, ir.AwaitWaitAll, "collect")
		_, err := eng.processConvergence(rs, "collect", results, nil, nil)
		var d *LoopDeclined
		if !errors.As(err, &d) || d.Loop != "fix_2" {
			t.Errorf("%s delivered first: the aggregate carries the decline %v, want loop fix_2's — item 2, the first by index", first, d)
		}
	}
}

// TestConvergenceTerminal_keepsTheLastItemAndTheFirstBranchsTerminal: an
// all-done fan-out merges the same way, and the terminal it hands back is
// the first branch's by index, not the first to deliver.
func TestConvergenceTerminal_keepsTheLastItemAndTheFirstBranchsTerminal(t *testing.T) {
	build := func() []*branchResult {
		results := itemResults()
		for i, r := range results {
			r.joinNodeID = ""
			r.terminatedAtDone = true
			r.terminalNodeID = fmt.Sprintf("done_%d", i)
		}
		return results
	}
	for _, results := range arrivalOrders(build) {
		first := results[0].branchID
		eng, rs := convergenceEngine(t, ir.AwaitWaitAll)
		terminal, err := eng.processConvergenceTerminal(rs, results, ir.AwaitBestEffort)
		if err != nil {
			t.Fatalf("%s delivered first: %v", first, err)
		}
		if terminal != "done_0" {
			t.Errorf("%s delivered first: the run ends at %q, want done_0 — the first branch's by index", first, terminal)
		}
		if got := rs.outputs["work"]["item"]; got != 11 {
			t.Errorf("%s delivered first: outputs.work.item = %v, want 11 — the last item by index", first, got)
		}
	}
}

// TestResolveConvergence_bestEffortJoinsWhereTheFirstItemByIndexStopped:
// under best_effort, branches that stopped at different collectors keep the
// first reporting branch's collector — item 2's, items 0 and 1 having died
// before any — so the node the trunk continues from does not depend on which
// branch delivered first, nor on a byte-wise sort that puts item 10 ahead.
func TestResolveConvergence_bestEffortJoinsWhereTheFirstItemByIndexStopped(t *testing.T) {
	build := func() []*branchResult {
		results := itemResults(0, 1)
		for i, r := range results {
			if i > 2 {
				r.joinNodeID = "collect_late"
			}
		}
		return results
	}
	for _, results := range arrivalOrders(build) {
		first := results[0].branchID
		eng, rs := convergenceEngine(t, ir.AwaitBestEffort, "collect", "collect_late")
		next, err := eng.resolveConvergence(rs, "each", results, fanOutPlan{preComputedConvergence: "collect"})
		if err != nil {
			t.Fatalf("%s delivered first: %v", first, err)
		}
		if next != "collect" {
			t.Errorf("%s delivered first: the trunk continues from %q, want collect — where item 2, the first reporting item by index, stopped", first, next)
		}
	}
}

// TestByBranchID_ordersItemsOnTheirParsedIndex: fan_out_each ids compare on
// the index they end in — a router id carrying digits and underscores
// included — while ids with no index keep their byte order. The order is
// total: every arrival order of the same ids sorts to one sequence, mixed
// shapes included (a byte-wise rule for some pairs and a numeric one for
// others would not be transitive, and the result would follow the input).
func TestByBranchID_ordersItemsOnTheirParsedIndex(t *testing.T) {
	for _, tc := range []struct {
		ids  []string
		want []string
	}{
		{
			ids:  []string{"branch_r_10", "branch_r_2", "branch_r_0", "branch_r_11", "branch_r_1"},
			want: []string{"branch_r_0", "branch_r_1", "branch_r_2", "branch_r_10", "branch_r_11"},
		},
		{
			ids:  []string{"branch_r_2_10", "branch_r_2_9", "branch_r_2_0"},
			want: []string{"branch_r_2_0", "branch_r_2_9", "branch_r_2_10"},
		},
		{
			ids:  []string{"branch_split_review", "branch_split_lint", "branch_split_b1"},
			want: []string{"branch_split_b1", "branch_split_lint", "branch_split_review"},
		},
		{
			ids:  []string{"x_10", "x_2", "x_1a", "x_", "x"},
			want: []string{"x", "x_", "x_2", "x_10", "x_1a"},
		},
	} {
		for _, perm := range permutations(tc.ids) {
			results := make([]*branchResult, len(perm))
			for i, id := range perm {
				results[i] = &branchResult{branchID: id}
			}
			var got []string
			for _, r := range byBranchID(results) {
				got = append(got, r.branchID)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("byBranchID(%v) = %v, want %v", perm, got, tc.want)
			}
		}
	}
}

// permutations is every ordering of ids.
func permutations(ids []string) [][]string {
	if len(ids) <= 1 {
		return [][]string{append([]string(nil), ids...)}
	}
	var out [][]string
	for i := range ids {
		rest := append(append([]string(nil), ids[:i]...), ids[i+1:]...)
		for _, tail := range permutations(rest) {
			out = append(out, append([]string{ids[i]}, tail...))
		}
	}
	return out
}

// TestFanOutEach_theCollectorReadsTheLastItemWhicheverFinishedLast drives
// the real path — branch goroutines, the collector's channel, the join —
// with twelve items chained by `depends_on:` so that they finish one after
// the other, once in index order and once reversed. The collector's mapping
// `{{outputs.handle.value}}` reads item 11's value in both: the order the
// branches finished in does not reach what the collector receives.
func TestFanOutEach_theCollectorReadsTheLastItemWhicheverFinishedLast(t *testing.T) {
	for _, chain := range []struct {
		name  string
		after func(i int) int // the item i waits for
	}{
		{"items finish in index order", func(i int) int { return i - 1 }},
		{"items finish in reverse index order", func(i int) int { return i + 1 }},
	} {
		t.Run(chain.name, func(t *testing.T) {
			wf := fanOutEachWorkflow(true, ir.AwaitWaitAll, 0)
			for _, edge := range wf.Edges {
				if edge.From == "handle" && edge.To == "collect" {
					edge.With = []*ir.DataMapping{{
						Key:  "value",
						Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"handle", "value"}, Raw: "{{outputs.handle.value}}"}},
						Raw:  "{{outputs.handle.value}}",
					}}
				}
			}
			items := make([]any, twelveItems)
			wantFinished := make([]string, twelveItems)
			for i := range items {
				id := fmt.Sprintf("i%d", i)
				d := chain.after(i)
				if d >= 0 && d < twelveItems {
					items[i] = item(id, fmt.Sprintf("i%d", d))
				} else {
					items[i] = item(id)
				}
				// Item i finishes in position i of the forward chain, and in
				// position 11-i of the reversed one.
				pos := i
				if chain.after(0) > 0 {
					pos = twelveItems - 1 - i
				}
				wantFinished[pos] = id
			}

			var mu sync.Mutex
			var finished []string
			var collected []any
			exec := newStubExecutor()
			exec.on("entry", func(map[string]any) (map[string]any, error) {
				return map[string]any{"items": items}, nil
			})
			exec.on("handle", func(in map[string]any) (map[string]any, error) {
				id, _ := in["id"].(string)
				mu.Lock()
				finished = append(finished, id)
				mu.Unlock()
				return map[string]any{"value": id}, nil
			})
			exec.on("collect", func(in map[string]any) (map[string]any, error) {
				mu.Lock()
				collected = append(collected, in["value"])
				mu.Unlock()
				return map[string]any{"done": true}, nil
			})

			s := tmpStore(t)
			if err := New(wf, s, exec).Run(context.Background(), "run-capture-order", nil); err != nil {
				t.Fatalf("run: %v", err)
			}
			if r, _ := s.LoadRun(context.Background(), "run-capture-order"); r == nil || r.Status != store.RunStatusFinished {
				t.Fatalf("run did not finish: %+v", r)
			}
			mu.Lock()
			defer mu.Unlock()
			// The chain is what makes this subtest the order its name says.
			if !reflect.DeepEqual(finished, wantFinished) {
				t.Fatalf("the items finished in %v, want %v — the depends_on chain did not order them", finished, wantFinished)
			}
			if want := []any{"i11"}; !reflect.DeepEqual(collected, want) {
				t.Fatalf("the collector received value %v, want %v — item 11's, the last by index", collected, want)
			}
		})
	}
}
