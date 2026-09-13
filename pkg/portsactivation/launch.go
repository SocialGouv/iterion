package portsactivation

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// MintRunID reserves a distinct ID family for the native interpreter while
// leaving every legacy launch ID unchanged.
func MintRunID(semantics string) (string, error) {
	id, err := store.GenerateRunID()
	if err != nil {
		return "", err
	}
	switch semantics {
	case "":
		return id, nil
	case ir.RuntimeSemanticsPortsV1:
		return store.NativeRunIDPrefix + id, nil
	default:
		return "", fmt.Errorf("%w: unsupported runtime semantics %q", store.ErrRunSemantics, semantics)
	}
}

// RequireLaunch checks semantic identity and a current activation before
// launch surfaces create docs, queue messages, logs or executors. The Engine
// repeats the gate immediately before it claims or creates a run.
func RequireLaunch(ctx context.Context, s store.RunStore, semantics, runID string) error {
	if err := store.ValidateRunID(runID); err != nil {
		return err
	}
	if semantics == "" && !store.IsNativeRunID(runID) {
		return nil
	}
	if semantics != ir.RuntimeSemanticsPortsV1 || !store.IsNativeRunID(runID) {
		return fmt.Errorf("%w: workflow semantics %q do not match run ID %q", store.ErrRunSemantics, semantics, runID)
	}
	scope := store.PortActivationLocal
	if s.Root() == "" {
		scope = store.PortActivationDistributed
	}
	return store.RequirePortActivationCapability(ctx, s, scope, CapabilityDigest(scope), time.Now())
}
