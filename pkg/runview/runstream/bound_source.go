package runstream

import (
	"fmt"
	"reflect"

	"github.com/SocialGouv/iterion/pkg/store"
)

// BoundObservationSource explicitly endorses the actual outer Source for a
// subscription context carrying an original run binding (store.BindRunObservation):
// the source hands that context to the store reads that compare it, and ends
// the subscription with the store's refusal instead of a clean completion.
// Implementations must reject a different outer receiver, so a wrapper that
// merely inherits this method through embedding while overriding a subscribe
// method is not admitted. A wrapper implements the contract itself.
type BoundObservationSource interface {
	CheckBoundObservationSource(actual Source) error
}

// RequireBoundObservationSource admits the actual Source a bound subscription
// goes through, without unwrapping it. A source lacking the capability is
// refused with store.ErrRunDataCapability before any subscription starts:
// stripping the binding would silently serve whatever run holds the ID now.
func RequireBoundObservationSource(actual Source) error {
	if actual == nil {
		return fmt.Errorf("%w: no stream source", store.ErrRunDataCapability)
	}
	v := reflect.ValueOf(actual)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return fmt.Errorf("%w: nil stream source", store.ErrRunDataCapability)
		}
	}
	capable, ok := actual.(BoundObservationSource)
	if !ok {
		return fmt.Errorf("%w: stream source %T does not honor a bound run observation", store.ErrRunDataCapability, actual)
	}
	return capable.CheckBoundObservationSource(actual)
}

// CheckBoundObservationSource admits this FileSource itself: its selections
// are opened by the store under the subscription context.
func (f *FileSource) CheckBoundObservationSource(actual Source) error {
	receiver, ok := actual.(*FileSource)
	if f == nil || !ok || receiver != f {
		return fmt.Errorf("%w: stream source %T is not this file source", store.ErrRunDataCapability, actual)
	}
	return nil
}

// CheckBoundObservationSource admits this MongoSource only when it resolves its
// selections through the store (NewMongoResolved). The legacy collection
// constructor cannot honor a binding and refuses one at subscription anyway.
func (m *MongoSource) CheckBoundObservationSource(actual Source) error {
	receiver, ok := actual.(*MongoSource)
	if m == nil || !ok || receiver != m {
		return fmt.Errorf("%w: stream source %T is not this Mongo source", store.ErrRunDataCapability, actual)
	}
	if !m.resolved {
		return fmt.Errorf("%w: a Mongo source over raw collections cannot honor a bound run observation", store.ErrRunDataCapability)
	}
	return nil
}
