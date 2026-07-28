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

// RepoIntegration is the join row recording what the orchestrator
// provisioned for one (connection, repo): the bots enabled, the iterion
// webhook config, the forge-side hook, and the managed secret (the
// connection's forge token is pinned per-webhook via Config.SecretOverrides,
// not a bot binding). It is the studio Integrations tab's source of truth
// and the unit of deprovision.
type RepoIntegration struct {
	ID               string   `bson:"_id" json:"id"`
	TenantID         string   `bson:"tenant_id" json:"tenant_id"`
	ConnectionID     string   `bson:"connection_id" json:"connection_id"`
	Provider         Provider `bson:"provider" json:"provider"`
	RepoFullName     string   `bson:"repo_full_name" json:"repo_full_name"`
	BotIDs           []string `bson:"bot_ids" json:"bot_ids"`
	EventsNormalized []string `bson:"events_normalized" json:"events_normalized"`

	WebhookID string `bson:"webhook_id" json:"webhook_id"`                 // -> webhooks.Config._id
	HookID    string `bson:"hook_id" json:"hook_id"`                       // forge-side hook id
	HookURL   string `bson:"hook_url,omitempty" json:"hook_url,omitempty"` // the inbound URL we registered
	// ManagedSecretID is an internal store reference; never exposed via the
	// API (useless without the master key, but no reason to surface it).
	ManagedSecretID string `bson:"managed_secret_id,omitempty" json:"-"`

	// LaunchVars are the operator's per-repo overrides for every run this
	// integration launches, layered LAST (after the bots' own manifest vars),
	// and re-applied on every Provision. Provisioning rewrites the whole
	// webhook config from the manifests, so an override PATCHed onto the
	// webhook is silently lost at the next enable/update — persisting it here
	// is what makes a per-repo choice durable. The canonical case is naming
	// this repo's merge gate: with several bots able to post it, the repo has
	// exactly one required check and each bot fills it for the PRs it owns.
	LaunchVars map[string]string `bson:"launch_vars,omitempty" json:"launch_vars,omitempty"`

	// Overlap is the operator's concurrency policy for this repo's webhook
	// (pkg/schedgate vocabulary; empty = allow, the historical behaviour).
	// Persisted here for the same reason as LaunchVars: Provision rebuilds the
	// webhook config as a whole literal, so anything set only on the config is
	// wiped by the next enable.
	Overlap string `bson:"overlap,omitempty" json:"overlap,omitempty"`

	// SyncIssuesEnabled, when true, makes the forge→board sync worker mirror
	// this repo's forge issues into the team's kanban board (one-way: forge is
	// the source; a card's column is operator-owned once created). Toggled per
	// repo from the studio Integrations tab. Off by default.
	SyncIssuesEnabled bool `bson:"sync_issues_enabled,omitempty" json:"sync_issues_enabled,omitempty"`
	// LastSyncedAt is the high-water mark of the last successful issue sync,
	// passed as the `since` filter on the next sweep for incremental sync.
	LastSyncedAt time.Time `bson:"last_synced_at,omitempty" json:"last_synced_at,omitempty"`
	// MinAuthorRole is the minimum repo role (gitlab vocabulary; "" →
	// developer ≡ write) an issue author must hold for their synced card to
	// be stamped triage:auto (auto-triage) instead of parked needs:approval.
	MinAuthorRole string `bson:"min_author_role,omitempty" json:"min_author_role,omitempty"`

	CreatedBy string    `bson:"created_by" json:"created_by"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// RepoIntegrationStore persists repo integrations. GetByConnRepo backs the
// orchestrator's race-safe find-or-create; ListByWebhook backs the
// webhook-delete "in use by integration" guard.
type RepoIntegrationStore interface {
	Create(ctx context.Context, ri RepoIntegration) error
	Get(ctx context.Context, id string) (RepoIntegration, error)
	Update(ctx context.Context, ri RepoIntegration) error
	Delete(ctx context.Context, id string) error
	GetByConnRepo(ctx context.Context, tenantID, connID, repo string) (RepoIntegration, error)
	ListByTenant(ctx context.Context, tenantID string) ([]RepoIntegration, error)
	ListByConnection(ctx context.Context, tenantID, connID string) ([]RepoIntegration, error)
	ListByWebhook(ctx context.Context, tenantID, webhookID string) ([]RepoIntegration, error)
	// ListSyncEnabled returns every integration (across all tenants) with
	// SyncIssuesEnabled set, for the periodic forge→board sync worker.
	ListSyncEnabled(ctx context.Context) ([]RepoIntegration, error)
	// ListSyncEnabledForRepo returns the sync-enabled integrations for a single
	// repo slug — the per-webhook board projection's filter, pushed down to the
	// query so a webhook doesn't scan every tenant's integrations.
	ListSyncEnabledForRepo(ctx context.Context, repo string) ([]RepoIntegration, error)
}

// ---- in-memory store (tests / local) ----

type MemoryRepoIntegrationStore struct {
	mu    sync.RWMutex
	items map[string]RepoIntegration
}

func NewMemoryRepoIntegrationStore() *MemoryRepoIntegrationStore {
	return &MemoryRepoIntegrationStore{items: make(map[string]RepoIntegration)}
}

func (m *MemoryRepoIntegrationStore) Create(_ context.Context, ri RepoIntegration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.items {
		if ex.TenantID == ri.TenantID && ex.ConnectionID == ri.ConnectionID && ex.RepoFullName == ri.RepoFullName {
			return fmt.Errorf("forge: integration already exists for %s on connection %s", ri.RepoFullName, ri.ConnectionID)
		}
	}
	m.items[ri.ID] = ri
	return nil
}

func (m *MemoryRepoIntegrationStore) Get(_ context.Context, id string) (RepoIntegration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ri, ok := m.items[id]
	if !ok {
		return RepoIntegration{}, ErrIntegrationNotFound
	}
	return ri, nil
}

func (m *MemoryRepoIntegrationStore) Update(_ context.Context, ri RepoIntegration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[ri.ID]; !ok {
		return ErrIntegrationNotFound
	}
	m.items[ri.ID] = ri
	return nil
}

func (m *MemoryRepoIntegrationStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return ErrIntegrationNotFound
	}
	delete(m.items, id)
	return nil
}

func (m *MemoryRepoIntegrationStore) GetByConnRepo(_ context.Context, tenantID, connID, repo string) (RepoIntegration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ri := range m.items {
		if ri.TenantID == tenantID && ri.ConnectionID == connID && ri.RepoFullName == repo {
			return ri, nil
		}
	}
	return RepoIntegration{}, ErrIntegrationNotFound
}

func (m *MemoryRepoIntegrationStore) ListByTenant(_ context.Context, tenantID string) ([]RepoIntegration, error) {
	return m.filter(func(ri RepoIntegration) bool { return ri.TenantID == tenantID }), nil
}

func (m *MemoryRepoIntegrationStore) ListByConnection(_ context.Context, tenantID, connID string) ([]RepoIntegration, error) {
	return m.filter(func(ri RepoIntegration) bool {
		return ri.TenantID == tenantID && ri.ConnectionID == connID
	}), nil
}

func (m *MemoryRepoIntegrationStore) ListSyncEnabled(_ context.Context) ([]RepoIntegration, error) {
	return m.filter(func(ri RepoIntegration) bool { return ri.SyncIssuesEnabled }), nil
}

func (m *MemoryRepoIntegrationStore) ListSyncEnabledForRepo(_ context.Context, repo string) ([]RepoIntegration, error) {
	return m.filter(func(ri RepoIntegration) bool {
		return ri.SyncIssuesEnabled && ri.RepoFullName == repo
	}), nil
}

func (m *MemoryRepoIntegrationStore) ListByWebhook(_ context.Context, tenantID, webhookID string) ([]RepoIntegration, error) {
	return m.filter(func(ri RepoIntegration) bool {
		return ri.TenantID == tenantID && ri.WebhookID == webhookID
	}), nil
}

func (m *MemoryRepoIntegrationStore) filter(keep func(RepoIntegration) bool) []RepoIntegration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []RepoIntegration
	for _, ri := range m.items {
		if keep(ri) {
			out = append(out, ri)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ---- Mongo store ----

const RepoIntegrationsCollectionName = "repo_integrations"

type MongoRepoIntegrationStore struct {
	coll *mongo.Collection
}

func NewMongoRepoIntegrationStore(db *mongo.Database) *MongoRepoIntegrationStore {
	return &MongoRepoIntegrationStore{coll: db.Collection(RepoIntegrationsCollectionName)}
}

func (s *MongoRepoIntegrationStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "connection_id", Value: 1}, {Key: "repo_full_name", Value: 1}}, Options: options.Index().SetUnique(true).SetName("tenant_conn_repo_unique")},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "webhook_id", Value: 1}}, Options: options.Index().SetName("tenant_webhook")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("forge: ensure repo_integrations indexes: %w", err)
	}
	return nil
}

func (s *MongoRepoIntegrationStore) Create(ctx context.Context, ri RepoIntegration) error {
	if _, err := s.coll.InsertOne(ctx, ri); err != nil {
		return fmt.Errorf("forge: insert repo integration: %w", err)
	}
	return nil
}

func (s *MongoRepoIntegrationStore) Get(ctx context.Context, id string) (RepoIntegration, error) {
	return mongoutil.FindOne[RepoIntegration](ctx, s.coll, bson.M{"_id": id}, ErrIntegrationNotFound, "forge: get repo integration")
}

func (s *MongoRepoIntegrationStore) Update(ctx context.Context, ri RepoIntegration) error {
	return mongoutil.ReplaceOneChecked(ctx, s.coll, bson.M{"_id": ri.ID}, ri, nil, ErrIntegrationNotFound, "forge: update repo integration")
}

func (s *MongoRepoIntegrationStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, ErrIntegrationNotFound, "forge: delete repo integration")
}

func (s *MongoRepoIntegrationStore) GetByConnRepo(ctx context.Context, tenantID, connID, repo string) (RepoIntegration, error) {
	return mongoutil.FindOne[RepoIntegration](ctx, s.coll, bson.M{"tenant_id": tenantID, "connection_id": connID, "repo_full_name": repo}, ErrIntegrationNotFound, "forge: get repo integration by conn/repo")
}

func (s *MongoRepoIntegrationStore) ListByTenant(ctx context.Context, tenantID string) ([]RepoIntegration, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID})
}

func (s *MongoRepoIntegrationStore) ListByConnection(ctx context.Context, tenantID, connID string) ([]RepoIntegration, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID, "connection_id": connID})
}

func (s *MongoRepoIntegrationStore) ListByWebhook(ctx context.Context, tenantID, webhookID string) ([]RepoIntegration, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID, "webhook_id": webhookID})
}

func (s *MongoRepoIntegrationStore) ListSyncEnabled(ctx context.Context) ([]RepoIntegration, error) {
	return s.find(ctx, bson.M{"sync_issues_enabled": true})
}

func (s *MongoRepoIntegrationStore) ListSyncEnabledForRepo(ctx context.Context, repo string) ([]RepoIntegration, error) {
	return s.find(ctx, bson.M{"sync_issues_enabled": true, "repo_full_name": repo})
}

func (s *MongoRepoIntegrationStore) find(ctx context.Context, filter bson.M) ([]RepoIntegration, error) {
	return mongoutil.FindAllSorted[RepoIntegration](ctx, s.coll, filter, "created_at",
		"forge: list repo integrations", "forge: decode repo integrations")
}
