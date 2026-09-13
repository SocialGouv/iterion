package portsactivation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestLaunchIdentityAndActivation(t *testing.T) {
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	legacy, err := MintRunID("")
	if err != nil || store.IsNativeRunID(legacy) || RequireLaunch(ctx, s, "", legacy) != nil {
		t.Fatalf("legacy launch changed: %q %v", legacy, err)
	}
	native, err := MintRunID(ir.RuntimeSemanticsPortsV1)
	if err != nil || !strings.HasPrefix(native, store.NativeRunIDPrefix) {
		t.Fatalf("native ID: %q %v", native, err)
	}
	for _, tc := range []struct{ semantics, id string }{
		{"", native}, {ir.RuntimeSemanticsPortsV1, legacy}, {ir.RuntimeSemanticsPortsV1, "pc2_future"},
	} {
		if err := RequireLaunch(ctx, s, tc.semantics, tc.id); !errors.Is(err, store.ErrRunSemantics) {
			t.Fatalf("identity %q/%q accepted: %v", tc.semantics, tc.id, err)
		}
	}
	if err := RequireLaunch(ctx, s, ir.RuntimeSemanticsPortsV1, native); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("default-off launch accepted: %v", err)
	}
	proof, err := ProbeLocal(ctx, s, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateLocal(ctx, s, *proof); err != nil {
		t.Fatal(err)
	}
	if err := RequireLaunch(ctx, s, ir.RuntimeSemanticsPortsV1, native); err != nil {
		t.Fatal(err)
	}
	other, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	activationBytes, err := os.ReadFile(filepath.Join(s.Root(), "port_activation_v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other.Root(), "port_activation_v1.json"), activationBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RequireLaunch(ctx, other, ir.RuntimeSemanticsPortsV1, native); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("copied activation authorized another store: %v", err)
	}
	admittedCtx, err := AdmittedContext(ctx, s, ir.RuntimeSemanticsPortsV1, native)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRun(store.WithRuntimeSemantics(admittedCtx, ir.RuntimeSemanticsPortsV1), native, "accepted", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Disable(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err := RequireLaunch(ctx, s, ir.RuntimeSemanticsPortsV1, native); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("rollback allowed native launch: %v", err)
	}
	run, err := s.LoadRun(ctx, native)
	if err != nil || run.PortLaunch == nil || RequireExistingAdmission(s, run) != nil {
		t.Fatalf("accepted run lost proof after rollback: %+v %v", run, err)
	}
	if err := RequireExistingAdmission(other, run); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("accepted run moved to another store: %v", err)
	}
	tampered := *run
	tampered.PortLaunch = nil
	if err := RequireExistingAdmission(s, &tampered); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("bare native ID counted as prior admission: %v", err)
	}
	changed := *run.PortLaunch
	changed.CapabilityDigest = strings.Repeat("0", 64)
	tampered.PortLaunch = &changed
	if err := RequireExistingAdmission(s, &tampered); !errors.Is(err, store.ErrPortActivation) {
		t.Fatalf("changed binary capability retained admission: %v", err)
	}
	if err := s.SaveRun(ctx, &tampered); !errors.Is(err, store.ErrRunSemantics) {
		t.Fatalf("persisted native admission was mutable: %v", err)
	}
}
