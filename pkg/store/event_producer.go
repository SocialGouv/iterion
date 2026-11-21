package store

import (
	"context"
	"fmt"
	"reflect"
)

// EventAppender is the producer path itself, including its outer decorators.
type EventAppender interface {
	AppendEvent(context.Context, string, Event) (*Event, error)
}

// An owned append carries this private request to check its captured parent
// before opening payloads or invoking callbacks. It is not authorization.
type observedAppendIntentKey struct{}

// ObservedEventProducer explicitly admits an actual producer, not a capability
// discovered by unwrapping it. Implementations must reject a different outer
// receiver which merely inherits this method while overriding AppendEvent.
type ObservedEventProducer interface {
	CheckEventObservationProducer(actual EventAppender) error
}

func RequireEventObservationProducer(actual EventAppender) error {
	if actual == nil {
		return ErrRunDataCapability
	}
	v := reflect.ValueOf(actual)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return ErrRunDataCapability
		}
	}
	capable, ok := actual.(ObservedEventProducer)
	if !ok {
		return ErrRunDataCapability
	}
	return capable.CheckEventObservationProducer(actual)
}

func (s *FilesystemRunStore) CheckEventObservationProducer(actual EventAppender) error {
	receiver, ok := actual.(*FilesystemRunStore)
	if s == nil || !ok || receiver != s {
		return ErrRunDataCapability
	}
	return nil
}

// CheckEventObservationContext checks the append caller's original write intent.
// It never supplies a missing strict binding or opts an unbound writer into exact
// format. These pure checks neither reload metadata nor grant authorization.
func CheckEventObservationContext(ctx context.Context, original RunObservation) error {
	if ctx == nil {
		return ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !original.Observed() {
		return ErrRunDataCapability
	}
	if err := CheckRunDataBinding(ctx, original.Header()); err != nil {
		return err
	}
	if _, err := CheckRunValueFormat(ctx, original.Header().RunID, int(original.Header().Format)); err != nil {
		return err
	}
	if tenant, ok := TenantFromContext(ctx); ok && tenant != original.TenantID() {
		return ErrRunDataIdentityChanged
	}
	return nil
}

// AppendObservedEvent preserves the outer append path and validates its return
// before an owned caller dispatches observers. A bad producer may already have
// persisted a row: this refusal does not claim rollback of an external append.
func AppendObservedEvent(ctx context.Context, actual EventAppender, id string, event Event) (*Event, error) {
	if ctx == nil {
		return nil, ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := RequireEventObservationProducer(actual); err != nil {
		return nil, err
	}
	ctx = context.WithValue(ctx, observedAppendIntentKey{}, true)
	persisted, err := actual.AppendEvent(ctx, id, event)
	if err != nil {
		return nil, err
	}
	if persisted == nil {
		return nil, fmt.Errorf("%w: nil event from observed producer", ErrRunDataCapability)
	}
	original, ok := persisted.EventObservation()
	if !ok {
		return nil, fmt.Errorf("%w: unknown event origin", ErrRunDataCapability)
	}
	if persisted.RunID != id || original.Header().RunID != id {
		return nil, ErrRunDataIdentityChanged
	}
	if err = CheckEventObservationContext(ctx, original); err != nil {
		return nil, err
	}
	return persisted, nil
}

// ObserveEventParent captures the parent already selected by the store under its
// publication lock/transaction. A nil parent is observed historical absence.
// The caller must not use this pure constructor instead of storage selection.
func ObserveEventParent(ctx context.Context, id string, format ValueFormat, parent *Run) (RunObservation, error) {
	if ctx == nil {
		return RunObservation{}, ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return RunObservation{}, err
	}
	normalized, err := NormalizeValueFormat(int(format))
	if err != nil {
		return RunObservation{}, err
	}
	format = normalized
	tenant, _ := TenantFromContext(ctx)
	header := RunDataHeader{RunID: id, Format: format}
	if parent != nil {
		if parent.ID != id {
			return RunObservation{}, ErrRunDataIdentityChanged
		}
		parentFormat, err := NormalizeValueFormat(parent.FormatVersion)
		if err != nil {
			return RunObservation{}, err
		}
		if parentFormat != format {
			return RunObservation{}, ErrValueFormatChanged
		}
		header.Protocol, header.BirthID = parent.DataProtocol, parent.BirthID
		tenant = parent.TenantID
	}
	original, err := CaptureRunObservation(header, tenant, parent != nil)
	if err != nil {
		return RunObservation{}, err
	}
	if check, _ := ctx.Value(observedAppendIntentKey{}).(bool); check {
		if err := CheckEventObservationContext(ctx, original); err != nil {
			return RunObservation{}, err
		}
	}
	return original, nil
}

// EventProjectionFn samples invocation-owned log position and active duration.
// It must use captured resources, not select a current run by public ID. Owned
// stores invoke it at most once per append, outside retryable transactions.
type EventProjectionFn func(context.Context, RunObservation) (logOffset, activeMs int64, err error)
type eventProjectionKey struct{}

func WithEventProjection(ctx context.Context, fn EventProjectionFn) (context.Context, error) {
	if ctx == nil {
		return nil, ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if fn == nil {
		return nil, ErrRunDataCapability
	}
	return context.WithValue(ctx, eventProjectionKey{}, fn), nil
}

// ApplyEventProjection returns whether an invocation projection is installed.
// It never falls back to an ID-only callback after a projection refusal, and
// preserves explicitly supplied nonzero fields. No registry is selected here.
func ApplyEventProjection(ctx context.Context, original RunObservation, event *Event) (bool, error) {
	if ctx == nil {
		return false, ErrRunDataBindingRequired
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	fn, ok := ctx.Value(eventProjectionKey{}).(EventProjectionFn)
	if !ok {
		return false, nil
	}
	if event == nil {
		return true, ErrRunDataCapability
	}
	if err := CheckEventObservationContext(ctx, original); err != nil {
		return true, err
	}
	if event.LogOffset != 0 && event.ActiveMs != 0 {
		return true, nil
	}
	offset, active, err := fn(ctx, original)
	if err != nil {
		return true, err
	}
	if err = ctx.Err(); err != nil {
		return true, err
	}
	if event.LogOffset == 0 {
		event.LogOffset = offset
	}
	if event.ActiveMs == 0 {
		event.ActiveMs = active
	}
	return true, nil
}
