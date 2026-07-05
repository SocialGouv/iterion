package orgsso

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// VerifiedDomainsCollectionName is the Mongo collection backing tenant domain
// claims.
const VerifiedDomainsCollectionName = "org_verified_domains"

// MongoDomainStore is the production DomainStore.
type MongoDomainStore struct {
	coll *mongo.Collection
}

// NewMongoDomainStore wires a MongoDomainStore on the given database.
func NewMongoDomainStore(db *mongo.Database) *MongoDomainStore {
	return &MongoDomainStore{coll: db.Collection(VerifiedDomainsCollectionName)}
}

// EnsureSchema creates the unique (tenant_id, domain) index.
func (s *MongoDomainStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "domain", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("tenant_domain_unique"),
		},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("orgsso: ensure org_verified_domains indexes: %w", err)
	}
	return nil
}

func (s *MongoDomainStore) Create(ctx context.Context, d VerifiedDomain) error {
	d.Domain = NormalizeDomain(d.Domain)
	if d.Domain == "" {
		return ErrDomainInvalid
	}
	if _, err := s.coll.InsertOne(ctx, d); err != nil {
		if mongoutil.IsDuplicateKey(err) {
			return ErrDomainExists
		}
		return fmt.Errorf("orgsso: insert domain: %w", err)
	}
	return nil
}

func (s *MongoDomainStore) Get(ctx context.Context, id string) (VerifiedDomain, error) {
	var d VerifiedDomain
	err := s.coll.FindOne(ctx, bson.M{"_id": id}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return VerifiedDomain{}, ErrDomainNotFound
	}
	if err != nil {
		return VerifiedDomain{}, fmt.Errorf("orgsso: get domain: %w", err)
	}
	return d, nil
}

func (s *MongoDomainStore) Update(ctx context.Context, d VerifiedDomain) error {
	d.Domain = NormalizeDomain(d.Domain)
	res, err := s.coll.ReplaceOne(ctx, bson.M{"_id": d.ID}, d)
	if err != nil {
		return fmt.Errorf("orgsso: update domain: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrDomainNotFound
	}
	return nil
}

func (s *MongoDomainStore) Delete(ctx context.Context, id string) error {
	res, err := s.coll.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return fmt.Errorf("orgsso: delete domain: %w", err)
	}
	if res.DeletedCount == 0 {
		return ErrDomainNotFound
	}
	return nil
}

func (s *MongoDomainStore) ListByTenant(ctx context.Context, tenantID string) ([]VerifiedDomain, error) {
	return mongoutil.FindAllSorted[VerifiedDomain](ctx, s.coll, bson.M{"tenant_id": tenantID}, "created_at",
		"orgsso: list domains", "orgsso: decode domains")
}

func (s *MongoDomainStore) TenantsForDomain(ctx context.Context, domain string) ([]string, error) {
	domain = NormalizeDomain(domain)
	if domain == "" {
		return nil, nil
	}
	var out []string
	if err := s.coll.Distinct(ctx, "tenant_id", bson.M{
		"domain":      domain,
		"verified_at": bson.M{"$ne": nil},
	}).Decode(&out); err != nil {
		return nil, fmt.Errorf("orgsso: distinct tenants for domain: %w", err)
	}
	return out, nil
}

func (s *MongoDomainStore) IsVerifiedForTenant(ctx context.Context, tenantID, domain string) (bool, error) {
	domain = NormalizeDomain(domain)
	if domain == "" {
		return false, nil
	}
	n, err := s.coll.CountDocuments(ctx, bson.M{
		"tenant_id":   tenantID,
		"domain":      domain,
		"verified_at": bson.M{"$ne": nil},
	}, options.Count().SetLimit(1))
	if err != nil {
		return false, fmt.Errorf("orgsso: count verified domain: %w", err)
	}
	return n > 0, nil
}
