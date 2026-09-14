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
	admission := &store.PortLaunchAdmission{Version: store.PortLaunchAdmissionVersion, Scope: scope, StoreIdentity: record.StoreIdentity, ProofDigest: record.ProofDigest,
		CapabilityDigest: record.CapabilityDigest, ResumeDigest: ResumeCompatibilityDigest(scope), ActivationRevision: record.Revision,
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

// AdmittedChildContext carries a native parent's immutable admission into a
// child. Descendants are work already admitted by that root: disabling new
// roots or letting its short-lived proof expire must not strand them.
func AdmittedChildContext(ctx context.Context, s store.RunStore, parentID, childID, semantics string) (context.Context, error) {
	if !store.IsNativeRunID(parentID) || !store.IsNativeRunID(childID) ||
		(semantics != store.RuntimeSemanticsPortsV1 && semantics != store.RuntimeSemanticsLegacyAdapterV1) {
		return nil, fmt.Errorf("%w: native child requires a supported parent and interpreter", store.ErrPortActivation)
	}
	if err := store.ValidateRunID(childID); err != nil {
		return nil, err
	}
	parent, err := s.LoadRun(ctx, parentID)
	if err != nil || parent == nil || parent.Status != store.RunStatusRunning {
		return nil, fmt.Errorf("%w: native child has no running admitted parent", store.ErrPortActivation)
	}
	if err := RequireExistingAdmission(ctx, s, parent); err != nil {
		return nil, err
	}
	copy := *parent.PortLaunch
	return store.WithPortLaunchAdmission(ctx, &copy), nil
}

// RequireExistingAdmission admits only a run that carries the immutable
// proof observed when it was created. A rollback or proof refresh can then
// stop new launches without stranding already accepted work.
func RequireExistingAdmission(ctx context.Context, s store.RunStore, r *store.Run) error {
	return requireExistingAdmission(ctx, s, r, make(map[string]bool), 0)
}

func requireExistingAdmission(ctx context.Context, s store.RunStore, r *store.Run, seen map[string]bool, depth int) error {
	if r == nil || r.PortLaunch == nil {
		return fmt.Errorf("%w: native run has no launch admission", store.ErrPortActivation)
	}
	if depth >= 32 || seen[r.ID] || !store.IsNativeRunID(r.ID) || store.ValidateRunID(r.ID) != nil {
		return fmt.Errorf("%w: native admission lineage is invalid", store.ErrPortActivation)
	}
	seen[r.ID] = true
	a := r.PortLaunch
	if err := a.Validate(); err != nil {
		return err
	}
	scope := store.PortActivationLocal
	if s.Root() == "" {
		scope = store.PortActivationDistributed
	}
	identity, err := store.PortStoreIdentity(s)
	compatible := a.Version == store.PortLaunchAdmissionVersion && a.ResumeDigest == ResumeCompatibilityDigest(scope)
	if a.Version == 0 && ResumeCompatibilityVersion == 1 {
		// Version 0 was written with the same native run format before the
		// recovery digest existed. Its build-bound capability remains in the
		// immutable record, but an otherwise compatible upgrade may resume it.
		compatible = true
	}
	if err != nil || a.Scope != scope || a.StoreIdentity != identity || !compatible ||
		r.FormatVersion != store.NativeRunFormatVersion || r.CreatedAt.Before(a.AdmittedAt) {
		return fmt.Errorf("%w: native run admission does not match this store, runtime or creation time", store.ErrPortActivation)
	}
	if r.ParentRunID == "" || !store.IsNativeRunID(r.ParentRunID) {
		if r.RuntimeSemantics != ir.RuntimeSemanticsPortsV1 || !r.CreatedAt.Before(a.ExpiresAt) {
			return fmt.Errorf("%w: native root is not admitted under its proof", store.ErrPortActivation)
		}
		return nil
	}
	if r.RuntimeSemantics != store.RuntimeSemanticsPortsV1 && r.RuntimeSemantics != store.RuntimeSemanticsLegacyAdapterV1 {
		return fmt.Errorf("%w: native child has an unsupported interpreter", store.ErrPortActivation)
	}
	parent, err := s.LoadRun(ctx, r.ParentRunID)
	if err != nil || parent == nil || r.CreatedAt.Before(parent.CreatedAt) ||
		parent.PortLaunch == nil || !sameLaunchAdmission(a, parent.PortLaunch) {
		return fmt.Errorf("%w: native child cannot inherit its parent's admission", store.ErrPortActivation)
	}
	return requireExistingAdmission(ctx, s, parent, seen, depth+1)
}

func sameLaunchAdmission(a, b *store.PortLaunchAdmission) bool {
	return a.Version == b.Version && a.Scope == b.Scope && a.StoreIdentity == b.StoreIdentity &&
		a.ProofDigest == b.ProofDigest && a.CapabilityDigest == b.CapabilityDigest &&
		a.ResumeDigest == b.ResumeDigest && a.ActivationRevision == b.ActivationRevision &&
		a.AdmittedAt.Equal(b.AdmittedAt) && a.ExpiresAt.Equal(b.ExpiresAt)
}
