package mongo

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/store"
)

// withRunMetadataWrite binds a known format once before the parent-WRITE
// transaction. The callback is pure Mongo I/O, may be retried, and must reset
// its output on each attempt. Payload writers keep their explicit legacy
// default; this helper grants no execution or financial authority.
func (s *Store) withRunMetadataWrite(ctx context.Context, id string, apply func(context.Context, *store.Run) error) error {
	guard, err := s.prepareRunMetadataFormat(ctx, id)
	if err != nil {
		return err
	}
	bound, err := store.WithRunValueFormat(ctx, guard.original)
	if err != nil {
		return err
	}
	return s.withValueRecordWrite(bound, id, func(sc context.Context, _ store.ValueFormat, parent *store.Run) error { return apply(sc, parent) })
}
