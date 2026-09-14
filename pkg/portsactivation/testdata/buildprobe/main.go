package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
)

const runID = "pc1_compatible_build_probe"

func main() {
	if len(os.Args) != 3 {
		panic("usage: buildprobe admit|recover store-root")
	}
	ctx := context.Background()
	s, err := store.New(os.Args[2])
	if err != nil {
		panic(err)
	}
	switch os.Args[1] {
	case "admit":
		proof, err := portsactivation.ProbeLocal(ctx, s, true)
		if err != nil {
			panic(err)
		}
		if _, err := portsactivation.ActivateLocal(ctx, s, *proof); err != nil {
			panic(err)
		}
		admitted, err := portsactivation.AdmittedContext(ctx, s, ir.RuntimeSemanticsPortsV1, runID)
		if err != nil {
			panic(err)
		}
		if _, err := s.CreateRun(store.WithRuntimeSemantics(admitted, ir.RuntimeSemanticsPortsV1), runID, "accepted by first build", nil); err != nil {
			panic(err)
		}
	case "recover":
		run, err := s.LoadRun(ctx, runID)
		if err != nil {
			panic(err)
		}
		if run.PortLaunch == nil || run.PortLaunch.CapabilityDigest == portsactivation.CapabilityDigest(store.PortActivationLocal) {
			panic("fixture did not cross binary capability digests")
		}
		if err := portsactivation.RequireLaunch(ctx, s, ir.RuntimeSemanticsPortsV1, "pc1_new_build_probe"); !errors.Is(err, store.ErrPortActivation) {
			panic(fmt.Sprintf("unprobed second build launched: %v", err))
		}
		if _, err := portsactivation.Disable(ctx, s); err != nil {
			panic(err)
		}
		if err := portsactivation.RequireExistingAdmission(s, run); err != nil {
			panic(err)
		}
		fmt.Println("compatible build recovery admitted after rollback")
	default:
		panic("unknown mode")
	}
}
