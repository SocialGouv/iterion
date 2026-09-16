package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/store"
)

func compileBotText(t *testing.T, src string) *ir.Workflow {
	t.Helper()
	pr := parser.Parse("x.bot", src)
	for _, d := range pr.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("fixture does not parse: %s", d.Error())
		}
	}
	cr := ir.Compile(pr.File)
	for _, d := range cr.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("fixture does not compile: %s", d.Error())
		}
	}
	return cr.Workflow
}

const spentLoopBot = `schema verdict:
  ok: bool

agent check:
  model: "m"
  output: verdict

judge assess:
  model: "m"
  output: verdict

workflow w:
  worktree: none
  sandbox: none
  entry: check
  budget:
    max_iterations: 20
  check -> assess
  assess -> check when not ok as retry(2)
  assess -> done when ok
`

// A loop spent with no other edge left kills the run as LOOP_EXHAUSTED —
// the code the docs and the retry policy always named — not as a missing
// edge in general; with the loop-exhaustion exit the run goes on.
func TestALoopSpentWithNoExitFailsAsLoopExhausted(t *testing.T) {
	never := func(map[string]any) (map[string]any, error) { return map[string]any{"ok": false}, nil }
	exec := newStubExecutor()
	exec.on("check", never)
	exec.on("assess", never)
	s := tmpStore(t)
	eng := New(compileBotText(t, spentLoopBot), s, exec)
	if err := eng.Run(context.Background(), "run-spent", nil); err == nil {
		t.Fatal("a spent loop with no exit finished the run")
	}
	run, err := s.LoadRun(context.Background(), "run-spent")
	if err != nil {
		t.Fatal(err)
	}
	if run.FailureCode != store.FailureLoopExhausted {
		t.Fatalf("the run died of %q, want %q (%s)", run.FailureCode, store.FailureLoopExhausted, run.Error)
	}

	exec2 := newStubExecutor()
	exec2.on("check", never)
	exec2.on("assess", never)
	s2 := tmpStore(t)
	eng2 := New(compileBotText(t, spentLoopBot+"  assess -> done\n"), s2, exec2)
	if err := eng2.Run(context.Background(), "run-exit", nil); err != nil {
		t.Fatalf("the loop-exhaustion exit did not carry the run to done: %v", err)
	}
	run2, err := s2.LoadRun(context.Background(), "run-exit")
	if err != nil {
		t.Fatal(err)
	}
	if run2.Status != store.RunStatusFinished {
		t.Fatalf("the run with an exit ended %q", run2.Status)
	}
}

// The engine says why it declined a loop edge at its cap, as an event a
// reader of the run can tell apart: a bounded loop's cap, an unbounded
// loop's fuel.
// The engine says why it declined a loop edge, in the payload the
// budget_warning consumers read: `reason` for a reader of the run, and —
// the warning having no budget axis — `dimension` and `detail`, which the
// alert manager and the report render in place of a used/limit ratio.
func TestADeclinedLoopEdgeSaysWhy(t *testing.T) {
	never := func(map[string]any) (map[string]any, error) { return map[string]any{"ok": false}, nil }
	for _, tc := range []struct {
		edge, reason, figures string
		changing              bool
	}{
		{"  assess -> check when not ok as retry(unbounded 2)\n  assess -> done when ok\n", "loop_out_of_fuel", "2/2", true},
		{"  assess -> check when not ok as retry(unbounded 50)\n  assess -> done when ok\n", "liveness_stall", "3 crossings", false},
	} {
		exec := newStubExecutor()
		exec.on("check", never)
		exec.on("assess", func(map[string]any) (map[string]any, error) {
			if !tc.changing {
				// Unchanging outputs: the liveness monitor stalls the
				// unbounded loop long before its fuel is spent.
				return map[string]any{"ok": false}, nil
			}
			// Outputs that change every time, so the liveness monitor never
			// stalls the unbounded loop before its fuel is spent.
			return map[string]any{"ok": false, "n": time.Now().UnixNano()}, nil
		})
		s := tmpStore(t)
		var payloads []map[string]any
		src := strings.Replace(spentLoopBot, "  assess -> check when not ok as retry(2)\n  assess -> done when ok\n", tc.edge, 1)
		eng := New(compileBotText(t, src), s, exec, WithEventObserver(func(evt store.Event) {
			if evt.Type == store.EventBudgetWarning {
				payloads = append(payloads, evt.Data)
			}
		}))
		if err := eng.Run(context.Background(), "run-why-"+tc.reason, nil); err == nil {
			t.Fatalf("%s: the spent loop finished the run", tc.reason)
		}
		var found map[string]any
		for _, p := range payloads {
			if r, _ := p["reason"].(string); r == tc.reason {
				found = p
			}
		}
		if found == nil {
			t.Fatalf("%s: the engine did not say why it declined the edge: %v", tc.reason, payloads)
		}
		detail, _ := found["detail"].(string)
		if found["dimension"] != "loop" || !strings.Contains(detail, `"retry"`) || !strings.Contains(detail, tc.figures) {
			t.Fatalf("%s: the payload does not carry what the alert manager and the report render: %v", tc.reason, found)
		}
		// The decline is said before its outcome is known: the detail
		// must not claim an exit the run may not have.
		if !strings.Contains(detail, "or dies") {
			t.Fatalf("%s: the detail asserts an outcome the decline cannot know: %q", tc.reason, detail)
		}
	}
	// A bounded loop's cap is the program's design, not a warning: reaching
	// it emits nothing — the death carries it when nothing else matches.
	exec := newStubExecutor()
	exec.on("check", never)
	exec.on("assess", never)
	var warnings []map[string]any
	eng := New(compileBotText(t, spentLoopBot), tmpStore(t), exec, WithEventObserver(func(evt store.Event) {
		if evt.Type == store.EventBudgetWarning {
			warnings = append(warnings, evt.Data)
		}
	}))
	if err := eng.Run(context.Background(), "run-cap-silent", nil); err == nil {
		t.Fatal("the spent loop finished the run")
	}
	if len(warnings) != 0 {
		t.Fatalf("a bounded loop reaching its cap warned the operator: %v", warnings)
	}
}

// A death with no edge left carries the loop decline it follows, on the
// error the engine returns: the code says how the run died, the cause says
// why the edge was declined — a reader tells a ceiling from a dead end by
// it without reading the events.
func TestADeathWithNoEdgeLeftCarriesTheDecline(t *testing.T) {
	never := func(map[string]any) (map[string]any, error) { return map[string]any{"ok": false}, nil }
	for _, tc := range []struct {
		edge, reason string
		code         ErrorCode
		changing     bool
	}{
		{"  assess -> check when not ok as retry(2)\n  assess -> done when ok\n", "loop_cap", ErrCodeLoopExhausted, true},
		{"  assess -> check when not ok as retry(unbounded 2)\n  assess -> done when ok\n", "loop_out_of_fuel", ErrCodeLoopExhausted, true},
		{"  assess -> check when not ok as retry(unbounded 50)\n  assess -> done when ok\n", "liveness_stall", ErrCodeNoOutgoingEdge, false},
	} {
		exec := newStubExecutor()
		exec.on("check", never)
		exec.on("assess", func(map[string]any) (map[string]any, error) {
			if !tc.changing {
				return map[string]any{"ok": false}, nil
			}
			return map[string]any{"ok": false, "n": time.Now().UnixNano()}, nil
		})
		src := strings.Replace(spentLoopBot, "  assess -> check when not ok as retry(2)\n  assess -> done when ok\n", tc.edge, 1)
		err := New(compileBotText(t, src), tmpStore(t), exec).Run(context.Background(), "run-carries-"+tc.reason, nil)
		var rtErr *RuntimeError
		if !errors.As(err, &rtErr) || rtErr.Code != tc.code || rtErr.NodeID != "assess" {
			t.Fatalf("%s: the death is not typed as expected: %v", tc.reason, err)
		}
		var d *LoopDeclined
		if !errors.As(err, &d) || d.Reason != tc.reason || d.Loop != "retry" {
			t.Fatalf("%s: the death does not carry the decline it follows: %v", tc.reason, err)
		}
	}
	// A dead end that follows no decline carries none: the node's output
	// lacks the field its edges read.
	exec := newStubExecutor()
	exec.on("check", never)
	exec.on("assess", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	err := New(compileBotText(t, spentLoopBot), tmpStore(t), exec).Run(context.Background(), "run-carries-none", nil)
	var rtErr *RuntimeError
	var d *LoopDeclined
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeNoOutgoingEdge || errors.As(err, &d) {
		t.Fatalf("a dead end that follows no decline is not said as such: %v", err)
	}
}

// A fan-out whose one branch ends: the branch's end reaches the run's error
// with its code and what a reader asks the chain for — the loop decline a
// dead end followed, the sentinel of a refusal — and the decline's warning
// is attributed to the branch.
const branchEndBot = `schema verdict:
  ok: bool

agent survey:
  model: "m"
  output: verdict

router split:
  mode: fan_out_all

agent b1:
  model: "m"
  output: verdict

judge b2:
  model: "m"
  output: verdict

agent c1:
  model: "m"
  output: verdict

fail rejected:
  code: REJECTED
  message: "the branch said no"

judge join:
  model: "m"
  output: verdict
  await: wait_all

workflow be:
  worktree: none
  sandbox: none
  entry: survey
  budget:
    max_iterations: 40
  survey -> split
  split -> b1
  split -> c1
  b1 -> b2
  b2 -> b1 when not ok as retry(unbounded 10)
  b2 -> join when ok
  c1 -> join
  join -> done
`

func TestABranchEndReachesTheRun(t *testing.T) {
	yes := func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil }
	no := func(map[string]any) (map[string]any, error) { return map[string]any{"ok": false}, nil }
	t.Run("a stall with no edge left", func(t *testing.T) {
		exec := newStubExecutor()
		exec.on("survey", yes)
		exec.on("b1", no)
		exec.on("b2", no)
		exec.on("c1", yes)
		var branchIDs []string
		eng := New(compileBotText(t, branchEndBot), tmpStore(t), exec, WithEventObserver(func(evt store.Event) {
			if evt.Type == store.EventBudgetWarning {
				branchIDs = append(branchIDs, evt.BranchID)
			}
		}))
		err := eng.Run(context.Background(), "run-branch-stall", nil)
		var rtErr *RuntimeError
		if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeNoOutgoingEdge {
			t.Fatalf("the branch's dead end is not typed on the run: %v", err)
		}
		var d *LoopDeclined
		if !errors.As(err, &d) || d.Reason != "liveness_stall" {
			t.Fatalf("the run's error does not carry the branch's decline: %v", err)
		}
		if len(branchIDs) == 0 || branchIDs[0] == "" {
			t.Fatalf("the decline raised inside a branch is not attributed to it: %v", branchIDs)
		}
	})
	t.Run("a dead end that follows no decline", func(t *testing.T) {
		exec := newStubExecutor()
		exec.on("survey", yes)
		exec.on("b1", yes)
		exec.on("b2", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
		exec.on("c1", yes)
		err := New(compileBotText(t, branchEndBot), tmpStore(t), exec).Run(context.Background(), "run-branch-dead-end", nil)
		var rtErr *RuntimeError
		var d *LoopDeclined
		if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeNoOutgoingEdge || errors.As(err, &d) {
			t.Fatalf("the branch's dead end is not typed as the trunk's: %v", err)
		}
	})
	t.Run("a refusal", func(t *testing.T) {
		src := strings.Replace(branchEndBot, "  b2 -> b1 when not ok as retry(unbounded 10)\n", "  b2 -> rejected when not ok\n", 1)
		exec := newStubExecutor()
		exec.on("survey", yes)
		exec.on("b1", yes)
		exec.on("b2", no)
		exec.on("c1", yes)
		err := New(compileBotText(t, src), tmpStore(t), exec).Run(context.Background(), "run-branch-refusal", nil)
		var rtErr *RuntimeError
		if !errors.As(err, &rtErr) || rtErr.Code != "REJECTED" || !errors.Is(err, ErrDeliberateFailure) {
			t.Fatalf("the branch's refusal is not a decision on the run: %v", err)
		}
	})
}

// The collector reads the run's code and its cause from the branches that
// failed by themselves: a sibling the fan-out stopped is not read, the
// cause needs the failed branches' agreement, and the branch named is the
// first by id — whatever order the goroutines finished in.
func TestAStoppedSiblingNeitherLaundersTheCodeNorDecidesTheCause(t *testing.T) {
	stall := func(loop string) error {
		return &RuntimeError{Code: ErrCodeNoOutgoingEdge, Cause: &LoopDeclined{Loop: loop, Reason: "liveness_stall"}}
	}
	deadEnd := &RuntimeError{Code: ErrCodeNoOutgoingEdge}
	refusal := &RuntimeError{Code: "REJECTED", Cause: ErrDeliberateFailure}
	stopped := fmt.Errorf("branch stopped: %w", context.Canceled)
	for _, tc := range []struct {
		name    string
		results []*branchResult
		code    ErrorCode
		loop    string // the loop the cause names, "" for no decline
		refused bool
	}{
		{"a stopped sibling is not read", []*branchResult{{branchID: "b", err: stopped}, {branchID: "a", err: stall("retry")}}, ErrCodeNoOutgoingEdge, "retry", false},
		{"two stalls agree, the first branch by id named", []*branchResult{{branchID: "b", err: stall("later")}, {branchID: "a", err: stall("retry")}}, ErrCodeNoOutgoingEdge, "retry", false},
		{"a decline beside a dead end is no cause", []*branchResult{{branchID: "a", err: stall("retry")}, {branchID: "b", err: deadEnd}}, ErrCodeNoOutgoingEdge, "", false},
		{"two refusals agree", []*branchResult{{branchID: "a", err: refusal}, {branchID: "b", err: refusal}}, "REJECTED", "", true},
		{"a refusal beside a stall is no cause, and no common code", []*branchResult{{branchID: "a", err: refusal}, {branchID: "b", err: stall("retry")}}, "", "", false},
		{"only stopped siblings carry nothing", []*branchResult{{branchID: "a", err: stopped}, {branchID: "b", err: stopped}}, "", "", false},
	} {
		if got := commonBranchFailureCode(tc.results); got != tc.code {
			t.Fatalf("%s: code %q, want %q", tc.name, got, tc.code)
		}
		cause := branchEndCause(tc.results)
		var d *LoopDeclined
		loop := ""
		if errors.As(cause, &d) && d != nil {
			loop = d.Loop
		}
		if loop != tc.loop || errors.Is(cause, ErrDeliberateFailure) != tc.refused {
			t.Fatalf("%s: cause %v, want loop %q refused %v", tc.name, cause, tc.loop, tc.refused)
		}
	}
}
