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
	// Get returns one pool by id — the named-pool management surface's
	// lookup (ADR-164). ErrNotFound when absent.
	Get(ctx context.Context, id string) (Pool, error)
	// ListEnabled returns every pool currently accepting requests. The
	// broker filters them by audience in Go, because the audience is a
	// union of predicates (including "is the requester themselves a
	// donor") that no single index can answer.
	ListEnabled(ctx context.Context) ([]Pool, error)
	// ListAll returns every pool INCLUDING disabled ones — the owner's
	// management surface must see a disabled pool to re-enable or delete
	// it (a ListEnabled-only surface makes the id undiscoverable the
	// moment the pool is paused).
	ListAll(ctx context.Context) ([]Pool, error)
	Upsert(ctx context.Context, p Pool) error
	// Patch writes the named fields only (PoolPatch): a whole-document
	// write would drop columns a newer binary added while an older one
	// served — the rolling-update lesson ApiKeyPatch exists for. Both
	// implementations merge the patch into the stored row; ErrNotFound
	// when the pool does not exist.
	Patch(ctx context.Context, id string, p PoolPatch) (Pool, error)
	// Delete removes an empty pool. The route refuses a pool that still
	// holds entries or attachments; this method carries no cascade.
	Delete(ctx context.Context, id string) error
}

// PoolPatch names the fields a pool update may change. A nil field is left
// alone; a non-nil field is written as given, zero values included — the
// ApiKeyPatch discipline. The owner trio is deliberately absent: ownership
// is decided at creation and never patched (move the entries instead).
type PoolPatch struct {
	Name       *string
	Enabled    *bool
	Audience   *Audience
	Visibility *PoolVisibility
}

// IsEmpty reports whether the patch names no field at all. It reads the
// $set body rather than a second hand-kept list of the fields.
func (p PoolPatch) IsEmpty() bool { return len(p.setDoc()) == 0 }

// applyTo writes the named fields onto p for the memory twin. applyTo and
// setDoc enumerate the same fields: TestPoolPatch_everyFieldReachesBothWriters
// refuses a field one of them forgets.
func (p PoolPatch) applyTo(pool *Pool) {
	if p.Name != nil {
		pool.Name = *p.Name
	}
	if p.Enabled != nil {
		pool.Enabled = *p.Enabled
	}
	if p.Audience != nil {
		pool.Audience = *p.Audience
	}
	if p.Visibility != nil {
		pool.Visibility = *p.Visibility
	}
}

// AttachmentStore persists one attachment per scope (pool_attachments).
// CAS on UpdatedAt — the loud-409 discipline: two concurrent editors of
// one scope's attachment must not silently drop each other's write.
type AttachmentStore interface {
	// Get returns the scope's attachment, ErrNotFound when the scope has
	// none (which is the ordinary "inherit from the parent" answer).
	Get(ctx context.Context, scopeID string) (Attachment, error)
	// Put inserts or replaces the whole document, stamping UpdatedAt.
	Put(ctx context.Context, a Attachment) error
	// PutIfUnchanged writes only while the stored UpdatedAt still equals
	// expected; false means someone wrote in between (the route answers
	// 409). ErrNotFound when the attachment does not exist.
	PutIfUnchanged(ctx context.Context, a Attachment, expectedUpdatedAt time.Time) (bool, error)
	// DeleteIfUnchanged removes the attachment only while its UpdatedAt
	// still equals expected; false = someone wrote in between.
	DeleteIfUnchanged(ctx context.Context, scopeID string, expectedUpdatedAt time.Time) (bool, error)
	// Delete removes the attachment unconditionally (operator force-detach,
	// tests). ErrNotFound when absent.
	Delete(ctx context.Context, scopeID string) error
	// List returns every attachment — the signal reconciler's walk and the
	// orgsweep's opposite number.
	List(ctx context.Context) ([]Attachment, error)
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
	// CAS that keeps a redelivered report from charging twice. A superseded
	// close is not a report (Reopen).
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
	// Reopen undoes a supersede: it reopens the lease only while it is
	// closed as superseded at exactly supersededAt — the close an acquisition
	// made for a takeover that did not happen. That close is not a report: it
	// charged nothing (a superseded close adds $0) and no report has closed
	// the lease since, so reopening erases no charge, and the close CAS it
	// re-arms is the one the attempt still running on the lease reports
	// through. A lease reported, or closed at another instant, is left alone
	// (false).
	Reopen(ctx context.Context, leaseID string, supersededAt time.Time) (bool, error)
	// AddCost accumulates an interim attempt's spend on a lease its attempt
	// still reports against — open, or superseded (plainly or grantless),
	// whose stamp it leaves for the final report and whose Reopen it keeps
	// possible — so a redelivered run's audit trail matches the donor's
	// ledger. A lease reported, released or abandoned is left alone, and
	// landed=false says so: the lease closed that way before the interim
	// reached it.
	AddCost(ctx context.Context, leaseID string, costUSD float64) (landed bool, err error)
	// SetSupersededByPublication records, on a lease closed by a GRANTLESS
	// takeover, the publication that takeover was published at: a report
	// carrying exactly it is the takeover's own, and stays silent on the
	// lease (ReportAttempt).
	SetSupersededByPublication(ctx context.Context, leaseID string, publishedAt time.Time) error
	// ListByRun returns every lease of a run, any state, newest acquired
	// first — what an attempt-identified report reads to find the lease its
	// attempt executed under. The pin compares ACQUISITION and publication
	// at the millisecond both stores keep (a lease recorded inside the
	// publication's own millisecond is the attempt's; one recorded in a
	// later millisecond is a successor's), and a tie — rows written before
	// the broker's acquisition floor kept a run's leases strictly ordered —
	// is ordered by lease id, descending, so the pick is deterministic and
	// the same in both stores.
	ListByRun(ctx context.Context, runID string) ([]Lease, error)
	// ClearHeld lifts the reset_hold mark from every lease of the entry,
	// any state, and reports how many it cleared — the manual reset's
	// hold half (ADR-164).
	ClearHeld(ctx context.Context, pledgeID string) (int, error)
	// StampSupersededReport marks a superseded lease as reported by its own
	// attempt and adds the spend to it: a CAS on a superseded close — the
	// close that charged nothing, an acquisition's or a grantless
	// takeover's — so exactly one report of the attempt charges, and a
	// lease stamped this way no longer reads as a supersede (Reopen's CAS
	// misses it, and a refused resume's rollback cannot reopen it). False
	// when the lease is open, closed any other way, or already stamped;
	// ErrNotFound when it does not exist.
	StampSupersededReport(ctx context.Context, leaseID string, costUSD float64, when time.Time) (bool, error)
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
	// LiveCommitmentByAttachment reports what ONE ATTACHMENT currently has
	// at stake across its open leases: the runs it is serving (the run
	// counter the att: row's atomic reserve judges) and the allowance
	// promised to them (the committed half of the admission rule). The
	// attachment-level twin of LiveCommitment — without the committed half,
	// N concurrent draws under one capped attachment would each be handed
	// the same remaining allowance and could spend it N times over.
	LiveCommitmentByAttachment(ctx context.Context, attachmentScopeID, excludeRunID string, now time.Time) (runs int, committedUSD float64, err error)
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
	// AggregateTokens holds a CLI delegate's unsplittable total. Zero in
	// all three token fields means "not observed", never "nothing spent";
	// a true total is their sum.
	AggregateTokens int64 `json:"aggregate_tokens"`
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
	AddSpend(ctx context.Context, pledgeID string, when time.Time, costUSD float64, inputTokens, outputTokens, aggregateTokens int64) error
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
