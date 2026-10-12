package nats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/SocialGouv/iterion/pkg/store"
)

// ErrLockHeld is returned by AcquireLock when another runner currently
// holds the run lease. Callers (the consumer loop) treat it as
// "skip this delivery" — JetStream will redeliver to a different pod.
var ErrLockHeld = errors.New("queue/nats: run lock held by another runner")

// ErrLeaseUnattributed is returned by AcquireLock when the context
// carries no admitted identity. A distributed lease must carry the
// tenant of the message the holder admitted, so taking one without is
// a bug — the lease would be unattributable and unverifiable.
var ErrLeaseUnattributed = errors.New("queue/nats: run lease requires the run's admitted identity in context")

// ErrLeaseIdentityMismatch is returned when the lease stored in the KV
// bucket no longer carries the identity this Lock was acquired under.
// The holder refuses to act — no refresh, no release — on a lease that
// is not its run's.
var ErrLeaseIdentityMismatch = errors.New("queue/nats: run lease belongs to a different admitted identity")

// LeaseInfo is the JSON payload stored under each run lock key.
// Plan §C.2 calls out runner_id + started_at + run_status; plan v2
// §P5-c adds the run's admitted identity — the tenant (and runner
// pool, when stamped) of the message the holder actually admitted —
// so every later operation on the lease is checked against it before
// acting.
type LeaseInfo struct {
	RunnerID  string    `json:"runner_id"`
	StartedAt time.Time `json:"started_at"`
	Status    string    `json:"run_status"`
	TenantID  string    `json:"tenant_id,omitempty"`
	Pool      string    `json:"pool,omitempty"`
}

// newLeaseBody marshals a fresh "running" lease for runnerID under the
// admitted identity, stamped with the current time. Shared by
// AcquireLock and Refresh so both write the same lease shape.
func newLeaseBody(runnerID string, ident store.LeaseIdentity) ([]byte, error) {
	return json.Marshal(LeaseInfo{
		RunnerID:  runnerID,
		StartedAt: time.Now().UTC(),
		Status:    "running",
		TenantID:  ident.TenantID,
		Pool:      ident.Pool,
	})
}

// Lock represents an acquired run lease. The runner refreshes it
// periodically (via Refresh) while it owns the run, and releases it
// on completion (via Release). The TTL on the bucket means an
// abruptly-terminated runner's lease evaporates within ~60s without
// any cleanup.
type Lock struct {
	conn     *Conn
	runID    string
	runnerID string              // pod identity stamped at acquire time, re-used on every refresh
	ident    store.LeaseIdentity // admitted identity stamped at acquire time, re-verified before every action
	rev      uint64              // last observed revision for CAS Update
}

// AcquireLock atomically claims the run lease in the KV bucket. The
// CAS create rejects the call if another runner already wrote a
// lease in the same TTL window — that runner is "the" holder until
// its lease expires or it explicitly releases.
//
// The context must carry the run's admitted identity
// (store.WithLeaseIdentity, stamped by the runner from the message it
// admitted): the lease is written with it, and it fails closed with
// ErrLeaseUnattributed otherwise.
//
// Returns ErrLockHeld when contention is observed; callers Nak the
// JetStream delivery so a sibling pod can pick it up later.
func (c *Conn) AcquireLock(ctx context.Context, runID, runnerID string) (*Lock, error) {
	if c.kv == nil {
		return nil, fmt.Errorf("queue/nats: KV bucket not initialised")
	}
	ident, ok := store.LeaseIdentityFromContext(ctx)
	if !ok || ident.TenantID == "" {
		return nil, ErrLeaseUnattributed
	}
	body, err := newLeaseBody(runnerID, ident)
	if err != nil {
		return nil, fmt.Errorf("queue/nats: marshal lease: %w", err)
	}

	rev, err := c.kv.Create(ctx, runID, body)
	if err != nil {
		// jetstream.ErrKeyExists is the contention signal. Anything
		// else (network blip, malformed key) propagates as-is.
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, ErrLockHeld
		}
		return nil, fmt.Errorf("queue/nats: KV create %s: %w", runID, err)
	}
	return &Lock{conn: c, runID: runID, runnerID: runnerID, ident: ident, rev: rev}, nil
}

// Refresh updates the lease (resets the bucket TTL) so a long-running
// run keeps holding the lock past the default 60s TTL. The CAS Update
// against the previous revision detects a hijack — a sibling runner
// that grabbed the lease after a network partition would have bumped
// the revision and our Update would fail, signalling the caller to
// abort the run. On that failure one read classifies it: the one case
// that means something different from contention is a stored body now
// carrying another admission's identity (ErrLeaseIdentityMismatch).
func (l *Lock) Refresh(ctx context.Context) error {
	body, err := newLeaseBody(l.runnerID, l.ident)
	if err != nil {
		return err
	}
	rev, err := l.conn.kv.Update(ctx, l.runID, body, l.rev)
	if err != nil {
		if _, foreign := l.classify(ctx); foreign != nil {
			return foreign
		}
		return fmt.Errorf("queue/nats: refresh %s: %w", l.runID, err)
	}
	l.rev = rev
	return nil
}

// leaseState is what the stored lease says after a CAS write failed.
type leaseState int

const (
	// leaseContention: a plain CAS conflict — a sibling took the lease,
	// or the read could not tell. The CAS error the caller holds is the
	// honest answer.
	leaseContention leaseState = iota
	// leaseGone: the key no longer exists — the lease's TTL won.
	leaseGone
	// leaseForeign: the stored body belongs to another admitted
	// identity; the paired error names it.
	leaseForeign
)

// classify reads the stored lease — ON THE FAILURE PATH of a CAS write,
// never in the nominal path: the revision CAS already refuses every
// foreign write, so the extra round-trip is paid only when a write
// failed, to tell apart plain contention from the stored body now
// belonging to another admission.
func (l *Lock) classify(ctx context.Context) (leaseState, error) {
	entry, err := l.conn.kv.Get(ctx, l.runID)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return leaseGone, nil
		}
		return leaseContention, nil
	}
	var info LeaseInfo
	if err := json.Unmarshal(entry.Value(), &info); err != nil {
		return leaseContention, nil
	}
	if info.TenantID != l.ident.TenantID || info.Pool != l.ident.Pool {
		return leaseForeign, fmt.Errorf("queue/nats: lease %s stored under tenant %q pool %q, this holder claims tenant %q pool %q: %w",
			l.runID, info.TenantID, info.Pool, l.ident.TenantID, l.ident.Pool, ErrLeaseIdentityMismatch)
	}
	return leaseContention, nil
}

// Release deletes the lock so a subsequent run can pick up the
// run_id immediately. Non-fatal if the lease has already expired —
// the next Acquire will succeed regardless. A delete refused by the
// revision guard is classified once: a lease already gone is a no-op,
// a stored body of another admitted identity is reported as
// ErrLeaseIdentityMismatch (this holder never deletes a lease it does
// not own the identity of), anything else stays the CAS error.
func (l *Lock) Release(ctx context.Context) error {
	// Revision-guarded delete: only remove the lease if its latest
	// revision still matches the one we last wrote (at Acquire or the
	// most recent Refresh). If our lease expired (TTL) and a sibling
	// re-acquired it after a network partition or a long GC pause, the
	// key now carries the sibling's revision — an unconditional Delete
	// would silently evict *their* lock and let a third runner claim the
	// same run, i.e. split-brain. LastRevision turns that case into a
	// surfaced error instead of a stolen lock; a clean release (we still
	// own the revision) still succeeds.
	err := l.conn.kv.Delete(ctx, l.runID, jetstream.LastRevision(l.rev))
	if err == nil {
		return nil
	}
	switch state, foreign := l.classify(ctx); {
	case state == leaseGone:
		return nil
	case foreign != nil:
		return foreign
	default:
		return fmt.Errorf("queue/nats: release %s: %w", l.runID, err)
	}
}

// Unlock satisfies store.RunLock so the Mongo store can return *Lock
// directly from LockRun without an adapter shim.
//
// Bounded by a 5s timeout so a NATS partition during shutdown can't
// stall `defer lock.Unlock()` in the engine forever. NATS reconnect
// logic caps the wait in practice, but the explicit deadline keeps
// the shutdown path predictable.
func (l *Lock) Unlock() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return l.Release(ctx)
}

// LockProvider adapts a Conn for consumption by mongo.Config.
// LockProvider — the store package can't import pkg/queue/nats
// without creating a dependency cycle, so the type lives here and
// the runner injects it explicitly at boot.
//
// Plan §F T-26.
type LockProvider struct {
	conn     *Conn
	runnerID string
}

// NewLockProvider returns a LockProvider that mints leases keyed on
// the supplied runnerID. The runner picks runnerID = pod name (or
// hostname when running outside Kubernetes) so the observability
// dashboards can correlate runs to pods.
func NewLockProvider(conn *Conn, runnerID string) *LockProvider {
	return &LockProvider{conn: conn, runnerID: runnerID}
}

// AcquireLock satisfies the mongo.LockProvider contract. The admitted
// identity is read from ctx (store.WithLeaseIdentity): the runner
// stamps it from the message it admitted before taking the lease.
func (p *LockProvider) AcquireLock(ctx context.Context, runID, runnerID string) (store.RunLock, error) {
	if runnerID == "" {
		runnerID = p.runnerID
	}
	return p.conn.AcquireLock(ctx, runID, runnerID)
}

// RunnerID returns the identity stamped into each lease.
func (p *LockProvider) RunnerID() string { return p.runnerID }

// LockTTL is the configured ownership lease interval. The runner also uses
// it to space out a delivery it could not take the lock for: one interval
// is how long a lease that is not being refreshed takes to evaporate, so a
// retry after it either finds the run free or meets a live owner.
func (c *Conn) LockTTL() time.Duration { return c.cfg.LockTTL }
