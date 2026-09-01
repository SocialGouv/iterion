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

var _ store.ParentedRunCreator = (*Store)(nil)

// CreateChildRun inserts a new run document (status=queued, like
// CreateRun) with ParentRunID stamped in the same insert, so the cloud
// launch path persists the parent link without a follow-up SaveRun.
// Implements store.ParentedRunCreator.
func (s *Store) CreateChildRun(ctx context.Context, id, workflowName, parentRunID string, inputs map[string]any) (*store.Run, error) {
	now := time.Now().UTC()
	r := &store.Run{
		FormatVersion:  store.RunFormatVersion,
		ID:             id,
		WorkflowName:   workflowName,
		ParentRunID:    parentRunID,
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
		return nil, fmt.Errorf("store/mongo: insert child run: %w", err)
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
	if r.DeletedAt != nil {
		return nil, fmt.Errorf("store/mongo: run %s: %w", id, store.ErrRunDeleted)
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
	if err := s.blob.DeleteRunBackendSessions(ctx, id); err != nil {
		return fmt.Errorf("store/mongo: blob delete backend sessions %s: %w", id, err)
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
	// Durable tombstone (the Mongo twin of the filesystem .deleted
	// marker): strip the run document down to a skeleton stamped
	// deleted_at instead of removing it, so a late writer's
	// upsert/update matches the tombstone and gets ErrRunDeleted
	// instead of silently resurrecting the run. Reaped by runs prune.
	now := time.Now().UTC()
	tomb := bson.M{"$set": bson.M{"deleted_at": now, "status": "deleted", "updated_at": now},
		"$unset": bson.M{"checkpoint": "", "inputs": "", "launch_env": "", "model_overrides": "",
			"budget": "", "loop_overrides": "", "budget_raises": "", "attachments": ""}}
	if _, err := s.runs.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"_id": id}), tomb); err != nil {
		return fmt.Errorf("store/mongo: tombstone run %s: %w", id, err)
	}
	return nil
}

// PruneDeletionMarkers reaps tombstone skeleton docs older than cutoff.
func (s *Store) PruneDeletionMarkers(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := s.runs.DeleteMany(ctx, withTenantFilter(ctx, bson.M{
		"deleted_at": bson.M{"$exists": true, "$lt": cutoff},
	}))
	if err != nil {
		return 0, fmt.Errorf("store/mongo: prune deletion markers: %w", err)
	}
	return int(res.DeletedCount), nil
}

// runDeleted reports whether the run document carries the deletion
// tombstone. Used by the append-shaped writers (events, messages,
// interactions, attachments) whose inserts have no run-doc filter to
// piggyback the predicate on.
func (s *Store) runDeleted(ctx context.Context, id string) bool {
	var doc struct {
		DeletedAt *time.Time `bson:"deleted_at"`
	}
	err := s.runs.FindOne(ctx, withTenantFilter(ctx, bson.M{"_id": id}),
		options.FindOne().SetProjection(bson.M{"deleted_at": 1})).Decode(&doc)
	return err == nil && doc.DeletedAt != nil
}

// guardNotDeleted is the shared typed refusal for tombstoned runs.
func (s *Store) guardNotDeleted(ctx context.Context, id string) error {
	if s.runDeleted(ctx, id) {
		return fmt.Errorf("store/mongo: run %s: %w", id, store.ErrRunDeleted)
	}
	return nil
}

// notDeleted adds the tombstone-exclusion predicate to a runs-collection
// filter, so UpdateOne-shaped writers can never land on a skeleton doc.
func notDeleted(filter bson.M) bson.M {
	filter["deleted_at"] = bson.M{"$exists": false}
	return filter
}

// SaveRun replaces the run document atomically. Tenant-scoped
// callers can only overwrite documents belonging to their tenant.
func (s *Store) SaveRun(ctx context.Context, r *store.Run) error {
	if err := s.guardNotDeleted(ctx, r.ID); err != nil {
		return err
	}
	r.UpdatedAt = time.Now().UTC()
	r.SchemaVersion = SchemaVersion
	stampTenant(ctx, r)
	// The notDeleted predicate closes the guard's TOCTOU window: a
	// DeleteRun racing between the check above and this write leaves a
	// tombstoned doc the filter no longer matches, and the upsert then
	// trips the duplicate-_id error instead of resurrecting the run.
	// Best-effort guard: a copy whose STATUS is already non-failure
	// must not resurrect its failure code through this full-document
	// write. A copy stale on the status itself still rewrites
	// status+code together (the inherent SaveRun read-modify-write
	// hazard — a version CAS is the real fix, follow-up); callers on
	// that path re-stamp the fields by hand (see rewind.go).
	if !r.Status.CarriesFailureCode() {
		r.FailureCode = ""
	}
	// Same discipline for the pause pointer: a full-document write on a
	// non-carrying status must not resurrect consumed interaction
	// evidence (mirrors the FS twin).
	if !r.Status.CarriesPausePointer() && r.Checkpoint != nil &&
		(r.Checkpoint.InteractionID != "" || len(r.Checkpoint.InteractionQuestions) > 0) {
		cp := *r.Checkpoint
		cp.InteractionID = ""
		cp.InteractionQuestions = nil
		r.Checkpoint = &cp
	}
	_, err := s.runs.ReplaceOne(ctx, notDeleted(withTenantFilter(ctx, bson.M{"_id": r.ID})), r, options.Replace().SetUpsert(true))
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("store/mongo: run %s: %w", r.ID, store.ErrRunDeleted)
		}
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

// SetSubbotChild records childRunID under key in the parent run's
// subbot_children map. Per-key $set is atomic in Mongo, so concurrent
// fan-out branches writing distinct keys don't conflict. No-op when key
// is empty. key must be a Mongo-safe field name (no '.'/'$') — the engine
// sanitizes it at construction.
func (s *Store) SetSubbotChild(ctx context.Context, parentRunID, key, childRunID string) error {
	if key == "" {
		return nil
	}
	update := bson.M{
		"$set": bson.M{"subbot_children." + key: childRunID, "updated_at": time.Now().UTC()},
		"$inc": bson.M{"version": 1},
	}
	return s.updateSubbotChildren(ctx, parentRunID, update)
}

// ClearSubbotChild removes key from the parent run's subbot_children map.
// No-op when key is empty.
func (s *Store) ClearSubbotChild(ctx context.Context, parentRunID, key string) error {
	if key == "" {
		return nil
	}
	update := bson.M{
		"$unset": bson.M{"subbot_children." + key: ""},
		"$set":   bson.M{"updated_at": time.Now().UTC()},
		"$inc":   bson.M{"version": 1},
	}
	return s.updateSubbotChildren(ctx, parentRunID, update)
}

func (s *Store) updateSubbotChildren(ctx context.Context, runID string, update bson.M) error {
	res, err := s.runs.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"_id": runID}), update)
	if err != nil {
		return fmt.Errorf("store/mongo: update subbot children %s: %w", runID, err)
	}
	if res.MatchedCount == 0 {
		return fmt.Errorf("store/mongo: run %s not found", runID)
	}
	return nil
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
		notDeleted(withTenantFilter(ctx, bson.M{})),
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

// ListRunsBySchedule returns the ids of runs whose source.schedule_id
// equals scheduleID (the schedule←run reverse edge used by the
// pkg/schedgate overlap gate), sorted by created_at ascending. Indexed
// by (tenant_id, source.schedule_id, created_at); tenant scope is
// enforced when ctx carries a tenant_id.
func (s *Store) ListRunsBySchedule(ctx context.Context, scheduleID string) ([]string, error) {
	if scheduleID == "" {
		return []string{}, nil
	}
	return s.listRunIDsBy(ctx, bson.M{"source.schedule_id": scheduleID}, "list runs by schedule")
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
		notDeleted(withTenantFilter(ctx, filter)),
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

// NotifiableRunRef is one row of ListNotifiableRuns: run id + the fields
// the usernotify sweep needs to derive the notification episode key
// (status + pending interaction + updated_at) and to keyset-paginate,
// without loading the run.
type NotifiableRunRef struct {
	ID         string    `bson:"_id"`
	Status     string    `bson:"status"`
	UpdatedAt  time.Time `bson:"updated_at"`
	Checkpoint struct {
		InteractionID string `bson:"interaction_id"`
	} `bson:"checkpoint"`
}

// ListNotifiableRuns returns one page of the runs the usernotify
// reconciliation sweep should (re-)examine: every run currently paused on
// a human interaction (no time bound — it is still waiting, however old),
// plus runs that reached a terminal status since `since`; restricted to
// updated_at < `before` when non-zero (the sweep's keyset cursor, so a
// backlog beyond one page cannot starve the oldest rows). Platform-level
// scan: callers pass a WithoutTenantFilter ctx. The sent-notifications
// claim makes replays idempotent, so over-listing is cheap and
// under-listing is the only real failure.
func (s *Store) ListNotifiableRuns(ctx context.Context, since, before time.Time, limit int) ([]NotifiableRunRef, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	terminal := []string{
		string(store.RunStatusFinished),
		string(store.RunStatusFailed),
		string(store.RunStatusFailedResumable),
		string(store.RunStatusCancelled),
	}
	filter := bson.M{"$or": []bson.M{
		{"status": string(store.RunStatusPausedWaitingHuman)},
		{"status": bson.M{"$in": terminal}, "updated_at": bson.M{"$gte": since}},
	}}
	if !before.IsZero() {
		filter = bson.M{"$and": []bson.M{
			{"updated_at": bson.M{"$lt": before}},
			filter,
		}}
	}
	cur, err := s.runs.Find(ctx,
		withTenantFilter(ctx, filter),
		options.Find().
			SetProjection(bson.M{"_id": 1, "status": 1, "updated_at": 1, "checkpoint.interaction_id": 1}).
			SetSort(bson.M{"updated_at": -1}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list notifiable runs: %w", err)
	}
	defer cur.Close(ctx)
	var out []NotifiableRunRef
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("store/mongo: decode notifiable runs: %w", err)
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
// PatchRunSteering persists the live-steering state (loop grants +
// absolute budget raises) with a partial $set, tenant-scoped. Partial:
// nil inputs leave the stored field untouched.
func (s *Store) PatchRunSteering(ctx context.Context, id string, loopOverrides map[string]int, budgetRaises *store.RunBudgetRaises) error {
	set := bson.M{"updated_at": time.Now().UTC()}
	if loopOverrides != nil {
		set["loop_overrides"] = loopOverrides
	}
	if budgetRaises != nil {
		set["budget_raises"] = budgetRaises
	}
	res, err := s.runs.UpdateOne(ctx, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})), bson.M{"$set": set})
	if err != nil {
		return fmt.Errorf("store/mongo: patch run steering: %w", err)
	}
	if res.MatchedCount == 0 {
		return store.ErrRunNotFound
	}
	return nil
}

// PatchRunPermissionGrants persists the permission-gate allow rules the
// operator earned, tenant-scoped. Replaces the stored slice wholesale;
// a nil slice is a no-op patch.
func (s *Store) PatchRunPermissionGrants(ctx context.Context, id string, grants map[string][]string) error {
	if grants == nil {
		return nil
	}
	set := bson.M{"updated_at": time.Now().UTC(), "permission_grants": grants}
	res, err := s.runs.UpdateOne(ctx, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})), bson.M{"$set": set})
	if err != nil {
		return fmt.Errorf("store/mongo: patch run permission grants: %w", err)
	}
	if res.MatchedCount == 0 {
		return store.ErrRunNotFound
	}
	return nil
}

// RecordNodeServed persists the last (backend, model) that served
// nodeID with a per-key $set, so concurrent nodes writing distinct
// keys do not clobber each other. Empty nodeID is a no-op.
// Display-only last-write-wins patch; no $inc on version so a later
// checkpoint CAS cannot be invalidated by this stamp (same as
// PatchRunSteering / PatchRunPermissionGrants).
func (s *Store) RecordNodeServed(ctx context.Context, id, nodeID string, served store.NodeServed) error {
	if nodeID == "" {
		return nil
	}
	set := bson.M{
		"updated_at":             time.Now().UTC(),
		"nodes_served." + nodeID: served,
	}
	res, err := s.runs.UpdateOne(ctx, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})), bson.M{
		"$set": set,
	})
	if err != nil {
		return fmt.Errorf("store/mongo: record node served: %w", err)
	}
	if res.MatchedCount == 0 {
		return store.ErrRunNotFound
	}
	return nil
}

// runStatusUpdate builds the shared $set/$unset pair for a status
// transition — the Mongo twin of the FS store's applyStatusTransition,
// including the FailureCode discipline: set on a failure status,
// cleared by every transition to a non-failure one.
func runStatusUpdate(status store.RunStatus, runErr string, code store.FailureCode, now time.Time) (bson.M, bson.M) {
	set := bson.M{
		"status":     status,
		"updated_at": now,
		"error":      runErr,
	}
	unset := bson.M{}
	// The pause pointer is a consumable (see store.CarriesPausePointer):
	// a transition into a status that cannot truthfully carry it clears
	// the interaction evidence off the surviving checkpoint. Dotted
	// $unset is a no-op when the checkpoint (or field) is absent.
	// Callers that $set the whole checkpoint document in the same
	// update (failRunCheckpointed) must DROP these two keys and strip
	// the value instead — Mongo rejects conflicting paths.
	if !status.CarriesPausePointer() {
		unset["checkpoint.interaction_id"] = ""
		unset["checkpoint.interaction_questions"] = ""
	}
	if status.CarriesFailureCode() && code != "" {
		set["failure_code"] = code
	} else {
		// $unset, not $set "": keeps the persisted shape identical to
		// the FS twin's omitempty JSON — including an UNKNOWN (empty)
		// code on a failure status, so {$exists: false} means exactly
		// "unclassified" on both twins.
		unset["failure_code"] = ""
	}
	switch status {
	case store.RunStatusFinished, store.RunStatusFailed, store.RunStatusFailedResumable, store.RunStatusCancelled:
		set["finished_at"] = now
	case store.RunStatusQueued:
		set["queued_at"] = now
		unset["finished_at"] = ""
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
	return set, unset
}

func (s *Store) UpdateRunStatus(ctx context.Context, id string, status store.RunStatus, runErr string) error {
	return s.UpdateRunStatusCoded(ctx, id, status, runErr, "")
}

// UpdateRunStatusCoded is UpdateRunStatus carrying the typed failure
// classification in the same $set.
func (s *Store) UpdateRunStatusCoded(ctx context.Context, id string, status store.RunStatus, runErr string, code store.FailureCode) error {
	set, unset := runStatusUpdate(status, runErr, code, time.Now().UTC())
	update := bson.M{"$set": set, "$inc": bson.M{"version": 1}}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	return mongoutil.UpdateOneChecked(ctx, s.runs, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})), update,
		fmt.Errorf("store/mongo: run %s not found", id), fmt.Sprintf("store/mongo: update status %s", id))
}

// UpdateRunStatusIf is a compare-and-set on the status field
// implemented as a conditional UpdateOne — the write only lands when
// the persisted status matches one of expectedFrom. Returns
// changed=true on a successful write, false if the status had drifted
// since the caller's last read (concurrent transition by another
// publisher, runner, or operator).
func (s *Store) UpdateRunStatusIf(ctx context.Context, id string, status store.RunStatus, runErr string, expectedFrom []store.RunStatus) (bool, error) {
	return s.UpdateRunStatusIfCoded(ctx, id, status, runErr, "", expectedFrom)
}

// UpdateRunStatusIfCoded is the CAS variant carrying the typed failure
// classification — code and status land in one atomic UpdateOne.
func (s *Store) UpdateRunStatusIfCoded(ctx context.Context, id string, status store.RunStatus, runErr string, code store.FailureCode, expectedFrom []store.RunStatus) (bool, error) {
	set, unset := runStatusUpdate(status, runErr, code, time.Now().UTC())
	update := bson.M{"$set": set, "$inc": bson.M{"version": 1}}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	if len(expectedFrom) == 0 {
		// A CAS with no expected set is an unconditional write in
		// disguise (and the FS twin would silently no-op instead) —
		// refuse loudly rather than diverge.
		return false, fmt.Errorf("store/mongo: update status if %s: empty expectedFrom", id)
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": id}))
	filter["status"] = bson.M{"$in": expectedFrom}
	res, err := s.runs.UpdateOne(ctx, filter, update)
	if err != nil {
		return false, fmt.Errorf("store/mongo: update status if %s: %w", id, err)
	}
	return res.MatchedCount > 0, nil
}

// mergeStatusFilter builds the merge_status clause for a CAS filter:
// the empty status matches both an unset field and an explicit "".
func mergeStatusFilter(expectedFrom []store.MergeStatus) bson.M {
	hasEmpty := false
	vals := make([]store.MergeStatus, 0, len(expectedFrom))
	for _, st := range expectedFrom {
		if st == "" {
			hasEmpty = true
		}
		vals = append(vals, st)
	}
	in := bson.M{"merge_status": bson.M{"$in": vals}}
	if !hasEmpty {
		return in
	}
	return bson.M{"$or": bson.A{in, bson.M{"merge_status": bson.M{"$exists": false}}}}
}

// ClaimMerge is the compare-and-set entry to the merge state machine
// (see store.RunStore), implemented as a conditional FindOneAndUpdate:
// the flip to "merging" only lands when the persisted status is
// claimable — unset/pending/failed, or a "merging" whose claim stamp
// predates staleBefore (the previous claimant crashed mid-merge).
func (s *Store) ClaimMerge(ctx context.Context, id string, staleBefore time.Time) (bool, store.MergeStatus, error) {
	now := time.Now().UTC()
	claimable := bson.M{"$or": bson.A{
		bson.M{"merge_status": bson.M{"$in": bson.A{"", store.MergeStatusPending, store.MergeStatusFailed}}},
		bson.M{"merge_status": bson.M{"$exists": false}},
		bson.M{"merge_status": store.MergeStatusMerging, "merge_claimed_at": bson.M{"$lt": staleBefore}},
	}}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": id, "$and": bson.A{claimable}}))
	update := bson.M{
		"$set": bson.M{"merge_status": store.MergeStatusMerging, "merge_claimed_at": now, "updated_at": now},
		"$inc": bson.M{"version": 1},
	}
	opts := options.FindOneAndUpdate().
		SetReturnDocument(options.Before).
		SetProjection(bson.M{"merge_status": 1})
	var before struct {
		MergeStatus store.MergeStatus `bson:"merge_status"`
	}
	err := s.runs.FindOneAndUpdate(ctx, filter, update, opts).Decode(&before)
	if err == nil {
		return true, before.MergeStatus, nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return false, "", fmt.Errorf("store/mongo: claim merge %s: %w", id, err)
	}
	// Not claimable (or missing): read the current status so the caller
	// can say WHY the claim was refused.
	var cur struct {
		MergeStatus store.MergeStatus `bson:"merge_status"`
	}
	err = s.runs.FindOne(ctx, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})),
		options.FindOne().SetProjection(bson.M{"merge_status": 1})).Decode(&cur)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return false, "", fmt.Errorf("store/mongo: claim merge: run %s not found", id)
		}
		return false, "", fmt.Errorf("store/mongo: claim merge %s: %w", id, err)
	}
	return false, cur.MergeStatus, nil
}

// UpdateRunMergeIf is the compare-and-set exit from the merge state
// machine (see store.RunStore): a conditional UpdateOne on the full
// merge bookkeeping. Empty fields are $unset, mirroring the omitempty
// shape a full SaveRun would produce.
func (s *Store) UpdateRunMergeIf(ctx context.Context, id string, upd store.RunMergeUpdate, expectedFrom []store.MergeStatus) (bool, error) {
	set := bson.M{"updated_at": time.Now().UTC()}
	unset := bson.M{"merge_claimed_at": ""}
	stringField := func(key, val string) {
		if val == "" {
			unset[key] = ""
		} else {
			set[key] = val
		}
	}
	stringField("merge_status", string(upd.Status))
	stringField("merged_commit", upd.MergedCommit)
	stringField("merged_into", upd.MergedInto)
	stringField("merge_strategy", string(upd.MergeStrategy))
	stringField("pending_merge_message", upd.PendingMergeMessage)
	stringField("pending_merge_into", upd.PendingMergeInto)
	update := bson.M{"$set": set, "$unset": unset, "$inc": bson.M{"version": 1}}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": id, "$and": bson.A{mergeStatusFilter(expectedFrom)}}))
	res, err := s.runs.UpdateOne(ctx, filter, update)
	if err != nil {
		return false, fmt.Errorf("store/mongo: update merge if %s: %w", id, err)
	}
	return res.MatchedCount > 0, nil
}

// FailQueuedRunIfAttempt is the queue-attempt-aware counterpart to the
// status-only CAS above. queued_at and status are matched in the SAME Mongo
// update so a concurrent resume cannot slip a newer queued attempt between a
// read and the failure write.
func (s *Store) FailQueuedRunIfAttempt(ctx context.Context, id, runErr string, publishedAt time.Time) (bool, error) {
	if publishedAt.IsZero() {
		return false, fmt.Errorf("store/mongo: fail queued attempt %s without published_at", id)
	}
	now := time.Now().UTC()
	filter := notDeleted(withTenantFilter(ctx, bson.M{
		"_id":    id,
		"status": store.RunStatusQueued,
		"$or": bson.A{
			bson.M{"queued_at": bson.M{"$lte": publishedAt}},
			// Legacy queued documents predate QueuedAt. They have no newer
			// attempt marker, so the delivery is the best available identity.
			bson.M{"queued_at": bson.M{"$exists": false}},
			bson.M{"queued_at": nil},
		},
	}))
	// Queue-park classification is follow-up; the empty code reads as
	// unknown, which is honest here.
	set, unset := runStatusUpdate(store.RunStatusFailedResumable, runErr, "", now)
	update := bson.M{"$set": set, "$inc": bson.M{"version": 1}}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	res, err := s.runs.UpdateOne(ctx, filter, update)
	if err != nil {
		return false, fmt.Errorf("store/mongo: fail queued attempt %s: %w", id, err)
	}
	return res.MatchedCount > 0, nil
}

var _ store.QueuedAttemptStore = (*Store)(nil)

// SaveCheckpoint writes the checkpoint document and bumps CAS. Plan
// §F T-33 layers an explicit version-conditional update on top; this
// method is the simple "no contention" form used by the engine itself.
func (s *Store) SaveCheckpoint(ctx context.Context, id string, cp *store.Checkpoint) error {
	// Same pointer discipline as runStatusUpdate (mirrors the FS
	// twin): a checkpoint carrying interaction evidence may only land
	// while the run's status carries it — otherwise a stale in-memory
	// copy is being replayed. The status read costs one projection and
	// only fires when the checkpoint actually carries a pointer (the
	// rare pause-adjacent writes; ordinary boundary writes skip it).
	if cp != nil && (cp.InteractionID != "" || len(cp.InteractionQuestions) > 0) {
		var cur struct {
			Status store.RunStatus `bson:"status"`
		}
		if ferr := s.runs.FindOne(ctx, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})),
			options.FindOne().SetProjection(bson.M{"status": 1})).Decode(&cur); ferr == nil &&
			!cur.Status.CarriesPausePointer() {
			c := *cp
			c.InteractionID = ""
			c.InteractionQuestions = nil
			cp = &c
		}
	}
	update := bson.M{
		"$set": bson.M{
			"checkpoint": cp,
			"updated_at": time.Now().UTC(),
		},
		"$inc": bson.M{"version": 1},
	}
	return mongoutil.UpdateOneChecked(ctx, s.runs, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})), update,
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
		"$inc": bson.M{"version": 1},
		// A paused run carries no failure classification — same
		// discipline as runStatusUpdate, which this checkpoint-coupled
		// write bypasses.
		"$unset": bson.M{"finished_at": "", "failure_code": ""},
	}
	return mongoutil.UpdateOneChecked(ctx, s.runs, notDeleted(withTenantFilter(ctx, bson.M{"_id": id})), update,
		fmt.Errorf("store/mongo: run %s not found", id), fmt.Sprintf("store/mongo: pause %s", id))
}

// failRunCheckpointed is the shared body of FailRunResumable and
// FailRunTerminal: the shared transition $set (which owns the
// failure-code discipline) plus the checkpoint, guarded by the atomic
// cancelled-wins filter — an operator cancel is terminal and outranks a
// failure racing in behind it, and the failure would win simply by
// writing last, auto-resuming a run somebody deliberately stopped.
func (s *Store) failRunCheckpointed(ctx context.Context, id string, status store.RunStatus, cp *store.Checkpoint, runErr string, code store.FailureCode, opName string) error {
	set, unset := runStatusUpdate(status, runErr, code, time.Now().UTC())
	// The whole-checkpoint $set below conflicts with the dotted pointer
	// $unset — apply the same consumption to the VALUE instead. (Engine
	// failure boundaries never set a pointer; this guards the preserved-
	// checkpoint callers.)
	delete(unset, "checkpoint.interaction_id")
	delete(unset, "checkpoint.interaction_questions")
	if cp != nil && !status.CarriesPausePointer() &&
		(cp.InteractionID != "" || len(cp.InteractionQuestions) > 0) {
		consumed := *cp
		consumed.InteractionID = ""
		consumed.InteractionQuestions = nil
		cp = &consumed
	}
	set["checkpoint"] = cp
	update := bson.M{"$set": set, "$inc": bson.M{"version": 1}}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	filter := notDeleted(withTenantFilter(ctx, bson.M{"_id": id, "status": bson.M{"$ne": store.RunStatusCancelled}}))
	res, err := s.runs.UpdateOne(ctx, filter, update)
	if err != nil {
		return fmt.Errorf("store/mongo: %s %s: %w", opName, id, err)
	}
	if res.MatchedCount == 0 {
		// Either the run is gone (absent or tombstoned — LoadRun's
		// typed error says which), or it is already cancelled and
		// stays so.
		if _, gerr := s.LoadRun(ctx, id); gerr == nil {
			return nil
		} else {
			return fmt.Errorf("store/mongo: %s %s: %w", opName, id, gerr)
		}
	}
	return nil
}

// FailRunResumable writes the checkpoint, flips status to
// failed_resumable, and records the failure reason + code. Resume can
// then re-pick up at NodeID without replaying upstream work.
func (s *Store) FailRunResumable(ctx context.Context, id string, cp *store.Checkpoint, runErr string, code store.FailureCode) error {
	return s.failRunCheckpointed(ctx, id, store.RunStatusFailedResumable, cp, runErr, code, "fail resumable")
}

// FailRunTerminal writes the checkpoint, flips status to failed, and
// records the failure reason + code. The run is terminal — no
// auto-resume — but the checkpoint is preserved so the operator can
// still rewind it explicitly.
func (s *Store) FailRunTerminal(ctx context.Context, id string, cp *store.Checkpoint, runErr string, code store.FailureCode) error {
	return s.failRunCheckpointed(ctx, id, store.RunStatusFailed, cp, runErr, code, "fail terminal")
}
