package cli

import (
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestNativeCLIStartKeepsAcceptedRunAfterRollback(t *testing.T) {
	ctx := t.Context()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := portsactivation.ProbeLocal(ctx, s, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := portsactivation.ActivateLocal(ctx, s, *proof); err != nil {
		t.Fatal(err)
	}
	rootCtx, err := portsactivation.AdmittedContext(ctx, s, store.RuntimeSemanticsPortsV1, "pc1_cli_accepted")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(store.WithRuntimeSemantics(rootCtx, store.RuntimeSemanticsPortsV1),
		"pc1_cli_accepted", "accepted", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := portsactivation.Disable(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := requireNativeCLIStart(ctx, s, store.RuntimeSemanticsPortsV1, "pc1_cli_accepted"); err != nil {
		t.Fatalf("accepted run was stranded by rollback: %v", err)
	}
	if err := requireNativeCLIStart(ctx, s, store.RuntimeSemanticsPortsV1, "pc1_cli_new_root"); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("new root bypassed rollback: %v", err)
	}
	if err := requireNativeCLIStart(ctx, s, store.RuntimeSemanticsLegacyAdapterV1, "pc1_cli_accepted"); !errors.Is(err, store.ErrRunSemantics) {
		t.Fatalf("accepted run changed interpreter: %v", err)
	}
}
