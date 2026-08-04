package secrets

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/store"
)

// withApiKeyTenantFilter augments a Mongo filter with a tenant_id
// clause derived from the request context. Returns an error when ctx
// carries no tenant so callers can't accidentally bypass isolation.
// The matching error sentinel below maps to 401 in HTTP handlers.
func withApiKeyTenantFilter(ctx context.Context, base bson.M) (bson.M, error) {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return nil, ErrApiKeyTenantMissing
	}
	out := make(bson.M, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["tenant_id"] = tenantID
	return out, nil
}

// ErrApiKeyTenantMissing is returned when an ApiKeyStore call lacks
// the tenant_id needed to scope its query. Callers must propagate it
// up — falling back to an unscoped query would expose every tenant's
// keys.
var ErrApiKeyTenantMissing = errors.New("secrets: ApiKey store called without tenant context")

// Provider enumerates the supported LLM credential providers. The
// string values are stable wire identifiers; do not rename without a
// migration. Naming mirrors what the model registry consumes.
type Provider string

const (
	ProviderAnthropic  Provider = "anthropic"
	ProviderOpenAI     Provider = "openai"
	ProviderBedrock    Provider = "bedrock"
	ProviderVertex     Provider = "vertex"
	ProviderAzure      Provider = "azure"
	ProviderOpenRouter Provider = "openrouter"
	ProviderXAI        Provider = "xai"
	// ProviderZAI is z.ai's Coding-Plan token. The provider exposes an
	// Anthropic-compatible HTTP surface, so credentials flow through the
	// existing Anthropic codepath with ANTHROPIC_BASE_URL pointed at
	// z.ai's endpoint and the ZAI token used as ANTHROPIC_AUTH_TOKEN.
	// Kept as a distinct Provider so per-tenant BYOK can pick "z.ai"
	// vs "anthropic" explicitly.
	ProviderZAI Provider = "zai"
)

// ZAIDefaultBaseURL is the Anthropic-compatible endpoint z.ai's Coding
// Plan publishes. Centralised here so the delegate and the model
// registry agree without re-deriving the URL.
const ZAIDefaultBaseURL = "https://api.z.ai/api/anthropic"

// XAIDefaultBaseURL is the host claw's OpenAI-compatible client targets
// for xAI Grok. The openai provider appends `/v1/chat/completions`, so
// this value must NOT include a trailing `/v1` (unlike the public
// OpenAI-SDK convention of `base_url=https://api.x.ai/v1`). Override
// with `XAI_BASE_URL` when pointing at a proxy or regional endpoint.
const XAIDefaultBaseURL = "https://api.x.ai"

// Valid reports whether p is one of the known providers.
func (p Provider) Valid() bool {
	switch p {
	case ProviderAnthropic, ProviderOpenAI, ProviderBedrock,
		ProviderVertex, ProviderAzure, ProviderOpenRouter, ProviderXAI,
		ProviderZAI:
		return true
	}
	return false
}

// ApiKey is a BYOK record: a single API key (or AWS-style credential
// blob, JSON-encoded inside SealedSecret) attached to a team and
// optionally scoped to a single user. The plaintext secret is never
// persisted — only the AES-GCM-sealed blob.
//
// Scope semantics:
//   - ScopeUserID == "": team-wide. Any member of the team picks it up
//     when their per-user keys do not provide the requested provider.
//   - ScopeUserID != "": user-only. Visible only to that user even when
//     listed by other team members (the API list endpoint hides them).
//
// Default flag: per (team, user, provider) tuple at most ONE entry is
// flagged is_default. Resolution prefers it over non-default keys.
type ApiKey struct {
	ID           string     `bson:"_id" json:"id"`
	TenantID     string     `bson:"tenant_id" json:"tenant_id"`
	ScopeTeamID  string     `bson:"scope_team" json:"scope_team_id"`
	ScopeUserID  string     `bson:"scope_user,omitempty" json:"scope_user_id,omitempty"`
	Provider     Provider   `bson:"provider" json:"provider"`
	Name         string     `bson:"name" json:"name"`
	Last4        string     `bson:"last4,omitempty" json:"last4,omitempty"`
	SealedSecret []byte     `bson:"sealed_secret" json:"-"`
	IsDefault    bool       `bson:"is_default,omitempty" json:"is_default,omitempty"`
	CreatedBy    string     `bson:"created_by" json:"created_by"`
	CreatedAt    time.Time  `bson:"created_at" json:"created_at"`
	LastUsedAt   *time.Time `bson:"last_used_at,omitempty" json:"last_used_at,omitempty"`
	ExpiresAt    *time.Time `bson:"expires_at,omitempty" json:"expires_at,omitempty"`
	Fingerprint  string     `bson:"fingerprint,omitempty" json:"fingerprint,omitempty"`
}

// ApiKeyStore is the persistence interface for BYOK records.
type ApiKeyStore interface {
	Create(ctx context.Context, k ApiKey) error
	Get(ctx context.Context, id string) (ApiKey, error)
	// GetOwned reads a PERSONAL key by (id, owner) with no tenant filter.
	//
	// Get is tenant-scoped, which is right for a run spending its own
	// team's keys and wrong for the credential pool: a lent key is read
	// from the BORROWER's context, in another tenant entirely, so Get
	// finds nothing and the caller cannot tell "not yours" from "gone".
	// Ownership replaces tenancy as the boundary here — the key must be
	// user-scoped to ownerUserID, which is strictly narrower than an id
	// lookup, and is the only thing the pool ever lends.
	GetOwned(ctx context.Context, id, ownerUserID string) (ApiKey, error)
	Update(ctx context.Context, k ApiKey) error
	Delete(ctx context.Context, id string) error
	// ListByTeam returns every key visible from teamID — i.e. team-
	// scoped keys plus the requesting user's user-scoped keys. The
	// requestingUserID filter MUST be applied; passing "" returns
	// only team-wide keys (admin path).
	ListByTeam(ctx context.Context, teamID, requestingUserID string) ([]ApiKey, error)
	// ListByUser returns the requesting user's user-scoped keys
	// inside a given team.
	ListByUser(ctx context.Context, teamID, userID string) ([]ApiKey, error)
	// MarkUsed updates last_used_at without altering anything else.
	MarkUsed(ctx context.Context, id string, at time.Time) error
	// ClearDefault removes the is_default flag from any other key in
	// the same (team, user, provider) tuple. Used when a new key is
	// created with is_default=true or an existing one is promoted.
	ClearDefault(ctx context.Context, teamID, userID string, provider Provider, exceptID string) error
}

// Sentinel errors raised by Store implementations.
var (
	ErrApiKeyNotFound = errors.New("secrets: api key not found")
)

// Resolution describes a single resolved key the publisher needs to
// inject for one provider on a given run.
type Resolution struct {
	Provider Provider
	KeyID    string
	// Plaintext is filled by the resolver only if the caller passes
	// a Sealer; without one the resolver returns the sealed blob
	// untouched (handlers that do not need plaintext yet).
	Plaintext   []byte
	SealedBlob  []byte
	SourceScope string // "user" or "team" — for audit logging
}

// Resolve returns at most one ApiKey for each requested provider,
// applying the priority chain documented in the cloud admin plan:
//
//  1. KeyOverrides[provider] — caller-pinned key id (validated to
//     belong to the team and to be visible to userID).
//  2. (team, userID, provider, default=true)
//  3. (team, userID, provider) — first match
//  4. (team, "", provider, default=true)
//  5. (team, "", provider) — first match
//
// Providers without a hit are simply omitted. Callers consult the
// returned map and either inject what's there or fall back to env.
//
// When sealer is non-nil, every Resolution.Plaintext is decrypted; on
// decrypt failure the resolution is skipped and an error is logged
// to logErr. Pass nil sealer to get sealed blobs only.
func Resolve(
	ctx context.Context,
	store ApiKeyStore,
	teamID, userID string,
	providers []Provider,
	keyOverrides map[Provider]string,
	sealer Sealer,
) (map[Provider]Resolution, error) {
	if teamID == "" {
		return nil, fmt.Errorf("secrets: team id required for resolve")
	}
	if len(providers) == 0 {
		return map[Provider]Resolution{}, nil
	}
	visible, err := store.ListByTeam(ctx, teamID, userID)
	if err != nil {
		return nil, err
	}
	// Stable order: user-default, user-other, team-default, team-other.
	sort.SliceStable(visible, func(i, j int) bool {
		ai := keyRank(visible[i], userID)
		aj := keyRank(visible[j], userID)
		if ai != aj {
			return ai < aj
		}
		return visible[i].CreatedAt.Before(visible[j].CreatedAt)
	})

	out := make(map[Provider]Resolution, len(providers))
	wantSet := make(map[Provider]bool, len(providers))
	for _, p := range providers {
		wantSet[p] = true
	}

	// Pass 1: explicit overrides.
	for prov, keyID := range keyOverrides {
		if !wantSet[prov] || keyID == "" {
			continue
		}
		for _, k := range visible {
			if k.ID == keyID && k.Provider == prov {
				if r, ok := buildResolution(k, sealer, userID); ok {
					out[prov] = r
				}
				break
			}
		}
	}

	// Pass 2: walk visible in priority order, taking the first
	// match per provider that wasn't already pinned.
	for _, k := range visible {
		if !wantSet[k.Provider] {
			continue
		}
		if _, already := out[k.Provider]; already {
			continue
		}
		if r, ok := buildResolution(k, sealer, userID); ok {
			out[k.Provider] = r
		}
	}
	return out, nil
}

// keyRank assigns a sort key. Lower rank = higher priority.
func keyRank(k ApiKey, currentUserID string) int {
	userMatch := currentUserID != "" && k.ScopeUserID == currentUserID
	switch {
	case userMatch && k.IsDefault:
		return 0
	case userMatch:
		return 1
	case k.ScopeUserID == "" && k.IsDefault:
		return 2
	case k.ScopeUserID == "":
		return 3
	}
	// Other users' user-scoped keys never apply.
	return 99
}

// buildResolution decrypts (when sealer != nil) and packages the key
// for the publisher. AAD binds the sealed blob to its api_key id so
// a sealed payload moved between records cannot be opened.
func buildResolution(k ApiKey, sealer Sealer, currentUserID string) (Resolution, bool) {
	scope := "team"
	if k.ScopeUserID == currentUserID && currentUserID != "" {
		scope = "user"
	}
	r := Resolution{
		Provider:    k.Provider,
		KeyID:       k.ID,
		SealedBlob:  k.SealedSecret,
		SourceScope: scope,
	}
	if sealer == nil {
		return r, true
	}
	pt, err := OpenApiKey(sealer, k)
	if err != nil {
		return Resolution{}, false
	}
	r.Plaintext = pt
	return r, true
}

// OpenApiKey decrypts a stored key. The AAD binds the ciphertext to the
// key's id, so a sealed secret copied onto another record cannot be
// opened. Exported because resolution is no longer the only consumer: the
// credential pool serves a key its donor lends by id, outside the
// team-priority chain Resolve implements.
func OpenApiKey(sealer Sealer, k ApiKey) ([]byte, error) {
	if sealer == nil {
		return nil, fmt.Errorf("secrets: nil sealer for OpenApiKey")
	}
	return sealer.Open(k.SealedSecret, apiKeyAAD(k.ID))
}

// apiKeyAAD is the single definition of a key's additional authenticated
// data — sealing and opening must never disagree on it.
func apiKeyAAD(id string) []byte { return []byte("api_key:" + id) }

// SealAPIKey produces the sealed blob for storage. Pass the caller's
// shared Sealer (e.g. ITERION_SECRETS_KEY-driven AESGCMSealer) and
// the freshly-generated key ID; the AAD ties the ciphertext to that
// record so it cannot be moved.
func SealAPIKey(sealer Sealer, keyID string, plaintext []byte) ([]byte, error) {
	if sealer == nil {
		return nil, errNilSealer
	}
	return sealer.Seal(plaintext, apiKeyAAD(keyID))
}

// MemoryApiKeyStore is the in-process store used by tests of the
// resolution chain.
type MemoryApiKeyStore struct {
	mu   sync.Mutex
	keys map[string]ApiKey
}

func NewMemoryApiKeyStore() *MemoryApiKeyStore {
	return &MemoryApiKeyStore{keys: make(map[string]ApiKey)}
}

func (m *MemoryApiKeyStore) Create(_ context.Context, k ApiKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[k.ID] = k
	return nil
}

func (m *MemoryApiKeyStore) Get(_ context.Context, id string) (ApiKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return ApiKey{}, ErrApiKeyNotFound
	}
	return k, nil
}

func (m *MemoryApiKeyStore) GetOwned(_ context.Context, id, ownerUserID string) (ApiKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	// The ownership term is enforced here even though this store ignores
	// tenancy: it is the whole guarantee the method offers, and a memory
	// store that waved it through would let a pool test pass against a
	// key its "donor" does not own.
	if !ok || ownerUserID == "" || k.ScopeUserID != ownerUserID {
		return ApiKey{}, ErrApiKeyNotFound
	}
	return k, nil
}

func (m *MemoryApiKeyStore) Update(_ context.Context, k ApiKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.keys[k.ID]; !ok {
		return ErrApiKeyNotFound
	}
	m.keys[k.ID] = k
	return nil
}

func (m *MemoryApiKeyStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.keys[id]; !ok {
		return ErrApiKeyNotFound
	}
	delete(m.keys, id)
	return nil
}

func (m *MemoryApiKeyStore) ListByTeam(_ context.Context, teamID, userID string) ([]ApiKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ApiKey
	for _, k := range m.keys {
		if k.ScopeTeamID != teamID {
			continue
		}
		if k.ScopeUserID != "" && k.ScopeUserID != userID {
			continue
		}
		out = append(out, k)
	}
	return out, nil
}

func (m *MemoryApiKeyStore) ListByUser(_ context.Context, teamID, userID string) ([]ApiKey, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []ApiKey
	for _, k := range m.keys {
		if k.ScopeTeamID == teamID && k.ScopeUserID == userID {
			out = append(out, k)
		}
	}
	return out, nil
}

func (m *MemoryApiKeyStore) MarkUsed(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return ErrApiKeyNotFound
	}
	t := at
	k.LastUsedAt = &t
	m.keys[id] = k
	return nil
}

func (m *MemoryApiKeyStore) ClearDefault(_ context.Context, teamID, userID string, provider Provider, exceptID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, k := range m.keys {
		if id == exceptID {
			continue
		}
		if k.ScopeTeamID != teamID {
			continue
		}
		if k.ScopeUserID != userID {
			continue
		}
		if k.Provider != provider {
			continue
		}
		if k.IsDefault {
			k.IsDefault = false
			m.keys[id] = k
		}
	}
	return nil
}

// MongoApiKeyStore implements ApiKeyStore on Mongo.
type MongoApiKeyStore struct {
	coll *mongo.Collection
}

const ApiKeysCollectionName = "api_keys"

func NewMongoApiKeyStore(db *mongo.Database) *MongoApiKeyStore {
	return &MongoApiKeyStore{coll: db.Collection(ApiKeysCollectionName)}
}

// EnsureSchema creates the indexes used by the store.
func (s *MongoApiKeyStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "scope_team", Value: 1}, {Key: "scope_user", Value: 1}, {Key: "provider", Value: 1}}, Options: options.Index().SetName("team_user_provider")},
		{Keys: bson.D{{Key: "scope_team", Value: 1}, {Key: "provider", Value: 1}, {Key: "is_default", Value: 1}}, Options: options.Index().SetName("team_provider_default")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("secrets: ensure api_keys indexes: %w", err)
	}
	return nil
}

func (s *MongoApiKeyStore) Create(ctx context.Context, k ApiKey) error {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return ErrApiKeyTenantMissing
	}
	// Stamp tenant_id from ctx so the caller can't smuggle a different
	// tenant onto a writeable row. The struct field is the source of
	// truth on the wire; we overwrite it here unconditionally.
	k.TenantID = tenantID
	if _, err := s.coll.InsertOne(ctx, k); err != nil {
		return fmt.Errorf("secrets: insert api key: %w", err)
	}
	return nil
}

func (s *MongoApiKeyStore) Get(ctx context.Context, id string) (ApiKey, error) {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return ApiKey{}, err
	}
	return mongoutil.FindOne[ApiKey](ctx, s.coll, filter, ErrApiKeyNotFound, "secrets: get api key")
}

func (s *MongoApiKeyStore) GetOwned(ctx context.Context, id, ownerUserID string) (ApiKey, error) {
	if ownerUserID == "" {
		return ApiKey{}, ErrApiKeyNotFound
	}
	// No tenant term, by design — see the interface doc. scope_user is the
	// boundary: only the key's own owner can be served this way.
	return mongoutil.FindOne[ApiKey](ctx, s.coll,
		bson.M{"_id": id, "scope_user": ownerUserID},
		ErrApiKeyNotFound, "secrets: get owned api key")
}

func (s *MongoApiKeyStore) Update(ctx context.Context, k ApiKey) error {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return ErrApiKeyTenantMissing
	}
	// Match by both _id AND tenant_id so a tenant can never overwrite
	// another tenant's row by guessing an id.
	k.TenantID = tenantID
	return mongoutil.ReplaceOneChecked(ctx, s.coll, bson.M{"_id": k.ID, "tenant_id": tenantID}, k, nil, ErrApiKeyNotFound, "secrets: update api key")
}

func (s *MongoApiKeyStore) Delete(ctx context.Context, id string) error {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	return mongoutil.DeleteOneChecked(ctx, s.coll, filter, ErrApiKeyNotFound, "secrets: delete api key")
}

func (s *MongoApiKeyStore) ListByTeam(ctx context.Context, teamID, userID string) ([]ApiKey, error) {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{
		"scope_team": teamID,
		"$or": []bson.M{
			{"scope_user": bson.M{"$exists": false}},
			{"scope_user": ""},
			{"scope_user": userID},
		},
	})
	if err != nil {
		return nil, err
	}
	cur, err := s.coll.Find(ctx, filter, options.Find().SetSort(bson.M{"created_at": 1}))
	if err != nil {
		return nil, fmt.Errorf("secrets: list api keys: %w", err)
	}
	defer cur.Close(ctx)
	var out []ApiKey
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("secrets: decode api keys: %w", err)
	}
	return out, nil
}

func (s *MongoApiKeyStore) ListByUser(ctx context.Context, teamID, userID string) ([]ApiKey, error) {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{"scope_team": teamID, "scope_user": userID})
	if err != nil {
		return nil, err
	}
	cur, err := s.coll.Find(ctx, filter, options.Find().SetSort(bson.M{"created_at": 1}))
	if err != nil {
		return nil, fmt.Errorf("secrets: list user api keys: %w", err)
	}
	defer cur.Close(ctx)
	var out []ApiKey
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("secrets: decode user api keys: %w", err)
	}
	return out, nil
}

func (s *MongoApiKeyStore) MarkUsed(ctx context.Context, id string, at time.Time) error {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	if _, err := s.coll.UpdateOne(ctx, filter, bson.M{"$set": bson.M{"last_used_at": at}}); err != nil {
		return fmt.Errorf("secrets: mark used: %w", err)
	}
	return nil
}

func (s *MongoApiKeyStore) ClearDefault(ctx context.Context, teamID, userID string, provider Provider, exceptID string) error {
	filter, err := withApiKeyTenantFilter(ctx, bson.M{
		"scope_team": teamID,
		"provider":   provider,
		"is_default": true,
	})
	if err != nil {
		return err
	}
	if userID == "" {
		// Team-wide rows are persisted with scope_user omitted (the
		// field has `bson:"...,omitempty"`), so a literal "" filter
		// never matches them and the "at most one default per tuple"
		// invariant silently fractures. Match both absent and empty
		// forms here. See pattern in ListByTeam.
		filter["$or"] = []bson.M{
			{"scope_user": bson.M{"$exists": false}},
			{"scope_user": ""},
		}
	} else {
		filter["scope_user"] = userID
	}
	if exceptID != "" {
		filter["_id"] = bson.M{"$ne": exceptID}
	}
	if _, err := s.coll.UpdateMany(ctx, filter, bson.M{"$set": bson.M{"is_default": false}}); err != nil {
		return fmt.Errorf("secrets: clear default: %w", err)
	}
	return nil
}

// NewApiKeyID returns a fresh UUID-string id for an ApiKey record.
// Centralised so the routes layer doesn't reach for uuid directly.
func NewApiKeyID() string {
	return uuid.NewString()
}

// ParseProvider returns Provider when s matches one of the known
// names (case-insensitive) or an error otherwise.
func ParseProvider(s string) (Provider, error) {
	p := Provider(strings.ToLower(strings.TrimSpace(s)))
	if !p.Valid() {
		return "", fmt.Errorf("unknown provider %q", s)
	}
	return p, nil
}
