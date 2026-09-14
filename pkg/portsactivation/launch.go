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
	if err := requireProspectiveChildDepth(ctx, s, parent); err != nil {
		return nil, err
	}
	copy := *parent.PortLaunch
	if copy.Version == 0 {
		// A compatible older root remains authoritative, but newly written
		// child records must use the current admission representation.
		copy.Version = store.PortLaunchAdmissionVersion
		copy.ResumeDigest = ResumeCompatibilityDigest(copy.Scope)
	}
	return store.WithPortLaunchAdmission(ctx, &copy), nil
}

const maxNativeAdmissionLineageDepth = 32

// A new child must fit within the physical run tree even if an older child
// had its own independent launch proof that ended its proof lineage early.
func requireProspectiveChildDepth(ctx context.Context, s store.RunStore, parent *store.Run) error {
	seen := make(map[string]bool)
	for depth := 1; ; depth++ {
		if parent == nil || !store.IsNativeRunID(parent.ID) || seen[parent.ID] ||
			depth >= maxNativeAdmissionLineageDepth {
			return fmt.Errorf("%w: native child exceeds the supported lineage depth", store.ErrPortActivation)
		}
		seen[parent.ID] = true
		if !store.IsNativeRunID(parent.ParentRunID) {
			return nil
		}
		var err error
		parent, err = s.LoadRun(ctx, parent.ParentRunID)
		if err != nil {
			return fmt.Errorf("%w: native child has an unavailable ancestor", store.ErrPortActivation)
		}
	}
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
	if depth >= maxNativeAdmissionLineageDepth || seen[r.ID] || !store.IsNativeRunID(r.ID) || store.ValidateRunID(r.ID) != nil {
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
	// Before inherited admissions existed, every native child obtained a
	// fresh launch proof. Its own unexpired-at-creation proof is sufficient
	// even when its parent has a different activation revision or no longer
	// exists. Preserve that already accepted recovery contract.
	if r.RuntimeSemantics == ir.RuntimeSemanticsPortsV1 && r.CreatedAt.Before(a.ExpiresAt) {
		return nil
	}
	if r.ParentRunID == "" || !store.IsNativeRunID(r.ParentRunID) {
		return fmt.Errorf("%w: native run has no admitted ancestor", store.ErrPortActivation)
	}
	if r.RuntimeSemantics != store.RuntimeSemanticsPortsV1 && r.RuntimeSemantics != store.RuntimeSemanticsLegacyAdapterV1 {
		return fmt.Errorf("%w: native child has an unsupported interpreter", store.ErrPortActivation)
	}
	parent, err := s.LoadRun(ctx, r.ParentRunID)
	if err != nil || parent == nil || r.CreatedAt.Before(parent.CreatedAt) ||
		parent.PortLaunch == nil || !sameInheritedAdmission(a, parent.PortLaunch) {
		return fmt.Errorf("%w: native child cannot inherit its parent's admission", store.ErrPortActivation)
	}
	return requireExistingAdmission(ctx, s, parent, seen, depth+1)
}

func sameInheritedAdmission(child, parent *store.PortLaunchAdmission) bool {
	common := child.Scope == parent.Scope && child.StoreIdentity == parent.StoreIdentity &&
		child.ProofDigest == parent.ProofDigest && child.CapabilityDigest == parent.CapabilityDigest &&
		child.ActivationRevision == parent.ActivationRevision &&
		child.AdmittedAt.Equal(parent.AdmittedAt) && child.ExpiresAt.Equal(parent.ExpiresAt)
	return common && ((child.Version == parent.Version && child.ResumeDigest == parent.ResumeDigest) ||
		(parent.Version == 0 && child.Version == store.PortLaunchAdmissionVersion &&
			parent.ResumeDigest == "" && child.ResumeDigest == ResumeCompatibilityDigest(parent.Scope)))
}
