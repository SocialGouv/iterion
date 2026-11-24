package runview

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

// Builder behavior fixtures now own preparation, exactly as their production
// caller does. Admission/refusal tests call BuildExecutor directly instead.
func preparedExecutorFixture(t *testing.T, spec ExecutorSpec) ExecutorSpec {
	t.Helper()
	ctx := spec.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	runs, ok := spec.Store.(store.RunStore)
	if !ok {
		t.Fatal("fixture requires real run store")
	}
	inv, err := store.PrepareRunInvocation(ctx, runs, store.ExistingRunInvocation{ID: spec.RunID})
	if errors.Is(err, store.ErrRunNotFound) {
		inv, err = store.PrepareRunInvocation(ctx, runs, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: spec.RunID, WorkflowName: spec.Workflow.Name, ParentRunID: spec.ParentRunID}})
	}
	if err != nil {
		t.Fatal(err)
	}
	spec.Ctx = inv.Context
	return spec
}
