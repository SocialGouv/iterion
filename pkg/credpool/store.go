package credpool

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is the shared sentinel for every store in this package.
var ErrNotFound = errors.New("credpool: not found")

// PoolStore persists pools.
type PoolStore interface {
	// GetByOrg returns the pool an org OWNS — the lookup the management
	// API uses. NOT the launch path's lookup: a pool whose audience opens
	// it to other orgs must be reachable from those orgs, so selection
	// goes through ListEnabled instead.
	GetByOrg(ctx context.Context, orgID string) (Pool, error)
	// ListEnabled returns every pool currently accepting requests. The
	// broker filters them by audience in Go, because the audience is a
	// union of predicates (including "is the requester themselves a
	// donor") that no single index can answer.
	ListEnabled(ctx context.Context) ([]Pool, error)
	Upsert(ctx context.Context, p Pool) error
}

// PledgeStore persists donors' offers.
type PledgeStore interface {
	Get(ctx context.Context, pledgeID string) (Pledge, error)
	// ListByPool returns every pledge attached to a pool, enabled or not —
	// the operator roster. Selection filters on top.
	ListByPool(ctx context.Context, poolID string) ([]Pledge, error)
	// ListByUser returns a donor's own pledges (one per credential kind).
	ListByUser(ctx context.Context, userID string) ([]Pledge, error)
	Upsert(ctx context.Context, p Pledge) error
	Delete(ctx context.Context, pledgeID string) error
	// TouchLastServed stamps the fairness tie-break instant. A dedicated
	// single-field write, not an Upsert: this runs on the launch path for
	// every acquisition, and rewriting the whole document there would also
	// let a stale in-flight copy clobber a concurrent change — a donor
	// pausing, or a cooldown just set by a finishing run.
	TouchLastServed(ctx context.Context, pledgeID string, when time.Time) error
}

// LeaseStore persists the run↔pledge bindings.
type LeaseStore interface {
	// Put inserts an attempt's lease. Leases are never reused: a finished
	// attempt's record is the donor's evidence for the charge on their
	// ledger, and re-opening it would both erase that and re-arm the close
	// CAS that keeps a redelivered report from charging twice.
	Put(ctx context.Context, l Lease) error
	// Get returns one lease by id.
	Get(ctx context.Context, leaseID string) (Lease, error)
	// GetOpenByRun returns the run's currently-open lease — what a
	// finishing run reports against. Acquiring supersedes any earlier open
	// lease of the same run, so there is normally exactly one; the MOST
	// RECENT wins if that invariant is ever broken, because guessing
	// arbitrarily would charge an arbitrary donor.
	GetOpenByRun(ctx context.Context, runID string) (Lease, error)
	// ListOpenByRun returns every open lease of a run, so acquiring can
	// supersede the ones it replaces.
	ListOpenByRun(ctx context.Context, runID string) ([]Lease, error)
	// HasServedAttempt reports whether this pledge already admitted this
	// run — the test for "this is a resume, not a new run". An abandoned
	// attempt does not count: nothing ever learned what it spent, so the
	// next attempt is charged as new rather than renewing indefinitely.
	HasServedAttempt(ctx context.Context, runID, pledgeID string) (bool, error)
	// Close marks a lease reported and frees its slot. It is a CAS on
	// "still open": the caller may only charge the donor when it wins,
	// which is what stops a redelivered report double-charging. costUSD is
	// ADDED to whatever interim charges the lease already carries.
	Close(ctx context.Context, leaseID string, costUSD float64, outcome string, when time.Time) (won bool, err error)
	// AddCost accumulates an interim attempt's spend on a still-open lease,
	// so a redelivered run's audit trail matches the donor's ledger.
	AddCost(ctx context.Context, leaseID string, costUSD float64) error
	// LiveCommitment reports what a pledge currently has at stake: how many
	// live (unclosed, unexpired) leases it holds, and the total allowance
	// already handed to them. Derived from the leases rather than
	// accumulated in a counter, so an abandoned run can never leave a slot
	// or an allowance permanently consumed.
	//
	// excludeRunID is the run currently asking. A resume re-acquires for a
	// run that still holds its own lease; counting that would let a run be
	// refused by itself, silently dropping its donor at every resume of a
	// pledge whose concurrency cap is 1.
	LiveCommitment(ctx context.Context, pledgeID, excludeRunID string, now time.Time) (runs int, committedUSD float64, err error)
	// ListExpired returns live leases past their expiry, for the sweeper.
	ListExpired(ctx context.Context, now time.Time, limit int) ([]Lease, error)
	// ListByDonor returns a donor's recent leases, newest first — the
	// "what did my quota run" history.
	ListByDonor(ctx context.Context, donorID string, limit int) ([]Lease, error)
}

// Usage is one pledge's consumption within a period.
type Usage struct {
	// Period is "day" or "week"; Key is its bucket ("2026-08-03",
	// "2026-W31").
	Period       string  `json:"period"`
	Key          string  `json:"key"`
	Runs         int     `json:"runs"`
	CostUSD      float64 `json:"cost_usd"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
}

// Ledger meters per-pledge consumption and enforces the donor's limits.
//
// Shape and CAS strategy mirror pkg/orgusage deliberately: one document per
// (pledge, period), all increments through findOneAndUpdate/$inc, so an
// operator reasoning about one quota system can reason about the other.
type Ledger interface {
	// Reserve admits one run against pledgeID's limits and meters it.
	//
	// The daily bucket is the atomic one: the run counter is incremented
	// and both daily caps are read off the SAME post-increment document,
	// then rolled back on refusal. The weekly cap and the concurrency
	// reading are checked around it and are soft by nature — a run's
	// future spend is unknowable, so in-flight runs always finish and only
	// new admissions are refused.
	//
	// remainingUSD is what is left of the tightest spend cap; it becomes
	// the admitted run's own cost budget. Zero means the donor set no
	// spend cap at all — the caller must NOT read it as "nothing left".
	Reserve(ctx context.Context, pledgeID string, when time.Time, l Limits, live LiveCommitment) (remainingUSD float64, deny DenyReason, err error)
	// Renew re-checks the SPEND caps for a run this pledge already
	// admitted, without counting a second run against the daily run quota.
	// A resumed run is the same run: charging the donor another unit of
	// "runs per day" for it would let one flaky run that resumes a few
	// times consume a contributor's entire day.
	Renew(ctx context.Context, pledgeID string, when time.Time, l Limits, live LiveCommitment) (remainingUSD float64, deny DenyReason, err error)
	// ReleaseRun undoes a Reserve whose run was never created.
	ReleaseRun(ctx context.Context, pledgeID string, when time.Time) error
	// AddSpend records what a served run actually consumed, into both the
	// day and week buckets.
	AddSpend(ctx context.Context, pledgeID string, when time.Time, costUSD float64, inputTokens, outputTokens int64) error
	// Usage reads one pledge's day and week buckets.
	Usage(ctx context.Context, pledgeID string, when time.Time) (day Usage, week Usage, err error)
	// UsageMany reads the day bucket of several pledges at once — the
	// selection path needs every candidate's consumption to rank them, and
	// doing that one query per donor would put N round trips on every
	// launch.
	UsageMany(ctx context.Context, pledgeIDs []string, when time.Time) (map[string]Usage, error)
}

// DenyReason says which limit refused an admission. Stable tokens: they
// reach the donor's UI and the acquisition trace.
type DenyReason string

const (
	DenyNone        DenyReason = ""
	DenyRunsPerDay  DenyReason = "runs_per_day"
	DenyCostPerDay  DenyReason = "cost_per_day"
	DenyCostPerWeek DenyReason = "cost_per_week"
	DenyConcurrency DenyReason = "concurrency"
)

// LiveCommitment is what a pledge already has at stake across its open
// leases: the runs it is serving, and the allowance handed to them but not
// yet spent. Both bound the next admission.
type LiveCommitment struct {
	Runs         int
	CommittedUSD float64
}

// Deny is the single expression of "has this donor given all they offered".
//
// It is THE rule: both ledger implementations enforce admission with it,
// and the donor's status projection displays with it. Keeping one copy is
// what stops a UI reading "sharing" while the ledger refuses every run —
// and makes a new limit axis a one-line change instead of three.
//
// runsAfterAdmit is the day's run count INCLUDING the admission being
// judged: the Mongo ledger reads it post-increment, the memory ledger and
// the display add one themselves.
func (l Limits) Deny(runsAfterAdmit int, daySpent, weekSpent float64) DenyReason {
	switch {
	case l.MaxRunsPerDay > 0 && runsAfterAdmit > l.MaxRunsPerDay:
		return DenyRunsPerDay
	case l.MaxUSDPerDay > 0 && daySpent >= l.MaxUSDPerDay:
		return DenyCostPerDay
	case l.MaxUSDPerWeek > 0 && weekSpent >= l.MaxUSDPerWeek:
		return DenyCostPerWeek
	}
	return DenyNone
}

// decide is the admission judgement both ledgers share: spend already
// accounted PLUS spend already promised to in-flight runs, against the
// donor's ceilings.
//
// The returned allowance is what THIS run may spend. A capped donor with
// nothing left denies rather than granting zero — zero on the wire means
// "no ceiling", so handing it out here would turn an exhausted donor into
// an unlimited one.
func decide(l Limits, runsAfterAdmit int, daySpent, weekSpent float64, live LiveCommitment) (float64, DenyReason) {
	if deny := l.Deny(runsAfterAdmit, daySpent+live.CommittedUSD, weekSpent+live.CommittedUSD); deny != DenyNone {
		return 0, deny
	}
	remaining, capped := remainingAllowance(l, daySpent+live.CommittedUSD, weekSpent+live.CommittedUSD)
	if capped && remaining <= 0 {
		return 0, DenyCostPerDay
	}
	return shareAcrossSlots(l, remaining, live.Runs), DenyNone
}

// shareAcrossSlots splits what is left of a capped allowance across the
// concurrency slots still free.
//
// Handing the WHOLE remaining allowance to the first run would promise it
// away: the committed half of the admission rule above then denies every
// further run on cost, and the concurrency dial a donor set could never
// bind. Dividing keeps the ceiling exact — n slots each hold 1/n, summing
// to the allowance — while letting the runs the donor allowed actually run
// side by side.
//
// An uncapped allowance (0 = no ceiling) is returned untouched, and so is a
// donor who allowed a single run at a time: there is nothing to share.
func shareAcrossSlots(l Limits, remaining float64, liveRuns int) float64 {
	if remaining <= 0 || l.MaxConcurrentRuns <= 1 {
		return remaining
	}
	free := l.MaxConcurrentRuns - liveRuns
	if free <= 1 {
		return remaining
	}
	return remaining / float64(free)
}

// remainingAllowance returns what is left of the donor's spend caps given
// the current day and week consumption, and whether any cap is set at all.
// The tightest of the two wins — a weekly budget nearly spent must bound
// today's run even when the daily cap looks generous.
func remainingAllowance(l Limits, daySpent, weekSpent float64) (remaining float64, capped bool) {
	if l.MaxUSDPerDay > 0 {
		remaining, capped = l.MaxUSDPerDay-daySpent, true
	}
	if l.MaxUSDPerWeek > 0 {
		if w := l.MaxUSDPerWeek - weekSpent; !capped || w < remaining {
			remaining, capped = w, true
		}
	}
	if capped && remaining < 0 {
		remaining = 0
	}
	return remaining, capped
}
