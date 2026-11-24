package mongo

import (
	"fmt"

	"github.com/SocialGouv/iterion/pkg/store"
)

var _ store.BoundObservationStore = (*Store)(nil)

// CheckBoundObservationStore admits this Store itself: its run, event and log
// reads compare the binding the caller's context carries.
func (s *Store) CheckBoundObservationStore(actual store.RunStore) error {
	receiver, ok := actual.(*Store)
	if s == nil || !ok || receiver != s {
		return fmt.Errorf("%w: run store %T is not this Mongo store", store.ErrRunDataCapability, actual)
	}
	return nil
}
