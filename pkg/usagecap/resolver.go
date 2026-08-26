package usagecap

import (
	"context"
	"sync"
	"time"
)

// PolicySource answers "what is the usage-cap policy RIGHT NOW" — the seam
// that lets an enforcement point read a value that can change under it. A
// static env-resolved Policy and the DB-backed Resolver both implement it,
// so an enforcement point never knows which deployment shape it runs in.
type PolicySource interface {
	Effective(ctx context.Context) Policy
}

// StaticPolicy is a PolicySource that always answers the same policy — the
// env-only deployments, and every test that wants a fixed cap.
type StaticPolicy Policy

// Effective returns the wrapped policy.
func (p StaticPolicy) Effective(context.Context) Policy { return Policy(p) }

// Origin says, per window, whether the effective percentage came from the
// DB record or the env default — the read surface operators use to verify
// a runtime change actually landed.
type Origin struct {
	FiveHourDB bool
	WeekDB     bool
}

// String renders the origin for the health envelope.
func (o Origin) String() string {
	switch {
	case o.FiveHourDB && o.WeekDB:
		return "db"
	case o.FiveHourDB || o.WeekDB:
		return "db+env"
	default:
		return "env"
	}
}

// DefaultSettingsTTL bounds how stale an enforcement point's view of the
// runtime settings may be. A change made through the admin API is
// therefore effective everywhere within this bound — no restart, no
// coordination. Short enough that "I changed the cap" and "the fleet
// honours it" are the same minute; long enough that a busy runner does
// not turn every stream event into a DB read.
const DefaultSettingsTTL = 30 * time.Second

// settingsFetchTimeout bounds one settings read. The resolver is consulted
// from enforcement hot paths; a wedged store must cost one bounded fetch
// per TTL window, never a hang.
const settingsFetchTimeout = 3 * time.Second

// Resolver is the DB-backed PolicySource: env defaults + the runtime
// settings record, cached for TTL. Every read failure fails toward the
// last value it successfully resolved (or the env defaults before the
// first success) — the cap machinery's standing rule that bookkeeping
// unavailability must never change enforcement abruptly in either
// direction.
type Resolver struct {
	store SettingsStore
	def   Policy
	ttl   time.Duration
	now   func() time.Time
	warn  func(format string, args ...any)

	mu        sync.Mutex
	pol       Policy
	origin    Origin
	fetchedAt time.Time
	has       bool
}

// ResolverOption tunes a Resolver.
type ResolverOption func(*Resolver)

// WithSettingsTTL overrides the cache TTL (tests use a tiny one; zero or
// negative keeps the default).
func WithSettingsTTL(ttl time.Duration) ResolverOption {
	return func(r *Resolver) {
		if ttl > 0 {
			r.ttl = ttl
		}
	}
}

// WithClock injects a clock for deterministic TTL tests.
func WithClock(now func() time.Time) ResolverOption {
	return func(r *Resolver) {
		if now != nil {
			r.now = now
		}
	}
}

// WithWarnLogger receives one line per failed settings read. Optional —
// the resolver stays dependency-free.
func WithWarnLogger(warn func(format string, args ...any)) ResolverOption {
	return func(r *Resolver) { r.warn = warn }
}

// NewResolver builds a resolver over a settings store with the
// env-resolved policy as the default every unset field inherits.
func NewResolver(store SettingsStore, def Policy, opts ...ResolverOption) *Resolver {
	r := &Resolver{store: store, def: def, ttl: DefaultSettingsTTL, now: time.Now}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Effective returns the current effective policy (db-or-env).
func (r *Resolver) Effective(ctx context.Context) Policy {
	pol, _ := r.EffectiveOrigin(ctx)
	return pol
}

// EffectiveOrigin returns the effective policy plus where each window's
// percentage came from.
func (r *Resolver) EffectiveOrigin(ctx context.Context) (Policy, Origin) {
	r.mu.Lock()
	if r.has && r.now().Sub(r.fetchedAt) < r.ttl {
		pol, origin := r.pol, r.origin
		r.mu.Unlock()
		return pol, origin
	}
	r.mu.Unlock()

	// Fetch outside the lock: a slow store may cost two concurrent
	// callers one fetch each (last write wins — they resolve the same
	// record), but must never serialize enforcement behind one read.
	fctx, cancel := context.WithTimeout(ctx, settingsFetchTimeout)
	rec, err := r.store.GetSettings(fctx)
	cancel()

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if r.warn != nil {
			r.warn("usagecap: settings read failed — serving %s values: %v",
				map[bool]string{true: "last-known", false: "env"}[r.has], err)
		}
		if !r.has {
			r.pol, r.origin, r.has = r.def, Origin{}, true
		}
		// Re-arm the TTL on failure too: a wedged store is retried once
		// per window, not hammered on every evaluation.
		r.fetchedAt = r.now()
		return r.pol, r.origin
	}
	r.pol = rec.Apply(r.def)
	r.origin = Origin{
		FiveHourDB: rec != nil && rec.FiveHourPct != nil,
		WeekDB:     rec != nil && rec.WeekPct != nil,
	}
	r.has = true
	r.fetchedAt = r.now()
	return r.pol, r.origin
}

// Invalidate drops the cache so the next read hits the store — called by
// the mutating API handler so the pod that served the update is coherent
// immediately; every other replica converges within the TTL.
func (r *Resolver) Invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.has = false
}
