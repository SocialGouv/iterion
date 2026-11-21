package mongo

import (
	"context"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// mutateOutputCorrection retries only its pure decision against fresh state.
// The closure's authority/expected episode never changes on a CAS conflict.
// It updates just correction fields; it is not a whole-Run SaveRun retry.
func (s *Store) mutateOutputCorrection(ctx context.Context, id string, apply func(*store.Run) (bool, error)) (bool, error) {
	guard, err := s.prepareRunMetadataFormat(ctx, id)
	if err != nil {
		return false, err
	}
	for range 3 {
		r, err := s.LoadRun(ctx, id)
		if err != nil {
			return false, err
		}
		current, err := store.CaptureRunValueFormat(r)
		if err != nil {
			return false, err
		}
		if current != guard.original {
			return false, store.ErrValueFormatChanged
		}
		preimageEpoch := r.ExecutionEpoch
		ok, err := apply(r)
		if !ok || err != nil {
			return ok, err
		}
		filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": id, "$and": bson.A{correctionVersionFilter(r.CASVersion), correctionEpochFilter(preimageEpoch)}}))
		update := bson.M{"$set": bson.M{"initial_usage_reservations": r.InitialUsageReservations, "usage_session_marks": r.UsageSessionMarks, "output_corrections": r.OutputCorrections, "output_correction_history": r.OutputCorrectionHistory, "updated_at": time.Now().UTC()}, "$inc": bson.M{"version": 1}}
		result, err := s.runs.UpdateOne(ctx, guard.filter(ctx, filter), update)
		if err != nil {
			return false, err
		}
		if result.MatchedCount == 1 {
			return true, nil
		}
		if err = guard.checkMiss(ctx, s); err != nil {
			return false, err
		}
	}
	return false, store.ErrRunConflict
}

func (s *Store) SetRunOutputCorrectionIf(ctx context.Context, id string, a store.RunExecutionAuthority, key string, expected store.OutputCorrectionExpected, next store.OutputCorrectionEpisode) (bool, error) {
	next = store.CloneOutputCorrectionEpisode(next)
	return s.mutateOutputCorrection(ctx, id, func(r *store.Run) (bool, error) {
		return store.ApplyOutputCorrectionDecision(r, a, key, expected, next)
	})
}

func (s *Store) RecordOutputCorrectionReceipt(ctx context.Context, id string, target store.OutputCorrectionReceiptTarget, receipt store.OutputCorrectionReceipt) (bool, error) {
	var freezeErr error
	receipt, freezeErr = store.FreezeOutputCorrectionReceipt(receipt)
	if freezeErr != nil {
		return false, freezeErr
	}
	return s.mutateOutputCorrection(ctx, id, func(r *store.Run) (bool, error) {
		return store.ApplyOutputCorrectionReceipt(r, target, receipt)
	})
}
