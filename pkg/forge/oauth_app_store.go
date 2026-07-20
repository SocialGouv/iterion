package forge

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
)

// ForgeOAuthApp holds one forge OAuth *application*'s credentials — the
// client_id + sealed client_secret iterion uses to drive the OAuth connect
// flow against a specific forge instance. Scoped by (tenant, provider,
// instance base URL) so an org can register a distinct app per forge and per
// self-hosted instance (gitlab.com vs a private GitLab) with no global env
// config. Replaces the legacy process-global ITERION_FORGE_*_OAUTH_* map.
type ForgeOAuthApp struct {
	ID       string   `bson:"_id" json:"id"`
	TenantID string   `bson:"tenant_id" json:"tenant_id"`
	Provider Provider `bson:"provider" json:"provider"`
	// ForgeBaseURL pins the instance this app belongs to (canonical
	// scheme+host, no trailing slash; "" → the provider's canonical SaaS host).
	// Always stored canonicalised via CanonicalBaseURL.
	ForgeBaseURL string `bson:"forge_base_url,omitempty" json:"forge_base_url,omitempty"`

	// ClientID is stored in the clear (not a secret; the admin UI lists it).
	// SealedSecret holds the client_secret sealed via secrets.Sealer with AAD
	// "forge_oauth_app:<ID>" — never serialised out of the server.
	ClientID     string `bson:"client_id" json:"client_id"`
	SealedSecret []byte `bson:"sealed_secret" json:"-"`

	// Scopes requested at authorize time (observability) and RedirectURI the
	// app was registered with (must match iterion's OAuth callback).
	Scopes      []string `bson:"scopes,omitempty" json:"scopes,omitempty"`
	RedirectURI string   `bson:"redirect_uri,omitempty" json:"redirect_uri,omitempty"`

	// ProviderAppID is the forge-side application id (GitLab application_id,
	// Forgejo/GitHub app id), retained so the app can later be removed on the
	// forge. AutoCreated marks apps iterion created via the forge API (vs an
	// operator-pasted client_id/secret).
	ProviderAppID string `bson:"provider_app_id,omitempty" json:"provider_app_id,omitempty"`
	AutoCreated   bool   `bson:"auto_created,omitempty" json:"auto_created"`

	// AppManageURL deep-links the forge page where this app can be removed
	// (e.g. a GitHub App's settings/advanced page). Populated for apps iterion
	// auto-created and can locate; surfaced in the delete confirmation so the
	// operator can clean up the forge side — no provider exposes an
	// app-deletion API iterion could call itself.
	AppManageURL string `bson:"app_manage_url,omitempty" json:"app_manage_url,omitempty"`

	// AppSlug is the GitHub App's URL slug (github.com/apps/<slug>) — used to
	// build the install URL for the least-privilege github_app path.
	// SealedPrivateKey holds the App's private key (PEM), sealed via
	// forge_oauth_app_key:<id>. Both are populated for manifest-created GitHub
	// Apps; their presence means the App can be INSTALLED (github_app), not only
	// OAuth-authorized (oauth_app). ProviderAppID doubles as the GitHub App id.
	AppSlug          string `bson:"app_slug,omitempty" json:"app_slug,omitempty"`
	SealedPrivateKey []byte `bson:"sealed_private_key,omitempty" json:"-"`
	// OwnerLogin is the forge account that OWNS the app (a GitHub org login, or
	// a user login for a personal app). It is the natural discriminator once a
	// tenant may hold several apps on the same host: a private GitHub App can
	// only ever be installed on its owning account, so "which app" is really
	// "which org". Surfaced so the UI can label the picker by org rather than by
	// opaque slug.
	OwnerLogin string `bson:"owner_login,omitempty" json:"owner_login,omitempty"`
	// Installable is a computed view flag (never persisted): true when the App
	// holds a private key, so the UI can offer the "Install" (github_app) action.
	Installable bool `bson:"-" json:"installable,omitempty"`

	CreatedBy string    `bson:"created_by" json:"created_by"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// OAuthAppStore persists per-tenant, per-instance forge OAuth-app credentials.
// GetByInstance backs the connect-flow resolver (which app to use for a given
// tenant + provider + base URL). Like ConnectionStore, Get is keyed by id only
// — the HTTP layer asserts tenant ownership before mutating.
type OAuthAppStore interface {
	Create(ctx context.Context, a ForgeOAuthApp) error
	Get(ctx context.Context, id string) (ForgeOAuthApp, error)
	Update(ctx context.Context, a ForgeOAuthApp) error
	Delete(ctx context.Context, id string) error
	ListByTenant(ctx context.Context, tenantID string) ([]ForgeOAuthApp, error)
	// ListByInstance returns every app a tenant holds on one instance, oldest
	// first — a tenant may register one per owning org. GetByInstance keeps the
	// legacy single-app answer (the oldest) for callers that predate the
	// Connection→app link and for providers where one app per instance is still
	// the right model.
	ListByInstance(ctx context.Context, tenantID string, provider Provider, baseURL string) ([]ForgeOAuthApp, error)
	GetByInstance(ctx context.Context, tenantID string, provider Provider, baseURL string) (ForgeOAuthApp, error)
}

// firstAppOnInstance collapses a per-instance listing to the legacy
// single-app answer: the OLDEST app on that instance. Determinism is the point
// — once several apps may share a host, an unordered "any match" would hand
// callers a different private key run to run. The oldest is the app that
// existed while the one-per-host constraint held, so pre-FK connections keep
// resolving exactly as they did.
func firstAppOnInstance(apps []ForgeOAuthApp, err error) (ForgeOAuthApp, error) {
	if err != nil {
		return ForgeOAuthApp{}, err
	}
	if len(apps) == 0 {
		return ForgeOAuthApp{}, ErrOAuthAppNotFound
	}
	return apps[0], nil
}

// ownerOrInstance labels an app in operator-facing errors by the account that
// owns it, falling back to the instance when unknown. With several apps per
// host, "already exists for github on https://github.com" no longer identifies
// which one — the owning org is what the operator actually recognises.
func ownerOrInstance(a ForgeOAuthApp) string {
	if a.OwnerLogin != "" {
		return a.OwnerLogin + " (" + a.ForgeBaseURL + ")"
	}
	return a.ForgeBaseURL
}

// ---- in-memory store (tests / local) ----

type MemoryOAuthAppStore struct {
	mu   sync.RWMutex
	apps map[string]ForgeOAuthApp
}

func NewMemoryOAuthAppStore() *MemoryOAuthAppStore {
	return &MemoryOAuthAppStore{apps: make(map[string]ForgeOAuthApp)}
}

func (m *MemoryOAuthAppStore) Create(_ context.Context, a ForgeOAuthApp) error {
	a.ForgeBaseURL = CanonicalBaseURL(a.Provider, a.ForgeBaseURL)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.apps {
		if ex.TenantID == a.TenantID && ex.Provider == a.Provider &&
			ex.ForgeBaseURL == a.ForgeBaseURL && ex.OwnerLogin == a.OwnerLogin {
			return fmt.Errorf("%w for %s on %s", ErrOAuthAppExists, a.Provider, ownerOrInstance(a))
		}
	}
	m.apps[a.ID] = a
	return nil
}

func (m *MemoryOAuthAppStore) Get(_ context.Context, id string) (ForgeOAuthApp, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.apps[id]
	if !ok {
		return ForgeOAuthApp{}, ErrOAuthAppNotFound
	}
	return a, nil
}

func (m *MemoryOAuthAppStore) Update(_ context.Context, a ForgeOAuthApp) error {
	a.ForgeBaseURL = CanonicalBaseURL(a.Provider, a.ForgeBaseURL)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.apps[a.ID]; !ok {
		return ErrOAuthAppNotFound
	}
	m.apps[a.ID] = a
	return nil
}

func (m *MemoryOAuthAppStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.apps[id]; !ok {
		return ErrOAuthAppNotFound
	}
	delete(m.apps, id)
	return nil
}

func (m *MemoryOAuthAppStore) ListByTenant(_ context.Context, tenantID string) ([]ForgeOAuthApp, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ForgeOAuthApp
	for _, a := range m.apps {
		if a.TenantID == tenantID {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryOAuthAppStore) ListByInstance(_ context.Context, tenantID string, provider Provider, baseURL string) ([]ForgeOAuthApp, error) {
	base := CanonicalBaseURL(provider, baseURL)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ForgeOAuthApp
	for _, a := range m.apps {
		if a.TenantID == tenantID && a.Provider == provider && a.ForgeBaseURL == base {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (m *MemoryOAuthAppStore) GetByInstance(ctx context.Context, tenantID string, provider Provider, baseURL string) (ForgeOAuthApp, error) {
	return firstAppOnInstance(m.ListByInstance(ctx, tenantID, provider, baseURL))
}

// ---- Mongo store ----

const OAuthAppsCollectionName = "forge_oauth_apps"

type MongoOAuthAppStore struct {
	coll *mongo.Collection
}

func NewMongoOAuthAppStore(db *mongo.Database) *MongoOAuthAppStore {
	return &MongoOAuthAppStore{coll: db.Collection(OAuthAppsCollectionName)}
}

// legacyOAuthAppInstanceIndex is the pre-owner uniqueness key. Creating the
// replacement does NOT retire it, and while it survives it keeps enforcing
// one app per host — which is exactly the constraint being lifted. Dropped
// explicitly, before the new index is created, so a deployment actually
// changes behaviour instead of silently keeping the old rule.
const legacyOAuthAppInstanceIndex = "tenant_provider_baseurl_unique"

func (s *MongoOAuthAppStore) EnsureSchema(ctx context.Context) error {
	if err := s.coll.Indexes().DropOne(ctx, legacyOAuthAppInstanceIndex); err != nil {
		// IndexNotFound (fresh install, or already migrated) is the expected
		// steady state — anything else is a real failure worth surfacing.
		if !mongoutil.IsIndexNotFound(err) {
			return fmt.Errorf("forge: drop legacy oauth app index: %w", err)
		}
	}
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		// Uniqueness is per OWNING ACCOUNT, not per host: a tenant legitimately
		// holds one app per GitHub org (a private App is only installable on its
		// owner), so keying on the host alone made the second org impossible.
		// Legacy rows carry no owner_login; they all collapse to owner_login:""
		// and therefore remain bound by exactly the old one-per-host rule, which
		// is what keeps this index creatable over existing data.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "provider", Value: 1}, {Key: "forge_base_url", Value: 1}, {Key: "owner_login", Value: 1}}, Options: options.Index().SetUnique(true).SetName("tenant_provider_baseurl_owner_unique")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("forge: ensure forge_oauth_apps indexes: %w", err)
	}
	return nil
}

func (s *MongoOAuthAppStore) Create(ctx context.Context, a ForgeOAuthApp) error {
	a.ForgeBaseURL = CanonicalBaseURL(a.Provider, a.ForgeBaseURL)
	if _, err := s.coll.InsertOne(ctx, a); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("%w for %s on %s", ErrOAuthAppExists, a.Provider, ownerOrInstance(a))
		}
		return fmt.Errorf("forge: insert oauth app: %w", err)
	}
	return nil
}

func (s *MongoOAuthAppStore) Get(ctx context.Context, id string) (ForgeOAuthApp, error) {
	return mongoutil.FindOne[ForgeOAuthApp](ctx, s.coll, bson.M{"_id": id}, ErrOAuthAppNotFound, "forge: get oauth app")
}

func (s *MongoOAuthAppStore) Update(ctx context.Context, a ForgeOAuthApp) error {
	a.ForgeBaseURL = CanonicalBaseURL(a.Provider, a.ForgeBaseURL)
	return mongoutil.ReplaceOneChecked(ctx, s.coll, bson.M{"_id": a.ID}, a, nil, ErrOAuthAppNotFound, "forge: update oauth app")
}

func (s *MongoOAuthAppStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, ErrOAuthAppNotFound, "forge: delete oauth app")
}

func (s *MongoOAuthAppStore) ListByTenant(ctx context.Context, tenantID string) ([]ForgeOAuthApp, error) {
	return mongoutil.FindAllSorted[ForgeOAuthApp](ctx, s.coll, bson.M{"tenant_id": tenantID}, "created_at",
		"forge: list oauth apps", "forge: decode oauth apps")
}

func (s *MongoOAuthAppStore) ListByInstance(ctx context.Context, tenantID string, provider Provider, baseURL string) ([]ForgeOAuthApp, error) {
	base := CanonicalBaseURL(provider, baseURL)
	return mongoutil.FindAllSorted[ForgeOAuthApp](ctx, s.coll,
		bson.M{"tenant_id": tenantID, "provider": provider, "forge_base_url": base}, "created_at",
		"forge: list oauth apps by instance", "forge: decode oauth apps by instance")
}

func (s *MongoOAuthAppStore) GetByInstance(ctx context.Context, tenantID string, provider Provider, baseURL string) (ForgeOAuthApp, error) {
	return firstAppOnInstance(s.ListByInstance(ctx, tenantID, provider, baseURL))
}
