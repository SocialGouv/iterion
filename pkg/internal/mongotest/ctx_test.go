package mongotest

import (
	"testing"
	"time"
)

// TestCtxMarginCoversTheTeardown pins the relation the two budgets have to
// keep. A body that expires at D-CtxMargin is followed by a cleanup allowed
// TeardownBudget; if the margin is the narrower of the two, the harness
// deadline D lands mid-teardown and the whole package panics naming an
// unrelated row — the ejector shape this package removes, reintroduced
// through the cleanup path. Cheap to state, and the only thing standing
// between a future "30s is plenty" edit and a red merge queue.
func TestCtxMarginCoversTheTeardown(t *testing.T) {
	t.Parallel()
	if CtxMargin < TeardownBudget {
		t.Fatalf("CtxMargin (%s) is narrower than TeardownBudget (%s): a cleanup starting at the body's expiry runs %s past the harness deadline",
			CtxMargin, TeardownBudget, TeardownBudget-CtxMargin)
	}
	if CtxMargin <= TeardownBudget {
		t.Errorf("CtxMargin (%s) leaves no slack past TeardownBudget (%s) for the harness to report the failure", CtxMargin, TeardownBudget)
	}
}

// TestCtxSurvivesADeadlineShorterThanTheMargin covers the hand-run case:
// `go test -timeout 30s` against a package whose margin is wider than that.
// Applying the full margin would return a context already past its deadline,
// so every row would fail before it ran a single operation.
func TestCtxSurvivesADeadlineShorterThanTheMargin(t *testing.T) {
	t.Parallel()
	for _, remaining := range []time.Duration{time.Second, 10 * time.Second, CtxMargin, 4 * CtxMargin} {
		got := marginBefore(time.Now().Add(remaining))
		if got >= remaining {
			t.Errorf("marginBefore(now+%s) = %s: the body would get no budget at all", remaining, got)
		}
		if got > CtxMargin {
			t.Errorf("marginBefore(now+%s) = %s: never wider than CtxMargin (%s)", remaining, got, CtxMargin)
		}
	}
	// Past the deadline there is nothing left to reserve; the context is
	// expired either way, and a negative margin would push it INTO the future.
	if got := marginBefore(time.Now().Add(-time.Minute)); got != 0 {
		t.Errorf("marginBefore(past) = %s, want 0", got)
	}
}

// TestCtxForNoDeadlineImposesNone keeps `go test -timeout 0` meaning what it
// says: the operator asked for no bound, so the row carries none. This binary
// runs under a -timeout of its own, so the branch is driven through ctxFor.
func TestCtxForNoDeadlineImposesNone(t *testing.T) {
	t.Parallel()
	ctx, cancel := ctxFor(time.Time{}, false)
	defer cancel()
	if dl, ok := ctx.Deadline(); ok {
		t.Errorf("no harness deadline, yet the row carries one: %s", dl)
	}
}

// TestCtxForShortensAgainstTheHarnessDeadline is the end-to-end shape: a
// generous -timeout yields a context expiring exactly CtxMargin early, which
// is what leaves the teardown its room.
func TestCtxForShortensAgainstTheHarnessDeadline(t *testing.T) {
	t.Parallel()
	harness := time.Now().Add(10 * time.Minute)
	ctx, cancel := ctxFor(harness, true)
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("a harness deadline was given, yet the row carries none")
	}
	if got := harness.Sub(dl); got != CtxMargin {
		t.Errorf("row expires %s before the harness deadline, want CtxMargin (%s)", got, CtxMargin)
	}
}
