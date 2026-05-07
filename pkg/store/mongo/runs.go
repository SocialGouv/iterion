package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// CreateRun inserts a new run document with status=queued. Cloud
// runs always start queued; the runner pod transitions them to
// running on pickup (plan §F T-31).
func (s *Store) CreateRun(ctx context.Context, id, workflowName string, inputs map[string]interface{}) (*store.Run, error) {
	now := time.Now().UTC()
	r := &store.Run{
		FormatVersion: store.RunFormatVersion,
		ID:            id,
		WorkflowName:  workflowName,
		Status:        store.RunStatusQueued,
		Inputs:        inputs,
		CreatedAt:     now,
		UpdatedAt:     now,
		QueuedAt:      &now,
		SchemaVersion: SchemaVersion,
		CASVersion:    1,
	}
	stampTenant(ctx, r)
	if _, err := s.runs.InsertOne(ctx, r); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return nil, fmt.Errorf("store/mongo: run %s already exists", id)
		}
		return nil, fmt.Errorf("store/mongo: insert run: %w", err)
	}
	return r, nil
}

// LoadRun fetches the run document by _id. The query is implicitly
// scoped by tenant_id when the ctx carries one — a tenant-scoped
// caller asking for a run that belongs to another tenant gets a
// not-found, never a leak. Refuses documents written by a future
// schema version (plan §D.5).
func (s *Store) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	var r store.Run
	err := s.runs.FindOne(ctx, withTenantFilter(ctx, bson.M{"_id": id})).Decode(&r)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("store/mongo: run %s not found", id)
		}
		return nil, fmt.Errorf("store/mongo: load run %s: %w", id, err)
	}
	if r.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("store/mongo: run %s schema version %d unknown, upgrade required", id, r.SchemaVersion)
	}
	return &r, nil
}

// SaveRun replaces the run document atomically. Tenant-scoped
// callers can only overwrite documents belonging to their tenant.
func (s *Store) SaveRun(ctx context.Context, r *store.Run) error {
	r.UpdatedAt = time.Now().UTC()
	r.SchemaVersion = SchemaVersion
	stampTenant(ctx, r)
	_, err := s.runs.ReplaceOne(ctx, withTenantFilter(ctx, bson.M{"_id": r.ID}), r, options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("store/mongo: replace run %s: %w", r.ID, err)
	}
	return nil
}

// ListRuns returns every run id sorted by created_at ascending. The
// caller filters in higher layers (runview.Service.List). Tenant
// scope is enforced when ctx carries a tenant_id.
func (s *Store) ListRuns(ctx context.Context) ([]string, error) {
	cur, err := s.runs.Find(
		ctx,
		withTenantFilter(ctx, bson.M{}),
		options.Find().SetProjection(bson.M{"_id": 1}).SetSort(bson.D{{Key: "created_at", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list runs: %w", err)
	}
	defer cur.Close(ctx)

	ids := []string{}
	for cur.Next(ctx) {
		var doc struct {
			ID string `bson:"_id"`
		}
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("store/mongo: decode run id: %w", err)
		}
		ids = append(ids, doc.ID)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("store/mongo: cursor: %w", err)
	}
	return ids, nil
}

// UpdateRunStatus mutates only the status / error / timestamps and
// bumps the CAS counter. Resume paths clear the FinishedAt sentinel
// (plan parity with FilesystemRunStore.UpdateRunStatus).
func (s *Store) UpdateRunStatus(ctx context.Context, id string, status store.RunStatus, runErr string) error {
	now := time.Now().UTC()
	set := bson.M{
		"status":     status,
		"updated_at": now,
		"error":      runErr,
	}
	unset := bson.M{}
	switch status {
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
		set["finished_at"] = now
	case store.RunStatusRunning:
		// Resume must clear FinishedAt or the elapsed-time ticker
		// freezes mid-run (mirrors FilesystemRunStore).
		set["error"] = ""
		unset["finished_at"] = ""
	}
	update := bson.M{"$set": set, "$inc": bson.M{"version": 1}}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	res, err := s.runs.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"_id": id}), update)
	if err != nil {
		return fmt.Errorf("store/mongo: update status %s: %w", id, err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("store/mongo: run %s not found", id)
	}
	return nil
}

// SaveCheckpoint writes the checkpoint document and bumps CAS. Plan
// §F T-33 layers an explicit version-conditional update on top; this
// method is the simple "no contention" form used by the engine itself.
func (s *Store) SaveCheckpoint(ctx context.Context, id string, cp *store.Checkpoint) error {
	res, err := s.runs.UpdateOne(
		ctx,
		withTenantFilter(ctx, bson.M{"_id": id}),
		bson.M{
			"$set": bson.M{
				"checkpoint": cp,
				"updated_at": time.Now().UTC(),
			},
			"$inc": bson.M{"version": 1},
		},
	)
	if err != nil {
		return fmt.Errorf("store/mongo: save checkpoint %s: %w", id, err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("store/mongo: run %s not found", id)
	}
	return nil
}

// PauseRun atomically writes the checkpoint, flips status to paused,
// and stamps updated_at. Single-document update is naturally atomic.
func (s *Store) PauseRun(ctx context.Context, id string, cp *store.Checkpoint) error {
	now := time.Now().UTC()
	res, err := s.runs.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"_id": id}), bson.M{
		"$set": bson.M{
			"status":     store.RunStatusPausedWaitingHuman,
			"checkpoint": cp,
			"updated_at": now,
		},
		"$inc":   bson.M{"version": 1},
		"$unset": bson.M{"finished_at": ""},
	})
	if err != nil {
		return fmt.Errorf("store/mongo: pause %s: %w", id, err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("store/mongo: run %s not found", id)
	}
	return nil
}

// FailRunResumable writes the checkpoint, flips status to
// failed_resumable, and records the failure reason. Resume can then
// re-pick up at NodeID without replaying upstream work.
func (s *Store) FailRunResumable(ctx context.Context, id string, cp *store.Checkpoint, runErr string) error {
	now := time.Now().UTC()
	res, err := s.runs.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"_id": id}), bson.M{
		"$set": bson.M{
			"status":      store.RunStatusFailedResumable,
			"checkpoint":  cp,
			"error":       runErr,
			"updated_at":  now,
			"finished_at": now,
		},
		"$inc": bson.M{"version": 1},
	})
	if err != nil {
		return fmt.Errorf("store/mongo: fail resumable %s: %w", id, err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("store/mongo: run %s not found", id)
	}
	return nil
}
