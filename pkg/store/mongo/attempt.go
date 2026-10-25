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
	meta, body, err := store.EncodeAttempt(a)
	if err != nil {
		return meta, err
	}
	if _, err := s.LoadRun(ctx, runID); err != nil {
		return meta, err
	}
	if err := s.blob.PutAttachment(ctx, runID, "attempts", meta.ID+".json", "application/json", body); err != nil {
		return meta, err
	}
	_, err = s.runs.UpdateOne(ctx, withTenantFilter(ctx, bson.M{"_id": runID}), versionRunUpdate(bson.M{"$addToSet": bson.M{"attempts": meta}}))
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
	if _, err := s.LoadRun(ctx, runID); err != nil {
		return nil, err
	}
	r, _, err := s.blob.GetAttachment(ctx, runID, "attempts", id+".json")
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
