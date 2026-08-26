package platformcfg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// colPlatformSettings is the shared one-doc-per-family collection —
// deliberately the SAME collection usagecap's settings store writes its
// "usage_caps" document to, so every platform runtime-settings family
// lives in one place.
const colPlatformSettings = "platform_settings"

// Family doc ids.
const (
	FamilyBotRoles = "bot_roles"
	FamilySandbox  = "sandbox"
)

// MongoStore is the cloud Store for one family: the single document every
// replica reads.
type MongoStore[T any] struct {
	col   *mongo.Collection
	docID string
}

// NewMongoBotRoles binds the bot_roles family to a database.
func NewMongoBotRoles(db *mongo.Database) *MongoStore[BotRoles] {
	return &MongoStore[BotRoles]{col: db.Collection(colPlatformSettings), docID: FamilyBotRoles}
}

// NewMongoSandbox binds the sandbox family to a database.
func NewMongoSandbox(db *mongo.Database) *MongoStore[Sandbox] {
	return &MongoStore[Sandbox]{col: db.Collection(colPlatformSettings), docID: FamilySandbox}
}

func (s *MongoStore[T]) Get(ctx context.Context) (*T, error) {
	var out T
	if err := s.col.FindOne(ctx, bson.M{"_id": s.docID}).Decode(&out); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, nil
		}
		return nil, fmt.Errorf("platformcfg: get %s: %w", s.docID, err)
	}
	return &out, nil
}

func (s *MongoStore[T]) Put(ctx context.Context, rec T) error {
	body, err := bson.Marshal(rec)
	if err != nil {
		return fmt.Errorf("platformcfg: encode %s: %w", s.docID, err)
	}
	var doc bson.M
	if err := bson.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("platformcfg: encode %s: %w", s.docID, err)
	}
	doc["_id"] = s.docID
	// ReplaceOne (not $set) so a cleared override really disappears from the
	// document instead of lingering.
	if _, err := s.col.ReplaceOne(ctx, bson.M{"_id": s.docID}, doc, options.Replace().SetUpsert(true)); err != nil {
		return fmt.Errorf("platformcfg: put %s: %w", s.docID, err)
	}
	return nil
}

// MemoryStore is the in-process Store for tests and single-process wiring.
type MemoryStore[T any] struct {
	mu  sync.Mutex
	rec *T
}

func NewMemoryStore[T any]() *MemoryStore[T] { return &MemoryStore[T]{} }

func (m *MemoryStore[T]) Get(context.Context) (*T, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rec == nil {
		return nil, nil
	}
	cp := *m.rec
	return &cp, nil
}

func (m *MemoryStore[T]) Put(_ context.Context, rec T) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rec = &rec
	return nil
}

// fetchTimeout bounds one refresh read so a wedged store can never hang a
// request-path Get longer than this (the usagecap resolver's discipline).
const fetchTimeout = 3 * time.Second

// funcStore adapts a fetch function to the Store interface (read side
// only) so a Resolver can cache ANY derivable value, not just a settings
// document — e.g. the server's materialized platform-bot entry set.
type funcStore[T any] struct {
	fetch func(ctx context.Context) (*T, error)
}

func (f funcStore[T]) Get(ctx context.Context) (*T, error) { return f.fetch(ctx) }
func (f funcStore[T]) Put(context.Context, T) error {
	return fmt.Errorf("platformcfg: func-backed resolver is read-only")
}

// Resolver is the TTL-bounded read cache over one family's store: the
// per-replica availability layer of the ADR-090 posture. On a store
// failure it serves the LAST successfully read value (logged by the
// caller-supplied warn func) — freshness is best-effort, correctness never
// depends on it, and the enforcement backstop stays wherever the family's
// values are consumed. A refresh runs OUTSIDE the mutex and only one runs
// at a time: concurrent callers during a refresh (or an outage) are served
// the stale value immediately instead of queueing behind the store read.
type Resolver[T any] struct {
	store Store[T]
	ttl   time.Duration
	warn  func(format string, args ...any)
	now   func() time.Time // injectable for deterministic TTL tests

	mu         sync.Mutex
	cached     *T
	fetchedAt  time.Time
	refreshing bool
}

// NewResolver builds a resolver with DefaultTTL. warn may be nil.
func NewResolver[T any](store Store[T], warn func(string, ...any)) *Resolver[T] {
	return &Resolver[T]{store: store, ttl: DefaultTTL, warn: warn, now: time.Now}
}

// NewResolverFunc is NewResolver over a plain fetch function.
func NewResolverFunc[T any](fetch func(ctx context.Context) (*T, error), warn func(string, ...any)) *Resolver[T] {
	return NewResolver[T](funcStore[T]{fetch: fetch}, warn)
}

// Get returns the family record, nil when none is stored (inherit
// defaults). Never errors and never blocks behind another caller's
// refresh: a store failure — or a refresh in flight — serves the
// last-known value.
func (r *Resolver[T]) Get(ctx context.Context) *T {
	if r == nil || r.store == nil {
		return nil
	}
	r.mu.Lock()
	if (!r.fetchedAt.IsZero() && r.now().Sub(r.fetchedAt) < r.ttl) || r.refreshing {
		v := r.cached
		r.mu.Unlock()
		return v
	}
	r.refreshing = true
	r.mu.Unlock()

	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	rec, err := r.store.Get(fctx)
	cancel()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshing = false
	if err != nil {
		if r.warn != nil {
			r.warn("platformcfg: settings read failed — serving last-known values: %v", err)
		}
		// Re-arm the TTL so an outage doesn't hammer the store per request.
		r.fetchedAt = r.now()
		return r.cached
	}
	r.cached = rec
	r.fetchedAt = r.now()
	return r.cached
}

// Invalidate forces the next Get to re-read the store, so the replica that
// served a mutation reads its own write immediately. It deliberately KEEPS
// the cached value: it doubles as the last-known fallback an outage is
// served from — dropping it here would turn "invalidate then Mongo blip"
// into a silent reset to defaults. Safe on nil.
func (r *Resolver[T]) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.fetchedAt = time.Time{}
	r.mu.Unlock()
}
