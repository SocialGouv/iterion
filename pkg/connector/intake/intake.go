// Package intake holds what a connector source owes after it answered a
// vendor (ADR-168 §8): the durable INTAKE row a delivery or a polled item
// becomes, and the PAIRS that record, for each candidate subscription, what
// the intake owes it — and drives both through their state machines.
//
//	intake:  pending → claimed(owner, generation, expiry) → matched
//	                     ↘ pending (retry, backoff)  ↘ dead (attempts spent)
//	pair:    pending → claimed(owner, generation, expiry) → launched
//	                     ↘ pending (retry, backoff)  ↘ refused | dead
//	         not_matched (terminal: the subscription's filters said no)
//
// A claim is a lease on one row: the claimer is recorded with the row's next
// generation and an expiry, and every later write about that claim names the
// generation it holds. A write whose generation is no longer the row's — the
// claim expired and another worker took it — changes nothing and fails with
// ErrClaimLost, so a late worker can neither resurrect nor clobber what the
// current one decided. A claim past its expiry is claimable again, and that
// reclaim spends an attempt: work that never returns still reaches its dead
// letter.
//
// Three twins implement Store with one contract (conformance_test.go): a
// memory store for tests, a file store under the run store (.iterion/) for a
// local server, and Mongo for the cloud.
package intake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Intake states.
const (
	// IntakePending waits for matching, or for its retry backoff (NotBefore).
	IntakePending = "pending"
	// IntakeClaimed is held by one matcher until NotBefore.
	IntakeClaimed = "claimed"
	// IntakeMatched is terminal: every candidate subscription has its pair.
	IntakeMatched = "matched"
	// IntakeDead is the dead letter: matching failed past its attempts.
	// Replayable.
	IntakeDead = "dead"
)

// Pair states.
const (
	// PairPending is owed a launch, now or after its backoff (NotBefore).
	PairPending = "pending"
	// PairClaimed is held by one launcher until NotBefore.
	PairClaimed = "claimed"
	// PairLaunched is terminal: the submission was acknowledged (RunID).
	PairLaunched = "launched"
	// PairNotMatched is terminal: the subscription's filters refused the
	// event. Written so a fan-out records every candidate's verdict.
	PairNotMatched = "not_matched"
	// PairRefused is terminal: a verdict no retry changes — an expression
	// past its budget, a submission refused for good. Reason says which. A
	// launch-time refusal replays into the next key; a match-time one is not
	// replayed into a launch (MatchRefusal).
	PairRefused = "refused"
	// PairDead is the dead letter: the launch failed past its attempts.
	// Replayable under its current key, which the launch path answers with
	// the run when one was handed on.
	PairDead = "dead"
)

// Store errors.
var (
	// ErrNotFound: no row with that id in that tenant.
	ErrNotFound = errors.New("intake: not found")
	// ErrClaimLost: a write named a claim the row no longer holds — it
	// expired and was taken, or the row moved on.
	ErrClaimLost = errors.New("intake: claim lost")
	// ErrNotReplayable: only a dead intake, or a dead or refused pair, is
	// replayed.
	ErrNotReplayable = errors.New("intake: not replayable")
	// ErrInvalid: a row the store refuses to write.
	ErrInvalid = errors.New("intake: invalid row")
)

// Intake is one accepted delivery or polled item: the sanitized event and
// where it came from. Its id is derived from (tenant, source, dedup key), so a
// redelivery of the same authenticated material meets the existing row.
type Intake struct {
	ID       string `json:"id" bson:"_id"`
	TenantID string `json:"tenant_id" bson:"tenant_id"`
	// ConnectionID is the connection the delivery authenticated against.
	ConnectionID string `json:"connection_id" bson:"connection_id"`
	// IngressID or PollerID — exactly one — is the source within the
	// connection.
	IngressID string `json:"ingress_id,omitempty" bson:"ingress_id,omitempty"`
	PollerID  string `json:"poller_id,omitempty" bson:"poller_id,omitempty"`
	// TriggerID is the package trigger the event was read as.
	TriggerID string `json:"trigger_id" bson:"trigger_id"`
	// DedupKey is the digest of the authenticated material (ADR-168 §3), or
	// a polled item's (item id, item version).
	DedupKey string `json:"dedup_key" bson:"dedup_key"`
	// Event is the sanitized event as JSON — outputs, payload without its
	// redacted fields, listed headers (ADR-168 §4). Opaque here: a vendor's
	// keys may hold anything a document field name may not.
	Event json.RawMessage `json:"event,omitempty" bson:"event,omitempty"`

	ReceivedAt time.Time `json:"received_at" bson:"received_at"`
	// ExpiresAt is when the row and its pairs are dropped; nil keeps them.
	ExpiresAt *time.Time `json:"expires_at,omitempty" bson:"expires_at,omitempty"`

	State      string    `json:"state" bson:"state"`
	Attempts   int       `json:"attempts" bson:"attempts"`
	Owner      string    `json:"owner,omitempty" bson:"owner,omitempty"`
	Generation int64     `json:"generation" bson:"generation"`
	NotBefore  time.Time `json:"not_before" bson:"not_before"`
	LastError  string    `json:"last_error,omitempty" bson:"last_error,omitempty"`
	// MatchedAt is when the intake reached IntakeMatched.
	MatchedAt *time.Time `json:"matched_at,omitempty" bson:"matched_at,omitempty"`
	UpdatedAt time.Time  `json:"updated_at" bson:"updated_at"`
}

// source names the intake's source within its tenant.
func (in Intake) source() string {
	if in.IngressID != "" {
		return "ingress:" + in.IngressID
	}
	return "poller:" + in.PollerID
}

// IntakeID derives an intake's id. The fields are length-prefixed so no two
// tuples share a digest.
func IntakeID(tenantID, source, dedupKey string) string {
	return "in-" + digest(tenantID, source, dedupKey)
}

// Pair is what one intake owes one candidate subscription.
type Pair struct {
	ID             string `json:"id" bson:"_id"`
	TenantID       string `json:"tenant_id" bson:"tenant_id"`
	IntakeID       string `json:"intake_id" bson:"intake_id"`
	SubscriptionID string `json:"subscription_id" bson:"subscription_id"`
	// ReceivedAt and ExpiresAt are the intake's, so a pair is listed with
	// and expires with what it was matched from.
	ReceivedAt time.Time  `json:"received_at" bson:"received_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty" bson:"expires_at,omitempty"`

	State string `json:"state" bson:"state"`
	// Reason says why a pair was not matched or refused.
	Reason string `json:"reason,omitempty" bson:"reason,omitempty"`
	// MatchRefusal marks a pair refused by MATCHING — its filters could not
	// be judged — as opposed to a launch the launch path refused. No filter
	// ever said yes to it, so it is never replayed into a launch.
	MatchRefusal bool `json:"match_refusal,omitempty" bson:"match_refusal,omitempty"`
	// Epoch numbers the pair's submissions: its key, and so its run id, is
	// derived from (pair, epoch). It advances when the launch path proves a
	// submission spent — its run never left the launch path — and when a
	// launch-time refusal is replayed. It never advances while a run of the
	// current key may live: a dead pair replays under the same key.
	Epoch int `json:"epoch" bson:"epoch"`
	// RunID is the run of the acknowledged submission.
	RunID string `json:"run_id,omitempty" bson:"run_id,omitempty"`

	Attempts   int       `json:"attempts" bson:"attempts"`
	Owner      string    `json:"owner,omitempty" bson:"owner,omitempty"`
	Generation int64     `json:"generation" bson:"generation"`
	NotBefore  time.Time `json:"not_before" bson:"not_before"`
	LastError  string    `json:"last_error,omitempty" bson:"last_error,omitempty"`
	// LaunchedAt is when the pair reached PairLaunched.
	LaunchedAt *time.Time `json:"launched_at,omitempty" bson:"launched_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at" bson:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at" bson:"updated_at"`
}

// PairID derives a pair's id: one pair per (intake, subscription).
func PairID(intakeID, subscriptionID string) string {
	return "pr-" + digest(intakeID, subscriptionID)
}

// SubmissionKey is the key the pair's current epoch submits its launch under
// (runview.LaunchSpec.SubmissionKey): stable across the retries of one epoch,
// distinct across epochs.
func (p Pair) SubmissionKey() string {
	return "connector-pair:" + p.ID + "#" + strconv.Itoa(p.Epoch)
}

// PairRetry is what a failed launch attempt writes back.
type PairRetry struct {
	Attempts  int
	NotBefore time.Time
	LastError string
	// Epoch is the epoch the next attempt submits under: the pair's own, or
	// the next one when this attempt's submission was spent.
	Epoch int
}

// PairQuery narrows ListPairs; zero fields match everything.
type PairQuery struct {
	IntakeID string
	State    string
}

// Store is the durable state of intakes and pairs. Reads and replays are
// tenant-scoped; claims are not — a worker drains every tenant — and the
// fenced writes name the row and the generation their claim returned.
type Store interface {
	// Accept writes an intake once. The id is derived (IntakeID) and the
	// row starts pending; a row that exists is left untouched whatever
	// state it reached. It returns the stored row and whether this call
	// wrote it.
	Accept(ctx context.Context, in Intake) (Intake, bool, error)
	GetIntake(ctx context.Context, tenantID, id string) (Intake, error)
	// ListIntakes lists a tenant's intakes in state ("" for every state),
	// newest first, at most limit (0 = no bound).
	ListIntakes(ctx context.Context, tenantID, state string, limit int) ([]Intake, error)
	// ClaimIntakes claims up to limit claimable intakes for owner until
	// now+ttl: pending rows past their NotBefore and claims past their
	// expiry, in ONE order — NotBefore, then id — so a retry or an expired
	// claim is never passed over by rows that arrived after it (a fresh row's
	// NotBefore is its arrival). A reclaim spends an attempt. ttl is at least
	// the store's millisecond precision.
	ClaimIntakes(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]Intake, error)
	MarkIntakeMatched(ctx context.Context, id string, generation int64, at time.Time) error
	RetryIntake(ctx context.Context, id string, generation int64, attempts int, notBefore time.Time, lastErr string) error
	// BuryIntake moves a claimed intake to its dead letter with the
	// attempts it spent.
	BuryIntake(ctx context.Context, id string, generation int64, attempts int, lastErr string) error
	// ReplayIntake returns a dead intake to pending with its attempts
	// reset, under a new generation, due now.
	ReplayIntake(ctx context.Context, tenantID, id string, now time.Time) (Intake, error)

	// WritePairs inserts pairs that do not exist yet; an existing pair is
	// left untouched whatever state it reached, so a matcher repeating its
	// work after a lost claim adds nothing. Ids are derived (PairID).
	WritePairs(ctx context.Context, pairs []Pair) error
	GetPair(ctx context.Context, tenantID, id string) (Pair, error)
	// ListPairs lists a tenant's pairs, newest intake first, then by intake
	// and subscription id, at most limit (0 = no bound).
	ListPairs(ctx context.Context, tenantID string, q PairQuery, limit int) ([]Pair, error)
	// ClaimPairs is ClaimIntakes for pending pairs.
	ClaimPairs(ctx context.Context, owner string, now time.Time, ttl time.Duration, limit int) ([]Pair, error)
	MarkPairLaunched(ctx context.Context, id string, generation int64, runID string, at time.Time) error
	RetryPair(ctx context.Context, id string, generation int64, r PairRetry) error
	RefusePair(ctx context.Context, id string, generation int64, reason string) error
	// BuryPair moves a claimed pair to its dead letter with the attempts it
	// spent.
	BuryPair(ctx context.Context, id string, generation int64, attempts int, lastErr string) error
	// ReplayPair returns a dead or launch-refused pair to pending, due now,
	// with its attempts reset and a new generation. A dead pair keeps its
	// epoch: its last attempt may have been handed on, and re-submitting the
	// same key is answered by that run instead of starting a second. A
	// launch-refused pair moves to the next epoch, a new submission. A
	// match-refused pair is not replayable: nothing ever matched it.
	ReplayPair(ctx context.Context, tenantID, id string, now time.Time) (Pair, error)
}

// prepareIntake validates an intake and fills what Accept derives.
func prepareIntake(in Intake) (Intake, error) {
	switch {
	case in.TenantID == "":
		return Intake{}, fmt.Errorf("%w: intake without a tenant", ErrInvalid)
	case in.ConnectionID == "":
		return Intake{}, fmt.Errorf("%w: intake without a connection", ErrInvalid)
	case (in.IngressID == "") == (in.PollerID == ""):
		return Intake{}, fmt.Errorf("%w: an intake comes from exactly one ingress or poller (ingress=%q poller=%q)", ErrInvalid, in.IngressID, in.PollerID)
	case in.TriggerID == "":
		return Intake{}, fmt.Errorf("%w: intake without a trigger", ErrInvalid)
	case in.DedupKey == "":
		return Intake{}, fmt.Errorf("%w: intake without a dedup key", ErrInvalid)
	case in.ReceivedAt.IsZero():
		return Intake{}, fmt.Errorf("%w: intake without a reception instant", ErrInvalid)
	}
	if len(in.Event) > 0 && !json.Valid(in.Event) {
		return Intake{}, fmt.Errorf("%w: intake event is not JSON", ErrInvalid)
	}
	if len(in.Event) > 0 {
		// One canonical encoding whichever twin stores it: the file twin
		// re-encodes its journal, so bytes kept verbatim by the others would
		// read back different there.
		canon, err := json.Marshal(in.Event)
		if err != nil {
			return Intake{}, fmt.Errorf("%w: intake event: %v", ErrInvalid, err)
		}
		in.Event = canon
	}
	in.ID = IntakeID(in.TenantID, in.source(), in.DedupKey)
	in.ReceivedAt = stamp(in.ReceivedAt)
	in.ExpiresAt = stampPtr(in.ExpiresAt)
	in.State = IntakePending
	in.Attempts = 0
	in.Owner = ""
	in.Generation = 0
	// Due from its arrival: claims take rows in NotBefore order, so a fresh
	// row queues behind the retries and expired claims that came before it.
	in.NotBefore = in.ReceivedAt
	in.LastError = ""
	in.MatchedAt = nil
	in.UpdatedAt = in.ReceivedAt
	return in, nil
}

// preparePair validates a pair and fills what WritePairs derives.
func preparePair(p Pair) (Pair, error) {
	switch {
	case p.TenantID == "":
		return Pair{}, fmt.Errorf("%w: pair without a tenant", ErrInvalid)
	case p.IntakeID == "" || p.SubscriptionID == "":
		return Pair{}, fmt.Errorf("%w: pair without its intake and subscription (intake=%q subscription=%q)", ErrInvalid, p.IntakeID, p.SubscriptionID)
	case p.CreatedAt.IsZero():
		return Pair{}, fmt.Errorf("%w: pair without a creation instant", ErrInvalid)
	}
	switch p.State {
	case PairPending, PairNotMatched, PairRefused:
	default:
		return Pair{}, fmt.Errorf("%w: a pair is written pending, not_matched or refused, not %q", ErrInvalid, p.State)
	}
	if p.State != PairPending && p.Reason == "" {
		return Pair{}, fmt.Errorf("%w: a %s pair says why", ErrInvalid, p.State)
	}
	p.ID = PairID(p.IntakeID, p.SubscriptionID)
	p.MatchRefusal = p.State == PairRefused
	p.ReceivedAt = stamp(p.ReceivedAt)
	p.ExpiresAt = stampPtr(p.ExpiresAt)
	p.CreatedAt = stamp(p.CreatedAt)
	p.UpdatedAt = p.CreatedAt
	p.Epoch = 0
	p.RunID = ""
	p.Attempts = 0
	p.Owner = ""
	p.Generation = 0
	p.NotBefore = p.CreatedAt
	p.LastError = ""
	p.LaunchedAt = nil
	return p, nil
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
// the others do too, and a row reads back equal whichever twin wrote it.
func stamp(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return t.UTC().Truncate(time.Millisecond)
}

func stampPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	s := stamp(*t)
	return &s
}
