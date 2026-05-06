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

// LeaseInfo is the JSON payload stored under each run lock key.
// Plan §C.2 calls out runner_id + started_at + run_status.
type LeaseInfo struct {
	RunnerID  string    `json:"runner_id"`
	StartedAt time.Time `json:"started_at"`
	Status    string    `json:"run_status"`
}

// Lock represents an acquired run lease. The runner refreshes it
// periodically (via Refresh) while it owns the run, and releases it
// on completion (via Release). The TTL on the bucket means an
// abruptly-terminated runner's lease evaporates within ~60s without
// any cleanup.
type Lock struct {
	conn  *Conn
	runID string
	rev   uint64 // last observed revision for CAS Update
}

// AcquireLock atomically claims the run lease in the KV bucket. The
// CAS create rejects the call if another runner already wrote a
// lease in the same TTL window — that runner is "the" holder until
// its lease expires or it explicitly releases.
//
// Returns ErrLockHeld when contention is observed; callers Nak the
// JetStream delivery so a sibling pod can pick it up later.
func (c *Conn) AcquireLock(ctx context.Context, runID, runnerID string) (*Lock, error) {
	if c.kv == nil {
		return nil, fmt.Errorf("queue/nats: KV bucket not initialised")
	}
	body, err := json.Marshal(LeaseInfo{
		RunnerID:  runnerID,
		StartedAt: time.Now().UTC(),
		Status:    "running",
	})
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
	return &Lock{conn: c, runID: runID, rev: rev}, nil
}

// Refresh updates the lease (resets the bucket TTL) so a long-running
// run keeps holding the lock past the default 60s TTL. The CAS
// Update against the previous revision detects a hijack — a sibling
// runner that grabbed the lease after a network partition would have
// bumped the revision and our Update would fail, signalling the
// caller to abort the run.
func (l *Lock) Refresh(ctx context.Context) error {
	body, err := json.Marshal(LeaseInfo{
		RunnerID:  "",
		StartedAt: time.Now().UTC(),
		Status:    "running",
	})
	if err != nil {
		return err
	}
	rev, err := l.conn.kv.Update(ctx, l.runID, body, l.rev)
	if err != nil {
		return fmt.Errorf("queue/nats: refresh %s: %w", l.runID, err)
	}
	l.rev = rev
	return nil
}

// Release deletes the lock so a subsequent run can pick up the
// run_id immediately. Non-fatal if the lease has already expired —
// the next Acquire will succeed regardless.
func (l *Lock) Release(ctx context.Context) error {
	if err := l.conn.kv.Delete(ctx, l.runID); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return fmt.Errorf("queue/nats: release %s: %w", l.runID, err)
	}
	return nil
}

// Unlock satisfies store.RunLock so the Mongo store can return *Lock
// directly from LockRun without an adapter shim.
func (l *Lock) Unlock() error {
	return l.Release(context.Background())
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

// AcquireLock satisfies the mongo.LockProvider contract.
func (p *LockProvider) AcquireLock(ctx context.Context, runID, runnerID string) (store.RunLock, error) {
	if runnerID == "" {
		runnerID = p.runnerID
	}
	return p.conn.AcquireLock(ctx, runID, runnerID)
}

// RunnerID returns the identity stamped into each lease.
func (p *LockProvider) RunnerID() string { return p.runnerID }
