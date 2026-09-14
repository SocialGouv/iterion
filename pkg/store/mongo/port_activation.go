package mongo

import (
	"bytes"
	"context"
	"encoding/json"
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

type portDistributedProofDocument struct {
	ID                         string `bson:"_id"`
	store.PortDistributedProof `bson:",inline"`
}

func (s *Store) portActivationCollection() *mongo.Collection {
	journal := true
	return s.db.Collection("activation_ports_v1", options.Collection().
		SetWriteConcern(&writeconcern.WriteConcern{W: "majority", Journal: &journal}).
		SetReadConcern(readconcern.Majority()))
}

func (s *Store) portDistributedProofCollection() *mongo.Collection {
	journal := true
	return s.db.Collection("activation_ports_proof_v1", options.Collection().
		SetWriteConcern(&writeconcern.WriteConcern{W: "majority", Journal: &journal}).
		SetReadConcern(readconcern.Majority()))
}

func (s *Store) LoadPortDistributedProof(ctx context.Context) (*store.PortDistributedProof, error) {
	return s.loadPortDistributedProof(ctx, "current")
}

func (s *Store) loadPortDistributedProof(ctx context.Context, id string) (*store.PortDistributedProof, error) {
	var doc portDistributedProofDocument
	err := s.portDistributedProofCollection().FindOne(ctx, bson.M{"_id": id}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return &doc.PortDistributedProof, nil
}

func (s *Store) LoadPortDistributedProofCandidate(ctx context.Context) (*store.PortDistributedProof, error) {
	return s.loadPortDistributedProof(ctx, "candidate")
}

func (s *Store) SavePortDistributedProofCandidate(ctx context.Context, proof *store.PortDistributedProof) error {
	if proof == nil {
		return fmt.Errorf("%w: invalid distributed proof candidate", store.ErrPortActivation)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, proof.Snapshot); err != nil {
		return fmt.Errorf("%w: invalid distributed proof candidate", store.ErrPortActivation)
	}
	proof.Snapshot = bytes.Clone(compact.Bytes())
	if err := proof.Validate(); err != nil {
		return err
	}
	doc := portDistributedProofDocument{ID: "candidate", PortDistributedProof: *proof}
	_, err := s.portDistributedProofCollection().ReplaceOne(ctx, bson.M{"_id": "candidate"}, doc, options.Replace().SetUpsert(true))
	return err
}

func (s *Store) ClearPortDistributedProofCandidate(ctx context.Context) error {
	_, err := s.portDistributedProofCollection().DeleteOne(ctx, bson.M{"_id": "candidate"})
	return err
}

// SavePortDistributedProof is intentionally a narrow authority write. The
// caller must be the trusted server path; database credentials remain within
// the deployment administrative trust boundary.
func (s *Store) SavePortDistributedProof(ctx context.Context, expectedPolicy uint64, proof *store.PortDistributedProof) error {
	if expectedPolicy >= math.MaxInt64 || proof == nil || (expectedPolicy == 0 && proof.PolicyRevision != 1) ||
		(expectedPolicy != 0 && proof.PolicyRevision != expectedPolicy && proof.PolicyRevision != expectedPolicy+1) {
		return fmt.Errorf("%w: invalid distributed proof write", store.ErrPortActivation)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, proof.Snapshot); err != nil {
		return fmt.Errorf("%w: invalid distributed proof snapshot", store.ErrPortActivation)
	}
	proof.Snapshot = bytes.Clone(compact.Bytes())
	if err := proof.Validate(); err != nil {
		return err
	}
	doc := portDistributedProofDocument{ID: "current", PortDistributedProof: *proof}
	filter := bson.M{"_id": "current", "policy_revision": expectedPolicy}
	if expectedPolicy == 0 {
		_, err := s.portDistributedProofCollection().InsertOne(ctx, doc)
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("%w: distributed proof already exists", store.ErrRunConflict)
		}
		return err
	}
	result, err := s.portDistributedProofCollection().ReplaceOne(ctx, filter, doc)
	if err != nil {
		return err
	}
	if result.MatchedCount != 1 {
		return fmt.Errorf("%w: distributed proof policy changed", store.ErrRunConflict)
	}
	return nil
}

// SavePortDistributedActivation commits the proof and the activation that
// references it in one Mongo transaction. A launch verifier therefore never
// observes a new activation with an absent or unrelated proof, even if the
// server crashes during publication.
func (s *Store) SavePortDistributedActivation(ctx context.Context, expectedPolicy uint64,
	proof *store.PortDistributedProof, next *store.PortActivation) error {
	if proof == nil {
		return fmt.Errorf("%w: distributed activation is missing its proof", store.ErrPortActivation)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, proof.Snapshot); err != nil {
		return fmt.Errorf("%w: invalid distributed proof snapshot", store.ErrPortActivation)
	}
	normalizedProof := *proof
	normalizedProof.Snapshot = bytes.Clone(compact.Bytes())
	proof = &normalizedProof
	if err := store.ValidatePortDistributedActivationWrite(expectedPolicy, proof, next); err != nil {
		return err
	}
	proofDoc := portDistributedProofDocument{ID: "current", PortDistributedProof: *proof}
	activationDoc := portActivationDocument{ID: "current", PortActivation: *next}
	session, err := s.client.StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(ctx)
	_, err = session.WithTransaction(ctx, func(txctx context.Context) (any, error) {
		if expectedPolicy == 0 {
			if _, err := s.portDistributedProofCollection().InsertOne(txctx, proofDoc); err != nil {
				return nil, err
			}
			if _, err := s.portActivationCollection().InsertOne(txctx, activationDoc); err != nil {
				return nil, err
			}
			return nil, nil
		}
		var currentActivation portActivationDocument
		if err := s.portActivationCollection().FindOne(txctx, bson.M{
			"_id": "current", "version": store.PortActivationVersion, "policy_revision": expectedPolicy,
		}).Decode(&currentActivation); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return nil, fmt.Errorf("%w: activation revision changed", store.ErrRunConflict)
			}
			return nil, err
		}
		if err := currentActivation.Validate(); err != nil {
			return nil, err
		}
		currentProof, err := s.loadPortDistributedProof(txctx, "current")
		if err != nil {
			return nil, err
		}
		previousProofPolicy := expectedPolicy
		if !currentActivation.Enabled {
			// Disable advances policy while deliberately retaining the last
			// proof. Reactivation replaces that retained proof atomically.
			previousProofPolicy = expectedPolicy - 1
		}
		if currentProof == nil || currentProof.PolicyRevision != previousProofPolicy ||
			currentProof.StoreIdentity != currentActivation.StoreIdentity || proof.ProofRevision <= currentProof.ProofRevision {
			return nil, fmt.Errorf("%w: distributed proof changed during activation", store.ErrRunConflict)
		}
		proofResult, err := s.portDistributedProofCollection().ReplaceOne(txctx,
			bson.M{"_id": "current", "policy_revision": previousProofPolicy}, proofDoc)
		if err != nil {
			return nil, err
		}
		if proofResult.MatchedCount != 1 {
			return nil, fmt.Errorf("%w: distributed proof policy changed", store.ErrRunConflict)
		}
		activationResult, err := s.portActivationCollection().ReplaceOne(txctx,
			bson.M{"_id": "current", "version": store.PortActivationVersion, "policy_revision": expectedPolicy}, activationDoc)
		if err != nil {
			return nil, err
		}
		if activationResult.MatchedCount != 1 {
			return nil, fmt.Errorf("%w: activation revision changed", store.ErrRunConflict)
		}
		return nil, nil
	})
	if mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("%w: distributed activation already exists", store.ErrRunConflict)
	}
	return err
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
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return &doc.PortActivation, nil
}

func (s *Store) SavePortActivation(ctx context.Context, expectedRevision uint64, next *store.PortActivation) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if expectedRevision >= math.MaxInt64 || next.Revision != expectedRevision+1 || next.RefreshLease != nil {
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
			bson.M{"$gt": bson.A{"$expires_at", "$$NOW"}},
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

// RenewPortActivationWithProof publishes a refreshed proof and the activation
// freshness fields that reference it in one Mongo transaction. The separate
// low-level RenewPortActivation method remains available for storage CAS
// conformance, but the production authority path must use this paired write.
func (s *Store) RenewPortActivationWithProof(ctx context.Context, renewal store.PortActivationRenewal,
	proof *store.PortDistributedProof) (*store.PortActivation, error) {
	now := time.Now().UTC()
	if proof == nil {
		return nil, fmt.Errorf("%w: distributed renewal is missing its proof", store.ErrPortActivation)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, proof.Snapshot); err != nil {
		return nil, fmt.Errorf("%w: invalid distributed proof snapshot", store.ErrPortActivation)
	}
	normalizedProof := *proof
	normalizedProof.Snapshot = bytes.Clone(compact.Bytes())
	proof = &normalizedProof
	if err := renewal.Validate(now); err != nil {
		return nil, err
	}
	session, err := s.client.StartSession()
	if err != nil {
		return nil, err
	}
	defer session.EndSession(ctx)
	var next *store.PortActivation
	_, err = session.WithTransaction(ctx, func(txctx context.Context) (any, error) {
		filter := renewalActivationFilter(renewal)
		var currentDoc portActivationDocument
		if err := s.portActivationCollection().FindOne(txctx, filter).Decode(&currentDoc); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return nil, fmt.Errorf("%w: activation policy, proof or authority lease changed", store.ErrRunConflict)
			}
			return nil, err
		}
		current := currentDoc.PortActivation
		if err := current.Validate(); err != nil {
			return nil, err
		}
		currentProof, err := s.loadPortDistributedProof(txctx, "current")
		if err != nil {
			return nil, err
		}
		if current.Scope != store.PortActivationDistributed || !current.Enabled ||
			!storeProofMatchesRenewal(currentProof, renewal, current.StoreIdentity) {
			return nil, fmt.Errorf("%w: activation policy, proof or authority lease changed", store.ErrRunConflict)
		}
		candidate := current
		candidate.ProofRevision = renewal.ProofRevision + 1
		candidate.VerifiedAt = renewal.VerifiedAt
		candidate.ExpiresAt = renewal.ExpiresAt
		if err := store.ValidatePortDistributedRenewalWrite(renewal, proof, &candidate, now); err != nil {
			return nil, err
		}
		proofDoc := portDistributedProofDocument{ID: "current", PortDistributedProof: *proof}
		proofResult, err := s.portDistributedProofCollection().ReplaceOne(txctx, bson.M{
			"_id": "current", "policy_revision": renewal.PolicyRevision,
			"proof_revision": renewal.ProofRevision, "proof_digest": renewal.ProofDigest,
			"store_identity": current.StoreIdentity,
		}, proofDoc)
		if err != nil {
			return nil, err
		}
		if proofResult.MatchedCount != 1 {
			return nil, fmt.Errorf("%w: distributed proof changed during renewal", store.ErrRunConflict)
		}
		activationDoc := portActivationDocument{ID: "current", PortActivation: candidate}
		activationResult, err := s.portActivationCollection().ReplaceOne(txctx, filter, activationDoc)
		if err != nil {
			return nil, err
		}
		if activationResult.MatchedCount != 1 {
			return nil, fmt.Errorf("%w: activation changed during renewal", store.ErrRunConflict)
		}
		next = &candidate
		return nil, nil
	})
	if err != nil {
		return nil, err
	}
	return next, nil
}

func renewalActivationFilter(renewal store.PortActivationRenewal) bson.M {
	return bson.M{
		"_id": "current", "version": store.PortActivationVersion,
		"policy_revision": renewal.PolicyRevision, "proof_revision": renewal.ProofRevision,
		"proof_digest": renewal.ProofDigest, "enabled": true,
		"scope":               store.PortActivationDistributed,
		"refresh_lease.owner": renewal.Lease.Owner, "refresh_lease.token": renewal.Lease.Token,
		"refresh_lease.expires_at": renewal.Lease.ExpiresAt,
		"verified_at":              bson.M{"$lte": renewal.VerifiedAt},
		"$expr": bson.M{"$and": bson.A{
			bson.M{"$gt": bson.A{"$expires_at", "$$NOW"}},
			bson.M{"$gt": bson.A{"$refresh_lease.expires_at", "$$NOW"}},
			bson.M{"$gt": bson.A{renewal.ExpiresAt, "$$NOW"}},
			bson.M{"$lte": bson.A{renewal.VerifiedAt, "$$NOW"}},
		}},
	}
}

func storeProofMatchesRenewal(proof *store.PortDistributedProof, renewal store.PortActivationRenewal, storeIdentity string) bool {
	return proof != nil && proof.PolicyRevision == renewal.PolicyRevision &&
		proof.ProofRevision == renewal.ProofRevision && proof.ProofDigest == renewal.ProofDigest &&
		proof.StoreIdentity == storeIdentity
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
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	return &doc.PortActivation, nil
}
