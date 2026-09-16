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
	t.Run("two refusals at two fail nodes keep the decision", func(t *testing.T) {
		src := strings.Replace(branchEndBot, "  b2 -> b1 when not ok as retry(unbounded 10)\n", "  b2 -> rejected when not ok\n", 1)
		src = strings.Replace(src, "  c1 -> join\n", "  c1 -> join when ok\n  c1 -> refused when not ok\n", 1)
		src = strings.Replace(src, "judge join:", "fail refused:\n  code: REFUSED\n  message: \"the c branch said no\"\n\njudge join:", 1)
		exec := newStubExecutor()
		exec.on("survey", yes)
		exec.on("b1", yes)
		exec.on("b2", no)
		exec.on("c1", no)
		eng := New(compileBotText(t, src), tmpStore(t), exec, WithSimulation(Simulation{BranchesRunToTheirEnd: true}))
		err := eng.Run(context.Background(), "run-two-refusals", nil)
		var rtErr *RuntimeError
		if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeExecutionFailed || !errors.Is(err, ErrDeliberateFailure) {
			t.Fatalf("two refusals with codes that disagree lost the decision: %v", err)
		}
	})
	t.Run("a simulated fan-out runs every branch to its end", func(t *testing.T) {
		src := strings.Replace(branchEndBot, "  c1 -> join\n", "  c1 -> c2\n  c2 -> join\n", 1)
		src = strings.Replace(src, "fail rejected:", "judge c2:\n  model: \"m\"\n  output: verdict\n\nfail rejected:", 1)
		exec := newStubExecutor()
		exec.on("survey", yes)
		exec.on("b1", yes)
		exec.on("b2", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
		exec.on("c1", func(map[string]any) (map[string]any, error) {
			// Slow enough that the b branch's dead end lands while c is here:
			// a real run cancels c, a simulation lets it reach c2.
			time.Sleep(50 * time.Millisecond)
			return map[string]any{"ok": true}, nil
		})
		exec.on("c2", yes)
		var started []string
		eng := New(compileBotText(t, src), tmpStore(t), exec, WithSimulation(Simulation{BranchesRunToTheirEnd: true}), WithEventObserver(func(evt store.Event) {
			if evt.Type == store.EventNodeStarted {
				started = append(started, evt.NodeID)
			}
		}))
		if err := eng.Run(context.Background(), "run-branches-to-their-end", nil); err == nil {
			t.Fatal("the b branch's dead end did not fail the run")
		}
		reached := false
		for _, id := range started {
			if id == "c2" {
				reached = true
			}
		}
		if !reached {
			t.Fatalf("the sibling of a failed branch was cancelled under simulation: %v", started)
		}
	})
}

// Branch ends that read alike agree on a cause: two ceilings of different
// reasons are one ceiling; a ceiling beside a bounded cap is no cause.
func TestBranchEndsThatReadAlikeAgree(t *testing.T) {
	stall := &RuntimeError{Code: ErrCodeNoOutgoingEdge, Cause: &LoopDeclined{Loop: "review", Reason: "liveness_stall"}}
	fuel := &RuntimeError{Code: ErrCodeLoopExhausted, Cause: &LoopDeclined{Loop: "spin", Reason: "loop_out_of_fuel"}}
	cap := &RuntimeError{Code: ErrCodeLoopExhausted, Cause: &LoopDeclined{Loop: "fix", Reason: "loop_cap"}}
	for _, tc := range []struct {
		name    string
		results []*branchResult
		ceiling bool
	}{
		{"two ceilings of different reasons", []*branchResult{{branchID: "a", err: stall}, {branchID: "b", err: fuel}}, true},
		{"a ceiling beside a bounded cap", []*branchResult{{branchID: "a", err: stall}, {branchID: "b", err: cap}}, false},
		{"a bounded cap beside a ceiling", []*branchResult{{branchID: "a", err: cap}, {branchID: "b", err: stall}}, false},
	} {
		var d *LoopDeclined
		got := errors.As(branchEndCause(tc.results), &d) && d != nil && d.Ceiling()
		if got != tc.ceiling {
			t.Fatalf("%s: read as a ceiling %v, want %v", tc.name, got, tc.ceiling)
		}
	}
}

// A node carrying an unbounded loop and a bounded one, both declined at the
// same crossing: the death carries no decline whatever order the edges were
// written in — a disagreement is no cause, never a guess.
const twoLoopsBot = `schema verdict:
  ok: bool
  ready: bool

agent a0:
  model: "m"
  output: verdict

agent a1:
  model: "m"
  output: verdict

judge a2:
  model: "m"
  output: verdict

workflow tl:
  worktree: none
  sandbox: none
  entry: a1
  budget:
    max_iterations: 60
  a1 -> a2
  a2 -> a1 when not ok as outer(unbounded 2)
  a2 -> a0 when not ready as inner(3)
  a0 -> a1
  a2 -> done when ok
`

func TestTwoLoopsOfDifferentKindsAtOneNodeCarryNoDecline(t *testing.T) {
	swapped := strings.Replace(twoLoopsBot,
		"  a2 -> a1 when not ok as outer(unbounded 2)\n  a2 -> a0 when not ready as inner(3)\n",
		"  a2 -> a0 when not ready as inner(3)\n  a2 -> a1 when not ok as outer(unbounded 2)\n", 1)
	for name, src := range map[string]string{"outer first": twoLoopsBot, "inner first": swapped} {
		exec := newStubExecutor()
		never := func(map[string]any) (map[string]any, error) {
			return map[string]any{"ok": false, "ready": false, "n": time.Now().UnixNano()}, nil
		}
		exec.on("a0", never)
		exec.on("a1", never)
		exec.on("a2", never)
		err := New(compileBotText(t, src), tmpStore(t), exec).Run(context.Background(), "run-two-loops-"+strings.ReplaceAll(name, " ", "-"), nil)
		var rtErr *RuntimeError
		var d *LoopDeclined
		if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeLoopExhausted {
			t.Fatalf("%s: the spent loops did not end the run as LOOP_EXHAUSTED: %v", name, err)
		}
		if errors.As(err, &d) {
			t.Fatalf("%s: a death after declines that disagree carries one of them: %v", name, err)
		}
	}
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
