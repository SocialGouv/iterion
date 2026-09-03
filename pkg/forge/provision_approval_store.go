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
)

// ProvisionApproval is a repo-bot provisioning request parked for an ORG
// admin's decision (Org.RequireProvisionApproval). It records the FULL
// requested change — the same payload the orchestrator would have
// received — so an approval replays it verbatim and a rejection leaves
// zero forge-side surface (no hook, no webhook, no managed secret is
// created while a request is pending).
type ProvisionApproval struct {
	ID string `bson:"_id" json:"id"`
	// OrgID keys the approver's work list; TenantID is the requesting team.
	OrgID    string `bson:"org_id" json:"org_id"`
	TenantID string `bson:"tenant_id" json:"tenant_id"`

	ConnectionID string   `bson:"connection_id" json:"connection_id"`
	RepoFullName string   `bson:"repo_full_name" json:"repo_full_name"`
	BotIDs       []string `bson:"bot_ids" json:"bot_ids"`

	// IntegrationID is non-empty when the request UPDATES an existing
	// integration's bot set (Replace semantics) rather than enabling a new
	// repo; the integration keeps serving its current bots while pending.
	IntegrationID string `bson:"integration_id,omitempty" json:"integration_id,omitempty"`
	Replace       bool   `bson:"replace,omitempty" json:"replace,omitempty"`
	// BaseBotIDs snapshots the integration's bot set at park time (update
	// requests only). Approve refuses (409) when the live set has since
	// diverged: replaying the recorded request over a changed integration
	// could silently resurrect a bot the team removed meanwhile.
	BaseBotIDs []string `bson:"base_bot_ids,omitempty" json:"base_bot_ids,omitempty"`

	// The optional per-repo settings of the original request, replayed
	// verbatim on approval (forgeEnableReq semantics: nil leaves stored
	// values untouched).
	//
	// NO omitempty on the nil-significant ones. For these fields nil and
	// empty mean OPPOSITE things to the orchestrator — nil is "leave the
	// stored value alone", empty is "clear it" — and bson's omitempty drops
	// an empty slice/map, so `hold_labels: []` (lift every hold) would come
	// back from Mongo as nil and silently become a no-op on approval. Only
	// the cloud store serializes, so the memory store used by tests and
	// local mode hides this entirely.
	ScheduleCrons  map[string]string `bson:"schedule_crons,omitempty" json:"schedule_crons,omitempty"`
	LaunchVars     map[string]string `bson:"launch_vars" json:"launch_vars,omitempty"`
	Overlap        string            `bson:"overlap,omitempty" json:"overlap,omitempty"`
	AutoFix        *bool             `bson:"auto_fix,omitempty" json:"auto_fix,omitempty"`
	HoldLabels     []string          `bson:"hold_labels" json:"hold_labels,omitempty"`
	LabelAllowlist []string          `bson:"label_allowlist" json:"label_allowlist,omitempty"`

	// Base* snapshot the integration's live value at park time for each
	// REPLACE-style setting above, so approve can refuse a request whose
	// field the team has since changed on its own. Without them a parked
	// "lift the hold label" replays over a NEWER hold the team added while
	// the request sat in the queue, silently removing a brake nobody
	// decided to remove — the orchestrator assigns these fields
	// (existing.HoldLabels = operatorHold), it does not merge them.
	//
	// Only meaningful where the request carries the field (non-nil / non-
	// empty); a request that never mentioned a field adopts the live value
	// at replay time and is safe by construction.
	BaseLaunchVars     map[string]string `bson:"base_launch_vars" json:"base_launch_vars,omitempty"`
	BaseOverlap        string            `bson:"base_overlap,omitempty" json:"base_overlap,omitempty"`
	BaseAutoFix        bool              `bson:"base_auto_fix,omitempty" json:"base_auto_fix,omitempty"`
	BaseHoldLabels     []string          `bson:"base_hold_labels" json:"base_hold_labels,omitempty"`
	BaseLabelAllowlist []string          `bson:"base_label_allowlist" json:"base_label_allowlist,omitempty"`

	RequestedBy string    `bson:"requested_by" json:"requested_by"`
	CreatedAt   time.Time `bson:"created_at" json:"created_at"`
}

// ProvisionApprovalStore persists pending provisioning approvals. A
// record only exists while the decision is pending: approve replays the
// request then deletes it, reject deletes it — the audit log is the
// durable trail of both outcomes.
type ProvisionApprovalStore interface {
	Create(ctx context.Context, a ProvisionApproval) error
	Get(ctx context.Context, id string) (ProvisionApproval, error)
	Delete(ctx context.Context, id string) error
	ListByOrg(ctx context.Context, orgID string) ([]ProvisionApproval, error)
	ListByTenant(ctx context.Context, tenantID string) ([]ProvisionApproval, error)
}

var ErrProvisionApprovalNotFound = errors.New("forge: provision approval not found")

// ---- in-memory store (tests / local) ----

type MemoryProvisionApprovalStore struct {
	mu    sync.RWMutex
	items map[string]ProvisionApproval
}

func NewMemoryProvisionApprovalStore() *MemoryProvisionApprovalStore {
	return &MemoryProvisionApprovalStore{items: make(map[string]ProvisionApproval)}
}

func (m *MemoryProvisionApprovalStore) Create(_ context.Context, a ProvisionApproval) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ex := range m.items {
		if ex.TenantID == a.TenantID && ex.ConnectionID == a.ConnectionID && ex.RepoFullName == a.RepoFullName {
			return fmt.Errorf("forge: a provisioning request for %s is already pending approval", a.RepoFullName)
		}
	}
	m.items[a.ID] = a
	return nil
}

func (m *MemoryProvisionApprovalStore) Get(_ context.Context, id string) (ProvisionApproval, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.items[id]
	if !ok {
		return ProvisionApproval{}, ErrProvisionApprovalNotFound
	}
	return a, nil
}

func (m *MemoryProvisionApprovalStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[id]; !ok {
		return ErrProvisionApprovalNotFound
	}
	delete(m.items, id)
	return nil
}

func (m *MemoryProvisionApprovalStore) ListByOrg(_ context.Context, orgID string) ([]ProvisionApproval, error) {
	return m.filter(func(a ProvisionApproval) bool { return a.OrgID == orgID }), nil
}

func (m *MemoryProvisionApprovalStore) ListByTenant(_ context.Context, tenantID string) ([]ProvisionApproval, error) {
	return m.filter(func(a ProvisionApproval) bool { return a.TenantID == tenantID }), nil
}

func (m *MemoryProvisionApprovalStore) filter(keep func(ProvisionApproval) bool) []ProvisionApproval {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []ProvisionApproval
	for _, a := range m.items {
		if keep(a) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// ---- Mongo store ----

const ProvisionApprovalsCollectionName = "forge_provision_approvals"

type MongoProvisionApprovalStore struct {
	coll *mongo.Collection
}

func NewMongoProvisionApprovalStore(db *mongo.Database) *MongoProvisionApprovalStore {
	return &MongoProvisionApprovalStore{coll: db.Collection(ProvisionApprovalsCollectionName)}
}

func (s *MongoProvisionApprovalStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "org_id", Value: 1}}, Options: options.Index().SetName("org")},
		// One pending request per (team, connection, repo): a second submit
		// while the first awaits a decision is a duplicate, not a queue.
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "connection_id", Value: 1}, {Key: "repo_full_name", Value: 1}}, Options: options.Index().SetUnique(true).SetName("tenant_conn_repo_unique")},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("forge: ensure forge_provision_approvals indexes: %w", err)
	}
	return nil
}

func (s *MongoProvisionApprovalStore) Create(ctx context.Context, a ProvisionApproval) error {
	if _, err := s.coll.InsertOne(ctx, a); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("forge: a provisioning request for %s is already pending approval", a.RepoFullName)
		}
		return fmt.Errorf("forge: insert provision approval: %w", err)
	}
	return nil
}

func (s *MongoProvisionApprovalStore) Get(ctx context.Context, id string) (ProvisionApproval, error) {
	return mongoutil.FindOne[ProvisionApproval](ctx, s.coll, bson.M{"_id": id}, ErrProvisionApprovalNotFound, "forge: get provision approval")
}

func (s *MongoProvisionApprovalStore) Delete(ctx context.Context, id string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": id}, ErrProvisionApprovalNotFound, "forge: delete provision approval")
}

func (s *MongoProvisionApprovalStore) ListByOrg(ctx context.Context, orgID string) ([]ProvisionApproval, error) {
	return s.find(ctx, bson.M{"org_id": orgID})
}

func (s *MongoProvisionApprovalStore) ListByTenant(ctx context.Context, tenantID string) ([]ProvisionApproval, error) {
	return s.find(ctx, bson.M{"tenant_id": tenantID})
}

func (s *MongoProvisionApprovalStore) find(ctx context.Context, filter bson.M) ([]ProvisionApproval, error) {
	return mongoutil.FindAllSorted[ProvisionApproval](ctx, s.coll, filter, "created_at",
		"forge: list provision approvals", "forge: decode provision approvals")
}
