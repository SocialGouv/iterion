package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/store"
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
	// GenericSecrets maps workflow secret names to plaintext payloads
	// resolved from the tenant/user secret store at publish time.
	GenericSecrets map[string]string `json:"generic_secrets,omitempty"`
	// GenericSecretHosts maps a workflow secret name to the egress host
	// allowlist a bot-secret binding imposes on it (empty/absent = no
	// binding-level restriction). The runner intersects this with the
	// workflow's own declared `secrets.<name>.hosts` so a binding can
	// only NARROW egress, never broaden it. This is what makes a
	// binding's AllowedHosts an enforced control rather than metadata.
	GenericSecretHosts map[string][]string `json:"generic_secret_hosts,omitempty"`
	// GenericSecretRefs maps a workflow secret name to the ID of the
	// generic-secret store record it was resolved from (IDs only, never
	// values). A short-lived credential (a GitHub App installation token
	// lives 1h) can expire while the run executes; the server-side
	// refresh worker keeps the STORE record fresh, so these refs let the
	// runner re-read the current value mid-run and rewrite the secret's
	// materialised file — the bundle snapshot alone would go stale.
	GenericSecretRefs map[string]string `json:"generic_secret_refs,omitempty"`
	// OAuthCredentials maps "claude_code" / "codex" → opaque blob
	// that the runner materialises as a credentials.json /
	// auth.json before spawning the CLI subprocess.
	OAuthCredentials map[string][]byte `json:"oauth_credentials,omitempty"`
	// OAuthFingerprints maps the same kinds to the SUBSCRIPTION's audit
	// fingerprint (OAuthRecord.Fingerprint) — stable across automatic
	// token refreshes, re-stamped when a human posts new credentials.
	// Metering keys on it; empty for records that predate stamping.
	OAuthFingerprints map[string]string `json:"oauth_fingerprints,omitempty"`
	// ForgeAppBotLogin is the GitHub-App bot login (e.g.
	// "iterion-forge-1234[bot]") when the run's forge_token was resolved
	// from a github_app connection. An installation token can't `GET /user`
	// (403), so the runner can't self-resolve the committer identity from
	// the token alone — this login lets it look up the bot's numeric id via
	// `GET /users/<login>` (which an installation token CAN read) and seed
	// the canonical `<id>+<login>@users.noreply.github.com` committer, so a
	// bot's commits are attributed to the App bot, not the neutral fallback.
	// Empty for PAT/OAuth connections (the token's own /user resolves them).
	ForgeAppBotLogin string `json:"forge_app_bot_login,omitempty"`
	// PlatformSourced marks the credential slots the PLATFORM tier filled —
	// provider names for APIKeys entries ("anthropic", …) and OAuth kinds
	// for OAuthCredentials entries ("claude_code", "codex"); the two
	// namespaces never overlap. The runner's usage-cap scope check needs
	// it: a platform credential riding the bundle must still be metered on
	// the shared platform key, not fragmented per tenant as if the tenant
	// had brought its own.
	PlatformSourced map[string]bool `json:"platform_sourced,omitempty"`
}

// SetOAuthFingerprint records the audit identity of the credential that
// filled an OAuth slot, allocating the map on first use. Every tier that
// writes OAuthCredentials must call it: a slot left without an identity
// falls back to the historical slot-shaped meter key, where the credential
// that REPLACED an exhausted one inherits its readings — the failure this
// field exists to close. An empty fp is a no-op (a record that predates
// stamping keeps the key it always had).
func (b *RunBundle) SetOAuthFingerprint(kind, fp string) {
	if fp == "" {
		return
	}
	if b.OAuthFingerprints == nil {
		b.OAuthFingerprints = map[string]string{}
	}
	b.OAuthFingerprints[kind] = fp
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
	if err != nil && !mongoutil.IsIndexConflict(err) {
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
	return mongoutil.FindOne[RunSecretsRecord](ctx, s.coll, withRunSecretsTenantFilter(ctx, bson.M{"_id": id}), ErrRunSecretsNotFound, "secrets: get run secrets")
}

func (s *MongoRunSecretsStore) Delete(ctx context.Context, id string) error {
	_, err := s.coll.DeleteOne(ctx, withRunSecretsTenantFilter(ctx, bson.M{"_id": id}))
	if err != nil {
		return fmt.Errorf("secrets: delete run secrets: %w", err)
	}
	return nil
}

// withRunSecretsTenantFilter mirrors the mongo package's withTenantFilter:
// when ctx carries a tenant, scope by it; otherwise pass through for
// privileged callers (cluster admin, bootstrap, migration tooling).
func withRunSecretsTenantFilter(ctx context.Context, base bson.M) bson.M {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok {
		return base
	}
	out := make(bson.M, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["tenant_id"] = tenantID
	return out
}

// MemoryRunSecretsStore is the test variant.
type MemoryRunSecretsStore struct {
	mu sync.Mutex
	m  map[string]RunSecretsRecord
}

func NewMemoryRunSecretsStore() *MemoryRunSecretsStore {
	return &MemoryRunSecretsStore{m: make(map[string]RunSecretsRecord)}
}

func (s *MemoryRunSecretsStore) Put(_ context.Context, rec RunSecretsRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[rec.ID] = rec
	return nil
}

func (s *MemoryRunSecretsStore) Get(ctx context.Context, id string) (RunSecretsRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.m[id]
	if !ok {
		return RunSecretsRecord{}, ErrRunSecretsNotFound
	}
	if tenantID, has := store.TenantFromContext(ctx); has && rec.TenantID != "" && rec.TenantID != tenantID {
		return RunSecretsRecord{}, ErrRunSecretsNotFound
	}
	return rec, nil
}

func (s *MemoryRunSecretsStore) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.m[id]
	if !ok {
		return nil
	}
	if tenantID, has := store.TenantFromContext(ctx); has && rec.TenantID != "" && rec.TenantID != tenantID {
		// Don't reveal cross-tenant ID existence — treat as not-found.
		return nil
	}
	delete(s.m, id)
	return nil
}
