package store

import (
	"context"
	"fmt"
	"time"
)

// RunInvocationPreparation makes birth versus existing pickup explicit. A
// missing existing run and a duplicate birth are errors, never interchangeable.
type RunInvocationPreparation interface{ runInvocationPreparation() }
type ExistingRunInvocation struct{ ID string }

func (ExistingRunInvocation) runInvocationPreparation() {}

// NewLegacyRunInvocation prepares the historical launch format only. Exact
// births need the complete transactional launch preimage, not this local port.
type NewLegacyRunInvocation struct{ Spec RunCreateSpec }

func (NewLegacyRunInvocation) runInvocationPreparation() {}

// PreparedRunInvocation belongs to one caller. OwnedBirth records only a
// successful exclusive create by that caller; it grants no execution authority.
// Context must be carried through the builder and Engine.Run/Resume unchanged
// except for additive context values. Reloads compare its original binding.
type PreparedRunInvocation struct {
	Context       context.Context
	Run           *Run
	OwnedBirth    bool
	birthIdentity string
	birthID       string
	birthFormat   ValueFormat
	birthEpoch    string
	birthVersion  int64
	birthStatus   RunStatus
	birthQueuedAt *time.Time
}

func PrepareRunInvocation(ctx context.Context, runs RunStore, preparation RunInvocationPreparation) (PreparedRunInvocation, error) {
	switch p := preparation.(type) {
	case ExistingRunInvocation:
		bound, r, err := LoadRunWithValueFormat(ctx, runs, p.ID)
		if err != nil {
			return PreparedRunInvocation{}, err
		}
		return PreparedRunInvocation{Context: bound, Run: r}, nil
	case ReservedRunInvocation:
		return prepareReservedRunInvocation(ctx, p)
	case NewLegacyRunInvocation:
		spec, err := p.Spec.Normalize(ctx)
		if err != nil {
			return PreparedRunInvocation{}, err
		}
		if err := CheckLegacyRunInvocationFormat(spec.Format); err != nil {
			return PreparedRunInvocation{}, err
		}
		original, err := WithRunValueFormat(ctx, RunValueFormat{RunID: spec.ID, Format: spec.Format})
		if err != nil {
			return PreparedRunInvocation{}, err
		}
		r, err := CreateRunWithValueFormat(original, runs, spec)
		if err != nil {
			return PreparedRunInvocation{}, err
		}
		if r == nil || r.ID != spec.ID {
			return PreparedRunInvocation{}, fmt.Errorf("%w: created run differs from %q", ErrValueFormatChanged, spec.ID)
		}
		// The declaration was captured before birth, not from a later read.
		// Verify the creator's response even if cancellation just arrived.
		actual, err := CaptureRunValueFormat(r)
		if err != nil {
			return PreparedRunInvocation{}, err
		}
		if actual.Format != spec.Format {
			return PreparedRunInvocation{}, fmt.Errorf("%w: creator returned a different format", ErrValueFormatChanged)
		}
		result := PreparedRunInvocation{Context: original, Run: r, OwnedBirth: true, birthIdentity: r.BirthID, birthID: spec.ID, birthFormat: spec.Format, birthEpoch: r.ExecutionEpoch, birthVersion: r.CASVersion, birthStatus: r.Status}
		if r.QueuedAt != nil {
			copy := *r.QueuedAt
			result.birthQueuedAt = &copy
		}
		return result, original.Err()

	default:
		return PreparedRunInvocation{}, fmt.Errorf("%w: missing invocation preparation", ErrInvalidValueFormat)
	}
}

// RequireRunValueFormat distinguishes a captured legacy invocation from an
// unbound legacy API caller. It validates intention only, never existence or
// execution authority; the caller must still load and compare the durable run.
func RequireRunValueFormat(ctx context.Context, id string) error {
	if _, err := ValueFormatForWrite(ctx, id); err != nil {
		return err
	}
	if _, bound := ctx.Value(runValueFormatKey{}).(RunValueFormat); !bound {
		return fmt.Errorf("%w: invocation has no original format binding", ErrValueFormatCapability)
	}
	return nil
}

// OriginalBirth is the immutable successful-create observation, not a grant.
// Editing Run or public OwnedBirth cannot manufacture or change this snapshot.
func (p PreparedRunInvocation) OriginalBirth() (RunValueFormat, RunExecutionExpectation, bool) {
	if !p.OwnedBirth || p.birthID == "" || p.birthEpoch == "" {
		return RunValueFormat{}, RunExecutionExpectation{}, false
	}
	expected := RunExecutionExpectation{Version: p.birthVersion, Epoch: p.birthEpoch, Status: p.birthStatus}
	if p.birthQueuedAt != nil {
		copy := *p.birthQueuedAt
		expected.QueuedAt = &copy
	}
	return RunValueFormat{RunID: p.birthID, Format: p.birthFormat}, expected, true
}

// CheckLegacyRunInvocationFormat checks only the format supported by the
// NewLegacyRunInvocation mode. It grants no authority and performs no store IO.
// Callers may use it before publishing a child relationship; preparation keeps
// the same policy after normalizing the complete birth specification.
func CheckLegacyRunInvocationFormat(format ValueFormat) error {
	normalized, err := NormalizeValueFormat(int(format))
	if err != nil {
		return err
	}
	if normalized != LegacyValueFormat {
		return fmt.Errorf("%w: exact invocation birth is not connected", ErrValueFormatCapability)
	}
	return nil
}

// OriginalBirthID retains the immutable store-owned incarnation from the
// original create receipt, independently of the mutable public Run snapshot.
// Historical creators without a BirthID cannot supply a strong child reference.
func (p PreparedRunInvocation) OriginalBirthID() (string, bool) {
	_, _, owned := p.OriginalBirth()
	return p.birthIdentity, owned && p.birthIdentity != ""
}
