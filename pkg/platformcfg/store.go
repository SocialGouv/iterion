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
	FamilyBotVars  = "bot_vars"
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

// NewMongoBotVars binds the bot_vars family to a database.
func NewMongoBotVars(db *mongo.Database) *MongoStore[BotVars] {
	return &MongoStore[BotVars]{col: db.Collection(colPlatformSettings), docID: FamilyBotVars}
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
	stampUpdatedAt(&rec)
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

// updatedAtOf reads the family record's UpdatedAt (the CAS token for
// PutIfUnchanged). Zero for families that would not have the field —
// none today, the switch is the same shape stampUpdatedAt maintains.
func updatedAtOf[T any](rec *T) time.Time {
	switch v := any(rec).(type) {
	case *BotRoles:
		return v.UpdatedAt
	case *Sandbox:
		return v.UpdatedAt
	case *BotVars:
		return v.UpdatedAt
	}
	return time.Time{}
}

// PutIfUnchanged writes rec only if the stored document's updated_at
// still equals prevUpdatedAt (zero = "no document existed"). Returns
// false when another writer got there first — the read-modify-write
// callers (the per-key merge handlers) surface that as a 409 and
// re-read, instead of silently dropping the concurrent writer's keys
// under ReplaceOne semantics.
func (s *MongoStore[T]) PutIfUnchanged(ctx context.Context, rec T, prevUpdatedAt time.Time) (bool, error) {
	stampUpdatedAt(&rec)
	body, err := bson.Marshal(rec)
	if err != nil {
		return false, fmt.Errorf("platformcfg: encode %s: %w", s.docID, err)
	}
	var doc bson.M
	if err := bson.Unmarshal(body, &doc); err != nil {
		return false, fmt.Errorf("platformcfg: encode %s: %w", s.docID, err)
	}
	doc["_id"] = s.docID
	filter := bson.M{"_id": s.docID, "updated_at": prevUpdatedAt}
	upsert := false
	if prevUpdatedAt.IsZero() {
		// First write: match only a missing document, and upsert it.
		filter = bson.M{"_id": s.docID, "updated_at": bson.M{"$exists": false}}
		upsert = true
	}
	res, err := s.col.ReplaceOne(ctx, filter, doc, options.Replace().SetUpsert(upsert))
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			// The upsert raced another first write.
			return false, nil
		}
		return false, fmt.Errorf("platformcfg: put %s: %w", s.docID, err)
	}
	return res.MatchedCount > 0 || res.UpsertedCount > 0, nil
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
	stampUpdatedAt(&rec)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rec = &rec
	return nil
}

// PutIfUnchanged is the MemoryStore mirror of the Mongo CAS write.
func (m *MemoryStore[T]) PutIfUnchanged(_ context.Context, rec T, prevUpdatedAt time.Time) (bool, error) {
	stampUpdatedAt(&rec)
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case m.rec == nil:
		if !prevUpdatedAt.IsZero() {
			return false, nil
		}
	case !updatedAtOf(m.rec).Equal(prevUpdatedAt):
		return false, nil
	}
	m.rec = &rec
	return true, nil
}

// CASStore is the optional conditional-write surface a Store may offer;
// the merge handlers use it when present and fall back to Put otherwise.
type CASStore[T any] interface {
	PutIfUnchanged(ctx context.Context, rec T, prevUpdatedAt time.Time) (bool, error)
}

// stampUpdatedAt sets the family record's UpdatedAt on write — in the
// stores (covering every writer, incl. the CLI) rather than per handler.
func stampUpdatedAt[T any](rec *T) {
	switch v := any(rec).(type) {
	case *BotRoles:
		v.UpdatedAt = time.Now().UTC()
	case *Sandbox:
		v.UpdatedAt = time.Now().UTC()
	case *BotVars:
		v.UpdatedAt = time.Now().UTC()
	}
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
// values are consumed.
//
// Concurrency contract, each clause paid for by a live failure mode:
//   - One refresh at a time, run OUTSIDE the mutex; once a value has EVER
//     been resolved, concurrent callers are served the stale value instead
//     of queueing behind the store read.
//   - Before the FIRST successful read (process cold start), concurrent
//     callers WAIT for the in-flight refresh (bounded by fetchTimeout)
//     instead of silently resolving nil → "no overrides": a rolling deploy
//     must not serve hardcoded defaults for its first request burst.
//   - The refresh context is detached from the caller's cancellation
//     (context.WithoutCancel): a shared cache fill must not fail — and
//     re-arm the TTL — because ONE caller aborted.
//   - Invalidate bumps a generation; a refresh that started before the
//     bump discards its (pre-write) result instead of re-pinning it for a
//     full TTL over the mutation.
type Resolver[T any] struct {
	store Store[T]
	ttl   time.Duration
	warn  func(format string, args ...any)
	now   func() time.Time // injectable for deterministic TTL tests

	mu         sync.Mutex
	cached     *T
	fetchedAt  time.Time
	refreshing bool
	ready      bool          // a value has been successfully read at least once
	gen        uint64        // bumped by Invalidate; stale refreshes discard
	waiters    chan struct{} // closed when the in-flight refresh completes
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
// defaults). Never errors: a store failure serves the last-known value.
func (r *Resolver[T]) Get(ctx context.Context) *T {
	if r == nil || r.store == nil {
		return nil
	}
	for {
		r.mu.Lock()
		if !r.fetchedAt.IsZero() && r.now().Sub(r.fetchedAt) < r.ttl {
			v := r.cached
			r.mu.Unlock()
			return v
		}
		if r.refreshing {
			if r.ready {
				// Serve stale rather than queue behind the refresh.
				v := r.cached
				r.mu.Unlock()
				return v
			}
			// Cold start: no value has ever resolved — wait for the
			// in-flight first read (it is bounded by fetchTimeout) rather
			// than silently answering "no overrides".
			ch := r.waiters
			r.mu.Unlock()
			select {
			case <-ch:
				continue // re-evaluate: the refresh committed (or failed)
			case <-ctx.Done():
				return nil
			}
		}
		r.refreshing = true
		r.waiters = make(chan struct{})
		gen := r.gen
		r.mu.Unlock()

		// Detached from the caller: one aborted request must not poison the
		// shared cache for everyone behind it. The fetch runs inside a
		// closure whose defer converts a panic into an error: leaving
		// refreshing=true with waiters unclosed would wedge every later Get
		// forever (production callers pass context.Background()), and the
		// platform-bots fetch parses operator-pushed bundle content.
		var rec *T
		var err error
		func() {
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("platformcfg: store read panicked: %v", p)
				}
			}()
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fetchTimeout)
			defer cancel()
			rec, err = r.store.Get(fctx)
		}()

		r.mu.Lock()
		r.refreshing = false
		close(r.waiters)
		switch {
		case err != nil:
			if r.warn != nil {
				r.warn("platformcfg: settings read failed — serving last-known values: %v", err)
			}
			// Re-arm the TTL so an outage doesn't hammer the store per
			// request. A cold-start failure stays not-ready, so the next
			// window retries the first read.
			r.fetchedAt = r.now()
		case gen != r.gen:
			// Invalidated mid-flight: this result predates the write —
			// discard it and leave the TTL expired so the next Get refetches.
		default:
			r.cached = rec
			r.fetchedAt = r.now()
			r.ready = true
		}
		v := r.cached
		r.mu.Unlock()
		return v
	}
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
	// A refresh already in flight read the PRE-write state; the generation
	// bump makes it discard its result instead of re-pinning it for a TTL.
	r.gen++
	r.mu.Unlock()
}
