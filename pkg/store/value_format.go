package store

import (
	"context"
	"errors"
	"fmt"
)

// ValueFormat is the immutable value contract of one durable run. It is
// independent of execution authority, queue versions and Mongo schema versions.
type ValueFormat int

const (
	LegacyValueFormat ValueFormat = 1
	ExactValueFormat  ValueFormat = 2
)

var (
	ErrUnsupportedValueFormat = errors.New("unsupported run value format")
	ErrValueFormatChanged     = errors.New("run value format changed")
	ErrInvalidValueFormat     = errors.New("invalid run value format")
	ErrValueFormatCapability  = errors.New("run value format capability unavailable")
)

// NormalizeValueFormat recognizes the absent/zero legacy header without
// promoting it. Unknown versions are never treated as legacy.
func NormalizeValueFormat(raw int) (ValueFormat, error) {
	switch raw {
	case 0, 1:
		return LegacyValueFormat, nil
	case 2:
		return ExactValueFormat, nil
	}
	return 0, fmt.Errorf("%w: %d", ErrUnsupportedValueFormat, raw)
}

// RunValueFormat is an immutable copy of an invocation's original format.
// It is an intent checked against the durable run, not an execution grant.
type RunValueFormat struct {
	RunID  string
	Format ValueFormat
}
type runValueFormatKey struct{}

func (b RunValueFormat) validate() (RunValueFormat, error) {
	if b.RunID == "" {
		return b, fmt.Errorf("%w: empty run ID", ErrInvalidValueFormat)
	}
	f, err := NormalizeValueFormat(int(b.Format))
	if err != nil {
		return b, err
	}
	b.Format = f
	return b, nil
}

// CaptureRunValueFormat must happen before delayed work. A CAS retry retains
// this value instead of capturing the format of a later run with the same ID.
func CaptureRunValueFormat(r *Run) (RunValueFormat, error) {
	if r == nil {
		return RunValueFormat{}, fmt.Errorf("%w: nil run", ErrInvalidValueFormat)
	}
	return (RunValueFormat{RunID: r.ID, Format: ValueFormat(r.FormatVersion)}).validate()
}

// WithRunValueFormat copies and validates b. Child invocations must bind their
// own run ID explicitly; contexts never infer inheritance from a payload.
func WithRunValueFormat(ctx context.Context, b RunValueFormat) (context.Context, error) {
	b, err := b.validate()
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidValueFormat)
	}
	return context.WithValue(ctx, runValueFormatKey{}, b), nil
}

// ValueFormatForWrite returns a granular payload writer's original intent.
// Missing bindings mean legacy, preserving existing Go callers without ever
// adopting exact mode from the current run or a value's concrete type.
func ValueFormatForWrite(ctx context.Context, runID string) (ValueFormat, error) {
	if ctx == nil {
		return 0, fmt.Errorf("%w: nil context", ErrInvalidValueFormat)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	b, ok := ctx.Value(runValueFormatKey{}).(RunValueFormat)
	if !ok {
		return LegacyValueFormat, nil
	}
	if b.RunID != runID {
		return 0, fmt.Errorf("%w: bound run %q differs from %q", ErrValueFormatChanged, b.RunID, runID)
	}
	return b.Format, nil
}

// CheckRunValueFormat compares a granular writer's captured intent against a
// durable header. The store must hold its publication lock/CAS when calling it.
func CheckRunValueFormat(ctx context.Context, runID string, current int) (ValueFormat, error) {
	original, err := ValueFormatForWrite(ctx, runID)
	if err != nil {
		return 0, err
	}
	actual, err := NormalizeValueFormat(current)
	if err != nil {
		return 0, err
	}
	if original != actual {
		return 0, fmt.Errorf("%w: original %d, current %d", ErrValueFormatChanged, original, actual)
	}
	return actual, nil
}

// CheckRunFormatReplacement guards a whole-document snapshot. Unlike a new
// granular payload, the snapshot already carries its format, so an unbound
// metadata save preserves exact runs. The returned raw header preserves legacy
// zero as well as one. The check is pure; refusal never mutates either run.
func CheckRunFormatReplacement(ctx context.Context, current, incoming *Run) (int, error) {
	if ctx == nil {
		return 0, fmt.Errorf("%w: nil context", ErrInvalidValueFormat)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if incoming != nil {
		if err := RequireRunDataStorage(incoming.DataProtocol); err != nil {
			return 0, err
		}
	}
	if current != nil {
		if err := RequireRunDataStorage(current.DataProtocol); err != nil {
			return 0, err
		}
	}
	proposed, err := CaptureRunValueFormat(incoming)
	if err != nil {
		return 0, err
	}
	if _, bound := ctx.Value(runValueFormatKey{}).(RunValueFormat); bound {
		original, err := ValueFormatForWrite(ctx, incoming.ID)
		if err != nil {
			return 0, err
		}
		if original != proposed.Format {
			return 0, fmt.Errorf("%w: snapshot differs from binding", ErrValueFormatChanged)
		}
	}
	if current == nil {
		if proposed.Format == ExactValueFormat {
			return 0, fmt.Errorf("%w: exact birth requires RunValueFormatCreator", ErrValueFormatCapability)
		}
		return incoming.FormatVersion, nil
	}
	durable, err := CaptureRunValueFormat(current)
	if err != nil {
		return 0, err
	}
	if durable.RunID != proposed.RunID || durable.Format != proposed.Format {
		return 0, fmt.Errorf("%w: snapshot %d, durable %d", ErrValueFormatChanged, proposed.Format, durable.Format)
	}
	return current.FormatVersion, nil
}

// BindRunValueFormat admits a loaded or newly created run into an invocation.
// An absent binding captures this run's immutable format. An existing binding,
// including legacy, is compared and retained: a reload cannot adopt a successor.
// This is a format intention only, never execution or creation authority.
func BindRunValueFormat(ctx context.Context, run *Run) (context.Context, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrInvalidValueFormat)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	binding, err := CaptureRunValueFormat(run)
	if err != nil {
		return nil, err
	}
	if _, bound := ctx.Value(runValueFormatKey{}).(RunValueFormat); bound {
		if _, err := CheckRunValueFormat(ctx, run.ID, run.FormatVersion); err != nil {
			return nil, err
		}
		return ctx, nil
	}
	return WithRunValueFormat(ctx, binding)
}

// LoadRunWithValueFormat loads an existing run and binds its original format.
// It never creates a run or converts a load error into absence. Both sides of
// LoadRun check cancellation, including stores whose reads ignore the context.
// Callers retain the returned context through retries and delayed operations.
func LoadRunWithValueFormat(ctx context.Context, runs interface {
	LoadRun(context.Context, string) (*Run, error)
}, id string) (context.Context, *Run, error) {
	if _, err := ValueFormatForWrite(ctx, id); err != nil {
		return nil, nil, err
	}
	if runs == nil {
		return nil, nil, fmt.Errorf("%w: nil run store", ErrValueFormatCapability)
	}
	run, err := runs.LoadRun(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, nil, ctxErr
	}
	if run == nil || run.ID != id {
		return nil, nil, fmt.Errorf("%w: loaded run differs from %q", ErrValueFormatChanged, id)
	}
	bound, err := BindRunValueFormat(ctx, run)
	if err != nil {
		return nil, nil, err
	}
	return bound, run, nil
}

// IsValueFormatRefusal identifies an invocation that cannot address the current
// row. Such a refusal must not be attributed as a failure to that successor.
func IsValueFormatRefusal(err error) bool {
	return errors.Is(err, ErrValueFormatChanged) || errors.Is(err, ErrUnsupportedValueFormat) || errors.Is(err, ErrInvalidValueFormat) || errors.Is(err, ErrValueFormatCapability)
}
