// Package mongotest supplies the contexts the Mongo-gated test suites use.
// It is test support that lives in a normal package so every gated package
// can import it (the pkg/store/storetest precedent); nothing in production
// links it.
package mongotest

import (
	"context"
	"testing"
	"time"
)

// Ctx is the context a Mongo-gated row uses for its setup AND its body.
//
// A hand-picked wall clock is the wrong bound here: it is invisible to
// `-timeout`, it does not move when the suite grows, and a runner several
// times slower reaches it while nothing is wrong. The driver then refuses the
// next operation with "calculated server-side timeout (0 ms) is less than or
// equal to 0" and the row reads as a store regression — a merge-queue ejector,
// not a finding. The package `-timeout` is the budget the operator actually
// chose, so derive from it, stopping a hair short so an expiry surfaces as
// this context's error rather than the harness's whole-package panic.
//
// Teardown contexts do NOT come from here — see TeardownCtx.
func Ctx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return ctxFor(t.Deadline())
}

// ctxFor is Ctx's decision, split out from the *testing.T so it can be
// exercised for deadlines this binary does not itself run under.
func ctxFor(dl time.Time, ok bool) (context.Context, context.CancelFunc) {
	if !ok {
		// No -timeout at all (`go test -timeout 0`): the operator asked for no
		// bound, so this context imposes none either.
		return context.WithCancel(context.Background())
	}
	return context.WithDeadline(context.Background(), dl.Add(-marginBefore(dl)))
}

// TeardownBudget is what TeardownCtx grants a cleanup, and the reason
// CtxMargin is the size it is: the failing body has to expire early enough
// that a teardown spending the WHOLE budget still returns before the harness
// deadline. A margin narrower than this budget just moves the package-wide
// panic from the body to the cleanup — the row fails at D-margin, t.Cleanup
// starts a drop against the same unresponsive server, and the harness reaches
// D mid-drop and panics naming whichever row happens to be in flight. That is
// the merge-queue-ejector shape this package exists to remove.
const TeardownBudget = 30 * time.Second

// reportingSlack is what is left after a worst-case teardown for the harness
// to print the failure and move to the next row.
const reportingSlack = 5 * time.Second

// CtxMargin is how far ahead of the harness's own deadline Ctx expires, so a
// failure names the operation instead of arriving as a package-wide panic —
// counting the cleanup that runs after it, not only the body.
const CtxMargin = TeardownBudget + reportingSlack

// marginBefore is the margin actually applied at dl. Nominally CtxMargin, but
// never more than half of what is still left: a `-timeout` shorter than the
// margin (a contributor running `-timeout 30s` by hand) would otherwise hand
// back an already-expired context and fail every row before it ran. Below the
// full margin the body and its teardown simply share the remaining room —
// there is no split that keeps both promises, and a runnable row beats a
// uniformly dead one.
func marginBefore(dl time.Time) time.Duration {
	remaining := time.Until(dl)
	if remaining <= 0 {
		return 0
	}
	if half := remaining / 2; half < CtxMargin {
		return half
	}
	return CtxMargin
}

// TeardownCtx bounds a cleanup's database drop + disconnect. Fixed and
// independent of t.Deadline() on purpose: the body may have spent the whole
// budget, and a cleanup that inherited an expired deadline would leak a
// database on every timeout. CtxMargin is what makes room for it.
func TeardownCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), TeardownBudget)
}
