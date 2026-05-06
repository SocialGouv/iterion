package mongo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
)

// WriteArtifact PUTs the body to S3 first, then inserts the
// `artifact_written` event in Mongo (plan §D.6 ordering rule). The
// blob is the source of truth for body content; the event is the
// source of truth for "this version exists". A blob-without-event is
// tolerated — a re-run writes the same key, idempotently.
//
// Plan §F T-18.
func (s *Store) WriteArtifact(ctx context.Context, a *store.Artifact) error {
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("store/mongo: marshal artifact: %w", err)
	}

	// Step 1: PUT to S3. Idempotent under the canonical key so a retry
	// after a partial failure simply rewrites the same bytes.
	if err := s.blob.PutArtifact(ctx, a.RunID, a.NodeID, a.Version, body); err != nil {
		return fmt.Errorf("store/mongo: blob put %s/%s/%d: %w", a.RunID, a.NodeID, a.Version, err)
	}

	// Step 2: append the artifact_written event so observers (run
	// console, change-stream subscribers) learn about the new version.
	// AppendEvent allocates the seq + writes to events; we don't
	// double-write the body here.
	_, err = s.AppendEvent(ctx, a.RunID, store.Event{
		Type:   store.EventArtifactWritten,
		RunID:  a.RunID,
		NodeID: a.NodeID,
		Data: map[string]interface{}{
			"version":    a.Version,
			"written_at": a.WrittenAt,
		},
	})
	if err != nil {
		return fmt.Errorf("store/mongo: append artifact event: %w", err)
	}

	// Step 3: maintain run.artifact_index so LoadLatestArtifact has a
	// fast path. Best-effort — the event is the canonical record.
	_, _ = s.runs.UpdateOne(
		ctx,
		bson.M{"_id": a.RunID},
		bson.M{"$set": bson.M{
			fmt.Sprintf("artifact_index.%s", a.NodeID): a.Version,
			"updated_at": a.WrittenAt,
		}},
	)
	return nil
}

// LoadArtifact fetches the body from S3 and returns it as a
// *store.Artifact. Mongo isn't consulted: the blob is authoritative.
func (s *Store) LoadArtifact(ctx context.Context, runID, nodeID string, version int) (*store.Artifact, error) {
	body, err := s.blob.GetArtifact(ctx, runID, nodeID, version)
	if err != nil {
		if errors.Is(err, blob.ErrArtifactNotFound) {
			return nil, fmt.Errorf("store/mongo: artifact %s/%s/v%d not found", runID, nodeID, version)
		}
		return nil, fmt.Errorf("store/mongo: blob get %s/%s/%d: %w", runID, nodeID, version, err)
	}
	var art store.Artifact
	if err := json.Unmarshal(body, &art); err != nil {
		return nil, fmt.Errorf("store/mongo: unmarshal artifact %s/%s/v%d: %w", runID, nodeID, version, err)
	}
	return &art, nil
}

// LoadLatestArtifact uses the run.artifact_index fast path when it's
// present, falling back to a blob LIST when the index is empty (e.g.
// for a run created before the index was populated).
func (s *Store) LoadLatestArtifact(ctx context.Context, runID, nodeID string) (*store.Artifact, error) {
	r, err := s.LoadRun(ctx, runID)
	if err == nil && r.ArtifactIndex != nil {
		if v, ok := r.ArtifactIndex[nodeID]; ok {
			return s.LoadArtifact(ctx, runID, nodeID, v)
		}
	}

	versions, err := s.blob.ListArtifactVersions(ctx, runID, nodeID)
	if err != nil {
		if errors.Is(err, blob.ErrArtifactNotFound) {
			return nil, fmt.Errorf("store/mongo: no artifacts for %s/%s", runID, nodeID)
		}
		return nil, fmt.Errorf("store/mongo: list artifact versions %s/%s: %w", runID, nodeID, err)
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("store/mongo: no artifacts for %s/%s", runID, nodeID)
	}
	sort.Ints(versions)
	return s.LoadArtifact(ctx, runID, nodeID, versions[len(versions)-1])
}

// ListArtifactVersions enumerates persisted versions of an artifact
// node. Reads from the blob LIST so the result matches what
// LoadArtifact can actually return.
func (s *Store) ListArtifactVersions(ctx context.Context, runID, nodeID string) ([]store.ArtifactVersionInfo, error) {
	versions, err := s.blob.ListArtifactVersions(ctx, runID, nodeID)
	if err != nil {
		if errors.Is(err, blob.ErrArtifactNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("store/mongo: list versions %s/%s: %w", runID, nodeID, err)
	}
	sort.Ints(versions)

	// Fold artifact_written events to populate the WrittenAt
	// timestamps so callers get a consistent shape with the
	// filesystem store. A missing event is tolerated — we fall back
	// to zero time, matching the filesystem reader's behaviour.
	tsByVersion := map[int]any{}
	cur, err := s.events.Find(ctx, bson.M{
		"run_id":  runID,
		"node_id": nodeID,
		"type":    store.EventArtifactWritten,
	}, options.Find().SetProjection(bson.M{"data.version": 1, "ts": 1}))
	if err == nil {
		defer cur.Close(ctx)
		for cur.Next(ctx) {
			var doc struct {
				Data map[string]any `bson:"data"`
				TS   any            `bson:"ts"`
			}
			if err := cur.Decode(&doc); err != nil {
				continue
			}
			if v, ok := doc.Data["version"]; ok {
				switch n := v.(type) {
				case int32:
					tsByVersion[int(n)] = doc.TS
				case int64:
					tsByVersion[int(n)] = doc.TS
				case int:
					tsByVersion[n] = doc.TS
				case float64:
					tsByVersion[int(n)] = doc.TS
				}
			}
		}
	}

	out := make([]store.ArtifactVersionInfo, 0, len(versions))
	for _, v := range versions {
		info := store.ArtifactVersionInfo{Version: v}
		if t, ok := tsByVersion[v].(bson.DateTime); ok {
			info.WrittenAt = t.Time().UTC()
		}
		out = append(out, info)
	}
	return out, nil
}

// LockRun delegates to the configured LockProvider when present so a
// runner-side store gets a real distributed lease (NATS KV TTL +
// CAS), and falls back to a no-op lock for server-side instances
// where the runtime engine never executes runs and locking is
// pointless. The plan §F T-26 contract is "fail-fast on contention" —
// if the provider returns an error (the lease is held elsewhere),
// surface it to the caller so the engine refuses to proceed.
func (s *Store) LockRun(ctx context.Context, runID string) (store.RunLock, error) {
	if s.lockProv == nil {
		return noopLock{}, nil
	}
	lock, err := s.lockProv.AcquireLock(ctx, runID, s.lockProv.RunnerID())
	if err != nil {
		return nil, fmt.Errorf("store/mongo: acquire lock %s: %w", runID, err)
	}
	return lock, nil
}

type noopLock struct{}

func (noopLock) Unlock() error { return nil }

// Compile-time assertion: keep store.RunStore + blob.Client wires
// honest so a refactor on either side surfaces here at build time.
var (
	_ store.RunStore = (*Store)(nil)
	_ blob.Client    = (*blob.S3Client)(nil)
)
