package mongo

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/store"
)

func (s *Store) CheckEventObservationProducer(actual store.EventAppender) error {
	receiver, ok := actual.(*Store)
	if s == nil || !ok || receiver != s {
		return store.ErrRunDataCapability
	}
	return nil
}

// prepareEventObservation keeps the real selected parent for callback sampling.
// The transaction rechecks this observation before publishing sequence/event.
func (s *Store) prepareEventObservation(ctx context.Context, id string) (store.ValueFormat, store.RunObservation, error) {
	parent, err := s.valueParent(ctx, id)
	if err != nil {
		return 0, store.RunObservation{}, err
	}
	raw := 0
	if parent != nil {
		raw = parent.FormatVersion
	}
	format, err := store.CheckRunValueFormat(ctx, id, raw)
	if err != nil {
		return 0, store.RunObservation{}, err
	}
	original, err := store.ObserveEventParent(ctx, id, format, parent)
	return format, original, err
}
