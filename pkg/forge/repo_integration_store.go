package forge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/SocialGouv/iterion/pkg/internal/mongoutil"
	"github.com/SocialGouv/iterion/pkg/internal/storekit"
	"github.com/SocialGouv/iterion/pkg/llmroute"
)

// RepoIntegration is the join row recording what the orchestrator
// provisioned for one (connection, repo): the bots enabled, the iterion
// webhook config, the forge-side hook, and the managed secret (the
// connection's forge token is pinned per-webhook via Config.SecretOverrides,
// not a bot binding). It is the studio Integrations tab's source of truth
// and the unit of deprovision.
type RepoIntegration struct {
	// Version fences every snapshot write, including activation and retirement.
	Version int64 `bson:"version" json:"-"`
	// A retired row retains its monotonic version; new activations use distinct webhook epochs.
	Retired               bool     `bson:"retired,omitempty" json:"-"`
	RetireCleanupComplete bool     `bson:"retire_cleanup_complete,omitempty" json:"-"`
	WebhookSecretSealed   []byte   `bson:"webhook_secret_sealed,omitempty" json:"-"`
	CandidateWebhookID    string   `bson:"candidate_webhook_id,omitempty" json:"-"`
	CandidateSecretSealed []byte   `bson:"candidate_secret_sealed,omitempty" json:"-"`
	ProvisionPending      bool     `bson:"provision_pending,omitempty" json:"provision_pending,omitempty"`
	ID                    string   `bson:"_id" json:"id"`
	TenantID              string   `bson:"tenant_id" json:"tenant_id"`
	ConnectionID          string   `bson:"connection_id" json:"connection_id"`
	Provider              Provider `bson:"provider" json:"provider"`
	RepoFullName          string   `bson:"repo_full_name" json:"repo_full_name"`
	BotIDs                []string `bson:"bot_ids" json:"bot_ids"`
	EventsNormalized      []string `bson:"events_normalized" json:"events_normalized"`

	WebhookID string `bson:"webhook_id" json:"webhook_id"`                 // -> webhooks.Config._id
	HookID    string `bson:"hook_id" json:"hook_id"`                       // forge-side hook id
	HookURL   string `bson:"hook_url,omitempty" json:"hook_url,omitempty"` // the inbound URL we registered
	// ManagedSecretID is an internal store reference; never exposed via the
	// API (useless without the master key, but no reason to surface it).
	ManagedSecretID string `bson:"managed_secret_id,omitempty" json:"-"`

	// ProjectBot is the per-repository bot credential iterion minted for this
	// integration, on a connection whose BotIdentity is
	// BotIdentityProjectToken: the GitLab project access token whose bot user
	// every review is posted under. Nil on every other integration. When set,
	// ManagedSecretID is this integration's OWN generic secret (the minted
	// token), not the connection's: the provisioner pins it on the webhook,
	// the rotation worker rewrites it, and deprovision revokes the token and
	// deletes the secret with the row.
	ProjectBot *ProjectBot `bson:"project_bot,omitempty" json:"project_bot,omitempty"`

	// LaunchVars are the operator's per-repo overrides for every run this
	// integration launches, layered LAST (after the bots' own manifest vars),
	// and re-applied on every Provision. Provisioning rewrites the whole
	// webhook config from the manifests, so an override PATCHed onto the
	// webhook is silently lost at the next enable/update — persisting it here
	// is what makes a per-repo choice durable. The canonical case is naming
	// this repo's merge gate: with several bots able to post it, the repo has
	// exactly one required check and each bot fills it for the PRs it owns.
	LaunchVars map[string]string `bson:"launch_vars,omitempty" json:"launch_vars,omitempty"`

	// Routing is the operator's per-repo routing policy (pkg/llmroute,
	// ADR-129): the binding level for every launch this integration's
	// webhook and schedules make. It lives here, like LaunchVars, because
	// Provision rebuilds the whole webhook config from the manifests — a
	// pin set only on the config was silently lost at the next enable —
	// and every projection of this record (the webhook copy, the scheduled
	// rows, the verify pass, the org-approval park/apply) is rebuilt from
	// it. Nil = the level says nothing.
	Routing *llmroute.Policy `bson:"routing,omitempty" json:"routing,omitempty"`

	// HoldLabels is the operator's bot-agnostic pause: a pull request or issue
	// carrying one of these labels vetoes EVERY auto-launch on this repo. It
	// lives here for the same reason as LaunchVars — Provision rebuilds the
	// webhook config from the manifests, so a pause set only on the config was
	// silently disarmed by the next `bots enable`. That mattered little while
	// every automatic lane also had a human trigger; it is the only brake on the
	// zero-touch lane below.
	HoldLabels []string `bson:"hold_labels,omitempty" json:"hold_labels,omitempty"`

	// LabelAllowlist narrows the issue-labeled lane to the labels this repo
	// treats as "dispatch the implementer" (e.g. ["implement"]); empty means
	// ANY freshly-applied label launches it. It lives here, not only on the
	// webhook config, for the same reason as HoldLabels: Provision rebuilds
	// that config from the manifests, so a narrowing set only on the config is
	// wiped by the next enable — and that regression is both silent and
	// fail-OPEN, turning every triage label back into a feature-dev campaign.
	LabelAllowlist []string `bson:"label_allowlist,omitempty" json:"label_allowlist,omitempty"`

	// AutoFixOnGateFailure opts this repo into the ZERO-TOUCH lane: when a review
	// leaves the merge gate red, the enabled bot that consumes reviews is
	// launched on that head to answer the findings, with no human command.
	//
	// Off by default, and that is a decision rather than caution. A reviewer
	// alone leaves the developer in the middle — findings land, they choose what
	// to act on, and they can hand the work over with a comment whenever they
	// want. Turning the hand-over on for everyone removes that arbitration from
	// every developer on the repo to save one comment, so it is the repo's call.
	//
	// Persisted here, like LaunchVars and Overlap: Provision rebuilds the webhook
	// config from the manifests, so a choice living only on the config is wiped
	// by the next enable.
	AutoFixOnGateFailure bool `bson:"auto_fix_on_gate_failure,omitempty" json:"auto_fix_on_gate_failure,omitempty"`

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

// ProjectBot records a minted per-repository bot credential: enough to rotate
// it (TokenID, ExpiresAt), to recognise it (Login — what the actor guard reads
// off the webhook config as BotLogin), and to say what last happened to it.
type ProjectBot struct {
	// TokenID is the forge-side id of the access token.
	TokenID string `bson:"token_id" json:"token_id"`
	// Login is the bot user's username (GitLab: project_<id>_bot_<hex>).
	Login string `bson:"login" json:"login"`
	// UserID is the bot user's id on the forge.
	UserID string `bson:"user_id,omitempty" json:"user_id,omitempty"`
	// Name is the token's name and the bot's display name (ProjectBotName).
	Name string `bson:"name,omitempty" json:"name,omitempty"`
	// ExpiresAt is the token's expiry in the CLEAR, so the rotation worker
	// lists due bots without opening a secret. Nil = the forge set none, and
	// the bot is never due.
	ExpiresAt *time.Time `bson:"expires_at,omitempty" json:"expires_at,omitempty"`
	// MintedAt dates the credential; with ExpiresAt it is the lifetime the
	// forge granted, which a rotation asks for again.
	MintedAt time.Time `bson:"minted_at" json:"minted_at"`
	// RotatedAt is the last successful rotation; nil = never rotated.
	RotatedAt *time.Time `bson:"rotated_at,omitempty" json:"rotated_at,omitempty"`
	// RotationError is the forge's refusal of the last rotation attempt, kept
	// so the operator reads why a bot is about to expire; cleared by the next
	// successful rotation.
	RotationError string `bson:"rotation_error,omitempty" json:"rotation_error,omitempty"`
	// AvatarAppliedAt is when the iterion-bot avatar was set on the bot user,
	// AvatarError the forge's refusal when it was not. Neither blocks anything.
	AvatarAppliedAt *time.Time `bson:"avatar_applied_at,omitempty" json:"avatar_applied_at,omitempty"`
	AvatarError     string     `bson:"avatar_error,omitempty" json:"avatar_error,omitempty"`
}

// RepoIntegrationStore persists repo integrations. GetByConnRepo backs the
// orchestrator's race-safe find-or-create; ListByWebhook backs the
// webhook-delete "in use by integration" guard.
type RepoIntegrationStore interface {
	Create(ctx context.Context, ri RepoIntegration) error
	Get(ctx context.Context, id string) (RepoIntegration, error)
	// Lifecycle reads include pending-only reservations and retired tombstones.
	GetProvision(ctx context.Context, id string) (RepoIntegration, error)
	GetProvisionByConnRepo(ctx context.Context, tenantID, connID, repo string) (RepoIntegration, error)
	Update(ctx context.Context, ri RepoIntegration) error
	// WithProvisionVersion fences a local projection against activation/retirement
	// and advances Version after a successful callback.
	// The callback performs store writes only, with the supplied context, and must
	// not call this integration store. Mongo callbacks may retry in a transaction.
	WithProvisionVersion(ctx context.Context, id string, version int64, retired bool, write func(context.Context) error) error
	// RecordProjectBot changes only the credential result while its previous
	// identity is still current. Unrelated operator writes do not lose a token
	// the forge has already rotated.
	RecordProjectBot(ctx context.Context, id, secretID, tokenID string, bot *ProjectBot, at time.Time) error
	// StampSynced writes the issue sync's high-water mark and nothing else.
	// Update writes the whole document from the caller's snapshot, which a sync
	// pass reads minutes before it stamps: that write reverts whatever an
	// operator changed in between. Returns ErrIntegrationNotFound for an
	// integration deleted during the pass.
	//
	// It arbitrates nothing: the last stamp wins, even one carrying an EARLIER
	// instant than the stored mark. Several passes stamp the same integration
	// (the leased sweep, a manual sync, the webhook projection), so the mark is
	// not a monotonic counter — every stamp is the start of a read that
	// completed, and a mark that stepped back only widens the next window.
	StampSynced(ctx context.Context, id string, at time.Time) error
	Delete(ctx context.Context, id string) error
	GetByConnRepo(ctx context.Context, tenantID, connID, repo string) (RepoIntegration, error)
	ListByTenant(ctx context.Context, tenantID string) ([]RepoIntegration, error)
	ListByConnection(ctx context.Context, tenantID, connID string) ([]RepoIntegration, error)
	// ListProvisionsByConnection includes unconfirmed reservations and tombstones
	// so disconnect can revoke their retained credentials as well.
	ListProvisionsByConnection(ctx context.Context, tenantID, connID string) ([]RepoIntegration, error)
	ListByWebhook(ctx context.Context, tenantID, webhookID string) ([]RepoIntegration, error)
	// ListSyncEnabled returns every integration (across all tenants) with
	// SyncIssuesEnabled set, for the periodic forge→board sync worker.
	ListSyncEnabled(ctx context.Context) ([]RepoIntegration, error)
	// ListSyncEnabledForRepo returns the sync-enabled integrations for a single
	// repo slug — the per-webhook board projection's filter, pushed down to the
	// query so a webhook doesn't scan every tenant's integrations.
	ListSyncEnabledForRepo(ctx context.Context, repo string) ([]RepoIntegration, error)
	// ListProjectBotsExpiringBefore returns every integration (across all
	// tenants) whose minted project bot expires at or before t — the rotation
	// worker's sweep. An integration with no bot, or a bot the forge gave no
	// expiry, is never due.
	ListProjectBotsExpiringBefore(ctx context.Context, t time.Time) ([]RepoIntegration, error)
}

var ErrIntegrationConflict = errors.New("forge: integration changed concurrently; re-read and retry")

func integrationVisible(ri RepoIntegration) bool {
	return !ri.Retired && (!ri.ProvisionPending || ri.WebhookID != "")
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
	ri = storekit.Snapshot(ri)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.items {
		if ex.ID == ri.ID || ex.TenantID == ri.TenantID && ex.ConnectionID == ri.ConnectionID && ex.RepoFullName == ri.RepoFullName {
			return ErrIntegrationConflict
		}
	}
	m.items[ri.ID] = ri
	return nil
}

func (m *MemoryRepoIntegrationStore) Get(_ context.Context, id string) (RepoIntegration, error) {
	ri, err := m.GetProvision(context.Background(), id)
	if err == nil && !integrationVisible(ri) {
		return RepoIntegration{}, ErrIntegrationNotFound
	}
	return ri, err
}

func (m *MemoryRepoIntegrationStore) GetProvision(_ context.Context, id string) (RepoIntegration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ri, ok := m.items[id]
	if !ok {
		return RepoIntegration{}, ErrIntegrationNotFound
	}
	return storekit.Snapshot(ri), nil
}

func (m *MemoryRepoIntegrationStore) Update(_ context.Context, ri RepoIntegration) error {
	ri = storekit.Snapshot(ri)
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, ok := m.items[ri.ID]
	if !ok {
		return ErrIntegrationNotFound
	}
	if cur.Version != ri.Version {
		return ErrIntegrationConflict
	}
	ri.Version++
	ri.LastSyncedAt = cur.LastSyncedAt
	m.items[ri.ID] = ri
	return nil
}

func (m *MemoryRepoIntegrationStore) StampSynced(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ri, ok := m.items[id]
	if !ok {
		return ErrIntegrationNotFound
	}
	ri.LastSyncedAt = at
	m.items[id] = ri
	return nil
}

func (m *MemoryRepoIntegrationStore) WithProvisionVersion(ctx context.Context, id string, version int64, retired bool, write func(context.Context) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ri, ok := m.items[id]
	if !ok {
		return ErrIntegrationNotFound
	}
	if ri.Version != version || ri.Retired != retired {
		return ErrIntegrationConflict
	}
	if err := write(ctx); err != nil {
		return err
	}
	ri.Version++
	m.items[id] = ri
	return nil
}

func (m *MemoryRepoIntegrationStore) RecordProjectBot(_ context.Context, id, secretID, tokenID string, bot *ProjectBot, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ri, ok := m.items[id]
	if !ok || !integrationVisible(ri) {
		return ErrIntegrationNotFound
	}
	if ri.ManagedSecretID != secretID || ri.ProjectBot == nil || ri.ProjectBot.TokenID != tokenID {
		return ErrIntegrationConflict
	}
	ri.ProjectBot = storekit.Snapshot(bot)
	ri.UpdatedAt = at
	ri.Version++
	m.items[id] = ri
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
	ri, err := m.GetProvisionByConnRepo(context.Background(), tenantID, connID, repo)
	if err == nil && !integrationVisible(ri) {
		return RepoIntegration{}, ErrIntegrationNotFound
	}
	return ri, err
}

func (m *MemoryRepoIntegrationStore) GetProvisionByConnRepo(_ context.Context, tenantID, connID, repo string) (RepoIntegration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, ri := range m.items {
		if ri.TenantID == tenantID && ri.ConnectionID == connID && ri.RepoFullName == repo {
			return storekit.Snapshot(ri), nil
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

func (m *MemoryRepoIntegrationStore) ListProvisionsByConnection(_ context.Context, tenantID, connID string) ([]RepoIntegration, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []RepoIntegration
	for _, ri := range m.items {
		if ri.TenantID == tenantID && ri.ConnectionID == connID {
			out = append(out, ri)
		}
	}
	return storekit.Snapshot(out), nil
}

func (m *MemoryRepoIntegrationStore) ListSyncEnabled(_ context.Context) ([]RepoIntegration, error) {
	return m.filter(func(ri RepoIntegration) bool { return ri.SyncIssuesEnabled }), nil
}

func (m *MemoryRepoIntegrationStore) ListSyncEnabledForRepo(_ context.Context, repo string) ([]RepoIntegration, error) {
	return m.filter(func(ri RepoIntegration) bool {
		return ri.SyncIssuesEnabled && ri.RepoFullName == repo
	}), nil
}

func (m *MemoryRepoIntegrationStore) ListProjectBotsExpiringBefore(_ context.Context, t time.Time) ([]RepoIntegration, error) {
	return m.filter(func(ri RepoIntegration) bool {
		return ri.ProjectBot != nil && ri.ProjectBot.ExpiresAt != nil && !ri.ProjectBot.ExpiresAt.After(t)
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
		if integrationVisible(ri) && keep(ri) {
			out = append(out, ri)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return storekit.Snapshot(out)
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
		// The rotation sweep's key: only integrations carrying a minted bot
		// with an expiry are indexed (sparse), so the sweep never scans the
		// rows that can never be due.
		{Keys: bson.D{{Key: "project_bot.expires_at", Value: 1}}, Options: options.Index().SetSparse(true).SetName("project_bot_expiry")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("forge: ensure repo_integrations indexes: %w", err)
	}
	return nil
}

func (s *MongoRepoIntegrationStore) Create(ctx context.Context, ri RepoIntegration) error {
	if _, err := s.coll.InsertOne(ctx, ri); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return ErrIntegrationConflict
		}
		return fmt.Errorf("forge: insert repo integration: %w", err)
	}
	return nil
}

func (s *MongoRepoIntegrationStore) Get(ctx context.Context, id string) (RepoIntegration, error) {
	ri, err := s.GetProvision(ctx, id)
	if err == nil && !integrationVisible(ri) {
		return RepoIntegration{}, ErrIntegrationNotFound
	}
	return ri, err
}

func (s *MongoRepoIntegrationStore) GetProvision(ctx context.Context, id string) (RepoIntegration, error) {
	return mongoutil.FindOne[RepoIntegration](ctx, s.coll, bson.M{"_id": id}, ErrIntegrationNotFound, "forge: get repo integration")
}

func (s *MongoRepoIntegrationStore) Update(ctx context.Context, ri RepoIntegration) error {
	filter := bson.M{"_id": ri.ID, "version": ri.Version}
	if ri.Version == 0 {
		delete(filter, "version")
		filter["$or"] = bson.A{bson.M{"version": 0}, bson.M{"version": bson.M{"$exists": false}}}
	}
	ri.Version++
	// Preserve independently stamped sync metadata in the same atomic write.
	raw, err := bson.Marshal(ri)
	if err != nil {
		return err
	}
	var doc bson.M
	if err = bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	delete(doc, "last_synced_at")
	pipeline := mongo.Pipeline{bson.D{{Key: "$replaceWith", Value: bson.M{"$mergeObjects": bson.A{bson.M{"last_synced_at": "$last_synced_at"}, bson.M{"$literal": doc}}}}}}
	r, err := s.coll.UpdateOne(ctx, filter, pipeline)
	if err != nil {
		return fmt.Errorf("forge: update repo integration: %w", err)
	}
	if r.MatchedCount == 0 {
		if _, err := s.GetProvision(ctx, ri.ID); err != nil {
			return err
		}
		return ErrIntegrationConflict
	}
	return nil
}

func (s *MongoRepoIntegrationStore) StampSynced(ctx context.Context, id string, at time.Time) error {
	return mongoutil.UpdateOneChecked(ctx, s.coll, bson.M{"_id": id},
		bson.M{"$set": bson.M{"last_synced_at": at}}, ErrIntegrationNotFound, "forge: stamp repo integration sync")
}

func (s *MongoRepoIntegrationStore) WithProvisionVersion(ctx context.Context, id string, version int64, retired bool, write func(context.Context) error) error {
	session, err := s.coll.Database().Client().StartSession()
	if err != nil {
		return err
	}
	defer session.EndSession(context.WithoutCancel(ctx))
	_, err = session.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		filter := bson.M{"_id": id, "retired": bson.M{"$ne": true}, "version": version}
		if retired {
			filter["retired"] = true
		}
		if version == 0 {
			filter["version"] = bson.M{"$in": bson.A{0, nil}}
		}
		// A real write conflicts with a concurrent retirement or replacement.
		// A separate version read would leave the projection's commit unfenced.
		r, err := s.coll.UpdateOne(ctx, filter, bson.M{"$inc": bson.M{"version": 1}})
		if err != nil {
			return nil, err
		}
		if r.MatchedCount != 1 {
			return nil, ErrIntegrationConflict
		}
		return nil, write(ctx)
	})
	return err
}

func (s *MongoRepoIntegrationStore) RecordProjectBot(ctx context.Context, id, secretID, tokenID string, bot *ProjectBot, at time.Time) error {
	r, err := s.coll.UpdateOne(ctx, bson.M{"_id": id, "retired": bson.M{"$ne": true}, "managed_secret_id": secretID, "project_bot.token_id": tokenID, "$or": bson.A{bson.M{"provision_pending": bson.M{"$ne": true}}, bson.M{"webhook_id": bson.M{"$nin": bson.A{"", nil}}}}}, bson.M{"$set": bson.M{"project_bot": bot, "updated_at": at}, "$inc": bson.M{"version": 1}})
	if err != nil {
		return fmt.Errorf("forge: record project bot: %w", err)
	}
	if r.MatchedCount == 0 {
		if _, err := s.Get(ctx, id); err != nil {
			return err
		}
		return ErrIntegrationConflict
	}
	return nil
}

func (s *MongoRepoIntegrationStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, ErrIntegrationNotFound, "forge: delete repo integration")
}

func (s *MongoRepoIntegrationStore) GetByConnRepo(ctx context.Context, tenantID, connID, repo string) (RepoIntegration, error) {
	ri, err := s.GetProvisionByConnRepo(ctx, tenantID, connID, repo)
	if err == nil && !integrationVisible(ri) {
		return RepoIntegration{}, ErrIntegrationNotFound
	}
	return ri, err
}

func (s *MongoRepoIntegrationStore) GetProvisionByConnRepo(ctx context.Context, tenantID, connID, repo string) (RepoIntegration, error) {
	return mongoutil.FindOne[RepoIntegration](ctx, s.coll, bson.M{"tenant_id": tenantID, "connection_id": connID, "repo_full_name": repo}, ErrIntegrationNotFound, "forge: get repo integration by conn/repo")
}

func (s *MongoRepoIntegrationStore) ListByTenant(ctx context.Context, tenantID string) ([]RepoIntegration, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID})
}

func (s *MongoRepoIntegrationStore) ListByConnection(ctx context.Context, tenantID, connID string) ([]RepoIntegration, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID, "connection_id": connID})
}

func (s *MongoRepoIntegrationStore) ListProvisionsByConnection(ctx context.Context, tenantID, connID string) ([]RepoIntegration, error) {
	return mongoutil.FindAllSorted[RepoIntegration](ctx, s.coll, bson.M{"tenant_id": tenantID, "connection_id": connID}, "created_at", "forge: list provisions", "forge: decode provisions")
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

func (s *MongoRepoIntegrationStore) ListProjectBotsExpiringBefore(ctx context.Context, t time.Time) ([]RepoIntegration, error) {
	// A missing project_bot.expires_at never matches $lte, so integrations
	// with no bot — or a bot with no expiry — are excluded by the query itself.
	return s.find(ctx, bson.M{"project_bot.expires_at": bson.M{"$lte": t}})
}

func (s *MongoRepoIntegrationStore) find(ctx context.Context, filter bson.M) ([]RepoIntegration, error) {
	filter["retired"] = bson.M{"$ne": true}
	filter["$or"] = bson.A{bson.M{"provision_pending": bson.M{"$ne": true}}, bson.M{"webhook_id": bson.M{"$nin": bson.A{"", nil}}}}
	return mongoutil.FindAllSorted[RepoIntegration](ctx, s.coll, filter, "created_at",
		"forge: list repo integrations", "forge: decode repo integrations")
}
