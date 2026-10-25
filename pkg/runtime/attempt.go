package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func (e *Engine) recordAttempt(ctx context.Context, runID string, data map[string]any) (map[string]any, error) {
	st := store.AsAttemptStore(e.store)
	if st == nil {
		return data, nil
	}
	source := e.workflow.SourceSnapshot
	// Only the compiler can bind bytes to an IR. Re-reading FilePath here
	// would promote a later edit to authoritative history for an older IR.

	a, err := st.RecordAttempt(ctx, runID, &store.AttemptSnapshot{
		StartedAt: time.Now().UTC(), WorkflowHash: e.workflowHash,
		Source: source, Budget: e.effectiveBudgetSnapshot(),
	})
	if err != nil {
		return nil, fmt.Errorf("persist attempt provenance: %w", err)
	}
	out := make(map[string]any, len(data)+1)
	for k, v := range data {
		out[k] = v
	}
	out["attempt"] = a
	return out, nil
}
