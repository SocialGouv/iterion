package portsactivation

import (
	"context"
	"fmt"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/queue"
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
	_, err := AuthorizeLaunch(ctx, s, semantics, runID)
	return err
}

func AuthorizeLaunch(ctx context.Context, s store.RunStore, semantics, runID string) (*store.PortLaunchAdmission, error) {
	if err := store.ValidateRunID(runID); err != nil {
		return nil, err
	}
	if semantics == "" && !store.IsNativeRunID(runID) {
		return nil, nil
	}
	if semantics != ir.RuntimeSemanticsPortsV1 || !store.IsNativeRunID(runID) {
		return nil, fmt.Errorf("%w: workflow semantics %q do not match run ID %q", store.ErrRunSemantics, semantics, runID)
	}
	scope := store.PortActivationLocal
	if s.Root() == "" {
		scope = store.PortActivationDistributed
	}
	now := time.Now().UTC()
	record, err := store.ActivePortActivationCapability(ctx, s, scope, CapabilityDigest(scope), now)
	if err != nil {
		return nil, err
	}
	if scope == store.PortActivationDistributed && record.QueueVersion != queue.SchemaVersion {
		return nil, fmt.Errorf("%w: distributed queue schema %d does not match this binary's %d", store.ErrPortActivation, record.QueueVersion, queue.SchemaVersion)
	}
	admission := &store.PortLaunchAdmission{Scope: scope, StoreIdentity: record.StoreIdentity, ProofDigest: record.ProofDigest,
		CapabilityDigest: record.CapabilityDigest, ActivationRevision: record.Revision,
		AdmittedAt: now.Truncate(time.Millisecond), ExpiresAt: record.ExpiresAt.UTC().Truncate(time.Millisecond)}
	if err := admission.Validate(); err != nil {
		return nil, err
	}
	return admission, nil
}

func AdmittedContext(ctx context.Context, s store.RunStore, semantics, runID string) (context.Context, error) {
	admission, err := AuthorizeLaunch(ctx, s, semantics, runID)
	if err != nil {
		return nil, err
	}
	if admission == nil {
		return ctx, nil
	}
	return store.WithPortLaunchAdmission(ctx, admission), nil
}

// RequireExistingAdmission admits only a run that carries the immutable
// proof observed when it was created. A rollback or proof refresh can then
// stop new launches without stranding already accepted work.
func RequireExistingAdmission(s store.RunStore, r *store.Run) error {
	if r == nil || r.PortLaunch == nil {
		return fmt.Errorf("%w: native run has no launch admission", store.ErrPortActivation)
	}
	a := r.PortLaunch
	if err := a.Validate(); err != nil {
		return err
	}
	scope := store.PortActivationLocal
	if s.Root() == "" {
		scope = store.PortActivationDistributed
	}
	identity, err := store.PortStoreIdentity(s)
	if err != nil || a.Scope != scope || a.StoreIdentity != identity || a.CapabilityDigest != CapabilityDigest(scope) ||
		r.CreatedAt.Before(a.AdmittedAt) || !r.CreatedAt.Before(a.ExpiresAt) {
		return fmt.Errorf("%w: native run admission does not match this store, binary or creation time", store.ErrPortActivation)
	}
	return nil
}
