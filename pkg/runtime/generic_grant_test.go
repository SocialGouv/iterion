package runtime

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

// The private adapter has no permission to acquire from a reload. Direct unit
// callers explicitly perform the same atomic claim as a public invocation.
func correctGenericWithGrant(t *testing.T, e *Engine, ctx context.Context, rs *runState, nodeID string, node ir.Node, out map[string]any) (map[string]any, error) {
	t.Helper()
	if e.genericCorrectionCapable() {
		r, err := e.store.LoadRun(ctx, rs.runID)
		if err != nil {
			t.Fatal(err)
		}
		ctx = e.beginCorrectionEntry(ctx, r)
		if _, err = e.acquireEntryExecution(ctx, r, store.ExecutionStart); err != nil {
			t.Fatal(err)
		}
		rs.ctx = ctx
		if rs.budget == nil {
			rs.budget = e.newRunState(rs.runID, nil).budget
		}
		rs.correctionAccounts, err = newCorrectionAccounts(rs.budget, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	return e.correctAndValidateNodeOutput(ctx, rs, nodeID, node, out)
}
