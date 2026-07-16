package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/appinfo"
	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/store"
)

// CreateRun inserts a new run document with status=queued. Cloud
// runs always start queued; the runner pod transitions them to
// running on pickup (plan §F T-31).
func (s *Store) CreateRun(ctx context.Context, id, workflowName string, inputs map[string]any) (*store.Run, error) {
	now := time.Now().UTC()
	r := &store.Run{
		FormatVersion:  store.RunFormatVersion,
		ID:             id,
		WorkflowName:   workflowName,
		Status:         store.RunStatusQueued,
		Inputs:         inputs,
		CreatedAt:      now,
		UpdatedAt:      now,
		QueuedAt:       &now,
		SchemaVersion:  SchemaVersion,
		CASVersion:     1,
		LaunchEnv:      store.CaptureLaunchEnv(),
		IterionVersion: appinfo.FullVersion(),
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
	r, err := mongoutil.FindOne[store.Run](ctx, s.runs, withTenantFilter(ctx, bson.M{"_id": id}),
		fmt.Errorf("store/mongo: run %s not found: %w", id, store.ErrRunNotFound),
		fmt.Sprintf("store/mongo: load run %s", id))
	if err != nil {
		return nil, err
	}
	if r.SchemaVersion > SchemaVersion {
		return nil, fmt.Errorf("store/mongo: run %s schema version %d unknown, upgrade required", id, r.SchemaVersion)
	}
	return &r, nil
}

// DeleteRun permanently removes a run and all of its data: the run
// document, its events, seq counter, interactions, queued user-messages
// (Mongo), and every artifact + attachment blob. Tenant-scoped when the
// ctx carries a tenant, so a tenant can only delete its own runs.
// Idempotent — a gone run is a no-op.
//
// Order: blobs first, then the child Mongo collections, then the run
// document last. A partial failure can only UNDER-delete (leave orphaned
// docs pointing at gone blobs — harmless), never leave the run visible
// while its data is gone.
func (s *Store) DeleteRun(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("store/mongo: DeleteRun requires a run id")
	}
	if err := s.blob.DeleteRun(ctx, id); err != nil {
		return fmt.Errorf("store/mongo: blob delete run %s: %w", id, err)
	}
	if err := s.blob.DeleteRunAttachments(ctx, id); err != nil {
		return fmt.Errorf("store/mongo: blob delete attachments %s: %w", id, err)
	}
	if err := s.blob.DeleteRunToolBlobs(ctx, id); err != nil {
		return fmt.Errorf("store/mongo: blob delete tool blobs %s: %w", id, err)
	}
	if err := s.blob.DeleteRunFiles(ctx, id); err != nil {
		return fmt.Errorf("store/mongo: blob delete run files %s: %w", id, err)
	}
	if err := s.blob.DeleteRunIR(ctx, id); err != nil {
		return fmt.Errorf("store/mongo: blob delete IR blob %s: %w", id, err)
	}
	// Also sweep the runner-local scratch dir if this store owns one (a
	// runner-side store; server-side stores leave runFilesScratch empty
	// so the join is harmless but the tree never exists).
	if s.runFilesScratch != "" {
		if err := os.RemoveAll(s.runFilesScratchDir(id)); err != nil {
			return fmt.Errorf("store/mongo: remove run files scratch %s: %w", id, err)
		}
	}
	children := []struct {
		name string
		coll *mongo.Collection
	}{
		{"events", s.events},
		{"run_seq", s.runSeq},
		{"interactions", s.interactions},
		{"user_messages", s.userMessages},
		{"run_gitmeta", s.runGitMeta},
		{"run_plans", s.runPlans},
		{"run_notes", s.runNotes},
		{"run_turns", s.runTurns},
		{"run_logs", s.runLogs},
		{"run_tags", s.runTags},
	}
	for _, c := range children {
		if _, err := c.coll.DeleteMany(ctx, withTenantFilter(ctx, bson.M{"run_id": id})); err != nil {
			return fmt.Errorf("store/mongo: delete %s for run %s: %w", c.name, id, err)
		}
	}
	if _, err := s.runs.DeleteOne(ctx, withTenantFilter(ctx, bson.M{"_id": id})); err != nil {
		return fmt.Errorf("store/mongo: delete run %s: %w", id, err)
	}
	return nil
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

// AddWatchedIssues merges issueIDs into the run's watched_issue_ids set
// ($addToSet is atomic and dedups) and returns the resulting set.
func (s *Store) AddWatchedIssues(ctx context.Context, runID string, issueIDs []string) ([]string, error) {
	clean := make([]string, 0, len(issueIDs))
	for _, id := range issueIDs {
		if id != "" {
			clean = append(clean, id)
		}
	}
	if len(clean) == 0 {
		return s.watchedIssues(ctx, runID)
	}
	update := bson.M{
		"$addToSet": bson.M{"watched_issue_ids": bson.M{"$each": clean}},
		"$set":      bson.M{"updated_at": time.Now().UTC()},
		"$inc":      bson.M{"version": 1},
	}
	return s.updateWatched(ctx, runID, update)
}

// RemoveWatchedIssues drops issueIDs from the run's watched_issue_ids
// set ($pull) and returns the resulting set.
func (s *Store) RemoveWatchedIssues(ctx context.Context, runID string, issueIDs []string) ([]string, error) {
	if len(issueIDs) == 0 {
		return s.watchedIssues(ctx, runID)
	}
	update := bson.M{
		"$pull": bson.M{"watched_issue_ids": bson.M{"$in": issueIDs}},
		"$set":  bson.M{"updated_at": time.Now().UTC()},
		"$inc":  bson.M{"version": 1},
	}
	return s.updateWatched(ctx, runID, update)
}

func (s *Store) updateWatched(ctx context.Context, runID string, update bson.M) ([]string, error) {
	var doc struct {
		Watched []string `bson:"watched_issue_ids"`
	}
	opts := options.FindOneAndUpdate().
		SetReturnDocument(options.After).
		SetProjection(bson.M{"watched_issue_ids": 1})
	err := s.runs.FindOneAndUpdate(ctx, withTenantFilter(ctx, bson.M{"_id": runID}), update, opts).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("store/mongo: run %s not found", runID)
		}
		return nil, fmt.Errorf("store/mongo: update watched issues %s: %w", runID, err)
	}
	return doc.Watched, nil
}

func (s *Store) watchedIssues(ctx context.Context, runID string) ([]string, error) {
	var doc struct {
		Watched []string `bson:"watched_issue_ids"`
	}
	err := s.runs.FindOne(
		ctx,
		withTenantFilter(ctx, bson.M{"_id": runID}),
		options.FindOne().SetProjection(bson.M{"watched_issue_ids": 1}),
	).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, fmt.Errorf("store/mongo: run %s not found", runID)
		}
		return nil, fmt.Errorf("store/mongo: load watched issues %s: %w", runID, err)
	}
	return doc.Watched, nil
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

// ListRunsBySourceIssue returns the ids of runs whose source.issue_id
// equals issueID (the card←run reverse edge), sorted by created_at
// ascending. Indexed by (tenant_id, source.issue_id, created_at); tenant
// scope is enforced when ctx carries a tenant_id. Refs #125 (T4b).
func (s *Store) ListRunsBySourceIssue(ctx context.Context, issueID string) ([]string, error) {
	if issueID == "" {
		return []string{}, nil
	}
	return s.listRunIDsBy(ctx, bson.M{"source.issue_id": issueID}, "list runs by source issue")
}

// ListChildRuns returns the ids of runs whose parent_run_id equals
// parentRunID (a run's shard/child subtree), sorted by created_at
// ascending. Indexed by (tenant_id, parent_run_id, created_at); tenant
// scope is enforced when ctx carries a tenant_id. Refs #125 (T4b).
func (s *Store) ListChildRuns(ctx context.Context, parentRunID string) ([]string, error) {
	if parentRunID == "" {
		return []string{}, nil
	}
	return s.listRunIDsBy(ctx, bson.M{"parent_run_id": parentRunID}, "list child runs")
}

// listRunIDsBy runs an indexed Find over the runs collection with the
// given (tenant-wrapped) filter, projecting _id and sorting by
// created_at ascending. Shared by the reverse-tree queries.
func (s *Store) listRunIDsBy(ctx context.Context, filter bson.M, what string) ([]string, error) {
	cur, err := s.runs.Find(
		ctx,
		withTenantFilter(ctx, filter),
		options.Find().SetProjection(bson.M{"_id": 1}).SetSort(bson.D{{Key: "created_at", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: %s: %w", what, err)
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

// StaleRunRef identifies one run the orphan sweeper should examine:
// id + tenant (the sweeper re-stamps per-run tenant ctx for the CAS
// status flip).
type StaleRunRef struct {
	ID       string `bson:"_id"`
	TenantID string `bson:"tenant_id"`
	Status   string `bson:"status"`
}

// ListStaleActiveRuns returns queued/running runs whose last update
// precedes `before` — orphan candidates (runner crashed pre-status-
// write, message purged, MaxDeliver exhausted without the DLQ
// bridge). Platform-level scan: callers pass a WithoutTenantFilter
// ctx; the per-run tenant comes back on the ref.
func (s *Store) ListStaleActiveRuns(ctx context.Context, statuses []store.RunStatus, before time.Time, limit int) ([]StaleRunRef, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	in := make([]string, 0, len(statuses))
	for _, st := range statuses {
		in = append(in, string(st))
	}
	cur, err := s.runs.Find(ctx,
		withTenantFilter(ctx, bson.M{
			"status":     bson.M{"$in": in},
			"updated_at": bson.M{"$lt": before},
		}),
		options.Find().
			SetProjection(bson.M{"_id": 1, "tenant_id": 1, "status": 1}).
			SetSort(bson.M{"updated_at": 1}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list stale runs: %w", err)
	}
	defer cur.Close(ctx)
	var out []StaleRunRef
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("store/mongo: decode stale runs: %w", err)
	}
	return out, nil
}

// CountActiveRunsByTenant counts the org's queued + running runs.
// Consumed by the server's launch gate (per-org concurrency cap) with
// an explicit tenant — deliberately NOT the ctx-derived tenant filter,
// because the gate evaluates a specific org and the parameter makes
// the scope auditable.
func (s *Store) CountActiveRunsByTenant(ctx context.Context, tenantID string) (int, error) {
	n, err := s.runs.CountDocuments(ctx, bson.M{
		"tenant_id": tenantID,
		"status":    bson.M{"$in": []string{string(store.RunStatusQueued), string(store.RunStatusRunning)}},
	})
	if err != nil {
		return 0, fmt.Errorf("store/mongo: count active runs: %w", err)
	}
	return int(n), nil
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
	case store.RunStatusPausedWaitingHuman:
		// Mirror the FS store: a generic UpdateRunStatus that crosses
		// from a previously-terminal (failed_resumable) state into
		// paused-waiting-human must also clear finished_at so the
		// elapsed-time UI doesn't stay frozen.
		unset["finished_at"] = ""
	}
	update := bson.M{"$set": set, "$inc": bson.M{"version": 1}}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	return mongoutil.UpdateOneChecked(ctx, s.runs, withTenantFilter(ctx, bson.M{"_id": id}), update,
		fmt.Errorf("store/mongo: run %s not found", id), fmt.Sprintf("store/mongo: update status %s", id))
}

// UpdateRunStatusIf is a compare-and-set on the status field
// implemented as a conditional UpdateOne — the write only lands when
// the persisted status matches one of expectedFrom. Returns
// changed=true on a successful write, false if the status had drifted
// since the caller's last read (concurrent transition by another
// publisher, runner, or operator).
func (s *Store) UpdateRunStatusIf(ctx context.Context, id string, status store.RunStatus, runErr string, expectedFrom []store.RunStatus) (bool, error) {
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
		set["error"] = ""
		unset["finished_at"] = ""
	case store.RunStatusPausedWaitingHuman:
		unset["finished_at"] = ""
	}
	update := bson.M{"$set": set, "$inc": bson.M{"version": 1}}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	filter := withTenantFilter(ctx, bson.M{"_id": id})
	if len(expectedFrom) > 0 {
		filter["status"] = bson.M{"$in": expectedFrom}
	}
	res, err := s.runs.UpdateOne(ctx, filter, update)
	if err != nil {
		return false, fmt.Errorf("store/mongo: update status if %s: %w", id, err)
	}
	return res.MatchedCount > 0, nil
}

// SaveCheckpoint writes the checkpoint document and bumps CAS. Plan
// §F T-33 layers an explicit version-conditional update on top; this
// method is the simple "no contention" form used by the engine itself.
func (s *Store) SaveCheckpoint(ctx context.Context, id string, cp *store.Checkpoint) error {
	update := bson.M{
		"$set": bson.M{
			"checkpoint": cp,
			"updated_at": time.Now().UTC(),
		},
		"$inc": bson.M{"version": 1},
	}
	return mongoutil.UpdateOneChecked(ctx, s.runs, withTenantFilter(ctx, bson.M{"_id": id}), update,
		fmt.Errorf("store/mongo: run %s not found", id), fmt.Sprintf("store/mongo: save checkpoint %s", id))
}

// PauseRun atomically writes the checkpoint, flips status to paused,
// and stamps updated_at. Single-document update is naturally atomic.
func (s *Store) PauseRun(ctx context.Context, id string, cp *store.Checkpoint) error {
	now := time.Now().UTC()
	update := bson.M{
		"$set": bson.M{
			"status":     store.RunStatusPausedWaitingHuman,
			"checkpoint": cp,
			"updated_at": now,
		},
		"$inc":   bson.M{"version": 1},
		"$unset": bson.M{"finished_at": ""},
	}
	return mongoutil.UpdateOneChecked(ctx, s.runs, withTenantFilter(ctx, bson.M{"_id": id}), update,
		fmt.Errorf("store/mongo: run %s not found", id), fmt.Sprintf("store/mongo: pause %s", id))
}

// FailRunResumable writes the checkpoint, flips status to
// failed_resumable, and records the failure reason. Resume can then
// re-pick up at NodeID without replaying upstream work.
func (s *Store) FailRunResumable(ctx context.Context, id string, cp *store.Checkpoint, runErr string) error {
	now := time.Now().UTC()
	update := bson.M{
		"$set": bson.M{
			"status":      store.RunStatusFailedResumable,
			"checkpoint":  cp,
			"error":       runErr,
			"updated_at":  now,
			"finished_at": now,
		},
		"$inc": bson.M{"version": 1},
	}
	return mongoutil.UpdateOneChecked(ctx, s.runs, withTenantFilter(ctx, bson.M{"_id": id}), update,
		fmt.Errorf("store/mongo: run %s not found", id), fmt.Sprintf("store/mongo: fail resumable %s", id))
}
