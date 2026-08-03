package secrets

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// OAuthPending is the short-lived server-side state held between the
// browser OAuth /authorize/start and /authorize/complete calls. It
// keeps the PKCE verifier (sealed) and the expected state so the
// completion can finish the exchange and validate CSRF — without
// trusting anything the client echoes back except the code.
//
// Keyed by (OwnerKey, Kind): one in-flight connect per owner+kind. A
// second start simply overwrites the first. Persisted (Mongo) rather
// than in-process so the start and complete may land on different
// replicas (HA-safe). Mirrors the TTL pattern of run_secrets.
type OAuthPending struct {
	ID             string    `bson:"_id" json:"id"`
	OwnerKey       string    `bson:"owner_key" json:"owner_key"`
	Kind           OAuthKind `bson:"kind" json:"kind"`
	SealedVerifier []byte    `bson:"sealed_verifier" json:"-"`
	State          string    `bson:"state" json:"state"`
	RedirectURI    string    `bson:"redirect_uri" json:"redirect_uri"`
	CreatedAt      time.Time `bson:"created_at" json:"created_at"`
	ExpiresAt      time.Time `bson:"expires_at" json:"expires_at"`
}

// OAuthPendingStore persists in-flight browser-OAuth connects.
type OAuthPendingStore interface {
	// Put upserts the pending record (overwriting any existing one for
	// the same owner+kind).
	Put(ctx context.Context, rec OAuthPending) error
	// Take reads and deletes the pending record (one-shot), returning
	// ErrOAuthPendingNotFound when absent or expired.
	Take(ctx context.Context, ownerKey string, kind OAuthKind) (OAuthPending, error)
}

// ErrOAuthPendingNotFound is the sentinel for a missing/expired pending.
var ErrOAuthPendingNotFound = errors.New("secrets: oauth pending not found")

// DefaultOAuthPendingTTL bounds how long a started browser-OAuth flow
// can wait for the user to paste the code back.
const DefaultOAuthPendingTTL = 10 * time.Minute

func mkPendingKey(ownerKey string, kind OAuthKind) string {
	return ownerKey + "|" + string(kind)
}

func oauthPendingAAD(ownerKey string, kind OAuthKind) []byte {
	return []byte("oauth_pending:" + ownerKey + ":" + string(kind))
}

// SealOAuthVerifier seals a PKCE verifier bound to (ownerKey, kind) so a
// persisted pending record never stores a usable verifier at rest.
func SealOAuthVerifier(sealer Sealer, ownerKey string, kind OAuthKind, verifier string) ([]byte, error) {
	if sealer == nil {
		return nil, errors.New("secrets: nil sealer for SealOAuthVerifier")
	}
	return sealer.Seal([]byte(verifier), oauthPendingAAD(ownerKey, kind))
}

// OpenOAuthVerifier is the inverse of SealOAuthVerifier.
func OpenOAuthVerifier(sealer Sealer, ownerKey string, kind OAuthKind, sealed []byte) (string, error) {
	if sealer == nil {
		return "", errors.New("secrets: nil sealer for OpenOAuthVerifier")
	}
	b, err := sealer.Open(sealed, oauthPendingAAD(ownerKey, kind))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// MemoryOAuthPendingStore — for tests / local mode.
type MemoryOAuthPendingStore struct {
	mu sync.Mutex
	m  map[string]OAuthPending
}

func NewMemoryOAuthPendingStore() *MemoryOAuthPendingStore {
	return &MemoryOAuthPendingStore{m: make(map[string]OAuthPending)}
}

func (s *MemoryOAuthPendingStore) Put(_ context.Context, rec OAuthPending) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.ID == "" {
		rec.ID = mkPendingKey(rec.OwnerKey, rec.Kind)
	}
	s.m[mkPendingKey(rec.OwnerKey, rec.Kind)] = rec
	return nil
}

func (s *MemoryOAuthPendingStore) Take(_ context.Context, ownerKey string, kind OAuthKind) (OAuthPending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := mkPendingKey(ownerKey, kind)
	rec, ok := s.m[key]
	if !ok {
		return OAuthPending{}, ErrOAuthPendingNotFound
	}
	delete(s.m, key)
	if !rec.ExpiresAt.IsZero() && time.Now().After(rec.ExpiresAt) {
		return OAuthPending{}, ErrOAuthPendingNotFound
	}
	return rec, nil
}

// MongoOAuthPendingStore — production impl with a TTL guard.
type MongoOAuthPendingStore struct {
	coll *mongo.Collection
}

const OAuthPendingCollectionName = "oauth_pending"

func NewMongoOAuthPendingStore(db *mongo.Database) *MongoOAuthPendingStore {
	return &MongoOAuthPendingStore{coll: db.Collection(OAuthPendingCollectionName)}
}

func (s *MongoOAuthPendingStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "owner_key", Value: 1}, {Key: "kind", Value: 1}}, Options: options.Index().SetUnique(true).SetName("owner_kind_unique")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("oauth_pending_ttl").SetExpireAfterSeconds(0)},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("secrets: ensure oauth_pending indexes: %w", err)
	}
	return nil
}

func (s *MongoOAuthPendingStore) Put(ctx context.Context, rec OAuthPending) error {
	if rec.ID == "" {
		rec.ID = mkPendingKey(rec.OwnerKey, rec.Kind)
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	setBody, err := mongoutil.SetBodyWithoutID(rec, "secrets: oauth pending")
	if err != nil {
		return err
	}
	_, err = s.coll.UpdateOne(
		ctx,
		bson.M{"owner_key": rec.OwnerKey, "kind": rec.Kind},
		bson.M{"$set": setBody, "$setOnInsert": bson.M{"_id": rec.ID}},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("secrets: upsert oauth pending: %w", err)
	}
	return nil
}

func (s *MongoOAuthPendingStore) Take(ctx context.Context, ownerKey string, kind OAuthKind) (OAuthPending, error) {
	rec, err := mongoutil.FindOneAndDeleteChecked[OAuthPending](ctx, s.coll, bson.M{"owner_key": ownerKey, "kind": kind}, ErrOAuthPendingNotFound, "secrets: take oauth pending")
	if err != nil {
		return OAuthPending{}, err
	}
	// The TTL monitor can lag up to ~60s, so reject an expired record
	// even if Mongo hasn't swept it yet.
	if !rec.ExpiresAt.IsZero() && time.Now().After(rec.ExpiresAt) {
		return OAuthPending{}, ErrOAuthPendingNotFound
	}
	return rec, nil
}
