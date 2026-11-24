package mongo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
)

// The cloud (Mongo) store satisfies the plan-snapshot seam so the
// studio Plans panel is fed identically in cloud and local mode.
var _ store.PlanStore = (*Store)(nil)

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

// AppendPlanSnapshot freezes its input, then deduplicates, seeds the counter
// from the committed tail and publishes under one parent-WRITE transaction.
// A no-op aborts that transaction, so neither parent nor counter advances.
func (s *Store) AppendPlanSnapshot(ctx context.Context, runID string, snap store.PlanSnapshot) (store.PlanSnapshot, bool, error) {
	snap.Todos = slices.Clone(snap.Todos)
	newTodos, err := json.Marshal(snap.Todos)
	if err != nil {
		return store.PlanSnapshot{}, false, fmt.Errorf("store/mongo: marshal plan todos: %w", err)
	}
	if snap.Timestamp.IsZero() {
		snap.Timestamp = time.Now().UTC()
	}
	tenantID, _ := store.TenantFromContext(ctx)
	var out store.PlanSnapshot
	var inserted bool
	err = s.withRunMetadataWrite(ctx, runID, func(sc context.Context, parent *store.Run) error {
		n, e := s.streamNamespace(sc, runID, parent)
		if e != nil {
			return e
		}
		out = store.PlanSnapshot{}
		inserted = false
		prev, hasPrev, err := s.lastStreamPlanSnapshot(sc, n)
		if err != nil {
			return err
		}
		floor := int64(0)
		if hasPrev {
			if prevTodos, e := json.Marshal(prev.Todos); e == nil && bytes.Equal(prevTodos, newTodos) {
				out = prev
				return errValuePublicationNoop
			}
			if int64(prev.Seq) == math.MaxInt64 {
				return fmt.Errorf("store/mongo: plan sequence exhausted")
			}
			floor = int64(prev.Seq) + 1
		}
		if err = s.seedStreamSeqField(sc, n, planSeqField, floor); err != nil {
			return err
		}
		seq, err := s.allocStreamSeqField(sc, n, planSeqField)
		if err != nil {
			return err
		}
		next := snap
		next.Seq = int(seq)
		doc := runPlanDoc{TenantID: tenantID, RunID: runID, Seq: next.Seq, NodeID: next.NodeID, Iteration: next.Iteration, Tool: next.Tool, Timestamp: next.Timestamp, Todos: next.Todos}
		raw, e := bson.Marshal(doc)
		if e != nil {
			return e
		}
		row := bson.M{}
		if e = bson.Unmarshal(raw, &row); e != nil {
			return e
		}
		n.stamp(row)
		if _, err = n.plans.InsertOne(sc, row); err != nil {
			return fmt.Errorf("store/mongo: insert plan snapshot %s/%d: %w", runID, seq, err)
		}
		out = next
		inserted = true
		return nil
	})
	if err != nil {
		return store.PlanSnapshot{}, false, err
	}
	return out, inserted, nil
}

// lastStreamPlanSnapshot returns the selected namespace's highest-seq snapshot,
// or (_, false, nil) when that namespace has no plans.
func (s *Store) lastStreamPlanSnapshot(ctx context.Context, n streamDataNamespace) (store.PlanSnapshot, bool, error) {
	var doc runPlanDoc
	e := n.plans.FindOne(ctx, n.filter(ctx, nil), options.FindOne().SetSort(bson.D{{Key: "seq", Value: -1}})).Decode(&doc)
	if e == mongo.ErrNoDocuments {
		return store.PlanSnapshot{}, false, nil
	}
	if e != nil {
		return store.PlanSnapshot{}, false, e
	}
	return doc.toSnapshot(), true, nil
}
func (s *Store) ListPlanSnapshots(ctx context.Context, runID string) ([]store.PlanSnapshot, error) {
	var out []store.PlanSnapshot
	err := s.withOpaqueStreamRead(ctx, runID, func(sc context.Context, n streamDataNamespace, _ *store.Run) error {
		out = []store.PlanSnapshot{}
		cur, e := n.plans.Find(sc, n.filter(sc, nil), options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}))
		if e != nil {
			return e
		}
		defer cur.Close(sc)
		for cur.Next(sc) {
			var doc runPlanDoc
			if e = cur.Decode(&doc); e != nil {
				return e
			}
			out = append(out, doc.toSnapshot())
		}
		return cur.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
