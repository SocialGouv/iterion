package expr

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The visit budget counts a combinator's ELEMENTS, so a body of thousands of
// nodes was evaluated visits × |body| times with no bound on the product —
// about an hour per evaluation at the read budget's edge (#2775). The work
// budget debits every node evaluation: this expression's 1 000 visits each
// evaluate an 18 000-node body, 18 000 000 units of work, past the default
// 2^24 — refused, and fast.
func TestEvalWorkBudgetCountsNodeEvaluations(t *testing.T) {
	var elems, body strings.Builder
	for i := 0; i < 1000; i++ {
		if i > 0 {
			elems.WriteByte(',')
		}
		elems.WriteByte('0')
	}
	for i := 0; i < 18000; i++ {
		body.WriteString("+1")
	}
	src := fmt.Sprintf("length(map([%s], x => x%s))", elems.String(), body.String())
	ast, err := Parse(src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	start := time.Now()
	_, err = ast.Eval(nil)
	el := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("18M node evaluations under 1k element visits must be refused on the work budget, got %v", err)
	}
	t.Logf("refused after %s", el.Round(time.Millisecond))
	// The wall clock is a tripwire, not the measurement: what it must catch
	// is the budget NOT biting (the full 18M-unit product evaluates for the
	// better part of an hour), so the bound tolerates a saturated CI runner
	// — under -race on a loaded ARC shard the bounded refusal measured 20 s
	// (2026-10-31 witness on 63aac5608, flake register) against 1.5 s
	// locally. Two minutes keeps a >70x margin on the regression it guards.
	if limit := 2 * time.Minute; el > limit {
		t.Errorf("refusal took %s, want under %s: the budget speaks after the work it bounds", el, limit)
	}
}

// A left-deep string `+` chain re-copies its prefix at every step while
// spending no element visit: 2 000 literals of 100 bytes copy 200 MB. The
// work budget charges the produced length of every concatenation, so the
// chain is refused after ~17 MB of copies instead of asking the host for
// gigabytes at the read budget's edge (#2775).
func TestEvalWorkBudgetChargesStringConcatenation(t *testing.T) {
	lit := "'" + strings.Repeat("a", 100) + "'"
	parts := make([]string, 2000)
	for i := range parts {
		parts[i] = lit
	}
	ast, err := Parse(strings.Join(parts, "+"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	start := time.Now()
	_, err = ast.Eval(nil)
	el := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("a 200 MB quadratic concatenation must be refused on the work budget, got %v", err)
	}
	t.Logf("refused after %s", el.Round(time.Millisecond))
	if limit := 5 * time.Second; el > limit {
		t.Errorf("refusal took %s, want under %s: the chain must die before its quadratic copy", el, limit)
	}
}

// The operator's knob: a small count refuses a modest evaluation, 0 removes
// the budget (the escape hatch a load-bearing bound owes).
func TestEvalWorkBudgetIsTheOperators(t *testing.T) {
	t.Setenv(EvalEnv, "100")
	if _, err := MustParse("length(map([1,2,3,4,5,6,7,8,9,10], x => x + x + x + x + x))").Eval(nil); err == nil ||
		!strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("60 node evaluations on a budget of 100 must be refused, got %v", err)
	}
	if _, err := MustParse("1 + 1").Eval(nil); err != nil {
		t.Fatalf("3 node evaluations on a budget of 100: %v", err)
	}
	t.Setenv(EvalEnv, "0")
	if _, err := MustParse("length(map([1,2,3,4,5,6,7,8,9,10], x => x + x + x + x + x))").Eval(nil); err != nil {
		t.Fatalf("a removed budget refuses nothing: %v", err)
	}
}

// The reviewer's probe of the first cut: the builtin charge was post-hoc —
// evalFuncCall built the whole result and THEN debited its length, so
// join(map(...)) allocated hundreds of MiB the budget would have refused,
// past any recover. join now charges each part BEFORE writing it: the 200 MB
// result is refused after ~17 MB of writes.
func TestEvalWorkBudgetRefusesAGiantJoinBeforeBuildingIt(t *testing.T) {
	big := strings.Repeat("a", 1<<20) // a 1 MiB element, allocated once, outside
	ctx := &Context{Vars: func(path []string) any { return big }}
	var elems strings.Builder
	for i := 0; i < 200; i++ {
		if i > 0 {
			elems.WriteByte(',')
		}
		elems.WriteByte('0')
	}
	ast, err := Parse("join(map([" + elems.String() + "], x => vars.big), '')")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err = ast.Eval(ctx)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("a 200 MB join must be refused on the work budget, got %v", err)
	}
	t.Logf("refused with %d MiB allocated", alloc>>20)
	if limit := uint64(128 << 20); alloc > limit {
		t.Errorf("the refused join allocated %d MiB, want under %d MiB: the charge must land BEFORE the result is built", alloc>>20, limit>>20)
	}
}

// A cancelled context interrupts the evaluation itself — the run's
// exec-timeout lands during a pathological expression, not at its end.
func TestEvalContextInterruptsTheEvaluation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := MustParse("1 + 1").EvalContext(ctx, nil)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("a cancelled context must interrupt the evaluation, got %v", err)
	}
}
