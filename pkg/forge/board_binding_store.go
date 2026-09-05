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

// The team ⇄ project-board binding (ADR-097 §4).
//
// One binding per team, because a Projects v2 board spans repositories by
// design: a per-repo binding would either duplicate the same project N times
// or force one repo to be "the" board owner. The team is the tenant every
// other store already keys on.
//
// The discovered ids (project, status field, per-state option) are a CACHE of
// what BindBoard read from the board by NAME — never an authority. Any sync
// may re-run discovery, and a write that fails on a stale id re-discovers.

// ErrBoardBindingNotFound reports a team with no project board bound.
var ErrBoardBindingNotFound = errors.New("forge: board binding not found")

// ErrBoardSyncLeaseLost reports a release by a pass that no longer holds the
// lease — it overran the TTL and another replica has since claimed the board.
//
// It is an ERROR rather than a quiet no-op because it is the only moment the
// overrun is knowable: the release is refused (clearing a successor's lease is
// precisely the damage), and the caller has just learned that its pass may
// have run alongside another one.
var ErrBoardSyncLeaseLost = errors.New("forge: board sync lease no longer held")

// DefaultBoardSyncEvery is the reconciliation interval a binding gets when the
// operator does not choose one. It is the net under the reflect, so it is on
// by default; `0` turns it off explicitly.
//
// Two minutes, not ten: a roadmap that lags a bot by ten minutes reads as
// broken to the human watching it, and the cost is one project read per bound
// team per interval — GitHub prices a Projects v2 page at a handful of points
// against a 5000-point hourly budget, so a board of a few hundred items costs
// well under 1% of it.
const DefaultBoardSyncEvery = 2 * time.Minute

// MinBoardSyncEvery is the floor on a binding's interval. Below it the pass
// stops being a reconciliation net and becomes a poll: the fast path is the
// reflect, and a sub-minute sweep would spend the API budget re-reading a
// board nothing changed on.
const MinBoardSyncEvery = time.Minute

// BoardSyncLeaseTTL bounds how long one claimed pass may hold a board.
//
// The watermark CAS alone elects one replica per TICK, not one pass at a time:
// a pass slower than the binding's interval (floor 1m) makes the binding due
// again while it is still running, and the next tick presents exactly the
// watermark that pass wrote — so it matches, and two replicas reconcile the
// same board at once, issuing duplicate writes on the same cards. The lease is
// the second half of the claim: "and nobody else is inside it".
//
// The TTL is a backstop, not the normal release: a pass hands the board back
// when it ends (ReleaseSync), so the TTL is only ever reached by a replica that
// DIED mid-pass — and it is what bounds the damage of that death to one TTL of
// staleness instead of a board nobody may claim again. Deliberately not
// heartbeated: a reconciliation net is not a run, and a lease it has to refresh
// is machinery a five-minute ceiling buys nothing over.
const BoardSyncLeaseTTL = 5 * time.Minute

// BoundLabelField is one board single-select field imported onto cards as a
// namespaced label.
type BoundLabelField struct {
	FieldID string `bson:"field_id" json:"field_id"`
	Name    string `bson:"name" json:"name"`
	Prefix  string `bson:"prefix" json:"prefix"`
}

// BoardBinding ties one team to one forge project board.
type BoardBinding struct {
	// TenantID is the team id — the primary key: one board per team.
	TenantID string   `bson:"_id" json:"tenant_id"`
	Provider Provider `bson:"provider" json:"provider"`

	// The board's address, as an operator types it.
	Owner     string           `bson:"owner" json:"owner"`
	OwnerKind ProjectOwnerKind `bson:"owner_kind" json:"owner_kind"`
	Number    int              `bson:"number" json:"number"`

	// ConnectionID is the forge.Connection supplying the credential.
	ConnectionID string `bson:"connection_id" json:"connection_id"`

	// ---- discovered at bind time, cached (never an authority) ----

	ProjectID     string `bson:"project_id" json:"project_id"`
	ProjectTitle  string `bson:"project_title,omitempty" json:"project_title,omitempty"`
	ProjectURL    string `bson:"project_url,omitempty" json:"project_url,omitempty"`
	StatusFieldID string `bson:"status_field_id,omitempty" json:"status_field_id,omitempty"`
	// StatusOptions maps a NATIVE STATE to the board option id to write for it
	// — the shape the reflect needs, resolved once instead of per write.
	StatusOptions map[string]string `bson:"status_options,omitempty" json:"status_options,omitempty"`
	// LabelFields are the board's other single-select fields, with the label
	// namespace each lands in.
	LabelFields []BoundLabelField `bson:"label_fields,omitempty" json:"label_fields,omitempty"`

	// ---- the effective policy ----

	// StatusMapping is the effective (column ⇄ native state) map — the default
	// five, or the operator's own. Stored so what a deployment actually runs
	// is readable, not inferred.
	StatusMapping []StatusMapping `bson:"status_mapping,omitempty" json:"status_mapping,omitempty"`
	// MissingStatuses are mapped columns the board does not carry. A binding
	// is not refused over them — it is REPORTED, so a half-covered board is a
	// visible fact rather than a silently inert half of the sync.
	MissingStatuses []string `bson:"missing_statuses,omitempty" json:"missing_statuses,omitempty"`

	// SyncEvery is the reconciliation interval. Zero = OFF (no periodic pass;
	// the reflect's fast path still runs, with no net under it).
	SyncEvery time.Duration `bson:"-" json:"sync_every"`
	// SyncEverySeconds is SyncEvery's wire form — a duration is not a portable
	// BSON/JSON type, and seconds are what an operator reasons in.
	SyncEverySeconds int64 `bson:"sync_every_seconds" json:"sync_every_seconds"`

	// LastSyncedAt is the periodic pass's watermark AND its CAS token: a
	// replica claims the next pass by presenting the value it read.
	LastSyncedAt time.Time `bson:"last_synced_at,omitempty" json:"last_synced_at,omitempty"`
	// SyncLeaseUntil is when the CURRENT pass's hold on this board expires.
	// Stamped by ClaimSync, cleared by ReleaseSync at pass end; a value in the
	// future refuses a second replica's claim. Zero = no pass running.
	SyncLeaseUntil time.Time `bson:"sync_lease_until,omitempty" json:"sync_lease_until,omitempty"`
	// SyncLeaseOwner identifies the pass holding the lease, and is what makes
	// the release CONDITIONAL. Without it a pass that overran the TTL would,
	// on finishing, clear the lease of the successor that legitimately took
	// the board — re-admitting the concurrent pass the lease exists to refuse.
	SyncLeaseOwner string `bson:"sync_lease_owner,omitempty" json:"sync_lease_owner,omitempty"`

	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// Ref renders the binding's board address.
func (b BoardBinding) Ref() ProjectRef {
	return ProjectRef{Owner: b.Owner, OwnerKind: b.OwnerKind.OrDefault(), Number: b.Number}
}

// Mapping returns the effective status map, falling back to the shipped
// vocabulary for a binding written before one was stored.
func (b BoardBinding) Mapping() []StatusMapping {
	if len(b.StatusMapping) > 0 {
		return b.StatusMapping
	}
	return DefaultStatusMapping()
}

// Fields returns the effective label-field list, falling back to the default.
func (b BoardBinding) Fields() []LabelField {
	if len(b.LabelFields) == 0 {
		return DefaultLabelFields()
	}
	out := make([]LabelField, 0, len(b.LabelFields))
	for _, f := range b.LabelFields {
		out = append(out, LabelField{Field: f.Name, Prefix: f.Prefix})
	}
	return out
}

// OptionForState returns the board option id to write for a native state.
func (b BoardBinding) OptionForState(state string) (string, bool) {
	id, ok := b.StatusOptions[state]
	return id, ok && id != ""
}

// DueAt reports when this binding's next periodic pass is due. The zero time
// means "not scheduled" (SyncEvery == 0).
func (b BoardBinding) DueAt() time.Time {
	if b.syncEvery() <= 0 {
		return time.Time{}
	}
	return b.LastSyncedAt.Add(b.syncEvery())
}

func (b BoardBinding) syncEvery() time.Duration {
	if b.SyncEvery > 0 {
		return b.SyncEvery
	}
	return time.Duration(b.SyncEverySeconds) * time.Second
}

// normalize fills the derived/wire fields so both twins persist the same
// document whichever form the caller set.
func (b *BoardBinding) normalize(now time.Time) {
	if b.SyncEvery > 0 {
		b.SyncEverySeconds = int64(b.SyncEvery / time.Second)
	} else if b.SyncEverySeconds > 0 {
		b.SyncEvery = time.Duration(b.SyncEverySeconds) * time.Second
	} else {
		b.SyncEvery, b.SyncEverySeconds = 0, 0
	}
	b.OwnerKind = b.OwnerKind.OrDefault()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.UpdatedAt = now
}

// hydrate is normalize's read-side counterpart: it restores SyncEvery from the
// stored seconds.
func (b *BoardBinding) hydrate() {
	b.SyncEvery = time.Duration(b.SyncEverySeconds) * time.Second
	b.OwnerKind = b.OwnerKind.OrDefault()
}

// Validate reports why a binding cannot be stored, or nil.
func (b BoardBinding) Validate() error {
	if b.TenantID == "" {
		return errors.New("forge: board binding needs a tenant")
	}
	if !b.Provider.Valid() {
		return fmt.Errorf("forge: board binding provider %q is not supported", b.Provider)
	}
	if err := b.Ref().Validate(); err != nil {
		return err
	}
	if b.ConnectionID == "" {
		return errors.New("forge: board binding needs a forge connection")
	}
	if b.ProjectID == "" {
		return errors.New("forge: board binding needs the discovered project id")
	}
	return nil
}

// BoardBindingStore persists the team ⇄ board bindings.
//
// GetByTenant/Delete are keyed on the TEAM, not on an opaque id: one board per
// team is the model, so a caller can never address the wrong tenant's binding
// by mistake. ListAll and DueBindings are the cross-tenant worker queries.
type BoardBindingStore interface {
	// Upsert stores the binding, REPLACING any the team already had.
	Upsert(ctx context.Context, b BoardBinding) error
	GetByTenant(ctx context.Context, tenantID string) (BoardBinding, error)
	Delete(ctx context.Context, tenantID string) error
	ListAll(ctx context.Context) ([]BoardBinding, error)
	// DueBindings returns the bindings whose periodic pass is due at now.
	// SyncEvery == 0 is never due.
	DueBindings(ctx context.Context, now time.Time) ([]BoardBinding, error)
	// ClaimSync atomically advances a binding's watermark from `seen` to `at`
	// AND takes a BoardSyncLeaseTTL lease on the board, returning whether THIS
	// caller won. It is what makes the periodic pass safe on N replicas: a
	// replica presenting a stale watermark loses, and so does one whose tick
	// finds a pass still running (the watermark alone cannot tell them apart —
	// an overrunning pass leaves a watermark that matches).
	// owner identifies the claiming pass, and is what ReleaseSync CASes on.
	ClaimSync(ctx context.Context, tenantID string, seen, at time.Time, owner string) (bool, error)
	// ReleaseSync clears the lease at pass end, whatever the pass's outcome,
	// so the next tick may claim immediately. Without it a board would sit out
	// the whole TTL after every pass; with it the TTL only ever fires for a
	// replica that died mid-pass.
	//
	// It is a CAS on `owner`: a pass that overran the TTL and lost the board
	// to a successor must not clear the successor's lease, and is told so with
	// ErrBoardSyncLeaseLost rather than left believing it released cleanly.
	ReleaseSync(ctx context.Context, tenantID, owner string) error
}

// ---- in-memory store (tests / local) ----

// MemoryBoardBindingStore is the in-process twin. Its ClaimSync is a real CAS
// under one mutex — the suite that proves the Mongo store must prove this one
// too, or the local path would be exempt from the contract it shares.
type MemoryBoardBindingStore struct {
	mu    sync.RWMutex
	items map[string]BoardBinding
}

func NewMemoryBoardBindingStore() *MemoryBoardBindingStore {
	return &MemoryBoardBindingStore{items: make(map[string]BoardBinding)}
}

func (m *MemoryBoardBindingStore) Upsert(_ context.Context, b BoardBinding) error {
	if err := b.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b.normalize(time.Now().UTC())
	if prev, ok := m.items[b.TenantID]; ok {
		b.CreatedAt = prev.CreatedAt
		if b.LastSyncedAt.IsZero() {
			b.LastSyncedAt = prev.LastSyncedAt
		}
		// A re-bind must not release a pass that is running: the Mongo twin
		// never names the field in its $set, so dropping it here would make
		// the two twins disagree on whether the board is held.
		b.SyncLeaseUntil, b.SyncLeaseOwner = prev.SyncLeaseUntil, prev.SyncLeaseOwner
	}
	m.items[b.TenantID] = b
	return nil
}

func (m *MemoryBoardBindingStore) GetByTenant(_ context.Context, tenantID string) (BoardBinding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.items[tenantID]
	if !ok {
		return BoardBinding{}, ErrBoardBindingNotFound
	}
	b.hydrate()
	return b, nil
}

func (m *MemoryBoardBindingStore) Delete(_ context.Context, tenantID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[tenantID]; !ok {
		return ErrBoardBindingNotFound
	}
	delete(m.items, tenantID)
	return nil
}

func (m *MemoryBoardBindingStore) ListAll(_ context.Context) ([]BoardBinding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshot(func(BoardBinding) bool { return true }), nil
}

func (m *MemoryBoardBindingStore) DueBindings(_ context.Context, now time.Time) ([]BoardBinding, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.snapshot(func(b BoardBinding) bool {
		due := b.DueAt()
		return !due.IsZero() && !due.After(now)
	}), nil
}

// snapshot collects matching bindings sorted by CreatedAt, mirroring the Mongo
// twin's sort so the two agree on order as well as content.
func (m *MemoryBoardBindingStore) snapshot(keep func(BoardBinding) bool) []BoardBinding {
	var out []BoardBinding
	for _, b := range m.items {
		b.hydrate()
		if keep(b) {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *MemoryBoardBindingStore) ClaimSync(_ context.Context, tenantID string, seen, at time.Time, owner string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.items[tenantID]
	if !ok {
		return false, ErrBoardBindingNotFound
	}
	if !b.LastSyncedAt.Equal(seen) {
		return false, nil
	}
	// A live lease refuses the claim, and refuses it WITHOUT advancing the
	// watermark: the running pass still owns it, and bumping it here would
	// hide the overrun from the pass that is actually in flight.
	if b.SyncLeaseUntil.After(at) {
		return false, nil
	}
	b.LastSyncedAt = at
	b.SyncLeaseUntil = at.Add(BoardSyncLeaseTTL)
	b.SyncLeaseOwner = owner
	b.UpdatedAt = time.Now().UTC()
	m.items[tenantID] = b
	return true, nil
}

func (m *MemoryBoardBindingStore) ReleaseSync(_ context.Context, tenantID, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.items[tenantID]
	if !ok {
		return ErrBoardBindingNotFound
	}
	if b.SyncLeaseOwner != owner {
		return ErrBoardSyncLeaseLost
	}
	b.SyncLeaseUntil = time.Time{}
	b.SyncLeaseOwner = ""
	b.UpdatedAt = time.Now().UTC()
	m.items[tenantID] = b
	return nil
}

// ---- Mongo store ----

// BoardBindingsCollectionName is the cloud collection holding the bindings.
const BoardBindingsCollectionName = "forge_board_bindings"

type MongoBoardBindingStore struct {
	coll *mongo.Collection
}

func NewMongoBoardBindingStore(db *mongo.Database) *MongoBoardBindingStore {
	return &MongoBoardBindingStore{coll: db.Collection(BoardBindingsCollectionName)}
}

// EnsureSchema creates the indexes. The tenant IS the _id (one board per
// team), so uniqueness needs no index of its own; what does need one is the
// due-scan the sync worker runs on every tick across every tenant.
func (s *MongoBoardBindingStore) EnsureSchema(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "sync_every_seconds", Value: 1}, {Key: "last_synced_at", Value: 1}},
			Options: options.Index().SetName("board_binding_due"),
		},
	})
	if err != nil && !mongoutil.IsIndexConflict(err) {
		return fmt.Errorf("forge: ensure forge_board_bindings indexes: %w", err)
	}
	return nil
}

func (s *MongoBoardBindingStore) Upsert(ctx context.Context, b BoardBinding) error {
	if err := b.Validate(); err != nil {
		return err
	}
	now := time.Now().UTC()
	b.normalize(now)
	// created_at survives a re-bind; last_synced_at is NOT reset by a re-bind
	// unless the caller set one, so a rebind does not trigger an immediate
	// duplicate pass.
	set := bson.M{
		"provider": b.Provider, "owner": b.Owner, "owner_kind": b.OwnerKind,
		"number": b.Number, "connection_id": b.ConnectionID,
		"project_id": b.ProjectID, "project_title": b.ProjectTitle, "project_url": b.ProjectURL,
		"status_field_id": b.StatusFieldID, "status_options": b.StatusOptions,
		"label_fields": b.LabelFields, "status_mapping": b.StatusMapping,
		"missing_statuses":   b.MissingStatuses,
		"sync_every_seconds": b.SyncEverySeconds, "updated_at": b.UpdatedAt,
	}
	if !b.LastSyncedAt.IsZero() {
		set["last_synced_at"] = b.LastSyncedAt
	}
	_, err := s.coll.UpdateOne(ctx,
		bson.M{"_id": b.TenantID},
		bson.M{"$set": set, "$setOnInsert": bson.M{"created_at": b.CreatedAt}},
		options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("forge: upsert board binding: %w", err)
	}
	return nil
}

func (s *MongoBoardBindingStore) GetByTenant(ctx context.Context, tenantID string) (BoardBinding, error) {
	b, err := mongoutil.FindOne[BoardBinding](ctx, s.coll, bson.M{"_id": tenantID},
		ErrBoardBindingNotFound, "forge: get board binding")
	if err != nil {
		return BoardBinding{}, err
	}
	b.hydrate()
	return b, nil
}

func (s *MongoBoardBindingStore) Delete(ctx context.Context, tenantID string) error {
	return mongoutil.DeleteOneChecked(ctx, s.coll, bson.M{"_id": tenantID},
		ErrBoardBindingNotFound, "forge: delete board binding")
}

func (s *MongoBoardBindingStore) ListAll(ctx context.Context) ([]BoardBinding, error) {
	return s.find(ctx, bson.M{})
}

// DueBindings filters on the interval server-side (`sync_every_seconds > 0`)
// and completes the arithmetic in Go: Mongo cannot compare a stored instant
// against `now - <per-document interval>` without an aggregation, and the
// binding set is one document per team.
func (s *MongoBoardBindingStore) DueBindings(ctx context.Context, now time.Time) ([]BoardBinding, error) {
	all, err := s.find(ctx, bson.M{"sync_every_seconds": bson.M{"$gt": 0}})
	if err != nil {
		return nil, err
	}
	out := make([]BoardBinding, 0, len(all))
	for _, b := range all {
		if due := b.DueAt(); !due.IsZero() && !due.After(now) {
			out = append(out, b)
		}
	}
	return out, nil
}

// ClaimSync is the multi-replica election: a conditional update on the
// watermark the caller read.
//
// The watermark is a BSON datetime, i.e. MILLISECOND precision, while the
// value written is a nanosecond time.Now. That is safe because the driver
// applies the same truncation to the FILTER value: a caller presenting either
// the instant it supplied or the one it read back matches the stored one
// either way (pinned by the conformance suite on a real replica set). A CAS
// comparing an un-truncated instant against a truncated stored one would never
// match after the first pass, which would stop the reconciliation dead. ModifiedCount == 1 means this replica owns the
// pass; 0 means another replica already claimed it (or the binding is gone,
// which the follow-up read distinguishes).
func (s *MongoBoardBindingStore) ClaimSync(ctx context.Context, tenantID string, seen, at time.Time, owner string) (bool, error) {
	filter := bson.M{"_id": tenantID}
	// The lease half of the claim: no pass may be running. An absent field is
	// a binding that never ran (or was released), so the predicate has to
	// accept all three shapes the field takes, exactly like the watermark.
	filter["$and"] = []bson.M{{"$or": []bson.M{
		{"sync_lease_until": bson.M{"$exists": false}},
		{"sync_lease_until": nil},
		{"sync_lease_until": bson.M{"$lte": at}},
	}}}
	if seen.IsZero() {
		// "never synced" is stored as an ABSENT field, so the CAS predicate
		// must accept both shapes — a missing field and an explicit zero.
		filter["$or"] = []bson.M{
			{"last_synced_at": bson.M{"$exists": false}},
			{"last_synced_at": time.Time{}},
			{"last_synced_at": nil},
		}
	} else {
		filter["last_synced_at"] = seen
	}
	res, err := s.coll.UpdateOne(ctx, filter,
		bson.M{"$set": bson.M{
			"last_synced_at":   at,
			"sync_lease_until": at.Add(BoardSyncLeaseTTL),
			"sync_lease_owner": owner,
			"updated_at":       time.Now().UTC(),
		}})
	if err != nil {
		return false, fmt.Errorf("forge: claim board sync: %w", err)
	}
	if res.ModifiedCount == 1 {
		return true, nil
	}
	// Nothing matched: a racing replica won, a pass is still running, or there
	// is no binding at all. The last one is a different answer to the caller,
	// so distinguish it rather than reporting "lost" for a missing row.
	if _, err := s.GetByTenant(ctx, tenantID); err != nil {
		return false, err
	}
	return false, nil
}

// ReleaseSync clears the lease, CAS'd on the owner that took it: a pass that
// overran the TTL and lost the board must not clear its SUCCESSOR's lease.
//
// Not matching is not the same as not existing, so the two are distinguished:
// a missing binding is ErrBoardBindingNotFound, a lease that moved on is
// ErrBoardSyncLeaseLost — the only moment an overrun is knowable, and worth a
// line in the log rather than a silent success.
func (s *MongoBoardBindingStore) ReleaseSync(ctx context.Context, tenantID, owner string) error {
	res, err := s.coll.UpdateOne(ctx,
		bson.M{"_id": tenantID, "sync_lease_owner": owner},
		bson.M{"$unset": bson.M{"sync_lease_until": "", "sync_lease_owner": ""},
			"$set": bson.M{"updated_at": time.Now().UTC()}})
	if err != nil {
		return fmt.Errorf("forge: release board sync: %w", err)
	}
	if res.MatchedCount == 1 {
		return nil
	}
	if _, err := s.GetByTenant(ctx, tenantID); err != nil {
		return err
	}
	return ErrBoardSyncLeaseLost
}

func (s *MongoBoardBindingStore) find(ctx context.Context, filter bson.M) ([]BoardBinding, error) {
	out, err := mongoutil.FindAllSorted[BoardBinding](ctx, s.coll, filter, "created_at",
		"forge: list board bindings", "forge: decode board bindings")
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].hydrate()
	}
	return out, nil
}
