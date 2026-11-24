package store

import (
	"fmt"
	"reflect"
)

// BoundObservationStore explicitly endorses the actual outer RunStore a bound
// stream reads through (BindRunObservation): every read it serves hands the
// caller's context to the comparison of the binding that context carries, and
// answers that comparison's refusal. Implementations must reject a different
// outer receiver, so a wrapper that merely inherits this method through
// embedding while overriding a read is not admitted. A wrapper implements the
// contract itself, and only when it forwards the context unchanged to the
// store it endorses.
type BoundObservationStore interface {
	CheckBoundObservationStore(actual RunStore) error
}

// RequireBoundObservationStore admits the actual RunStore a bound stream
// reads through, without unwrapping it: the capability probes look through
// Unwrap, but a decorator's own reads are the ones a stream calls. A store
// lacking the capability is refused with ErrRunDataCapability: its reads may
// drop the binding and serve whatever run holds the ID now.
func RequireBoundObservationStore(st RunStore) error {
	if st == nil {
		return fmt.Errorf("%w: no run store", ErrRunDataCapability)
	}
	v := reflect.ValueOf(st)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return fmt.Errorf("%w: nil run store %T", ErrRunDataCapability, st)
		}
	}
	capable, ok := st.(BoundObservationStore)
	if !ok {
		return fmt.Errorf("%w: run store %T does not honor a bound run observation", ErrRunDataCapability, st)
	}
	return capable.CheckBoundObservationStore(st)
}

// CheckBoundObservationStore admits this FilesystemRunStore itself: its run,
// event and log reads compare the binding the caller's context carries.
func (s *FilesystemRunStore) CheckBoundObservationStore(actual RunStore) error {
	receiver, ok := actual.(*FilesystemRunStore)
	if s == nil || !ok || receiver != s {
		return fmt.Errorf("%w: run store %T is not this filesystem store", ErrRunDataCapability, actual)
	}
	return nil
}
