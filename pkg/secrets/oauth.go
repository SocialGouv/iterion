package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// OAuthKind enumerates the third-party CLIs whose OAuth subscription
// (forfait) iterion can drive on behalf of an authenticated user. The
// names match the delegate.Backend slug so cloudpublisher and the
// runner can resolve them mechanically.
type OAuthKind string

const (
	OAuthKindClaudeCode OAuthKind = "claude_code"
	OAuthKindCodex      OAuthKind = "codex"
)

func (k OAuthKind) Valid() bool {
	switch k {
	case OAuthKindClaudeCode, OAuthKindCodex:
		return true
	}
	return false
}

// OAuthRecord is the per-(user, kind) sealed credential bundle.
//
// SealedPayload is opaque to iterion — it holds the verbatim
// credentials.json (Anthropic) or auth.json (OpenAI Codex) blob the
// user uploaded, sealed with the master key bound to the record id.
// We never decrypt for display; the only consumer is the runner,
// which materialises the file in a tmpdir and points the CLI at it.
//
// AccessTokenExpiresAt is captured separately from the sealed blob
// so the refresh worker can identify expiring records without
// decrypting. Best-effort: providers without an access-token expiry
// (or when the user pasted only the refresh token) leave it zero
// and the worker skips them.
type OAuthRecord struct {
	ID                   string     `bson:"_id" json:"id"`
	UserID               string     `bson:"user_id" json:"user_id"`
	Kind                 OAuthKind  `bson:"kind" json:"kind"`
	SealedPayload        []byte     `bson:"sealed_payload" json:"-"`
	Scopes               []string   `bson:"scopes,omitempty" json:"scopes,omitempty"`
	AccessTokenExpiresAt *time.Time `bson:"access_token_expires_at,omitempty" json:"access_token_expires_at,omitempty"`
	LastRefreshedAt      *time.Time `bson:"last_refreshed_at,omitempty" json:"last_refreshed_at,omitempty"`
	CreatedAt            time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt            time.Time  `bson:"updated_at" json:"updated_at"`
}

// OAuthStore is the persistence interface for sealed OAuth records.
type OAuthStore interface {
	Upsert(ctx context.Context, rec OAuthRecord) error
	Get(ctx context.Context, userID string, kind OAuthKind) (OAuthRecord, error)
	ListByUser(ctx context.Context, userID string) ([]OAuthRecord, error)
	Delete(ctx context.Context, userID string, kind OAuthKind) error
	// ExpiringBefore returns records whose access token is set and
	// expires before t — used by the background refresh worker.
	ExpiringBefore(ctx context.Context, t time.Time) ([]OAuthRecord, error)
}

// ErrOAuthNotFound is the sentinel for missing records.
var ErrOAuthNotFound = errors.New("secrets: oauth record not found")

// SealOAuthPayload encrypts the raw credentials JSON. AAD binds the
// ciphertext to (userID, kind) so a sealed payload moved between
// users or kinds cannot be opened.
func SealOAuthPayload(sealer Sealer, userID string, kind OAuthKind, payload []byte) ([]byte, error) {
	if sealer == nil {
		return nil, errors.New("secrets: nil sealer for SealOAuthPayload")
	}
	return sealer.Seal(payload, oauthAAD(userID, kind))
}

// OpenOAuthPayload is the inverse: returns the raw JSON blob.
func OpenOAuthPayload(sealer Sealer, userID string, kind OAuthKind, sealed []byte) ([]byte, error) {
	if sealer == nil {
		return nil, errors.New("secrets: nil sealer for OpenOAuthPayload")
	}
	return sealer.Open(sealed, oauthAAD(userID, kind))
}

func oauthAAD(userID string, kind OAuthKind) []byte {
	return []byte("oauth:" + userID + ":" + string(kind))
}

// AnthropicCredentialsView is the minimal shape we extract from a
// Claude Code credentials.json blob to drive expiry tracking + refresh.
// We do NOT replace the user-supplied JSON with this struct on store —
// extra fields the CLI cares about round-trip via the sealed payload.
type AnthropicCredentialsView struct {
	ClaudeAIOauth struct {
		AccessToken  string   `json:"accessToken"`
		RefreshToken string   `json:"refreshToken"`
		ExpiresAt    int64    `json:"expiresAt"` // ms epoch
		Scopes       []string `json:"scopes,omitempty"`
	} `json:"claudeAiOauth"`
}

// CodexCredentialsView is the analogous shape for the Codex CLI's
// auth.json. Field names mirror the Codex SDK.
type CodexCredentialsView struct {
	Tokens struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token,omitempty"`
		ExpiresIn    int64  `json:"expires_in,omitempty"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh,omitempty"`
}

// ParseAnthropicView extracts the lightweight metadata view from a
// raw credentials.json blob. Returns the parsed view; errors when the
// JSON is malformed but never inspects scopes / expiry validity.
func ParseAnthropicView(payload []byte) (AnthropicCredentialsView, error) {
	var v AnthropicCredentialsView
	if err := json.Unmarshal(payload, &v); err != nil {
		return v, fmt.Errorf("secrets: parse credentials.json: %w", err)
	}
	return v, nil
}

// ParseCodexView extracts the analogous view from auth.json.
func ParseCodexView(payload []byte) (CodexCredentialsView, error) {
	var v CodexCredentialsView
	if err := json.Unmarshal(payload, &v); err != nil {
		return v, fmt.Errorf("secrets: parse auth.json: %w", err)
	}
	return v, nil
}

// MemoryOAuthStore — for tests.
type MemoryOAuthStore struct {
	mu sync.Mutex
	m  map[string]OAuthRecord
}

func NewMemoryOAuthStore() *MemoryOAuthStore {
	return &MemoryOAuthStore{m: make(map[string]OAuthRecord)}
}

func mkOAuthKey(userID string, kind OAuthKind) string {
	return userID + "|" + string(kind)
}

func (s *MemoryOAuthStore) Upsert(_ context.Context, rec OAuthRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.ID == "" {
		rec.ID = mkOAuthKey(rec.UserID, rec.Kind)
	}
	s.m[mkOAuthKey(rec.UserID, rec.Kind)] = rec
	return nil
}

func (s *MemoryOAuthStore) Get(_ context.Context, userID string, kind OAuthKind) (OAuthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[mkOAuthKey(userID, kind)]
	if !ok {
		return OAuthRecord{}, ErrOAuthNotFound
	}
	return r, nil
}

func (s *MemoryOAuthStore) ListByUser(_ context.Context, userID string) ([]OAuthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []OAuthRecord
	for _, r := range s.m {
		if r.UserID == userID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *MemoryOAuthStore) Delete(_ context.Context, userID string, kind OAuthKind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, mkOAuthKey(userID, kind))
	return nil
}

func (s *MemoryOAuthStore) ExpiringBefore(_ context.Context, t time.Time) ([]OAuthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []OAuthRecord
	for _, r := range s.m {
		if r.AccessTokenExpiresAt != nil && r.AccessTokenExpiresAt.Before(t) {
			out = append(out, r)
		}
	}
	return out, nil
}

// MongoOAuthStore — production impl.
type MongoOAuthStore struct {
	coll *mongo.Collection
}

const OAuthCollectionName = "oauth_credentials"

func NewMongoOAuthStore(db *mongo.Database) *MongoOAuthStore {
	return &MongoOAuthStore{coll: db.Collection(OAuthCollectionName)}
}

func (s *MongoOAuthStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "user_id", Value: 1}, {Key: "kind", Value: 1}}, Options: options.Index().SetUnique(true).SetName("user_kind_unique")},
		{Keys: bson.D{{Key: "access_token_expires_at", Value: 1}}, Options: options.Index().SetName("access_expiry_partial").SetPartialFilterExpression(bson.M{"access_token_expires_at": bson.M{"$exists": true}})},
	})
	if err != nil {
		var cmd mongo.CommandError
		if errors.As(err, &cmd) {
			if cmd.Code == 85 || cmd.Code == 86 {
				return nil
			}
		}
		return fmt.Errorf("secrets: ensure oauth indexes: %w", err)
	}
	return nil
}

func (s *MongoOAuthStore) Upsert(ctx context.Context, rec OAuthRecord) error {
	if rec.ID == "" {
		rec.ID = mkOAuthKey(rec.UserID, rec.Kind)
	}
	rec.UpdatedAt = time.Now().UTC()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = rec.UpdatedAt
	}
	_, err := s.coll.UpdateOne(
		ctx,
		bson.M{"user_id": rec.UserID, "kind": rec.Kind},
		bson.M{"$set": rec},
		options.UpdateOne().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("secrets: upsert oauth: %w", err)
	}
	return nil
}

func (s *MongoOAuthStore) Get(ctx context.Context, userID string, kind OAuthKind) (OAuthRecord, error) {
	var r OAuthRecord
	err := s.coll.FindOne(ctx, bson.M{"user_id": userID, "kind": kind}).Decode(&r)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return OAuthRecord{}, ErrOAuthNotFound
	}
	if err != nil {
		return OAuthRecord{}, fmt.Errorf("secrets: get oauth: %w", err)
	}
	return r, nil
}

func (s *MongoOAuthStore) ListByUser(ctx context.Context, userID string) ([]OAuthRecord, error) {
	cur, err := s.coll.Find(ctx, bson.M{"user_id": userID}, options.Find().SetSort(bson.M{"kind": 1}))
	if err != nil {
		return nil, fmt.Errorf("secrets: list oauth: %w", err)
	}
	defer cur.Close(ctx)
	var out []OAuthRecord
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("secrets: decode oauth: %w", err)
	}
	return out, nil
}

func (s *MongoOAuthStore) Delete(ctx context.Context, userID string, kind OAuthKind) error {
	res, err := s.coll.DeleteOne(ctx, bson.M{"user_id": userID, "kind": kind})
	if err != nil {
		return fmt.Errorf("secrets: delete oauth: %w", err)
	}
	if res.DeletedCount == 0 {
		return ErrOAuthNotFound
	}
	return nil
}

func (s *MongoOAuthStore) ExpiringBefore(ctx context.Context, t time.Time) ([]OAuthRecord, error) {
	cur, err := s.coll.Find(ctx, bson.M{
		"access_token_expires_at": bson.M{"$lt": t, "$exists": true},
	})
	if err != nil {
		return nil, fmt.Errorf("secrets: list expiring oauth: %w", err)
	}
	defer cur.Close(ctx)
	var out []OAuthRecord
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("secrets: decode expiring oauth: %w", err)
	}
	return out, nil
}
