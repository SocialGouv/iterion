package portsactivation

import (
	"errors"
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
