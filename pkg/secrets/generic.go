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
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

type GenericSecret struct {
	ID           string     `bson:"_id" json:"id"`
	TenantID     string     `bson:"tenant_id" json:"tenant_id"`
	ScopeTeamID  string     `bson:"scope_team" json:"scope_team_id"`
	ScopeUserID  string     `bson:"scope_user,omitempty" json:"scope_user_id,omitempty"`
	Name         string     `bson:"name" json:"name"`
	Last4        string     `bson:"last4,omitempty" json:"last4,omitempty"`
	SealedSecret []byte     `bson:"sealed_secret" json:"-"`
	CreatedBy    string     `bson:"created_by" json:"created_by"`
	CreatedAt    time.Time  `bson:"created_at" json:"created_at"`
	LastUsedAt   *time.Time `bson:"last_used_at,omitempty" json:"last_used_at,omitempty"`
	Fingerprint  string     `bson:"fingerprint,omitempty" json:"fingerprint,omitempty"`
	// AllowedHosts, when non-empty, pins the egress host allowlist this
	// secret may ever be materialised toward (parent-domain match, e.g.
	// "github.com" also permits "api.github.com"). A managed forge token is
	// created with its forge host here so a prompt-injected bot cannot
	// exfiltrate it off-forge. It is the secret's OWN egress lock: it travels
	// through every resolution tier (buildGenericResolution) and is
	// intersected — never broadened — with any binding/workflow host policy
	// downstream (IntersectHosts / model.effectiveSecretHosts).
	AllowedHosts []string `bson:"allowed_hosts,omitempty" json:"allowed_hosts,omitempty"`
}

type GenericSecretStore interface {
	Create(ctx context.Context, s GenericSecret) error
	Get(ctx context.Context, id string) (GenericSecret, error)
	Update(ctx context.Context, s GenericSecret) error
	Delete(ctx context.Context, id string) error
	ListByTeam(ctx context.Context, teamID, requestingUserID string) ([]GenericSecret, error)
	ListByUser(ctx context.Context, teamID, userID string) ([]GenericSecret, error)
	MarkUsed(ctx context.Context, id string, at time.Time) error
}

var (
	ErrGenericSecretNotFound      = errors.New("secrets: generic secret not found")
	ErrGenericSecretTenantMissing = errors.New("secrets: generic secret store called without tenant context")
	// ErrGenericSecretConflict is returned by UpdateIfFingerprint when the
	// stored fingerprint no longer matches the expected one — a concurrent
	// writer landed first. Callers reread and remerge.
	ErrGenericSecretConflict = errors.New("secrets: generic secret was modified concurrently")
)

type GenericResolution struct {
	Name        string
	SecretID    string
	Plaintext   []byte
	SealedBlob  []byte
	SourceScope string // "user" | "binding" | "team"
	// AllowedHosts is the egress host allowlist a bot-secret binding
	// imposes on this secret (empty = no extra restriction). The publisher
	// threads it onto RunBundle.GenericSecretHosts; the runner's secret
	// guard intersects it with the workflow's declared hosts
	// (model.effectiveSecretHosts) so a binding can only narrow, never
	// broaden, the policy.
	AllowedHosts []string
}

func ResolveGeneric(
	ctx context.Context,
	secretStore GenericSecretStore,
	teamID, userID string,
	names []string,
	sealer Sealer,
	logger *iterlog.Logger,
) (map[string]GenericResolution, error) {
	if secretStore == nil {
		return map[string]GenericResolution{}, nil
	}
	if teamID == "" {
		return nil, fmt.Errorf("secrets: team id required for generic secret resolve")
	}
	if len(names) == 0 {
		return map[string]GenericResolution{}, nil
	}
	visible, err := secretStore.ListByTeam(ctx, teamID, userID)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(visible, func(i, j int) bool {
		ai := genericSecretRank(visible[i], userID)
		aj := genericSecretRank(visible[j], userID)
		if ai != aj {
			return ai < aj
		}
		return visible[i].CreatedAt.Before(visible[j].CreatedAt)
	})
	want := make(map[string]bool, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			want[name] = true
		}
	}
	out := make(map[string]GenericResolution, len(want))
	candidates := make(map[string]bool, len(want))
	for _, s := range visible {
		if !want[s.Name] {
			continue
		}
		candidates[s.Name] = true
		if _, exists := out[s.Name]; exists {
			continue
		}
		r, ok := buildGenericResolution(s, sealer, userID, logger)
		if ok {
			out[s.Name] = r
		}
	}
	// Expected fall-through: a requested name with no stored secret at all.
	// Debug (not warn) — a run may legitimately declare an optional secret the
	// team never configured. Names that HAD a candidate but dropped on unseal
	// are already warned by buildGenericResolution, so don't double-log them.
	for name := range want {
		if _, ok := out[name]; ok || candidates[name] {
			continue
		}
		logger.Debug("secrets: no generic secret named %q visible for team=%s — leaving credential unset", name, teamID)
	}
	return out, nil
}

func genericSecretRank(s GenericSecret, currentUserID string) int {
	if currentUserID != "" && s.ScopeUserID == currentUserID {
		return 0
	}
	if s.ScopeUserID == "" {
		return 1
	}
	return 99
}

func buildGenericResolution(s GenericSecret, sealer Sealer, currentUserID string, logger *iterlog.Logger) (GenericResolution, bool) {
	scope := "team"
	if currentUserID != "" && s.ScopeUserID == currentUserID {
		scope = "user"
	}
	r := GenericResolution{
		Name:        s.Name,
		SecretID:    s.ID,
		SealedBlob:  s.SealedSecret,
		SourceScope: scope,
		// The secret's own egress lock is the baseline for every tier; a
		// binding/workflow policy can only narrow it further, never broaden it.
		AllowedHosts: append([]string(nil), s.AllowedHosts...),
	}
	if sealer == nil {
		return r, true
	}
	pt, err := OpenGenericSecret(sealer, s.ID, s.SealedSecret)
	if err != nil {
		// Erreurs-explicites: a sealed blob that EXISTS but won't open is an
		// unexpected failure (corrupt store, wrong/rotated master key). Warn —
		// naming the secret's name + id + reason, never the value — so an
		// operator whose credential silently vanished has something to grep.
		logger.Warn("secrets: generic secret %q (id=%s) failed to unseal — credential dropped: %v", s.Name, s.ID, err)
		return GenericResolution{}, false
	}
	r.Plaintext = pt
	return r, true
}

// SealInto seals value for rec.ID and stamps the three derived fields
// (SealedSecret + Last4 + Fingerprint) onto rec. rec.ID must already be set
// (the AAD binds the ciphertext to it). This owns the seal-and-stamp triple so
// the CLI and every REST surface share one definition instead of hand-copying
// it (a missed field would silently ship a stale last4/fingerprint).
func SealInto(sealer Sealer, rec *GenericSecret, value string) error {
	sealed, err := SealGenericSecret(sealer, rec.ID, []byte(value))
	if err != nil {
		return err
	}
	rec.SealedSecret = sealed
	rec.Last4 = Last4(value)
	rec.Fingerprint = FingerprintSHA256(value)
	return nil
}

func SealGenericSecret(sealer Sealer, secretID string, plaintext []byte) ([]byte, error) {
	if sealer == nil {
		return nil, errors.New("secrets: nil sealer")
	}
	return sealer.Seal(plaintext, genericSecretAAD(secretID))
}

func OpenGenericSecret(sealer Sealer, secretID string, sealed []byte) ([]byte, error) {
	if sealer == nil {
		return nil, errors.New("secrets: nil sealer")
	}
	return sealer.Open(sealed, genericSecretAAD(secretID))
}

func genericSecretAAD(secretID string) []byte {
	return []byte("generic_secret:" + secretID)
}

func NewGenericSecretID() string {
	return uuid.NewString()
}

type MemoryGenericSecretStore struct {
	mu      sync.Mutex
	secrets map[string]GenericSecret
}

func NewMemoryGenericSecretStore() *MemoryGenericSecretStore {
	return &MemoryGenericSecretStore{secrets: make(map[string]GenericSecret)}
}

func (m *MemoryGenericSecretStore) Create(_ context.Context, s GenericSecret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.secrets[s.ID] = s
	return nil
}

func (m *MemoryGenericSecretStore) Get(_ context.Context, id string) (GenericSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[id]
	if !ok {
		return GenericSecret{}, ErrGenericSecretNotFound
	}
	return s, nil
}

func (m *MemoryGenericSecretStore) Update(_ context.Context, s GenericSecret) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.secrets[s.ID]; !ok {
		return ErrGenericSecretNotFound
	}
	m.secrets[s.ID] = s
	return nil
}

// ListAllInTeam returns EVERY secret of a team, personal scopes included —
// unlike ListByTeam, which is the resolution view of one user. Callers that
// must reason about NAME COLLISIONS across scopes (a personal secret
// outranks the team one at resolution) need this whole-team view.
func (m *MemoryGenericSecretStore) ListAllInTeam(_ context.Context, teamID string) ([]GenericSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []GenericSecret
	for _, s := range m.secrets {
		if s.ScopeTeamID == teamID {
			out = append(out, s)
		}
	}
	return out, nil
}

// UpdateIfFingerprint is the CAS write for documents SHARED between writers
// (the forge security-read map is rewritten by every replica's refresh
// worker): the replace only lands when the stored fingerprint still matches
// what the caller read, else ErrGenericSecretConflict.
func (m *MemoryGenericSecretStore) UpdateIfFingerprint(_ context.Context, s GenericSecret, expected string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.secrets[s.ID]
	if !ok {
		return ErrGenericSecretNotFound
	}
	if cur.Fingerprint != expected {
		return ErrGenericSecretConflict
	}
	m.secrets[s.ID] = s
	return nil
}

func (m *MemoryGenericSecretStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.secrets[id]; !ok {
		return ErrGenericSecretNotFound
	}
	delete(m.secrets, id)
	return nil
}

func (m *MemoryGenericSecretStore) ListByTeam(_ context.Context, teamID, userID string) ([]GenericSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []GenericSecret
	for _, s := range m.secrets {
		if s.ScopeTeamID != teamID {
			continue
		}
		if s.ScopeUserID != "" && s.ScopeUserID != userID {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (m *MemoryGenericSecretStore) ListByUser(_ context.Context, teamID, userID string) ([]GenericSecret, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []GenericSecret
	for _, s := range m.secrets {
		if s.ScopeTeamID == teamID && s.ScopeUserID == userID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *MemoryGenericSecretStore) MarkUsed(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.secrets[id]
	if !ok {
		return ErrGenericSecretNotFound
	}
	t := at
	s.LastUsedAt = &t
	m.secrets[id] = s
	return nil
}

type MongoGenericSecretStore struct {
	coll *mongo.Collection
}

const GenericSecretsCollectionName = "generic_secrets"

func NewMongoGenericSecretStore(db *mongo.Database) *MongoGenericSecretStore {
	return &MongoGenericSecretStore{coll: db.Collection(GenericSecretsCollectionName)}
}

func (s *MongoGenericSecretStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "scope_team", Value: 1}, {Key: "scope_user", Value: 1}, {Key: "name", Value: 1}}, Options: options.Index().SetName("team_user_name")},
		{Keys: bson.D{{Key: "scope_team", Value: 1}, {Key: "name", Value: 1}}, Options: options.Index().SetName("team_name")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("secrets: ensure generic_secrets indexes: %w", err)
	}
	return nil
}

func (s *MongoGenericSecretStore) Create(ctx context.Context, rec GenericSecret) error {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return ErrGenericSecretTenantMissing
	}
	rec.TenantID = tenantID
	if _, err := s.coll.InsertOne(ctx, rec); err != nil {
		return fmt.Errorf("secrets: insert generic secret: %w", err)
	}
	return nil
}

func (s *MongoGenericSecretStore) Get(ctx context.Context, id string) (GenericSecret, error) {
	filter, err := withGenericSecretTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return GenericSecret{}, err
	}
	var rec GenericSecret
	err = s.coll.FindOne(ctx, filter).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return GenericSecret{}, ErrGenericSecretNotFound
	}
	if err != nil {
		return GenericSecret{}, fmt.Errorf("secrets: get generic secret: %w", err)
	}
	return rec, nil
}

func (s *MongoGenericSecretStore) Update(ctx context.Context, rec GenericSecret) error {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return ErrGenericSecretTenantMissing
	}
	rec.TenantID = tenantID
	res, err := s.coll.ReplaceOne(ctx, bson.M{"_id": rec.ID, "tenant_id": tenantID}, rec)
	if err != nil {
		return fmt.Errorf("secrets: update generic secret: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrGenericSecretNotFound
	}
	return nil
}

// ListAllInTeam returns every secret of a team, personal scopes included
// (see the memory implementation for why callers need the whole-team view).
func (s *MongoGenericSecretStore) ListAllInTeam(ctx context.Context, teamID string) ([]GenericSecret, error) {
	filter, err := withGenericSecretTenantFilter(ctx, bson.M{"scope_team": teamID})
	if err != nil {
		return nil, err
	}
	cur, err := s.coll.Find(ctx, filter, options.Find().SetSort(bson.M{"created_at": 1}))
	if err != nil {
		return nil, fmt.Errorf("secrets: list team generic secrets: %w", err)
	}
	defer cur.Close(ctx)
	var out []GenericSecret
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("secrets: decode generic secrets: %w", err)
	}
	return out, nil
}

// UpdateIfFingerprint is the CAS variant of Update: the replace lands only
// when the stored fingerprint still matches what the caller read (see the
// memory implementation for the why). A no-match is disambiguated into
// not-found vs conflict with a second lookup.
func (s *MongoGenericSecretStore) UpdateIfFingerprint(ctx context.Context, rec GenericSecret, expected string) error {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return ErrGenericSecretTenantMissing
	}
	rec.TenantID = tenantID
	res, err := s.coll.ReplaceOne(ctx,
		bson.M{"_id": rec.ID, "tenant_id": tenantID, "fingerprint": expected}, rec)
	if err != nil {
		return fmt.Errorf("secrets: guarded update generic secret: %w", err)
	}
	if res.MatchedCount == 0 {
		if _, gerr := s.Get(ctx, rec.ID); errors.Is(gerr, ErrGenericSecretNotFound) {
			return ErrGenericSecretNotFound
		}
		return ErrGenericSecretConflict
	}
	return nil
}

func (s *MongoGenericSecretStore) Delete(ctx context.Context, id string) error {
	filter, err := withGenericSecretTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	res, err := s.coll.DeleteOne(ctx, filter)
	if err != nil {
		return fmt.Errorf("secrets: delete generic secret: %w", err)
	}
	if res.DeletedCount == 0 {
		return ErrGenericSecretNotFound
	}
	return nil
}

func (s *MongoGenericSecretStore) ListByTeam(ctx context.Context, teamID, userID string) ([]GenericSecret, error) {
	filter, err := withGenericSecretTenantFilter(ctx, bson.M{
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
		return nil, fmt.Errorf("secrets: list generic secrets: %w", err)
	}
	defer cur.Close(ctx)
	var out []GenericSecret
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("secrets: decode generic secrets: %w", err)
	}
	return out, nil
}

func (s *MongoGenericSecretStore) ListByUser(ctx context.Context, teamID, userID string) ([]GenericSecret, error) {
	filter, err := withGenericSecretTenantFilter(ctx, bson.M{"scope_team": teamID, "scope_user": userID})
	if err != nil {
		return nil, err
	}
	cur, err := s.coll.Find(ctx, filter, options.Find().SetSort(bson.M{"created_at": 1}))
	if err != nil {
		return nil, fmt.Errorf("secrets: list user generic secrets: %w", err)
	}
	defer cur.Close(ctx)
	var out []GenericSecret
	if err := cur.All(ctx, &out); err != nil {
		return nil, fmt.Errorf("secrets: decode user generic secrets: %w", err)
	}
	return out, nil
}

func (s *MongoGenericSecretStore) MarkUsed(ctx context.Context, id string, at time.Time) error {
	filter, err := withGenericSecretTenantFilter(ctx, bson.M{"_id": id})
	if err != nil {
		return err
	}
	if _, err := s.coll.UpdateOne(ctx, filter, bson.M{"$set": bson.M{"last_used_at": at}}); err != nil {
		return fmt.Errorf("secrets: mark generic secret used: %w", err)
	}
	return nil
}

func withGenericSecretTenantFilter(ctx context.Context, base bson.M) (bson.M, error) {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok || tenantID == "" {
		return nil, ErrGenericSecretTenantMissing
	}
	out := make(bson.M, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["tenant_id"] = tenantID
	return out, nil
}
