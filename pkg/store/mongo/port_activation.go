package mongo

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

type portActivationDocument struct {
	ID                   string `bson:"_id"`
	store.PortActivation `bson:",inline"`
}

func (s *Store) portActivationCollection() *mongo.Collection {
	journal := true
	return s.db.Collection("activation_ports_v1", options.Collection().
		SetWriteConcern(&writeconcern.WriteConcern{W: "majority", Journal: &journal}).
		SetReadConcern(readconcern.Majority()))
}

func (s *Store) LoadPortActivation(ctx context.Context) (*store.PortActivation, error) {
	var doc portActivationDocument
	err := s.portActivationCollection().FindOne(ctx, bson.M{"_id": "current"}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := doc.PortActivation.Validate(); err != nil {
		return nil, err
	}
	return &doc.PortActivation, nil
}

func (s *Store) SavePortActivation(ctx context.Context, expectedRevision uint64, next *store.PortActivation) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if next.Revision != expectedRevision+1 || next.RefreshLease != nil {
		return fmt.Errorf("%w: activation revision", store.ErrRunConflict)
	}
	doc := portActivationDocument{ID: "current", PortActivation: *next}
	if expectedRevision == 0 {
		_, err := s.portActivationCollection().InsertOne(ctx, doc)
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("%w: activation already exists", store.ErrRunConflict)
		}
		return err
	}
	result, err := s.portActivationCollection().ReplaceOne(ctx, bson.M{"_id": "current", "version": store.PortActivationVersion, "policy_revision": expectedRevision}, doc)
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return fmt.Errorf("%w: activation revision changed", store.ErrRunConflict)
	}
	return nil
}

// Disable only predicates on operator policy. Freshness updates cannot cause
// contention, and the atomic update preserves the most recent observation.
// A concurrent operator action still produces a meaningful policy conflict.
func (s *Store) DisablePortActivation(ctx context.Context, expectedPolicy uint64) (*store.PortActivation, error) {
	if expectedPolicy == 0 || expectedPolicy >= math.MaxInt64 {
		return nil, fmt.Errorf("%w: invalid policy revision", store.ErrRunConflict)
	}
	return s.updatePortActivation(ctx,
		bson.M{"_id": "current", "version": store.PortActivationVersion, "policy_revision": expectedPolicy},
		bson.M{"$set": bson.M{"enabled": false}, "$inc": bson.M{"policy_revision": 1}, "$unset": bson.M{"refresh_lease": ""}})
}

func (s *Store) ClaimPortActivationRefresh(ctx context.Context, policy uint64, lease store.PortActivationRefreshLease) (*store.PortActivation, error) {
	now := time.Now().UTC()
	if policy == 0 || policy > math.MaxInt64 || lease.Validate() != nil ||
		!now.Before(lease.ExpiresAt) || lease.ExpiresAt.After(now.Add(store.PortDistributedProofMaxAge)) {
		return nil, fmt.Errorf("%w: invalid authority refresh lease", store.ErrPortActivation)
	}
	// The database clock checks the lease at the actual write, including a
	// request delayed in transit. A paused old owner cannot write with an
	// expired lease even before its successor installs a higher fence.
	filter := bson.M{
		"_id": "current", "version": store.PortActivationVersion, "policy_revision": policy,
		"enabled": true, "scope": store.PortActivationDistributed,
		"$or": bson.A{
			bson.M{"refresh_lease": bson.M{"$exists": false}},
			bson.M{"refresh_lease.token": bson.M{"$lt": lease.Token}},
		},
		"$expr": bson.M{"$and": bson.A{
			bson.M{"$gt": bson.A{lease.ExpiresAt, "$$NOW"}},
			bson.M{"$lte": bson.A{lease.ExpiresAt, bson.M{"$add": bson.A{"$$NOW", store.PortDistributedProofMaxAge.Milliseconds()}}}},
		}},
	}
	return s.updatePortActivation(ctx, filter, bson.M{"$set": bson.M{"refresh_lease": lease}})
}

func (s *Store) RenewPortActivation(ctx context.Context, renewal store.PortActivationRenewal) (*store.PortActivation, error) {
	if err := renewal.Validate(time.Now().UTC()); err != nil {
		return nil, err
	}
	filter := bson.M{
		"_id": "current", "version": store.PortActivationVersion,
		"policy_revision": renewal.PolicyRevision, "proof_revision": renewal.ProofRevision,
		"proof_digest": renewal.ProofDigest, "enabled": true, "scope": store.PortActivationDistributed,
		"refresh_lease.owner": renewal.Lease.Owner, "refresh_lease.token": renewal.Lease.Token,
		"refresh_lease.expires_at": renewal.Lease.ExpiresAt,
		"verified_at":              bson.M{"$lte": renewal.VerifiedAt},
		"$expr": bson.M{"$and": bson.A{
			bson.M{"$gt": bson.A{"$refresh_lease.expires_at", "$$NOW"}},
			bson.M{"$gt": bson.A{renewal.ExpiresAt, "$$NOW"}},
			bson.M{"$lte": bson.A{renewal.VerifiedAt, "$$NOW"}},
		}},
	}
	return s.updatePortActivation(ctx, filter, bson.M{
		"$set": bson.M{"verified_at": renewal.VerifiedAt, "expires_at": renewal.ExpiresAt},
		"$inc": bson.M{"proof_revision": 1},
	})
}

func (s *Store) updatePortActivation(ctx context.Context, filter, update bson.M) (*store.PortActivation, error) {
	var doc portActivationDocument
	err := s.portActivationCollection().FindOneAndUpdate(ctx, filter, update,
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, fmt.Errorf("%w: activation policy, proof or authority lease changed", store.ErrRunConflict)
	}
	if err != nil {
		return nil, err
	}
	if err := doc.PortActivation.Validate(); err != nil {
		return nil, err
	}
	return &doc.PortActivation, nil
}
