// Package lease elects one holder per named lease across the replicas of a
// deployment, so a periodic net runs on ONE replica instead of every one.
//
// It is a load-reduction election, not a mutual-exclusion guarantee. A lease
// is only as exclusive as the clocks and the store round-trips around it: a
// holder that stalls past its TTL can overlap its successor for the length of
// the stall. So it is for work that is already idempotent — a reconciliation
// net that re-reads before it writes — and whose cost, not whose correctness,
// scales with the number of replicas running it. Work that must never run
// twice needs a claim on the item itself (a CAS, a fenced lease), which this
// package does not replace.
//
// The lease is sticky: its holder renews it for as long as it runs (Run), so
// the elected replica keeps its in-memory state — a cursor, a pass counter —
// across passes, and a term ends only when the holder stops, loses the store,
// or is outlived by its TTL.
package lease

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrLost reports that the caller does not hold the lease — another owner
// does: a holder that overran its TTL, or a release sent after the lease moved
// on. Callers log it rather than treat it as a success.
var ErrLost = errors.New("lease: held by another owner")

// Store persists named leases. Both twins (MemoryStore, MongoStore) honour the
// same contract, pinned by the shared conformance suite. Instants are
// millisecond-precise in both, as BSON datetimes are.
type Store interface {
	// Acquire takes the named lease for owner until now+ttl when it is free,
	// expired at now, or already held by owner. It reports false with a nil
	// error when another owner holds it unexpired. An error means the store's
	// answer did not arrive, NOT that nothing happened: a write held up in
	// the store can still land after its caller gave up.
	Acquire(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error)
	// Renew extends a lease owner holds until now+ttl, and never creates or
	// takes one: false when the lease is gone or belongs to another owner.
	// A lease past its expiry that nobody took is still its owner's, and
	// renewable. A renewal that lands after its holder released the lease
	// therefore changes nothing, where an Acquire would re-create it. Neither
	// call ever moves an expiry backward: an older stamp arriving late keeps
	// the later one.
	Renew(ctx context.Context, name, owner string, now time.Time, ttl time.Duration) (bool, error)
	// Release gives the lease up when owner holds it, so a successor need not
	// wait out the TTL. Releasing a lease nobody holds is a no-op; releasing
	// one another owner holds returns ErrLost and changes nothing.
	Release(ctx context.Context, name, owner string) error
}

// minTTL is the shortest lease the stores keep: they measure time to the
// millisecond, so a shorter one would expire as it is written.
const minTTL = time.Millisecond

// validate is the argument check both twins share: an empty name or owner
// would elect everybody under one anonymous identity, and a TTL under the
// stores' precision would hand out leases that are expired on arrival.
func validate(name, owner string, ttl time.Duration) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("lease: empty lease name")
	}
	if strings.TrimSpace(owner) == "" {
		return fmt.Errorf("lease %q: empty owner", name)
	}
	if ttl < minTTL {
		return fmt.Errorf("lease %q: TTL %s is under the %s the stores measure", name, ttl, minTTL)
	}
	return nil
}

// stamp is the instant both twins record and compare: UTC, truncated to the
// millisecond, so the memory twin answers exactly as Mongo does.
func stamp(t time.Time) time.Time {
	return t.UTC().Truncate(time.Millisecond)
}
