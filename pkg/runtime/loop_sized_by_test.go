package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// #2677 — the loop budget guard prices the next iteration at what the
// PREVIOUS one consumed, so a loop whose iterations shrink (a re-ask of
// the still-pending subset) is priced at its first, largest pass: the
// ticket's run declined a 43-item re-ask priced as the 104-item round.
//
// The `sized_by` clause names the loop's work signal
// (`as reask("vars.max_reask") sized_by "outputs.probe.pending"`): the
// guard scales the previous crossing's cost by
// size_now / size_at_previous_crossing, sampled when the loop's mark is
// taken. A loop with no clause keeps today's pricing word for word.

// reaskProbeBot is the ticket's shape, reduced: one probe node re-asked
// while its pending count is non-zero. Round 0 burns 2000 tokens and
// leaves 43 pending; the 43-item re-ask burns 2500 and leaves 10; the
// last re-ask drains them. max_tokens 7000 puts the 90% threshold at
// 6300, so the UNPRORATED crossing after the re-ask (4500 + 2500 =
// 7000) is declined while the prorated one (4500 + 2500×10/43 ≈ 5081)
// crosses.
const reaskProbeBot = `agent probe:
  model: "m"

workflow w:
  worktree: none
  sandbox: none
  entry: probe
  budget:
    max_tokens: 7000
    max_iterations: 20
  probe -> probe when "outputs.probe.pending > 0" as reask(5)%SIZED_BY%
  probe -> done
`

// probeExecutor scripts the probe's rounds: each call burns the round's
// tokens and reports what is still pending AFTER it — the signal the
// next crossing's price scales with.
func probeExecutor(calls *int) *stubExecutor {
	exec := newStubExecutor()
	exec.on("probe", func(_ map[string]any) (map[string]any, error) {
		*calls++
		switch *calls {
		case 1:
			return map[string]any{"pending": 43, "_tokens": 2_000}, nil
		case 2:
			return map[string]any{"pending": 10, "_tokens": 2_500}, nil
		default:
			return map[string]any{"pending": 0, "_tokens": 100}, nil
		}
	})
	return exec
}

func loopBudgetDeclines(t *testing.T, s store.RunStore, runID string) []map[string]any {
	t.Helper()
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	var out []map[string]any
	for _, ev := range events {
		if ev.Type == store.EventBudgetWarning && ev.Data["reason"] == "loop_budget_guard" {
			out = append(out, ev.Data)
		}
	}
	return out
}

// The re-ask the guard can afford once the price follows the traversal:
// after the 104-item round (2000 tokens, 43 pending) and the 43-item
// re-ask (2500 tokens, 10 pending), the next re-ask is TEN items —
// priced at the re-ask's cost scaled by 10/43, it crosses; priced at
// the whole previous iteration, it is declined.
func TestLoopBudgetGuard_SizedByPricesTheTraversalNotTheFirstPass(t *testing.T) {
	wf := compileBotText(t, strings.ReplaceAll(reaskProbeBot, "%SIZED_BY%", ` sized_by "outputs.probe.pending"`))
	var calls int
	s := tmpStore(t)
	eng := New(wf, s, probeExecutor(&calls))
	if err := eng.Run(context.Background(), "run-reask-sized", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 3 {
		t.Fatalf("probe ran %d times, want 3 — the 10-item re-ask was priced at the previous iteration's whole cost and declined", calls)
	}
	for _, d := range loopBudgetDeclines(t, s, "run-reask-sized") {
		t.Errorf("a sized loop was declined: %v", d)
	}
}

// Control: the same traversal with NO sized_by clause keeps today's
// pricing — the next re-ask costs what the previous iteration consumed,
// the crossing is declined, and the run leaves by its exit path. This
// is the behavior the ticket measured, kept word for word when the loop
// declares no signal.
func TestLoopBudgetGuard_UnsignedLoopKeepsThePessimisticPrice(t *testing.T) {
	wf := compileBotText(t, strings.ReplaceAll(reaskProbeBot, "%SIZED_BY%", ""))
	var calls int
	s := tmpStore(t)
	eng := New(wf, s, probeExecutor(&calls))
	if err := eng.Run(context.Background(), "run-reask-plain", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("probe ran %d times, want 2 — the guard should decline the third round at the unscaled price", calls)
	}
	declines := loopBudgetDeclines(t, s, "run-reask-plain")
	if len(declines) != 1 {
		t.Fatalf("declines = %d, want 1", len(declines))
	}
	if declines[0]["loop"] != "reask" {
		t.Errorf("decline names loop %v, want reask", declines[0]["loop"])
	}
}

// The proration is a SCALE, not a waiver: when even the size-scaled
// price does not fit, the back-edge is still declined — and the decline
// says the proration it applied, so an operator reads the real sizes.
func TestLoopBudgetGuard_SizedByStillDeclinesWhatDoesNotFit(t *testing.T) {
	wf := compileBotText(t, strings.ReplaceAll(reaskProbeBot, "%SIZED_BY%", ` sized_by "outputs.probe.pending"`))
	var calls int
	exec := newStubExecutor()
	exec.on("probe", func(_ map[string]any) (map[string]any, error) {
		calls++
		switch calls {
		case 1:
			return map[string]any{"pending": 43, "_tokens": 2_000}, nil
		default:
			// The re-ask leaves 38 of 43 pending: the scale (38/43)
			// barely moves the price, and 4500 + 2500×38/43 ≈ 6709 is
			// still past the 6300 threshold.
			return map[string]any{"pending": 38, "_tokens": 2_500}, nil
		}
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-reask-sized-decline", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls != 2 {
		t.Fatalf("probe ran %d times, want 2 — the barely-shrinking re-ask must still be declined", calls)
	}
	declines := loopBudgetDeclines(t, s, "run-reask-sized-decline")
	if len(declines) != 1 {
		t.Fatalf("declines = %d, want 1", len(declines))
	}
	d := declines[0]
	if d["sized_by"] != "outputs.probe.pending" {
		t.Errorf("the decline does not name the loop's signal: %v", d)
	}
	if d["size_previous"] != 43.0 || d["size_now"] != 38.0 {
		t.Errorf("the decline does not carry the sizes it priced with: %v", d)
	}
}

// The samples ride the checkpoint beside the marks: a run resumed onto
// a sized loop's decision node prices the crossing with the size the
// mark carried, not unscaled — the same pause-continuity the marks
// themselves keep (TestLoopBudgetGuard_MarksSurviveResume).
func TestLoopBudgetGuard_SizedBySamplesSurviveResume(t *testing.T) {
	wf := compileBotText(t, strings.ReplaceAll(reaskProbeBot, "%SIZED_BY%", ` sized_by "outputs.probe.pending"`))
	eng := New(wf, tmpStore(t), newStubExecutor())

	// The origin run took the first re-ask crossing: priced at 2000 with
	// the probe's 43 pending sampled into the mark.
	origin := eng.newRunState("r", nil)
	origin.budget = newSharedBudget(wf.Budget, eng.logger)
	origin.outputs["probe"] = map[string]any{"pending": 43}
	origin.budget.RecordUsage(spendOf(2_000, 0))
	eng.markLoopBudget(origin, "reask")
	if got := origin.loopBudgetSizes["reask"]; got != 43 {
		t.Fatalf("the mark sampled size %v, want 43", got)
	}

	cp := buildCheckpoint(origin, "probe")
	if cp.LoopBudgetSizes["reask"] != 43 {
		t.Fatalf("checkpoint carries sizes %v, want reask=43", cp.LoopBudgetSizes)
	}

	// Resumed onto the probe with 10 pending after a 2500-token re-ask:
	// the next crossing is priced 2500 × 10/43 ≈ 581, well under the
	// 6300 threshold — unscaled it would be 7000 and decline.
	resumed := eng.newRunState("r", nil)
	resumed.budget = newSharedBudget(wf.Budget, eng.logger)
	restoreBudgetAccounting(resumed, cp)
	eng.baselineUnpricedLoops(resumed)
	resumed.outputs["probe"] = map[string]any{"pending": 10}
	resumed.budget.RecordUsage(spendOf(2_500, 0))

	v := eng.loopBudgetShortfall("reask", resumed)
	if v != nil {
		t.Fatalf("resumed run declined the crossing at %.0f %s: the size sample did not survive the pause", v.spent, v.dimension)
	}
}
