package portsactivation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestNativeChildInheritsRootAdmissionAcrossRollbackAndExpiry(t *testing.T) {
	ctx := t.Context()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := ProbeLocal(ctx, s, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateLocal(ctx, s, *proof); err != nil {
		t.Fatal(err)
	}
	rootCtx, err := AdmittedContext(ctx, s, ir.RuntimeSemanticsPortsV1, "pc1_root")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.CreateRun(store.WithRuntimeSemantics(rootCtx, ir.RuntimeSemanticsPortsV1), "pc1_root", "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Disable(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := AdmittedContext(ctx, s, ir.RuntimeSemanticsPortsV1, "pc1_new_root"); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("new root bypassed rollback: %v", err)
	}
	childCtx, err := AdmittedChildContext(ctx, s, root.ID, "pc1_adapter", store.RuntimeSemanticsLegacyAdapterV1)
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateChildRun(store.WithRuntimeSemantics(childCtx, store.RuntimeSemanticsLegacyAdapterV1),
		"pc1_adapter", "legacy-child", root.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireExistingAdmission(ctx, s, child); err != nil {
		t.Fatalf("accepted descendant was stranded by rollback: %v", err)
	}
	late := *child
	late.CreatedAt = child.PortLaunch.ExpiresAt.Add(time.Second)
	if err := RequireExistingAdmission(ctx, s, &late); err != nil {
		t.Fatalf("accepted descendant was stranded by proof expiry: %v", err)
	}
	lateRoot := *root
	lateRoot.CreatedAt = root.PortLaunch.ExpiresAt.Add(time.Second)
	if err := RequireExistingAdmission(ctx, s, &lateRoot); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("late root inherited an expired admission: %v", err)
	}
}

func TestNativeIndependentlyAdmittedChildKeepsRecoveryAfterUpgrade(t *testing.T) {
	ctx := t.Context()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := ProbeLocal(ctx, s, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateLocal(ctx, s, *proof); err != nil {
		t.Fatal(err)
	}
	rootCtx, err := AdmittedContext(ctx, s, ir.RuntimeSemanticsPortsV1, "pc1_independent_root")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.CreateRun(store.WithRuntimeSemantics(rootCtx, ir.RuntimeSemanticsPortsV1),
		"pc1_independent_root", "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Older launchers admitted a native subbot independently; its proof has
	// its own observation time rather than a copy of its parent's admission.
	time.Sleep(2 * time.Millisecond)
	childCtx, err := AdmittedContext(ctx, s, ir.RuntimeSemanticsPortsV1, "pc1_independent_child")
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateChildRun(store.WithRuntimeSemantics(childCtx, ir.RuntimeSemanticsPortsV1),
		"pc1_independent_child", "child", root.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sameInheritedAdmission(child.PortLaunch, root.PortLaunch) {
		t.Fatal("fixture accidentally reused the parent's admission")
	}
	if _, err := Disable(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := RequireExistingAdmission(ctx, s, child); err != nil {
		t.Fatalf("older independently admitted child was stranded: %v", err)
	}
}

func TestNativeChildOfCompatibleVersionZeroRootIsCreated(t *testing.T) {
	ctx := t.Context()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := ProbeLocal(ctx, s, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateLocal(ctx, s, *proof); err != nil {
		t.Fatal(err)
	}
	rootCtx, err := AdmittedContext(ctx, s, ir.RuntimeSemanticsPortsV1, "pc1_v0_root")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.CreateRun(store.WithRuntimeSemantics(rootCtx, ir.RuntimeSemanticsPortsV1),
		"pc1_v0_root", "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce the on-disk admission shape written before the recovery
	// digest existed. Store writes deliberately cannot downgrade a record.
	path := filepath.Join(s.Root(), store.NativeRunsDirectory, root.ID, "run.ports-v1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	admission := document["port_launch"].(map[string]any)
	delete(admission, "admission_version")
	delete(admission, "resume_digest")
	raw, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	root, err = s.LoadRun(ctx, root.ID)
	if err != nil || root.PortLaunch.Version != 0 {
		t.Fatalf("version-zero root fixture is invalid: %+v %v", root, err)
	}
	childCtx, err := AdmittedChildContext(ctx, s, root.ID, "pc1_v0_adapter", store.RuntimeSemanticsLegacyAdapterV1)
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateChildRun(store.WithRuntimeSemantics(childCtx, store.RuntimeSemanticsLegacyAdapterV1),
		"pc1_v0_adapter", "adapter", root.ID, nil)
	if err != nil || child.PortLaunch.Version != store.PortLaunchAdmissionVersion {
		t.Fatalf("version-zero parent could not create a current-format child: %+v %v", child, err)
	}
	if _, err := Disable(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := RequireExistingAdmission(ctx, s, child); err != nil {
		t.Fatalf("upgraded child lost its version-zero parent: %v", err)
	}
}

func TestNativeChildDepthIsCheckedBeforeCreation(t *testing.T) {
	ctx := t.Context()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := ProbeLocal(ctx, s, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateLocal(ctx, s, *proof); err != nil {
		t.Fatal(err)
	}
	rootCtx, err := AdmittedContext(ctx, s, ir.RuntimeSemanticsPortsV1, "pc1_depth_00")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := s.CreateRun(store.WithRuntimeSemantics(rootCtx, ir.RuntimeSemanticsPortsV1),
		"pc1_depth_00", "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	for depth := 1; depth < maxNativeAdmissionLineageDepth; depth++ {
		id := fmt.Sprintf("pc1_depth_%02d", depth)
		childCtx, err := AdmittedChildContext(ctx, s, parent.ID, id, ir.RuntimeSemanticsPortsV1)
		if err != nil {
			t.Fatalf("depth %d was denied early: %v", depth, err)
		}
		parent, err = s.CreateChildRun(store.WithRuntimeSemantics(childCtx, ir.RuntimeSemanticsPortsV1),
			id, "child", parent.ID, nil)
		if err != nil {
			t.Fatalf("depth %d could not be created: %v", depth, err)
		}
	}
	if err := RequireExistingAdmission(ctx, s, parent); err != nil {
		t.Fatalf("last supported child cannot be recovered: %v", err)
	}
	if _, err := AdmittedChildContext(ctx, s, parent.ID, "pc1_depth_32", ir.RuntimeSemanticsPortsV1); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("unrecoverable 33rd record was admitted: %v", err)
	}
	if _, err := s.LoadRun(ctx, "pc1_depth_32"); !errors.Is(err, store.ErrRunNotFound) {
		t.Fatalf("denied child was persisted: %v", err)
	}
}

func TestNativeChildAdmissionRefusesBrokenLineage(t *testing.T) {
	ctx := t.Context()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	proof, err := ProbeLocal(ctx, s, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateLocal(ctx, s, *proof); err != nil {
		t.Fatal(err)
	}
	rootCtx, err := AdmittedContext(ctx, s, ir.RuntimeSemanticsPortsV1, "pc1_root")
	if err != nil {
		t.Fatal(err)
	}
	root, err := s.CreateRun(store.WithRuntimeSemantics(rootCtx, ir.RuntimeSemanticsPortsV1), "pc1_root", "root", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ parent, child, semantics string }{
		{"legacy_root", "pc1_child", store.RuntimeSemanticsLegacyAdapterV1},
		{root.ID, "legacy_child", store.RuntimeSemanticsLegacyAdapterV1},
		{root.ID, "pc1_child", ""},
	} {
		if _, err := AdmittedChildContext(ctx, s, tc.parent, tc.child, tc.semantics); !errors.Is(err, store.ErrPortActivation) {
			t.Fatalf("unsupported native child %q/%q was admitted: %v", tc.parent, tc.child, err)
		}
	}
	childCtx, err := AdmittedChildContext(ctx, s, root.ID, "pc1_adapter", store.RuntimeSemanticsLegacyAdapterV1)
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.CreateChildRun(store.WithRuntimeSemantics(childCtx, store.RuntimeSemanticsLegacyAdapterV1),
		"pc1_adapter", "legacy-child", root.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	changed := *child
	badProof := *child.PortLaunch
	badProof.ProofDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	changed.PortLaunch = &badProof
	if err := RequireExistingAdmission(ctx, s, &changed); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("child changed its parent's proof: %v", err)
	}
	changed = *child
	changed.ParentRunID = "pc1_missing"
	if err := RequireExistingAdmission(ctx, s, &changed); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("child without a parent retained proof: %v", err)
	}
}
