package mongo

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/store"
)

func (s *Store) ReserveInitialUsageIf(ctx context.Context, id string, a store.RunExecutionAuthority, next store.OutputCorrectionReservation) (bool, error) {
	var err error
	next, err = store.FreezeInitialUsageReservation(next)
	if err != nil {
		return false, err
	}
	return s.mutateOutputCorrection(ctx, id, func(r *store.Run) (bool, error) { return store.ApplyInitialUsageReservation(r, a, next) })
}
