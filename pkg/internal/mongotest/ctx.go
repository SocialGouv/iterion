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
	if dl, ok := t.Deadline(); ok {
		return context.WithDeadline(context.Background(), dl.Add(-CtxMargin))
	}
	// No -timeout at all (`go test -timeout 0`): the operator asked for no
	// bound, so this context imposes none either.
	return context.WithCancel(context.Background())
}

// CtxMargin is how far ahead of the harness's own deadline Ctx expires, so a
// failure names the operation instead of arriving as a package-wide panic.
const CtxMargin = 5 * time.Second

// TeardownCtx bounds a cleanup's database drop + disconnect. Fixed and
// independent of t.Deadline() on purpose: the body may have spent the whole
// budget, and a cleanup that inherited an expired deadline would leak a
// database on every timeout.
func TeardownCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}
