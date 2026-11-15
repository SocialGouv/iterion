package mongo

import (
	"context"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"time"
)

type valueDeletion struct {
	Format store.ValueFormat
	At     time.Time
}

const valueCleanupPendingField = "value_cleanup_pending"

// The original format and deleted_at identify this cleanup. Pending tombstones
// cannot be pruned. An old tombstone with no pending field already completed
// the historical cleanup-before-tombstone protocol.
func (s *Store) beginValueDeletion(ctx context.Context, id string) (valueDeletion, bool, error) {
	var target valueDeletion
	pending := false
	original, _, err := s.readRunFormatMetadata(ctx, id)
	if err != nil {
		return target, false, err
	}
	if original == nil {
		original = &store.Run{ID: id, FormatVersion: 1}
	}
	if _, err = store.CheckRunFormatReplacement(ctx, original, original); err != nil {
		return target, false, err
	}
	originalFormat, err := store.CaptureRunValueFormat(original)
	if err != nil {
		return target, false, err
	}
	session, err := s.client.StartSession()
	if err != nil {
		return target, false, err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		target = valueDeletion{}
		pending = false
		r, raw, err := s.readRunFormatMetadata(sc, id)
		if err == nil && r == nil {
			if originalFormat.Format != store.LegacyValueFormat {
				return nil, store.ErrValueFormatChanged
			}
			r = &store.Run{ID: id, FormatVersion: 1}
			if _, e := store.CheckRunFormatReplacement(sc, r, r); e != nil {
				return nil, e
			}
			target = valueDeletion{store.LegacyValueFormat, time.Now().UTC().Truncate(time.Millisecond)}
			pending = true
			doc := bson.M{"_id": id, "format_version": 1, "version": 1, "deleted_at": target.At, "status": "deleted", "updated_at": target.At, valueCleanupPendingField: true}
			if tenant, ok := store.TenantFromContext(sc); ok {
				doc["tenant_id"] = tenant
			}
			_, e := s.runs.InsertOne(sc, doc)
			return nil, e
		}
		if err != nil {
			return nil, err
		}
		current, err := store.CaptureRunValueFormat(r)
		if err != nil {
			return nil, err
		}
		if current != originalFormat {
			return nil, store.ErrValueFormatChanged
		}
		f := current.Format
		target.Format = f
		if r.DeletedAt != nil {
			target.At = *r.DeletedAt
			v := raw.Lookup(valueCleanupPendingField)
			if v.Type != 0 {
				var ok bool
				pending, ok = v.BooleanOK()
				if !ok {
					return nil, fmt.Errorf("store/mongo: invalid cleanup state")
				}
			}
			return nil, nil
		}
		target.At = time.Now().UTC().Truncate(time.Millisecond)
		pending = true
		update := bson.M{"$set": bson.M{"deleted_at": target.At, "status": "deleted", "updated_at": target.At, valueCleanupPendingField: true}, "$unset": bson.M{"checkpoint": "", "inputs": "", "inputs_pr_url": "", "launch_env": "", "model_overrides": "", "launch_model_overrides": "", "budget": "", "loop_overrides": "", "budget_raises": "", "attachments": "", "watched_issue_ids": "", "pre_engine_refusal": ""}}
		filter := notDeleted(withTenantFilter(sc, bson.M{"_id": id, "$and": bson.A{valueFormatPredicate(f), correctionVersionFilter(r.CASVersion)}}))
		resultUpdate, err := s.runs.UpdateOne(sc, filter, versionRunUpdate(update))
		if err != nil {
			return nil, err
		}
		if resultUpdate.MatchedCount != 1 {
			return nil, store.ErrRunConflict
		}
		return nil, nil
	})
	return target, pending, err
}
func valueDeletionFilter(id string, target valueDeletion) bson.M {
	return bson.M{"_id": id, "deleted_at": target.At, "$and": bson.A{valueFormatPredicate(target.Format)}, valueCleanupPendingField: true}
}
func (s *Store) completeValueDeletion(ctx context.Context, id string, target valueDeletion, cleanup func(context.Context) error) error {
	session, err := s.client.StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(sc context.Context) (any, error) {
		changed, err := s.runs.UpdateOne(sc, withTenantFilter(sc, valueDeletionFilter(id, target)), versionRunUpdate(bson.M{"$set": bson.M{valueCleanupPendingField: false}}))
		if err != nil {
			return nil, err
		}
		if changed.MatchedCount != 1 {
			// A duplicate delivery may finish after its peer. It must not sweep any
			// child unless it still owns this tombstone's pending cleanup.
			count, e := s.runs.CountDocuments(sc, withTenantFilter(sc, bson.M{"_id": id, "deleted_at": target.At, "$and": bson.A{valueFormatPredicate(target.Format)}, valueCleanupPendingField: false}))
			if e != nil {
				return nil, e
			}
			if count == 1 {
				return nil, nil
			}
			return nil, store.ErrRunConflict
		}
		return nil, cleanup(sc)
	})
	return err
}
func (s *Store) pruneValueDeletionMarkers(ctx context.Context, cutoff time.Time) (int, error) {
	cur, err := s.runs.Aggregate(ctx, mongo.Pipeline{{{Key: "$match", Value: withTenantFilter(ctx, bson.M{"deleted_at": bson.M{"$exists": true, "$lt": cutoff}, valueCleanupPendingField: bson.M{"$ne": true}})}}, {{Key: "$project", Value: runFormatMetadataProjection()}}})
	if err != nil {
		return 0, err
	}
	defer cur.Close(ctx)
	reaped := 0
	for cur.Next(ctx) {
		raw := cur.Current
		r, err := decodeRunFormatMetadata(ctx, raw)
		if err != nil {
			return reaped, err
		}
		f, err := store.NormalizeValueFormat(r.FormatVersion)
		if err != nil {
			return reaped, err
		}
		if r.DeletedAt == nil {
			continue
		}
		v := raw.Lookup(valueCleanupPendingField)
		if v.Type != 0 {
			p, ok := v.BooleanOK()
			if !ok {
				return reaped, fmt.Errorf("store/mongo: invalid cleanup state")
			}
			if p {
				continue
			}
		}
		changed, err := s.runs.DeleteOne(ctx, withTenantFilter(ctx, bson.M{"_id": r.ID, "deleted_at": *r.DeletedAt, "$and": bson.A{valueFormatPredicate(f)}, valueCleanupPendingField: bson.M{"$ne": true}}))
		if err != nil {
			return reaped, err
		}
		if changed.DeletedCount == 0 {
			current, _, e := s.readRunFormatMetadata(ctx, r.ID)
			if e != nil {
				return reaped, e
			}
			if current != nil {
				cf, e := store.NormalizeValueFormat(current.FormatVersion)
				if e != nil {
					return reaped, e
				}
				if cf != f {
					return reaped, store.ErrValueFormatChanged
				}
			}
		}
		reaped += int(changed.DeletedCount)
	}
	return reaped, cur.Err()
}
