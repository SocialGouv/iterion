package runtime

import (
	"errors"
	"os"
	"testing"

	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestNativeLegacyAdapterPauseResumeFilesystem(t *testing.T) {
	testNativeLegacyAdapterPauseResume(t, tmpStore)
}

func TestNativeLegacyAdapterPauseResumeMongo(t *testing.T) {
	if os.Getenv("ITERION_TEST_MONGO_URI") == "" {
		if os.Getenv("ITERION_TEST_REQUIRED") == "1" {
			t.Fatal("required adapter Mongo test needs a writable replica set")
		}
		t.Skip("ITERION_TEST_MONGO_URI not set")
	}
	testNativeLegacyAdapterPauseResume(t, portsTestMongoStore)
}

func testNativeLegacyAdapterPauseResume(t *testing.T, factory portsTestStoreFactory) {
	t.Helper()
	s := factory(t)
	activatePortsTestStore(t, s)
	ctx := portsTestContext(t)
	rootCtx, err := portsactivation.AdmittedContext(ctx, s, store.RuntimeSemanticsPortsV1, "pc1_adapter_root")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.CreateRun(store.WithRuntimeSemantics(rootCtx, store.RuntimeSemanticsPortsV1),
		"pc1_adapter_root", "native-root", nil)
	if err != nil {
		t.Fatal(err)
	}
	if root.Status == store.RunStatusQueued {
		if changed, err := s.UpdateRunStatusIf(ctx, root.ID, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusQueued}); err != nil || !changed {
			t.Fatalf("could not start native parent fixture: %v", err)
		}
	}
	if _, err := portsactivation.Disable(ctx, s); err != nil {
		t.Fatal(err)
	}
	wf := humanWorkflow()
	wf.RuntimeSemantics = store.RuntimeSemanticsLegacyAdapterV1
	exec := newStubExecutor()
	exec.on("analyze", func(map[string]any) (map[string]any, error) {
		return map[string]any{"summary": "needs review"}, nil
	})
	exec.on("integrate", func(map[string]any) (map[string]any, error) {
		return map[string]any{"result": "integrated"}, nil
	})
	pickupCtx, err := portsactivation.AdmittedChildContext(ctx, s, root.ID, "pc1_adapter_pickup", store.RuntimeSemanticsLegacyAdapterV1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AsParentedRunCreator(s).CreateChildRun(store.WithRuntimeSemantics(pickupCtx, store.RuntimeSemanticsLegacyAdapterV1),
		"pc1_adapter_pickup", "legacy-child", root.ID, nil); err != nil {
		t.Fatal(err)
	}
	pickup := New(wf, s, exec, WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
	if err := pickup.Run(ctx, "pc1_adapter_pickup", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("persisted adapter could not be picked up without a parent option: %v", err)
	}
	engine := New(wf, s, exec, WithParentRunID(root.ID), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
	if err := engine.Run(ctx, "pc1_legacy_adapter", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("legacy adapter did not preserve its human gate: %v", err)
	}
	child, err := s.LoadRun(ctx, "pc1_legacy_adapter")
	if err != nil || child.RuntimeSemantics != store.RuntimeSemanticsLegacyAdapterV1 ||
		child.ParentRunID != root.ID || child.PortExecution != nil || child.Checkpoint == nil {
		t.Fatalf("legacy adapter lost control state or native lineage: %+v %v", child, err)
	}
	if err := portsactivation.RequireExistingAdmission(ctx, s, child); err != nil {
		t.Fatalf("paused adapter lost its parent's admission: %v", err)
	}
	wrongParent := New(wf, s, exec, WithParentRunID("pc1_other_parent"))
	if err := wrongParent.Resume(ctx, child.ID, map[string]any{"approve": true}); !errors.Is(err, store.ErrRunSemantics) {
		t.Fatalf("adapter resumed under a different parent: %v", err)
	}
	resumer := New(wf, s, exec, WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
	if err := resumer.Resume(ctx, child.ID, map[string]any{"approve": true, "comment": "Ship it"}); err != nil {
		t.Fatalf("legacy adapter could not resume after rollback: %v", err)
	}
	finished, err := s.LoadRun(ctx, child.ID)
	if err != nil || finished.Status != store.RunStatusFinished || finished.PortExecution != nil {
		t.Fatalf("legacy adapter did not finish in its native namespace: %+v %v", finished, err)
	}
	if err := New(wf, s, exec).Run(ctx, "pc1_orphan_adapter", nil); !errors.Is(err, store.ErrRunSemantics) {
		t.Fatalf("standalone adapter escaped native-parent gate: %v", err)
	}
}
