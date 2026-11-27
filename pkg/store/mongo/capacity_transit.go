package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Mongo half of store.CapacityTransitStore and the Mongo-only
// store.CapacityParker and store.FrozenProgramWriter. Each is one conditional
// write on the run document: the transit's begin and end, a park whose
// status, failure, armed retry and continuation land together, and a
// usage-window park's frozen program.

var (
	_ store.CapacityTransitStore = (*Store)(nil)
	_ store.CapacityParker       = (*Store)(nil)
	_ store.FrozenProgramWriter  = (*Store)(nil)
)

// majorityRuns is the runs collection with majority acknowledgement: a
// transit the caller then acts on (it re-asserts its places, it resumes)
// must not be rolled back by a failover.
func (s *Store) majorityRuns() *mongo.Collection {
	return s.runs.Clone(options.Collection().SetWriteConcern(writeconcern.Majority()))
}

// BeginCapacityTransit implements store.CapacityTransitStore: one
// findOneAndUpdate returning the document as it was, so the prior
// continuation is the one this write replaced.
func (s *Store) BeginCapacityTransit(ctx context.Context, runID string, t store.CapacityTransit) (store.CapacityTransitStart, error) {
	if runID == "" || t.Until.IsZero() {
		return store.CapacityTransitStart{}, fmt.Errorf("store/mongo: begin the capacity transit of run %q: run id and the hold's end are required (until=%v)", runID, t.Until)
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{
		"_id":    runID,
		"status": string(store.RunStatusFailedResumable),
		// An armed retry is the sweeper's; absent matches $ne.
		"continuation_state": bson.M{"$ne": string(store.ContinuationRetryArmed)},
	}))
	if !applyExactQueuedAt(filter, t.ExactQueuedAt) {
		return store.CapacityTransitStart{}, nil
	}
	if !t.ExpectUntil.IsZero() {
		filter["continuation_state"] = string(store.ContinuationRedeliveryPending)
		filter["continuation_until"] = store.TransitInstant(t.ExpectUntil)
	}
	update := versionRunUpdate(bson.M{"$set": bson.M{
		"continuation_state": string(store.ContinuationRedeliveryPending),
		"continuation_until": store.TransitInstant(t.Until),
		"updated_at":         time.Now().UTC(),
	}})
	var before struct {
		ContinuationState store.ContinuationState `bson:"continuation_state"`
	}
	result, err := s.findOneAndUpdateRunMetadataOn(ctx, s.majorityRuns(), runID, filter, update,
		options.FindOneAndUpdate().SetReturnDocument(options.Before).SetProjection(bson.M{"continuation_state": 1}))
	if err == nil {
		err = result.Decode(&before)
	}
	if errors.Is(err, mongo.ErrNoDocuments) || errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted) {
		return store.CapacityTransitStart{}, nil
	}
	if err != nil {
		return store.CapacityTransitStart{}, fmt.Errorf("store/mongo: begin the capacity transit of run %s: %w", runID, err)
	}
	return store.CapacityTransitStart{Started: true, Prior: before.ContinuationState}, nil
}

// EndCapacityTransit implements store.CapacityTransitStore.
func (s *Store) EndCapacityTransit(ctx context.Context, runID string, until time.Time, restore store.ContinuationState) (bool, error) {
	if runID == "" || until.IsZero() {
		return false, fmt.Errorf("store/mongo: end the capacity transit of run %q: run id and the transit's end are required (until=%v)", runID, until)
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{
		"_id":                runID,
		"status":             string(store.RunStatusFailedResumable),
		"continuation_state": string(store.ContinuationRedeliveryPending),
		"continuation_until": store.TransitInstant(until),
	}))
	set := bson.M{"updated_at": time.Now().UTC()}
	unset := bson.M{"continuation_until": ""}
	if restore == "" {
		unset["continuation_state"] = ""
	} else {
		set["continuation_state"] = string(restore)
	}
	res, err := s.updateRunMetadataOn(ctx, s.majorityRuns(), runID, filter, versionRunUpdate(bson.M{"$set": set, "$unset": unset}))
	if errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store/mongo: end the capacity transit of run %s: %w", runID, err)
	}
	return res.MatchedCount > 0, nil
}

// ParkRunOnCapacity implements store.CapacityParker: the status transition
// (statusTransitionSet, the choke point every status writer goes through) and
// the retry's arming (retryArmingSet, the one every arming writes) in one
// pipeline update, fenced to the attempt (from queued) or to the transit
// (from failed_resumable) the caller owns. A charged park carries the budget
// in its filter; a free one does not and leaves the attempts as they are.
func (s *Store) ParkRunOnCapacity(ctx context.Context, runID string, p store.CapacityPark) (bool, int, error) {
	if runID == "" {
		return false, 0, errors.New("store/mongo: park run on capacity: run id is required")
	}
	if err := p.Validate(); err != nil {
		return false, 0, err
	}
	now := time.Now().UTC()
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": runID, "status": string(p.From)}))
	if p.From == store.RunStatusFailedResumable {
		filter["continuation_state"] = string(store.ContinuationRedeliveryPending)
		filter["continuation_until"] = store.TransitInstant(p.TransitUntil)
	}
	if !applyExactQueuedAt(filter, p.ExactQueuedAt) {
		return false, 0, nil
	}
	if p.Charge {
		filter["$and"] = bson.A{bson.M{"$or": retryBudgetClauses(p.MaxAttempts)}}
	}
	set := statusTransitionSet(store.RunStatusFailedResumable, p.Error,
		store.RunOutcomeMeta{Code: p.Code, Continuation: store.ContinuationRetryArmed}, now)
	for k, v := range retryArmingSet(p.RetryAfter, time.Time{}, p.Reason, string(p.Code), p.Charge, now) {
		set[k] = v
	}
	set[frozenProgramField] = frozenProgramValue(p.Frozen)
	var after struct {
		RetryState *store.RunRetryState `bson:"retry_state"`
	}
	result, err := s.findOneAndUpdateRunMetadata(ctx, runID, filter, versionRunUpdate(statusTransitionPipeline(set)),
		options.FindOneAndUpdate().SetReturnDocument(options.After).SetProjection(bson.M{retryStateField: 1}))
	if err == nil {
		err = result.Decode(&after)
	}
	if errors.Is(err, mongo.ErrNoDocuments) || errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted) {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("store/mongo: park run %s on capacity: %w", runID, err)
	}
	attempt := 0
	if after.RetryState != nil {
		attempt = after.RetryState.Attempts
	}
	return true, attempt, nil
}

// frozenProgramField is the run root's frozen-program record
// (store.Run.FrozenProgram).
const frozenProgramField = "frozen_program"

// frozenProgramValue is a park pipeline's value of the record (the capacity
// park's, SetRunFrozenProgram's): fp with the attempt marker read off the
// document by the same write ("$queued_at" — the write's own fence decided the
// attempt, so the record binds exactly it), or $$REMOVE when the park froze
// nothing. The strings are literals: a value starting with "$" would otherwise
// read as a field path.
func frozenProgramValue(fp *store.FrozenProgram) any {
	if fp == nil {
		return "$$REMOVE"
	}
	return bson.M{
		"key":           bson.M{"$literal": fp.Key},
		"digest":        bson.M{"$literal": fp.Digest},
		"workflow_hash": bson.M{"$literal": fp.WorkflowHash},
		"queued_at":     "$queued_at",
		"source":        bson.M{"$literal": fp.Source},
		"frozen_at":     fp.FrozenAt.UTC(),
	}
}

// SetRunFrozenProgram implements store.FrozenProgramWriter: one conditional
// pipeline update on the run root, fenced to the attempt the caller parks —
// status failed_resumable and queued_at exactly queuedAt — writing the record
// with the same builder the capacity park uses, its attempt marker read off
// the document by this write.
func (s *Store) SetRunFrozenProgram(ctx context.Context, runID string, queuedAt time.Time, fp *store.FrozenProgram) (bool, error) {
	if runID == "" || queuedAt.IsZero() {
		return false, fmt.Errorf("store/mongo: set the frozen program of run %q: run id and the attempt's queued_at are required (queued_at=%v)", runID, queuedAt)
	}
	if !fp.Valid() {
		return false, fmt.Errorf("store/mongo: set the frozen program of run %s: incomplete record %+v", runID, fp)
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": runID, "status": string(store.RunStatusFailedResumable)}))
	if !applyExactQueuedAt(filter, queuedAt) {
		return false, nil
	}
	set := bson.M{frozenProgramField: frozenProgramValue(fp), "updated_at": time.Now().UTC()}
	res, err := s.updateRunMetadata(ctx, runID, filter, versionRunUpdate(mongo.Pipeline{{{Key: "$set", Value: set}}}))
	if errors.Is(err, store.ErrRunNotFound) || errors.Is(err, store.ErrRunDeleted) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store/mongo: set the frozen program of run %s: %w", runID, err)
	}
	return res.MatchedCount > 0, nil
}

// SaveParkedLaunchRun implements store.CapacityParker: SaveRun's replacement
// and the park's arming in one update (saveRun). The document is the one the
// launch built, parked failed_resumable on the park's code.
func (s *Store) SaveParkedLaunchRun(ctx context.Context, r *store.Run, p store.CapacityPark) error {
	if r == nil {
		return errors.New("store/mongo: save parked launch run: no run")
	}
	if r.Status != store.RunStatusFailedResumable || r.FailureCode != p.Code {
		return fmt.Errorf("store/mongo: save parked launch run %s: the document reads %s/%q, a park writes failed_resumable/%q", r.ID, r.Status, r.FailureCode, p.Code)
	}
	if err := p.ValidateRetry(); err != nil {
		return err
	}
	return s.saveRun(ctx, r, &p)
}
