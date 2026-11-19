// Package poller holds the durable state of a connector source that iterion
// reads instead of being called (ADR-168 §7): one record per poller — a
// query against one connection's trigger — and the set of items it has
// already accepted.
//
// A poller is elected to one replica by pkg/lease, which elects but does not
// fence: a holder that stalls past its lease can overlap its successor. The
// record carries the fence lease does not. Every tick takes the record's
// next GENERATION by compare-and-swap (Acquire), and every write a tick makes
// afterwards — the sequence numbers it reserves, the checkpoint it moves —
// names that generation. A write under a generation the record has moved
// past changes nothing and fails with ErrFenced, so a late holder can
// neither move the checkpoint back nor hand out sequence numbers its
// successor also hands out. What two overlapping holders can still both do
// is accept the same item, which the intake's identity absorbs.
//
// Three twins implement Store with one contract (conformance_test.go): a
// memory store for tests, a file store under the run store (.iterion/) for a
// local server, and Mongo for the cloud.
package poller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Store errors.
var (
	// ErrNotFound: no poller with that id in that tenant.
	ErrNotFound = errors.New("poller: not found")
	// ErrFenced: a write named a generation the poller has moved past —
	// another holder took it since.
	ErrFenced = errors.New("poller: fenced by a newer generation")
	// ErrCheckpointBehind: a checkpoint write would move a timestamp
	// checkpoint back.
	ErrCheckpointBehind = errors.New("poller: checkpoint behind the stored one")
	// ErrInvalid: a record or a write the store refuses.
	ErrInvalid = errors.New("poller: invalid")
)

// The checkpoint kinds, spec.CursorTimestamp and spec.CursorOpaque.
const (
	KindTimestamp = "timestamp"
	KindOpaque    = "opaque"
)

// Poller is one query a connection's polled trigger is read with. Its id is
// derived from (tenant, connection, trigger, query values), so every
// subscription asking the same query of the same connection listens to one
// poller, and two different queries never share one.
type Poller struct {
	ID           string `json:"id" bson:"_id"`
	TenantID     string `json:"tenant_id" bson:"tenant_id"`
	ConnectionID string `json:"connection_id" bson:"connection_id"`
	TriggerID    string `json:"trigger_id" bson:"trigger_id"`
	// Query is what the operator typed, per declared query field id.
	Query map[string]string `json:"query,omitempty" bson:"query,omitempty"`

	// Generation is the fence: the count of ticks that took the poller.
	Generation int64 `json:"generation" bson:"generation"`
	// Holder is the owner of the last tick that took the poller; read by
	// operators, never by the fence.
	Holder string `json:"holder,omitempty" bson:"holder,omitempty"`
	// Checkpoint is what the last complete scan read up to; nil before one.
	Checkpoint *Checkpoint `json:"checkpoint,omitempty" bson:"checkpoint,omitempty"`
	// Primed: a complete scan recorded what was already there. Until then
	// every item the poller accepts is primed — matched to nothing. A poller
	// nobody listens to is unprimed again (Tick.Idle), so what changed while
	// no subscription listened is never launched when one comes back.
	Primed bool `json:"primed" bson:"primed"`
	// PrimedThrough is the last sequence a priming scan reserved: every
	// intake at or below it was primed or preceded its subscriptions, which
	// is what a backfill may pair. It never moves back.
	PrimedThrough int64 `json:"primed_through,omitempty" bson:"primed_through,omitempty"`
	// Sequence is the last sequence number reserved for an intake.
	Sequence int64 `json:"sequence" bson:"sequence"`

	// NextTickAt is when the poller is due.
	NextTickAt time.Time  `json:"next_tick_at" bson:"next_tick_at"`
	LastTickAt *time.Time `json:"last_tick_at,omitempty" bson:"last_tick_at,omitempty"`
	// LastError says why the last tick did not complete its scan, or which
	// items it could not read; empty when it did.
	LastError string    `json:"last_error,omitempty" bson:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt time.Time `json:"updated_at" bson:"updated_at"`
}

// Checkpoint is a poller's position: an instant for a timestamp cursor,
// the vendor's token for an opaque one.
type Checkpoint struct {
	Kind  string     `json:"kind" bson:"kind"`
	At    *time.Time `json:"at,omitempty" bson:"at,omitempty"`
	Token string     `json:"token,omitempty" bson:"token,omitempty"`
}

// Tick is what a tick writes back when it is done (Store.Complete).
type Tick struct {
	// At is when the tick ran.
	At time.Time
	// NextTickAt is when the poller is due again.
	NextTickAt time.Time
	// Checkpoint, when set, is the position a complete scan reached. A
	// timestamp checkpoint never moves back: a lower one is refused
	// (ErrCheckpointBehind). nil leaves the stored one.
	Checkpoint *Checkpoint
	// Primed marks the poller primed through PrimedThrough, which never
	// moves back.
	Primed        bool
	PrimedThrough int64
	// Idle marks a tick that found no subscription: the poller is unprimed,
	// and its next listened scan primes again. Exclusive with Primed; its
	// Checkpoint, a timestamp, is where nobody listened up to.
	Idle bool
	// Error is the tick's LastError; empty clears it.
	Error string
}

// ItemKey is an item's identity and revision: what the seen set holds.
type ItemKey struct {
	ID      string `json:"id" bson:"item_id"`
	Version string `json:"version" bson:"item_version"`
}

// Store is the durable state of pollers and their seen sets. Reads are
// tenant-scoped; ListDue is not — the elected worker drains every tenant —
// and the fenced writes name the poller and the generation Acquire returned.
type Store interface {
	// Ensure writes a poller once. The id is derived (ID) and the poller
	// starts at generation 0, unprimed, due at its CreatedAt; a poller that
	// exists is left untouched. It returns the stored poller and whether
	// this call wrote it.
	Ensure(ctx context.Context, p Poller) (Poller, bool, error)
	Get(ctx context.Context, tenantID, id string) (Poller, error)
	// List lists a tenant's pollers by id.
	List(ctx context.Context, tenantID string) ([]Poller, error)
	// ListDue lists every tenant's pollers due at now — NextTickAt at or
	// before it — by NextTickAt, then id, at most limit (0 = no bound).
	ListDue(ctx context.Context, now time.Time, limit int) ([]Poller, error)
	// Acquire takes the poller for owner: generation moves to
	// generation+1 when the stored one is generation, and the poller is
	// answered as it now stands. ErrFenced when the stored generation is
	// another — someone took it since it was read.
	Acquire(ctx context.Context, id string, generation int64, owner string, now time.Time) (Poller, error)
	// Reserve hands out n sequence numbers under generation and returns
	// the first; the poller's Sequence moves to first+n-1. ErrFenced under
	// a generation the poller moved past.
	Reserve(ctx context.Context, id string, generation int64, n int) (int64, error)
	// Wake makes the poller due at at when it is due later, and takes the
	// next generation: a subscription just written is read at once, and a
	// tick in flight — one that listed no subscriber before it was written,
	// and would park the poller dormant for a day — is fenced off.
	Wake(ctx context.Context, tenantID, id string, at time.Time) error
	// Complete records a tick's outcome under generation (Tick).
	// ErrFenced under a generation the poller moved past,
	// ErrCheckpointBehind for a timestamp checkpoint lower than the
	// stored one; either way nothing is written.
	Complete(ctx context.Context, id string, generation int64, t Tick) error

	// Seen reports which of keys the poller's seen set holds.
	Seen(ctx context.Context, pollerID string, keys []ItemKey) (map[ItemKey]bool, error)
	// MarkSeen records keys as observed at: added when absent, their last
	// observation moved forward — never back — when present. Each row
	// carries the poller's tenant, so an org purge reaches it.
	MarkSeen(ctx context.Context, tenantID, pollerID string, keys []ItemKey, at time.Time) error
	// PruneSeen drops the poller's keys last observed before before, and
	// returns how many it dropped.
	PruneSeen(ctx context.Context, pollerID string, before time.Time) (int, error)
}

// ID derives a poller's id from its tenant, connection, trigger and query
// values. The fields are length-prefixed, the query sorted by field id, so no
// two pollers share an id and one query typed in any order is one poller.
func ID(tenantID, connectionID, triggerID string, query map[string]string) string {
	fields := []string{tenantID, connectionID, triggerID}
	ids := make([]string, 0, len(query))
	for id := range query {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fields = append(fields, id, query[id])
	}
	return "pl-" + digest(fields...)
}

// preparePoller validates a poller and fills what Ensure derives.
func preparePoller(p Poller) (Poller, error) {
	switch {
	case p.TenantID == "":
		return Poller{}, fmt.Errorf("%w: a poller without a tenant", ErrInvalid)
	case p.ConnectionID == "":
		return Poller{}, fmt.Errorf("%w: a poller without a connection", ErrInvalid)
	case p.TriggerID == "":
		return Poller{}, fmt.Errorf("%w: a poller without a trigger", ErrInvalid)
	case p.CreatedAt.IsZero():
		return Poller{}, fmt.Errorf("%w: a poller without a creation instant", ErrInvalid)
	}
	p.ID = ID(p.TenantID, p.ConnectionID, p.TriggerID, p.Query)
	if len(p.Query) == 0 {
		p.Query = nil
	} else {
		q := make(map[string]string, len(p.Query))
		for k, v := range p.Query {
			q[k] = v
		}
		p.Query = q
	}
	p.CreatedAt = stamp(p.CreatedAt)
	p.UpdatedAt = p.CreatedAt
	p.NextTickAt = p.CreatedAt
	p.Generation = 0
	p.Holder = ""
	p.Checkpoint = nil
	p.Primed = false
	p.PrimedThrough = 0
	p.Sequence = 0
	p.LastTickAt = nil
	p.LastError = ""
	return p, nil
}

// checkTick validates a Tick and stamps its instants.
func checkTick(t Tick) (Tick, error) {
	if t.At.IsZero() || t.NextTickAt.IsZero() {
		return Tick{}, fmt.Errorf("%w: a tick says when it ran and when the poller is due again", ErrInvalid)
	}
	t.At, t.NextTickAt = stamp(t.At), stamp(t.NextTickAt)
	if t.Idle && (t.Primed || (t.Checkpoint != nil && t.Checkpoint.Kind != KindTimestamp)) {
		return Tick{}, fmt.Errorf("%w: an idle tick scanned nothing: it primes nothing, and moves only a timestamp checkpoint", ErrInvalid)
	}
	if c := t.Checkpoint; c != nil {
		switch c.Kind {
		case KindTimestamp:
			if c.At == nil || c.At.IsZero() || c.Token != "" {
				return Tick{}, fmt.Errorf("%w: a timestamp checkpoint holds an instant and no token", ErrInvalid)
			}
			at := stamp(*c.At)
			t.Checkpoint = &Checkpoint{Kind: KindTimestamp, At: &at}
		case KindOpaque:
			if c.Token == "" || c.At != nil {
				return Tick{}, fmt.Errorf("%w: an opaque checkpoint holds a token and no instant", ErrInvalid)
			}
			t.Checkpoint = &Checkpoint{Kind: KindOpaque, Token: c.Token}
		default:
			return Tick{}, fmt.Errorf("%w: checkpoint kind %q is not %s or %s", ErrInvalid, c.Kind, KindTimestamp, KindOpaque)
		}
	}
	return t, nil
}

// behind reports whether next would move cur back: only a timestamp against
// a timestamp is ordered. A cursor whose kind changed — the package moved
// from one to the other — replaces the old one.
func behind(cur, next *Checkpoint) bool {
	return cur != nil && next != nil && cur.Kind == KindTimestamp && next.Kind == KindTimestamp &&
		cur.At != nil && next.At != nil && next.At.Before(*cur.At)
}

// checkOwner refuses an anonymous holder: two ticks presenting no name would
// read as one on the record operators look at.
func checkOwner(owner string) error {
	if owner == "" {
		return fmt.Errorf("%w: a tick without an owner", ErrInvalid)
	}
	return nil
}

// seenID derives a seen-set row's id.
func seenID(pollerID string, k ItemKey) string {
	return "sn-" + digest(pollerID, k.ID, k.Version)
}

// digest hashes length-prefixed fields.
func digest(fields ...string) string {
	h := sha256.New()
	for _, f := range fields {
		_, _ = fmt.Fprintf(h, "%d:%s", len(f), f)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// stamp is the one precision every twin stores: Mongo keeps milliseconds, so
// the others do too, and a record reads back equal whichever twin wrote it.
func stamp(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Millisecond)
}
