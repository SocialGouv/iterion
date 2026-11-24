package mongo

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/store/blob"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Snapshots share the run's blob lifecycle (including deletion), but do not
// consume BSON document space or appear as workflow artifacts.
func (s *Store) RecordAttempt(ctx context.Context, runID string, a *store.AttemptSnapshot) (store.RunAttempt, error) {
	ctx, objects, err := s.captureRunObjects(ctx, runID)
	if err != nil {
		return store.RunAttempt{}, err
	}
	meta, body, err := store.EncodeAttempt(a)
	if err != nil {
		return meta, err
	}
	if _, err := s.LoadRun(ctx, runID); err != nil {
		return meta, err
	}
	if err := objects.PutAttachment(ctx, runID, "attempts", meta.ID+".json", "application/json", body); err != nil {
		return meta, err
	}
	err = s.withRunMetadataWrite(ctx, runID, func(sc context.Context, parent *store.Run) error {
		if parent == nil {
			return store.ErrRunNotFound
		}
		_, e := s.runs.UpdateOne(sc, withTenantFilter(sc, bson.M{"_id": runID}), bson.M{"$addToSet": bson.M{"attempts": meta}})
		return e
	})
	return meta, err
}

func (s *Store) LoadAttempt(ctx context.Context, runID, id string) (*store.AttemptSnapshot, error) {
	if !store.ValidAttemptID(id) {
		return nil, fmt.Errorf("invalid attempt id")
	}
	// The owning run mediates every blob read, including an operator supplying
	// a digest from another tenant. Blob topology grants no authority.
	if _, ok := store.TenantFromContext(ctx); !ok && !store.IsWithoutTenantFilter(ctx) {
		return nil, fmt.Errorf("attempt read requires a tenant")
	}
	parent, err := s.LoadRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	ctx, objects, err := s.runObjectsFromRun(ctx, parent)
	if err != nil {
		return nil, err
	}
	r, _, err := objects.GetAttachment(ctx, runID, "attempts", id+".json")
	if errors.Is(err, blob.ErrArtifactNotFound) {
		return nil, store.ErrAttemptNotFound
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	return store.DecodeAttempt(id, body)
}
