package runview

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestForkAndRewindRefuseUnadmittedNativeStateBeforeMutation(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	const id = "pc1_native_edit_safety"
	ctx := store.WithRuntimeSemantics(context.Background(), store.RuntimeSemanticsPortsV1)
	if _, err := st.CreateRun(ctx, id, "native", nil); err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Fork(context.Background(), ForkSpec{RunID: id, NodeID: "node"}); !errors.Is(err, store.ErrRunSemantics) {
		t.Fatalf("legacy turn-based fork accepted native run: %v", err)
	}
	if _, err := svc.Rewind(context.Background(), RewindSpec{RunID: id, NodeID: "node"}); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("native checkpoint rewind accepted unadmitted run: %v", err)
	}
	ids, err := st.ListRuns(context.Background())
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("native edit created or removed a run: %v %v", ids, err)
	}
	run, err := st.LoadRun(context.Background(), id)
	if err != nil || run.Status != store.RunStatusRunning || run.RuntimeSemantics != store.RuntimeSemanticsPortsV1 {
		t.Fatalf("native edit changed the parent: %+v %v", run, err)
	}
}
