package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// RunBundle is the per-run sealed payload the runner needs in order
// to execute. It carries every API-key + OAuth credential the
// publisher pre-resolved, keyed by provider/kind.
//
// The structure is JSON-marshalled and then sealed once with the
// run-scoped AAD ("run_secrets:<run_id>"). Runners decrypt with
// the shared master key.
type RunBundle struct {
	APIKeys map[Provider]string `json:"api_keys,omitempty"`
	// OAuthCredentials maps "claude_code" / "codex" → opaque blob
	// that the runner materialises as a credentials.json /
	// auth.json before spawning the CLI subprocess. Phase D wires
	// the OAuth path; Phase C leaves the map empty.
	OAuthCredentials map[string][]byte `json:"oauth_credentials,omitempty"`
}

// RunSecretsRecord is the persisted form of a sealed bundle. _id is
// the SecretsRef the publisher writes into the queue.RunMessage; the
// runner uses that ref to fetch + decrypt right before executing the
// run.
type RunSecretsRecord struct {
	ID           string    `bson:"_id" json:"id"`
	TenantID     string    `bson:"tenant_id" json:"tenant_id"`
	RunID        string    `bson:"run_id" json:"run_id"`
	SealedBundle []byte    `bson:"sealed_bundle" json:"-"`
	CreatedAt    time.Time `bson:"created_at" json:"created_at"`
	// ExpiresAt drives the Mongo TTL — the runner deletes the
	// record on success, but a TTL guard ensures abandoned bundles
	// never linger past 24h.
	ExpiresAt time.Time `bson:"expires_at" json:"expires_at"`
}

// RunSecretsStore persists sealed RunBundle records keyed by an
// opaque ref carried in the NATS message.
type RunSecretsStore interface {
	Put(ctx context.Context, rec RunSecretsRecord) error
	Get(ctx context.Context, id string) (RunSecretsRecord, error)
	Delete(ctx context.Context, id string) error
}

// ErrRunSecretsNotFound is returned by Get when the ref is unknown
// (already consumed or never published).
var ErrRunSecretsNotFound = errors.New("secrets: run secrets not found")

// SealRunBundle marshals + seals a RunBundle for a given run. Returns
// the sealed blob; the caller stores it as RunSecretsRecord.SealedBundle.
func SealRunBundle(sealer Sealer, runID string, b RunBundle) ([]byte, error) {
	if sealer == nil {
		return nil, errors.New("secrets: nil sealer for SealRunBundle")
	}
	body, err := json.Marshal(b)
	if err != nil {
		return nil, fmt.Errorf("secrets: marshal bundle: %w", err)
	}
	return sealer.Seal(body, runBundleAAD(runID))
}

// OpenRunBundle is the inverse: decrypt + unmarshal.
func OpenRunBundle(sealer Sealer, runID string, sealed []byte) (RunBundle, error) {
	var b RunBundle
	if sealer == nil {
		return b, errors.New("secrets: nil sealer for OpenRunBundle")
	}
	pt, err := sealer.Open(sealed, runBundleAAD(runID))
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(pt, &b); err != nil {
		return b, fmt.Errorf("secrets: unmarshal bundle: %w", err)
	}
	return b, nil
}

func runBundleAAD(runID string) []byte {
	return []byte("run_secrets:" + runID)
}

// NewSecretsRef returns a fresh opaque ref for a RunSecretsRecord.
// Random UUID rather than the run id so an attacker who can guess
// run ids cannot enumerate sealed bundles.
func NewSecretsRef() string {
	return uuid.NewString()
}

// MongoRunSecretsStore implements RunSecretsStore on Mongo with a
// 24h TTL guard.
type MongoRunSecretsStore struct {
	coll *mongo.Collection
}

const RunSecretsCollectionName = "run_secrets"

// DefaultRunSecretsTTL bounds how long a sealed bundle can live
// untouched. Resume paths re-publish so the runner can always re-
// fetch even after a TTL eviction (the publisher will re-resolve).
const DefaultRunSecretsTTL = 24 * time.Hour

func NewMongoRunSecretsStore(db *mongo.Database) *MongoRunSecretsStore {
	return &MongoRunSecretsStore{coll: db.Collection(RunSecretsCollectionName)}
}

func (s *MongoRunSecretsStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "run_id", Value: 1}}, Options: options.Index().SetName("tenant_run")},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("run_secrets_ttl").SetExpireAfterSeconds(0)},
	})
	if err != nil {
		var cmd mongo.CommandError
		if errors.As(err, &cmd) {
			if cmd.Code == 85 || cmd.Code == 86 {
				return nil
			}
		}
		return fmt.Errorf("secrets: ensure run_secrets indexes: %w", err)
	}
	return nil
}

func (s *MongoRunSecretsStore) Put(ctx context.Context, rec RunSecretsRecord) error {
	_, err := s.coll.InsertOne(ctx, rec)
	if err != nil {
		return fmt.Errorf("secrets: put run secrets: %w", err)
	}
	return nil
}

func (s *MongoRunSecretsStore) Get(ctx context.Context, id string) (RunSecretsRecord, error) {
	var rec RunSecretsRecord
	err := s.coll.FindOne(ctx, bson.M{"_id": id}).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return RunSecretsRecord{}, ErrRunSecretsNotFound
	}
	if err != nil {
		return RunSecretsRecord{}, fmt.Errorf("secrets: get run secrets: %w", err)
	}
	return rec, nil
}

func (s *MongoRunSecretsStore) Delete(ctx context.Context, id string) error {
	_, err := s.coll.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return fmt.Errorf("secrets: delete run secrets: %w", err)
	}
	return nil
}

// MemoryRunSecretsStore is the test variant.
type MemoryRunSecretsStore struct {
	m map[string]RunSecretsRecord
}

func NewMemoryRunSecretsStore() *MemoryRunSecretsStore {
	return &MemoryRunSecretsStore{m: make(map[string]RunSecretsRecord)}
}

func (s *MemoryRunSecretsStore) Put(_ context.Context, rec RunSecretsRecord) error {
	s.m[rec.ID] = rec
	return nil
}

func (s *MemoryRunSecretsStore) Get(_ context.Context, id string) (RunSecretsRecord, error) {
	rec, ok := s.m[id]
	if !ok {
		return RunSecretsRecord{}, ErrRunSecretsNotFound
	}
	return rec, nil
}

func (s *MemoryRunSecretsStore) Delete(_ context.Context, id string) error {
	delete(s.m, id)
	return nil
}
