package pricefeed

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/modelspecs"
)

// cloneConfigRecord and cloneStateRecord deep-copy the record's maps and
// slices: a store that hands out (or stores) aliased maps shares them with
// every reader and writer — a corruption channel and a data race (round 1
// caught both under -race). The Mongo twin is immune by construction (it
// decodes fresh documents); the memory twin is what the routes, the
// reconciler's tests and the conformance suite share.
func cloneConfigRecord(rec *ConfigRecord) *ConfigRecord {
	if rec == nil {
		return nil
	}
	cp := *rec
	cp.Feeds = append([]FeedConfig(nil), rec.Feeds...)
	for i := range cp.Feeds {
		cp.Feeds[i].Params = maps.Clone(rec.Feeds[i].Params)
	}
	return &cp
}

func cloneStateRecord(rec *StateRecord) *StateRecord {
	if rec == nil {
		return nil
	}
	cp := *rec
	// The capability flags are POINTERS and the accepted efforts a
	// SLICE: a shallow clone hands every reader the same *bool and the
	// same backing array. Clone each.
	cp.Entries = make(map[string]modelspecs.Spec, len(rec.Entries))
	for id, spec := range rec.Entries {
		spec.Reasoning = cloneBool(spec.Reasoning)
		spec.ToolCall = cloneBool(spec.ToolCall)
		spec.Temperature = cloneBool(spec.Temperature)
		spec.ReasoningEfforts = slices.Clone(spec.ReasoningEfforts)
		cp.Entries[id] = spec
	}
	return &cp
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	cp := *b
	return &cp
}

// The feed's durable state (ADR-163 §7): one config document per scope
// (platform, org or team) — the deployment's or the tenant's declaration
// — and one state document per (scope, feed) — the elected reconciler's
// answer. Composite ids carry the scope the way the routing-policy
// records do; org deletion sweeps the org's and its teams' documents of
// both collections by id and prefix (pkg/cloud/orgsweep), and the
// platform documents are no org's rows: they survive every org purge.

// The Mongo collections. ColConfigs holds every scope's feed list, the
// platform's included: one document shape, one reconciler walk, one CAS
// and one route body for the three levels — so the platform level is a
// document id here, not a platformcfg family.
const (
	ColConfigs = "price_feeds"
	ColStates  = "price_feed_state"
)

// PlatformConfigDocID is the platform's feed-list document id: the
// deployment's own feeds, folded below every org and team. Its fetched
// feeds' state documents key "platform/<feed>" (StateDocID).
const PlatformConfigDocID = "platform"

// OrgConfigDocID is an org's feed-list document id.
func OrgConfigDocID(orgID string) string { return "org:" + orgID }

// TeamConfigDocID is a team's feed-list document id.
func TeamConfigDocID(teamID string) string { return "team:" + teamID }

// StateDocID is one (scope, feed) state document id.
func StateDocID(scopeDocID, feed string) string { return scopeDocID + "/" + feed }

// ConfigStore is the feed-list surface: read, CAS write, clear, and the
// list the reconciler walks. The CAS token is the record's UpdatedAt —
// a client that read stale state loses the race as a loud 409, never
// silently drops a concurrent editor's feed list.
type ConfigStore interface {
	Get(ctx context.Context, docID string) (*ConfigRecord, error)
	Put(ctx context.Context, docID string, rec *ConfigRecord) error
	PutIfUnchanged(ctx context.Context, docID string, rec *ConfigRecord, prevUpdatedAt time.Time) (bool, error)
	DeleteIfUnchanged(ctx context.Context, docID string, prevUpdatedAt time.Time) (bool, error)
	List(ctx context.Context) (map[string]*ConfigRecord, error)
}

// StateStore is the reconciler's answer surface. No CAS: the elected
// reconciler and a manual sync both write real fetched data, and last
// write wins between two truths of the same age.
type StateStore interface {
	Get(ctx context.Context, docID string) (*StateRecord, error)
	Put(ctx context.Context, docID string, rec *StateRecord) error
	List(ctx context.Context) (map[string]*StateRecord, error)
}

// MemoryConfigStore is the config surface's in-process twin: tests and
// any single-process shape that wants feeds without a database. The
// conformance suite pins it to the Mongo twin's semantics.
type MemoryConfigStore struct {
	mu      sync.Mutex
	configs map[string]*ConfigRecord
}

// NewMemoryConfigStore builds an empty in-process config store.
func NewMemoryConfigStore() *MemoryConfigStore {
	return &MemoryConfigStore{configs: map[string]*ConfigRecord{}}
}

// Get answers one config document, nil when never written. A read under
// an expired or cancelled ctx answers the ctx's error, as a database read
// does: the fold's per-level deadline reads the same here as in Mongo.
func (m *MemoryConfigStore) Get(ctx context.Context, docID string) (*ConfigRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.configs[docID]; ok {
		return cloneConfigRecord(rec), nil
	}
	return nil, nil
}

// Put writes one config document, stamping UpdatedAt.
func (m *MemoryConfigStore) Put(_ context.Context, docID string, rec *ConfigRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := cloneConfigRecord(rec)
	cp.UpdatedAt = time.Now().UTC()
	m.configs[docID] = cp
	return nil
}

// PutIfUnchanged CASses on the stored UpdatedAt (zero = no document).
func (m *MemoryConfigStore) PutIfUnchanged(_ context.Context, docID string, rec *ConfigRecord, prevUpdatedAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.configs[docID]
	if (!ok && !prevUpdatedAt.IsZero()) || (ok && !old.UpdatedAt.Equal(prevUpdatedAt)) {
		return false, nil
	}
	cp := cloneConfigRecord(rec)
	cp.UpdatedAt = time.Now().UTC()
	m.configs[docID] = cp
	return true, nil
}

// DeleteIfUnchanged removes the document when the stamp still matches.
func (m *MemoryConfigStore) DeleteIfUnchanged(_ context.Context, docID string, prevUpdatedAt time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.configs[docID]
	if !ok {
		return false, nil
	}
	if !old.UpdatedAt.Equal(prevUpdatedAt) {
		return false, nil
	}
	delete(m.configs, docID)
	return true, nil
}

// List answers every config document by id.
func (m *MemoryConfigStore) List(_ context.Context) (map[string]*ConfigRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*ConfigRecord, len(m.configs))
	for id, rec := range m.configs {
		out[id] = cloneConfigRecord(rec)
	}
	return out, nil
}

// MemoryStateStore is the state surface's in-process twin.
type MemoryStateStore struct {
	mu     sync.Mutex
	states map[string]*StateRecord
}

// NewMemoryStateStore builds an empty in-process state store.
func NewMemoryStateStore() *MemoryStateStore {
	return &MemoryStateStore{states: map[string]*StateRecord{}}
}

// Get answers one state document, nil when never written; under an
// expired or cancelled ctx, the ctx's error (MemoryConfigStore.Get).
func (m *MemoryStateStore) Get(ctx context.Context, docID string) (*StateRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, ok := m.states[docID]; ok {
		return cloneStateRecord(rec), nil
	}
	return nil, nil
}

// Put writes one state document, stamping UpdatedAt.
func (m *MemoryStateStore) Put(_ context.Context, docID string, rec *StateRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := cloneStateRecord(rec)
	cp.UpdatedAt = time.Now().UTC()
	m.states[docID] = cp
	return nil
}

// List answers every state document by id.
func (m *MemoryStateStore) List(_ context.Context) (map[string]*StateRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]*StateRecord, len(m.states))
	for id, rec := range m.states {
		out[id] = cloneStateRecord(rec)
	}
	return out, nil
}
