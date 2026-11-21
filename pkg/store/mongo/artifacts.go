package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

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
	format, err := s.valueWriteFormat(ctx, a.RunID)
	if err != nil {
		return err
	}
	if a.WrittenAt.IsZero() {
		a.WrittenAt = time.Now().UTC()
	}
	body, err := store.EncodeValueRecordJSON(ctx, format, store.ExactArtifact, a)
	if err != nil {
		return err
	}
	evt := store.Event{Type: store.EventArtifactWritten, RunID: a.RunID, NodeID: a.NodeID, Timestamp: time.Now().UTC(), Data: map[string]any{"version": a.Version, "written_at": a.WrittenAt}, LogOffset: s.logPositionFor(a.RunID), ActiveMs: s.activeDurationFor(a.RunID)}
	stampTenantOnEvent(ctx, &evt)
	doc, err := valueRecordDocument(ctx, format, store.ExactEvent, &evt)
	if err != nil {
		return err
	}
	// The upload is deliberately outside the retryable transaction, using the
	// original namespace. A late upload cannot overwrite another format.
	if err = s.putValueArtifact(ctx, format, a.RunID, a.NodeID, a.Version, body); err != nil {
		return err
	}
	return s.withValueRecordWrite(ctx, a.RunID, func(sc context.Context, _ store.ValueFormat, parent *store.Run) error {
		if _, err := s.publishValueEvent(sc, a.RunID, doc); err != nil {
			return err
		}
		if parent == nil {
			return nil
		}
		_, err := s.runs.UpdateOne(sc, notDeleted(withTenantFilter(sc, bson.M{"_id": a.RunID})), versionRunUpdate(bson.M{"$max": bson.M{fmt.Sprintf("artifact_index.%s", a.NodeID): a.Version}, "$set": bson.M{"updated_at": a.WrittenAt}}))
		return err
	})
}

func (s *Store) LoadArtifact(ctx context.Context, runID, nodeID string, version int) (*store.Artifact, error) {
	format, err := s.valueReadFormat(ctx, runID)
	if err != nil {
		return nil, err
	}
	body, err := s.getValueArtifact(ctx, format, runID, nodeID, version)
	if err != nil {
		if errors.Is(err, blob.ErrArtifactNotFound) {
			return nil, fmt.Errorf("store/mongo: artifact %s/%s/v%d not found: %w", runID, nodeID, version, os.ErrNotExist)
		}
		return nil, fmt.Errorf("store/mongo: blob get %s/%s/%d: %w", runID, nodeID, version, err)
	}
	bound, err := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: runID, Format: format})
	if err != nil {
		return nil, err
	}
	if _, err = s.valueReadFormat(bound, runID); err != nil {
		return nil, err
	}
	var art store.Artifact
	if err := store.DecodeValueRecordJSON(ctx, format, store.ExactArtifact, body, &art); err != nil {
		return nil, fmt.Errorf("store/mongo: unmarshal artifact %s/%s/v%d: %w", runID, nodeID, version, err)
	}
	return &art, nil
}

// LoadLatestArtifact uses the run.artifact_index fast path when it's
// present, falling back to a blob LIST when the index is empty (e.g.
// for a run created before the index was populated).
func (s *Store) LoadLatestArtifact(ctx context.Context, runID, nodeID string) (*store.Artifact, error) {
	format, err := s.valueReadFormat(ctx, runID)
	if err != nil {
		return nil, err
	}
	ctx, err = store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: runID, Format: format})
	if err != nil {
		return nil, err
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil && !errors.Is(err, store.ErrRunNotFound) {
		return nil, err
	}
	if err == nil && r.ArtifactIndex != nil {
		if v, ok := r.ArtifactIndex[nodeID]; ok {
			return s.LoadArtifact(ctx, runID, nodeID, v)
		}
	}

	versions, err := s.listValueArtifacts(ctx, format, runID, nodeID)
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
	format, err := s.valueReadFormat(ctx, runID)
	if err != nil {
		return nil, err
	}
	versions, err := s.listValueArtifacts(ctx, format, runID, nodeID)
	if errors.Is(err, blob.ErrArtifactNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sort.Ints(versions)
	bound, err := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: runID, Format: format})
	if err != nil {
		return nil, err
	}
	if _, err = s.valueReadFormat(bound, runID); err != nil {
		return nil, err
	}
	// Data is a frame in2, so decode the complete selected event rather than
	// projecting a nonexistent data.version field from that frame.
	times := map[int]time.Time{}
	cur, err := s.events.Find(ctx, withTenantFilter(ctx, bson.M{"run_id": runID, "node_id": nodeID, "type": store.EventArtifactWritten}), options.Find())
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var evt store.Event
		if err = decodeValueBSON(ctx, format, store.ExactEvent, cur.Current, &evt); err != nil {
			if format == store.ExactValueFormat {
				return nil, err
			}
			continue
		}
		var v int
		switch n := evt.Data["version"].(type) {
		case int:
			v = n
		case int32:
			v = int(n)
		case int64:
			v = int(n)
		case float64:
			v = int(n)
		default:
			continue
		}
		times[v] = evt.Timestamp
	}
	if err = cur.Err(); err != nil {
		return nil, err
	}
	out := make([]store.ArtifactVersionInfo, 0, len(versions))
	for _, v := range versions {
		out = append(out, store.ArtifactVersionInfo{Version: v, WrittenAt: times[v]})
	}
	return out, nil
}

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
