package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The cloud (Mongo) store satisfies the plan-snapshot seam so the
// studio Plans panel is fed identically in cloud and local mode.
var _ store.PlanStore = (*Store)(nil)

// appendPlanMaxRetries caps the seq-collision retry loop for plan
// snapshots. Mirrors appendEventMaxRetries: parallel branches firing
// TodoWrite/todo_write concurrently can race on the next per-run
// sequence, and the (run_id, seq) unique index is the safety net that
// forces a re-read + realloc rather than a silent collapse.
const appendPlanMaxRetries = 10

// runPlanDoc is one persisted plan snapshot, one document per (run_id,
// seq). The cloud twin of the filesystem store's runs/<id>/plans/<NNNN>.json:
// the runner captures each snapshot as an agent's TodoWrite/todo_write
// fires, the server pod reads them back to render the studio Plans panel
// for a run whose worktree is gone. Tenant-stamped like run_gitmeta so a
// tenant only ever reads its own snapshots.
type runPlanDoc struct {
	TenantID  string           `bson:"tenant_id,omitempty"`
	RunID     string           `bson:"run_id"`
	Seq       int              `bson:"seq"`
	NodeID    string           `bson:"node_id,omitempty"`
	Iteration int              `bson:"iteration,omitempty"`
	Tool      string           `bson:"tool,omitempty"`
	Timestamp time.Time        `bson:"ts"`
	Todos     []store.PlanTodo `bson:"todos"`
}

// toSnapshot converts the persisted document back to the wire-shape
// store.PlanSnapshot the HTTP surface serves.
func (d runPlanDoc) toSnapshot() store.PlanSnapshot {
	return store.PlanSnapshot{
		Seq:       d.Seq,
		NodeID:    d.NodeID,
		Iteration: d.Iteration,
		Tool:      d.Tool,
		Timestamp: d.Timestamp,
		Todos:     d.Todos,
	}
}

// AppendPlanSnapshot implements store.PlanStore over the run_plans
// collection. It assigns snap.Seq itself (callers leave it zero) and
// DEDUPS against the immediately-previous snapshot: when the incoming
// Todos are byte-identical (via the same JSON marshal the filesystem
// impl uses) nothing is written and (previous, false, nil) is returned —
// TodoWrite fires often with no change. Otherwise the snapshot is
// inserted at the next sequence and (snap, true, nil) is returned.
//
// The (run_id, seq) unique index guards two parallel branches racing on
// the next seq: a duplicate-key error means a sibling grabbed it first,
// so we re-read the tail and realloc with a jittered backoff (mirroring
// AppendEvent), rather than surfacing a hard failure.
func (s *Store) AppendPlanSnapshot(ctx context.Context, runID string, snap store.PlanSnapshot) (store.PlanSnapshot, bool, error) {
	newTodos, err := json.Marshal(snap.Todos)
	if err != nil {
		return store.PlanSnapshot{}, false, fmt.Errorf("store/mongo: marshal plan todos: %w", err)
	}
	if snap.Timestamp.IsZero() {
		snap.Timestamp = time.Now().UTC()
	}
	tenantID, _ := store.TenantFromContext(ctx)

	var lastErr error
	for attempt := 0; attempt < appendPlanMaxRetries; attempt++ {
		if attempt > 0 {
			if err := backoffOrCancel(ctx, attempt); err != nil {
				return store.PlanSnapshot{}, false, err
			}
		}

		prev, hasPrev, perr := s.lastPlanSnapshot(ctx, runID)
		if perr != nil {
			return store.PlanSnapshot{}, false, perr
		}
		nextSeq := 0
		if hasPrev {
			if prevTodos, merr := json.Marshal(prev.Todos); merr == nil && bytes.Equal(prevTodos, newTodos) {
				return prev, false, nil
			}
			nextSeq = prev.Seq + 1
		}

		snap.Seq = nextSeq
		doc := runPlanDoc{
			TenantID:  tenantID,
			RunID:     runID,
			Seq:       nextSeq,
			NodeID:    snap.NodeID,
			Iteration: snap.Iteration,
			Tool:      snap.Tool,
			Timestamp: snap.Timestamp,
			Todos:     snap.Todos,
		}
		if _, err := s.runPlans.InsertOne(ctx, doc); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				// A sibling branch grabbed this seq; re-read the tail and
				// realloc after a brief jittered pause.
				lastErr = err
				continue
			}
			return store.PlanSnapshot{}, false, fmt.Errorf("store/mongo: insert plan snapshot %s/%d: %w", runID, nextSeq, err)
		}
		return snap, true, nil
	}
	return store.PlanSnapshot{}, false, fmt.Errorf("store/mongo: race on plan seq for run %s after %d attempts: %w", runID, appendPlanMaxRetries, lastErr)
}

// lastPlanSnapshot returns the highest-seq snapshot for the run (the
// dedup anchor + seq source), or (_, false, nil) when the run has none.
func (s *Store) lastPlanSnapshot(ctx context.Context, runID string) (store.PlanSnapshot, bool, error) {
	filter := withTenantFilter(ctx, bson.M{"run_id": runID})
	opts := options.FindOne().SetSort(bson.D{{Key: "seq", Value: -1}})
	var doc runPlanDoc
	if err := s.runPlans.FindOne(ctx, filter, opts).Decode(&doc); err != nil {
		if err == mongo.ErrNoDocuments {
			return store.PlanSnapshot{}, false, nil
		}
		return store.PlanSnapshot{}, false, fmt.Errorf("store/mongo: load last plan snapshot %s: %w", runID, err)
	}
	return doc.toSnapshot(), true, nil
}

// ListPlanSnapshots implements store.PlanStore: every persisted snapshot
// for the run in ascending Seq (chronological) order. A run with no
// snapshots yields (nil, nil).
func (s *Store) ListPlanSnapshots(ctx context.Context, runID string) ([]store.PlanSnapshot, error) {
	filter := withTenantFilter(ctx, bson.M{"run_id": runID})
	opts := options.Find().SetSort(bson.D{{Key: "seq", Value: 1}})
	cur, err := s.runPlans.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("store/mongo: list plan snapshots %s: %w", runID, err)
	}
	defer cur.Close(ctx)

	var out []store.PlanSnapshot
	for cur.Next(ctx) {
		var doc runPlanDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("store/mongo: decode plan snapshot %s: %w", runID, err)
		}
		out = append(out, doc.toSnapshot())
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("store/mongo: iterate plan snapshots %s: %w", runID, err)
	}
	return out, nil
}
