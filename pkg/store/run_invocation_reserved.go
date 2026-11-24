package store

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/errtrack"
)

// ReservedRunInvocation consumes an explicit reservation from the launcher's
// selected store. It is never an adapter for a legacy creator: only Publish's
// contract permits a committed birth receipt accompanying an error.
type ReservedRunInvocation struct {
	Spec        RunCreateSpec
	Reservation RunBirthReservation
}

func (ReservedRunInvocation) runInvocationPreparation() {}

func prepareReservedRunInvocation(ctx context.Context, p ReservedRunInvocation) (PreparedRunInvocation, error) {
	spec, err := p.Spec.Normalize(ctx)
	if err != nil {
		return PreparedRunInvocation{}, err
	}
	if err = CheckLegacyRunInvocationFormat(spec.Format); err != nil {
		return PreparedRunInvocation{}, err
	}
	if p.Reservation == nil {
		return PreparedRunInvocation{}, ErrRunBirthUnavailable
	}
	original, err := WithRunValueFormat(ctx, RunValueFormat{RunID: spec.ID, Format: spec.Format})
	if err != nil {
		return PreparedRunInvocation{}, err
	}
	r, publishErr := p.Reservation.Publish(original, spec)
	if r == nil {
		if publishErr != nil {
			return PreparedRunInvocation{}, publishErr
		}
		return PreparedRunInvocation{}, fmt.Errorf("%w: publication returned no receipt", ErrValueFormatCapability)
	}
	// Validate the immutable declaration even when acknowledgement failed. An
	// invalid/foreign receipt must never manufacture cleanup ownership.
	if r.ID != spec.ID || r.ParentRunID != spec.ParentRunID || r.BirthID == "" || r.ExecutionEpoch == "" || r.CASVersion < 1 || !r.Status.CanStartExecution() || spec.Status != "" && r.Status != spec.Status {
		return PreparedRunInvocation{}, errors.Join(publishErr, fmt.Errorf("%w: publication receipt differs from declared birth", ErrValueFormatChanged))
	}
	actual, err := CaptureRunValueFormat(r)
	if err != nil {
		return PreparedRunInvocation{}, errors.Join(publishErr, err)
	}
	if actual.Format != spec.Format {
		return PreparedRunInvocation{}, errors.Join(publishErr, ErrValueFormatChanged)
	}
	result := PreparedRunInvocation{Context: original, Run: r, OwnedBirth: true, birthIdentity: r.BirthID, birthID: spec.ID, birthFormat: spec.Format, birthEpoch: r.ExecutionEpoch, birthVersion: r.CASVersion, birthStatus: r.Status}
	if r.QueuedAt != nil {
		copy := *r.QueuedAt
		result.birthQueuedAt = &copy
	}
	if publishErr == nil {
		publishErr = original.Err()
	}
	if publishErr != nil {
		// Keep the exact cause graph for callers, but classify a pure publication
		// cancellation explicitly. Generic engine/retry error rules stay unchanged.
		return result, &runBirthPublicationFailure{cause: publishErr, cancellation: pureBirthCancellation(publishErr, original.Err())}
	}
	return result, nil
}

type runBirthPublicationFailure struct {
	cause        error
	cancellation error
}

func (e *runBirthPublicationFailure) Error() string { return e.cause.Error() }
func (e *runBirthPublicationFailure) Unwrap() error { return e.cause }

// IsRunBirthPublicationCancellation recognizes only the validated reserved
// preparation's marker under single-cause wrappers. Joining an independent
// cause outside that marker is not pure cancellation.
func IsRunBirthPublicationCancellation(err, target error) bool {
	if target != context.Canceled && target != context.DeadlineExceeded {
		return false
	}
	for range 128 {
		if e, ok := err.(*runBirthPublicationFailure); ok {
			return e.cancellation == target
		}
		e, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = e.Unwrap()
	}
	return false
}

// The shared cause-graph seam detects cycles and noncomparable errors. Only
// exact context leaves qualify; a custom Is method is not proof of purity.
func pureBirthCancellation(err, target error) error {
	if target != context.Canceled && target != context.DeadlineExceeded {
		return nil
	}
	if errtrack.AllLeafCauses(err, func(e error) bool { return e == target }) {
		return target
	}
	return nil
}
