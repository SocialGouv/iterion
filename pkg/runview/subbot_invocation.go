package runview

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/store"
)

// PrepareSubbotInvocation is the explicit child birth seam, after recording
// the parent relationship. It preserves cancellation/tenant values, replaces
// only the child's format intention, and never copies a model execution grant.
// Exact parents cannot silently launch a legacy child before exact birth wiring.
// The parent ID must already have its original format binding; missing or
// foreign bindings are refused without reloading or recapturing the parent.
// This helper does not grant execution authority or revalidate a later parent
// mutation; the adapter retains responsibility for its authoritative reads.
func PrepareSubbotInvocation(ctx context.Context, runs store.RunStore, parentID string, spec store.RunCreateSpec) (store.PreparedRunInvocation, error) {
	format, err := CheckSubbotBirthInvocation(ctx, parentID)
	if err != nil {
		return store.PreparedRunInvocation{}, err
	}
	spec.ParentRunID = parentID
	spec.Format = format
	childCtx, err := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: spec.ID, Format: format})
	if err != nil {
		return store.PreparedRunInvocation{}, err
	}
	childCtx = model.WithoutOutputCorrectionOwnership(model.WithExecutionUsageCollector(childCtx, nil))
	return store.PrepareRunInvocation(childCtx, runs, store.NewLegacyRunInvocation{Spec: spec})
}

// CheckSubbotBirthInvocation validates the original parent intention and the
// child birth mode without reading or writing either run. Reattachment needs
// only the original parent intention; call this after it declines to handle.
func CheckSubbotBirthInvocation(ctx context.Context, parentID string) (store.ValueFormat, error) {
	if err := store.RequireRunValueFormat(ctx, parentID); err != nil {
		return 0, err
	}
	format, err := store.ValueFormatForWrite(ctx, parentID)
	if err != nil {
		return 0, err
	}
	if err := store.CheckLegacyRunInvocationFormat(format); err != nil {
		return 0, err
	}
	return format, nil
}

// PrepareInvocation publishes this lease's reserved child before the host links
// it to its parent. ctx may carry the manager's cancellation, but must retain
// the original parent binding. Publication errors can carry an owned receipt;
// callers retire that receipt before releasing the lease.
func (l *SubbotLease) PrepareInvocation(ctx context.Context, runs store.RunStore, spec store.RunCreateSpec) (store.PreparedRunInvocation, error) {
	if l == nil || l.reservation == nil || spec.ID != l.childID {
		return store.PreparedRunInvocation{}, store.ErrRunBirthState
	}
	if err := l.Context.Err(); err != nil {
		return store.PreparedRunInvocation{}, err
	}
	format, err := CheckSubbotBirthInvocation(ctx, l.parentID)
	if err != nil {
		return store.PreparedRunInvocation{}, err
	}
	spec.ParentRunID, spec.Format = l.parentID, format
	childCtx, err := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: spec.ID, Format: format})
	if err != nil {
		return store.PreparedRunInvocation{}, err
	}
	childCtx = model.WithoutOutputCorrectionOwnership(model.WithExecutionUsageCollector(childCtx, nil))
	return store.PrepareRunInvocation(childCtx, runs, store.ReservedRunInvocation{Spec: spec, Reservation: l.reservation})
}
