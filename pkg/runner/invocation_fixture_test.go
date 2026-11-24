package runner

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
)

func preparedRunnerFixture(t *testing.T, ctx context.Context, r *Runner, id string) context.Context {
	t.Helper()
	inv, err := store.PrepareRunInvocation(ctx, r.cfg.Store, store.ExistingRunInvocation{ID: id})
	if errors.Is(err, store.ErrRunNotFound) {
		inv, err = store.PrepareRunInvocation(ctx, r.cfg.Store, store.NewLegacyRunInvocation{Spec: store.RunCreateSpec{ID: id, WorkflowName: "wf"}})
	}
	if err != nil {
		t.Fatal(err)
	}
	return inv.Context
}
