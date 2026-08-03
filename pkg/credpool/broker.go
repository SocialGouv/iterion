package credpool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// ErrNoDonor is returned by Acquire when no pledge could serve the request.
// It is an ordinary outcome, not a failure: the caller falls through to
// whatever it would have done with no pool at all (env credentials, or a
// visible "no credentials" at the LLM call site). Callers should log it and
// carry on, never abort the launch on it.
var ErrNoDonor = errors.New("credpool: no eligible donor")

// Broker selects a donor for a run and closes the loop when it ends.
//
// A nil *Broker is a valid "pool disabled" value: every method is
// nil-receiver-safe, so call sites need no nil checks — the same
// convention as runtime.DailyCapGuard.
type Broker struct {
	pools   PoolStore
	pledges PledgeStore
	leases  LeaseStore
	ledger  Ledger
	oauth   secrets.OAuthStore
	sealer  secrets.Sealer
	logger  *iterlog.Logger
	now     func() time.Time
	// leaseTTL bounds an unreported lease. Overridable for tests.
	leaseTTL time.Duration
}

// BrokerConfig bundles the Broker's dependencies.
type BrokerConfig struct {
	Pools   PoolStore
	Pledges PledgeStore
	Leases  LeaseStore
	Ledger  Ledger
	// OAuth + Sealer are how a granted pledge becomes an actual credential
	// blob: the record is stored sealed and bound to its owner, exactly as
	// for a personal or org forfait.
	OAuth  secrets.OAuthStore
	Sealer secrets.Sealer
	Logger *iterlog.Logger
	// Now overrides the clock (tests). Defaults to time.Now().UTC().
	Now func() time.Time
	// LeaseTTL overrides DefaultLeaseTTL.
	LeaseTTL time.Duration
}

// NewBroker builds a Broker. Returns nil — a usable "pool disabled" value —
// when any store required to serve a credential is missing, so a deployment
// that never configured a pool simply never has one.
func NewBroker(cfg BrokerConfig) *Broker {
	if cfg.Pools == nil || cfg.Pledges == nil || cfg.Leases == nil || cfg.Ledger == nil ||
		cfg.OAuth == nil || cfg.Sealer == nil {
		return nil
	}
	if cfg.Logger == nil {
		cfg.Logger = iterlog.New(iterlog.LevelInfo, nil)
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = DefaultLeaseTTL
	}
	return &Broker{
		pools: cfg.Pools, pledges: cfg.Pledges, leases: cfg.Leases, ledger: cfg.Ledger,
		oauth: cfg.OAuth, sealer: cfg.Sealer, logger: cfg.Logger,
		now: cfg.Now, leaseTTL: cfg.LeaseTTL,
	}
}

// Request describes the run asking for a donated credential.
type Request struct {
	RunID string
	// OrgID / TenantID / UserID identify the requester for the audience
	// check and for the donor's history.
	OrgID    string
	TenantID string
	UserID   string
	BotID    string
	// Kinds are the credential kinds that would serve this run, in
	// preference order. A run asks once for all of them: they share the
	// same pool, audience and candidate list, so re-resolving per kind
	// would be the same three queries again for a different in-memory
	// filter.
	Kinds []secrets.OAuthKind
}

// Grant is a served donation.
type Grant struct {
	PledgeID string
	DonorID  string
	Kind     string
	// Payload is the verbatim credentials blob, unsealed. The caller seals
	// it into the run bundle exactly like a personal or org forfait — it
	// must never be logged, persisted in the clear, or returned over an
	// API.
	Payload []byte
	// RemainingUSD is what was left of the donor's tightest spend cap.
	// Zero means the donor set no spend cap, NOT "nothing left" — callers
	// clamping a run budget must treat zero as "no ceiling from the pool".
	RemainingUSD float64
}

// Acquire finds a donor for req and records the lease. Returns ErrNoDonor
// when the pool is off, the requester is out of audience, or no pledge is
// currently eligible.
func (b *Broker) Acquire(ctx context.Context, req Request) (*Grant, error) {
	if b == nil {
		return nil, ErrNoDonor
	}
	if req.RunID == "" {
		return nil, fmt.Errorf("credpool: acquire without a run id")
	}
	if len(req.Kinds) == 0 {
		return nil, fmt.Errorf("credpool: acquire without a credential kind")
	}
	for _, k := range req.Kinds {
		if !k.Valid() {
			return nil, fmt.Errorf("credpool: unknown credential kind %q", k)
		}
	}
	now := b.now()

	pool, candidates, err := b.resolvePool(ctx, req)
	if err != nil {
		return nil, err
	}

	for _, kind := range req.Kinds {
		grant, err := b.acquireKind(ctx, pool, candidates, req, kind, now)
		if err != nil {
			return nil, err
		}
		if grant != nil {
			return grant, nil
		}
	}
	return nil, ErrNoDonor
}

// acquireKind tries one credential kind against an already-resolved
// candidate list. Returns (nil, nil) when no donor of that kind can serve —
// the caller falls through to the next kind.
func (b *Broker) acquireKind(ctx context.Context, pool Pool, candidates []Pledge, req Request, kind secrets.OAuthKind, now time.Time) (*Grant, error) {
	eligible := make([]Pledge, 0, len(candidates))
	for _, p := range candidates {
		if p.Kind != string(kind) {
			continue
		}
		// A donor never serves their own run: they would be lending to
		// themselves, consuming pool bookkeeping for nothing and skewing
		// the fairness ranking against the other donors.
		if p.UserID == req.UserID {
			continue
		}
		if ok, _ := p.Available(now, req.BotID); ok {
			eligible = append(eligible, p)
		}
	}
	if len(eligible) == 0 {
		return nil, nil
	}

	ranked, err := b.rank(ctx, eligible, now)
	if err != nil {
		return nil, err
	}

	// Walk the ranking: a donor whose ledger refuses (raced to their cap,
	// concurrency full) simply yields to the next.
	for _, p := range ranked {
		grant, err := b.tryPledge(ctx, pool, p, req, now)
		if err != nil {
			b.logger.Warn("credpool: pledge %s unusable for run %s: %v", p.ID, req.RunID, err)
			continue
		}
		if grant != nil {
			return grant, nil
		}
	}
	return nil, nil
}

// resolvePool finds the pool serving this request, with its pledges.
//
// Selection scans the ENABLED pools and applies each one's audience,
// rather than looking up the requester's own org: a pool that opens itself
// to other teams/orgs — or to contributors wherever they launch from — must
// be reachable FROM those places, otherwise every audience dial beyond the
// owning org is unreachable configuration. A deployment runs a handful of
// pools, so this is a small list, and the requester's own org pool wins
// when several would serve.
func (b *Broker) resolvePool(ctx context.Context, req Request) (Pool, []Pledge, error) {
	pools, err := b.pools.ListEnabled(ctx)
	if err != nil {
		return Pool{}, nil, err
	}
	if len(pools) == 0 {
		return Pool{}, nil, ErrNoDonor
	}
	// Own-org pool first: a team drawing on its own pool must not be
	// diverted to a community one that happens to sort earlier.
	sort.SliceStable(pools, func(i, j int) bool {
		own := func(p Pool) bool { return req.OrgID != "" && p.OrgID == req.OrgID }
		if own(pools[i]) != own(pools[j]) {
			return own(pools[i])
		}
		return pools[i].ID < pools[j].ID
	})
	now := b.now()
	for _, pool := range pools {
		candidates, err := b.pledges.ListByPool(ctx, pool.ID)
		if err != nil {
			return Pool{}, nil, err
		}
		hasActivePledge := false
		if pool.Audience.NeedsPledgeLookup() && req.UserID != "" {
			for _, p := range candidates {
				if p.UserID != req.UserID {
					continue
				}
				// Reciprocity is earned by an ACTIVE contribution: a donor
				// who switched their own sharing off stops borrowing too.
				if ok, _ := p.Available(now, ""); ok {
					hasActivePledge = true
					break
				}
			}
		}
		if pool.Audience.Allows(pool.OrgID, req.OrgID, req.TenantID, req.UserID, hasActivePledge) {
			return pool, candidates, nil
		}
	}
	return Pool{}, nil, ErrNoDonor
}

// rank orders eligible pledges by fairness: least consumed today first
// (as a fraction of what the donor offered, so a small pledge is not
// drained before a large one), ties broken by least-recently served.
func (b *Broker) rank(ctx context.Context, eligible []Pledge, now time.Time) ([]Pledge, error) {
	ids := make([]string, 0, len(eligible))
	for _, p := range eligible {
		ids = append(ids, p.ID)
	}
	usage, err := b.ledger.UsageMany(ctx, ids, now)
	if err != nil {
		return nil, err
	}
	score := func(p Pledge) float64 {
		u := usage[p.ID]
		switch {
		case p.Limits.MaxUSDPerDay > 0:
			return u.CostUSD / p.Limits.MaxUSDPerDay
		case p.Limits.MaxRunsPerDay > 0:
			return float64(u.Runs) / float64(p.Limits.MaxRunsPerDay)
		default:
			// An uncapped pledge has no ratio to speak of; rank it by raw
			// spend so it still rotates instead of absorbing everything.
			return u.CostUSD
		}
	}
	ranked := make([]Pledge, len(eligible))
	copy(ranked, eligible)
	sort.SliceStable(ranked, func(i, j int) bool {
		si, sj := score(ranked[i]), score(ranked[j])
		if si != sj {
			return si < sj
		}
		li, lj := ranked[i].LastServedAt, ranked[j].LastServedAt
		switch {
		case li == nil && lj == nil:
			return ranked[i].ID < ranked[j].ID
		case li == nil:
			return true
		case lj == nil:
			return false
		default:
			return li.Before(*lj)
		}
	})
	return ranked, nil
}

// tryPledge attempts one donor: concurrency + ledger admission, then
// unsealing the credential and recording the lease. Returns (nil, nil) when
// the donor declined for a quota reason — the caller moves on.
func (b *Broker) tryPledge(ctx context.Context, pool Pool, p Pledge, req Request, now time.Time) (*Grant, error) {
	// What this donor already has at stake: the runs they are serving and
	// the allowance promised to them. Both bound this admission — without
	// the promised half, ten runs launched together would each be handed
	// the same "remaining" and could spend it ten times over.
	//
	// The asking run's own lease is excluded: a resume re-acquires while
	// still holding one, and counting it would let a run be refused by
	// itself.
	liveRuns, committed, err := b.leases.LiveCommitment(ctx, p.ID, req.RunID, now)
	if err != nil {
		return nil, err
	}
	live := LiveCommitment{Runs: liveRuns, CommittedUSD: committed}

	// A run this donor already admitted (a resume) renews its allowance
	// instead of consuming a second unit of "runs per day": it is the same
	// run, and charging it again would let one flaky run that resumes a few
	// times eat a contributor's whole day. An ABANDONED attempt does not
	// count as admitted — nothing ever learned what it spent, so renewing
	// against it forever would let a crash-looping run draw unmetered.
	readmitting, err := b.leases.HasServedAttempt(ctx, req.RunID, p.ID)
	if err != nil {
		return nil, err
	}

	var remaining float64
	var deny DenyReason
	if readmitting { //nolint:staticcheck // reads better than an inverted condition
		remaining, deny, err = b.ledger.Renew(ctx, p.ID, now, p.Limits, live)
	} else {
		remaining, deny, err = b.ledger.Reserve(ctx, p.ID, now, p.Limits, live)
	}
	if err != nil {
		return nil, err
	}
	if deny != DenyNone {
		b.logger.Debug("credpool: pledge %s declined run %s (%s)", p.ID, req.RunID, deny)
		return nil, nil
	}

	// From here on, any failure must give back whatever was consumed.
	// Renew consumed nothing, so it has nothing to return.
	release := func() {
		if !readmitting {
			b.releaseReservation(ctx, p.ID, now)
		}
	}

	rec, err := b.oauth.Get(ctx, p.UserID, secrets.OAuthKind(p.Kind))
	if err != nil {
		release()
		// The donor pledged a credential they since disconnected. Park the
		// pledge rather than re-discovering this on every launch.
		if errors.Is(err, secrets.ErrOAuthNotFound) {
			b.markUnhealthy(ctx, p, HealthTokenExpired, "the connected subscription was disconnected — reconnect it to resume sharing")
			return nil, nil
		}
		return nil, err
	}
	// An expired token with no way to renew is dead: the refresh worker
	// skips it, so it will never come back on its own.
	if rec.NotRefreshable && rec.AccessTokenExpiresAt != nil && !now.Before(*rec.AccessTokenExpiresAt) {
		release()
		b.markUnhealthy(ctx, p, HealthTokenExpired, "the connected subscription expired and carries no refresh token — reconnect it to resume sharing")
		return nil, nil
	}
	payload, err := secrets.OpenOAuthPayload(b.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
	if err != nil {
		release()
		return nil, fmt.Errorf("credpool: unseal donated credential: %w", err)
	}

	// Supersede whatever was still serving this run. A pod that died
	// without reporting leaves its lease open; leaving it so would give the
	// run two open leases, and then "who is serving this run" — hence who
	// gets charged — is decided by whichever the store happens to return.
	b.supersedeOpenLeases(ctx, req.RunID, now)

	lease := Lease{
		ID:              uuid.NewString(),
		RunID:           req.RunID,
		PledgeID:        p.ID,
		PoolID:          pool.ID,
		DonorID:         p.UserID,
		Kind:            p.Kind,
		TenantID:        req.TenantID,
		RequesterID:     req.UserID,
		BotID:           req.BotID,
		GrantedCostUSD:  remaining,
		ConsumedRunUnit: !readmitting,
		AcquiredAt:      now,
		ExpiresAt:       now.Add(b.leaseTTL),
	}
	if err := b.leases.Put(ctx, lease); err != nil {
		release()
		return nil, fmt.Errorf("credpool: record lease: %w", err)
	}

	// Best-effort fairness bookkeeping; a miss only costs tie-break
	// precision on the next selection.
	if err := b.pledges.TouchLastServed(ctx, p.ID, now); err != nil {
		b.logger.Warn("credpool: could not stamp last_served_at on pledge %s: %v", p.ID, err)
	}

	b.logger.Info("credpool: run %s served by donor %s (kind=%s, allowance=$%.2f)", req.RunID, p.UserID, p.Kind, remaining)
	return &Grant{
		PledgeID:     p.ID,
		DonorID:      p.UserID,
		Kind:         p.Kind,
		Payload:      payload,
		RemainingUSD: remaining,
	}, nil
}

// supersedeOpenLeases closes any lease still marked as serving this run.
// Their donors get their slot and committed allowance back; the run unit
// stays consumed, because those attempts did run. Best-effort: a miss only
// costs a slot until the lease TTL.
func (b *Broker) supersedeOpenLeases(ctx context.Context, runID string, now time.Time) {
	open, err := b.leases.ListOpenByRun(ctx, runID)
	if err != nil {
		b.logger.Warn("credpool: could not check the open leases of run %s: %v", runID, err)
		return
	}
	for _, l := range open {
		if _, cerr := b.leases.Close(ctx, l.ID, l.CostUSD, OutcomeSuperseded, now); cerr != nil {
			b.logger.Warn("credpool: could not supersede lease %s of run %s: %v", l.ID, runID, cerr)
		}
	}
}

// releaseReservation gives back one admitted-but-unused run unit.
func (b *Broker) releaseReservation(ctx context.Context, pledgeID string, when time.Time) {
	if rerr := b.ledger.ReleaseRun(context.WithoutCancel(ctx), pledgeID, when); rerr != nil {
		b.logger.Warn("credpool: could not release the reserved unit of pledge %s: %v (its daily run count over-reports by one)", pledgeID, rerr)
	}
}

// Release undoes an acquisition whose run never started — a launch that
// failed after the credential was granted (the run document could not be
// saved, the queue publish failed).
//
// Without it the donor keeps paying for a run that never existed: the lease
// holds a concurrency slot and its promised allowance until the TTL, and
// the daily run unit is consumed for good, because nothing else ever
// revisits it. A donor could be locked out for the day by a Mongo blip.
//
// Idempotent and best-effort: it is called from error paths that must
// surface their OWN error, not this one.
func (b *Broker) Release(ctx context.Context, runID string) {
	if b == nil || runID == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	lease, err := b.leases.GetOpenByRun(ctx, runID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			b.logger.Warn("credpool: could not look up the lease of abandoned run %s: %v", runID, err)
		}
		return
	}
	won, err := b.leases.Close(ctx, lease.ID, 0, OutcomeNotLaunched, b.now())
	if err != nil {
		b.logger.Warn("credpool: could not release the lease of abandoned run %s: %v", runID, err)
		return
	}
	if !won {
		return
	}
	// Only an admission that TOOK a run unit gives one back. A resume
	// renewed rather than consumed, so refunding it would mint quota out of
	// a failed launch and let the donor be drawn on past their own ceiling.
	if lease.ConsumedRunUnit {
		b.releaseReservation(ctx, lease.PledgeID, lease.AcquiredAt)
	}
	b.logger.Info("credpool: run %s never launched — returned donor %s's admission", runID, lease.DonorID)
}

// Outcome is what a finished attempt reports back to the pool.
//
// Condition is pre-classified by the caller rather than derived here: the
// backend error types live in pkg/backend/delegate, and the runner already
// classifies them for its usage-window retry. Keeping that knowledge at the
// boundary leaves this package a pure domain — stores, limits, fairness —
// with no dependency on the execution stack.
type Outcome struct {
	CostUSD      float64
	InputTokens  int64
	OutputTokens int64
	Condition    Condition
	// CooldownUntil is the provider's own reset instant, when the caller
	// could parse one from ConditionUsageWindow. Zero falls back to a
	// bounded blind wait.
	CooldownUntil time.Time
}

// Condition is the part of an attempt's outcome that changes a donor's
// availability.
type Condition string

const (
	// ConditionOK — the credential worked; nothing to do but charge it.
	ConditionOK Condition = ""
	// ConditionUsageWindow — the provider's subscription quota window is
	// exhausted. Waiting is the only cure, so the donor rests until reset.
	ConditionUsageWindow Condition = "usage_window"
	// ConditionAuthFailed — the provider rejected the credential.
	ConditionAuthFailed Condition = "auth_failed"
)

// Report closes a run's lease: it charges the donor's ledger, frees the
// concurrency slot, and applies whatever the outcome says about the
// donor's availability. Idempotent — a redelivered report finds the lease
// already closed and does nothing.
//
// Never returns an error for "this run had no lease": most runs don't.
func (b *Broker) Report(ctx context.Context, runID string, out Outcome) error {
	if b == nil || runID == "" {
		return nil
	}
	lease, err := b.leases.GetOpenByRun(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	now := b.now()

	outcome := "ok"
	switch out.Condition {
	case ConditionUsageWindow:
		outcome = string(ConditionUsageWindow)
	case ConditionAuthFailed:
		outcome = string(ConditionAuthFailed)
	}

	// Close FIRST, and charge only if this call is the one that closed it.
	// A run can be reported twice — a redelivery whose first pod was merely
	// slow, a cancel racing a finish — and reading "still open" then
	// charging would debit the donor once per report. The CAS is the
	// arbiter: exactly one caller wins it, exactly one charge lands.
	won, err := b.leases.Close(ctx, lease.ID, out.CostUSD, outcome, now)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return fmt.Errorf("credpool: close lease: %w", err)
	}
	if !won {
		return nil // another report got there first; it did the charging
	}

	if out.CostUSD > 0 || out.InputTokens > 0 || out.OutputTokens > 0 {
		// Charged against the lease's ACQUISITION instant, not now: a run
		// that starts at 23:50 and ends at 00:10 must debit the day whose
		// allowance admitted it, or the donor's daily cap silently leaks
		// across the boundary.
		if err := b.ledger.AddSpend(ctx, lease.PledgeID, lease.AcquiredAt, out.CostUSD, out.InputTokens, out.OutputTokens); err != nil {
			// The lease is already closed, so this spend will never be
			// retried — say so loudly rather than under-reporting a
			// donation in silence.
			b.logger.Error("credpool: run %s spent $%.4f on donor %s but the charge failed: %v (their ledger under-reports it)", runID, out.CostUSD, lease.DonorID, err)
			return fmt.Errorf("credpool: charge donor: %w", err)
		}
	}

	switch out.Condition {
	case ConditionUsageWindow:
		b.applyCooldown(ctx, lease.PledgeID, out.CooldownUntil, now)
	case ConditionAuthFailed:
		b.noteAuthFailure(ctx, lease.PledgeID)
	default:
		b.clearAuthFailures(ctx, lease.PledgeID)
	}
	return nil
}

// ReleaseExpired closes leases whose run never reported. Returns how many
// were freed. Without it a pod killed mid-run would hold a donor's
// concurrency slot until the lease document is evicted.
func (b *Broker) ReleaseExpired(ctx context.Context, limit int) (int, error) {
	if b == nil {
		return 0, nil
	}
	now := b.now()
	expired, err := b.leases.ListExpired(ctx, now, limit)
	if err != nil {
		return 0, err
	}
	freed := 0
	for _, l := range expired {
		// No spend is charged: the run never told us what it consumed, and
		// inventing a number would misreport the donor's contribution in
		// the direction that costs them. The run WAS served, so its unit of
		// the daily run quota stays consumed — unlike Release, which undoes
		// an admission whose run never started.
		won, err := b.leases.Close(ctx, l.ID, 0, OutcomeAbandoned, now)
		if err != nil {
			b.logger.Warn("credpool: could not close abandoned lease %s: %v", l.ID, err)
			continue
		}
		if !won {
			continue
		}
		freed++
		b.logger.Warn("credpool: lease for run %s expired without a report — freed donor %s's slot (spend unaccounted)", l.RunID, l.DonorID)
	}
	return freed, nil
}

// ---------------------------------------------------------------------------
// Donor availability transitions
// ---------------------------------------------------------------------------

// authFailureThreshold is how many consecutive rejected-credential
// outcomes park a pledge. Two, not one: a single blip (a provider hiccup,
// a token mid-refresh) must not evict a donor who did nothing wrong.
const authFailureThreshold = 2

// blindCooldown is the rest imposed when the provider says a window is
// exhausted but not when it reopens. Bounded and short-ish: one wasted
// selection an hour beats both guessing a week and re-hitting the same
// wall on every launch. Mirrors the runner's usageWindowBlindWait.
const blindCooldown = time.Hour

// applyCooldown holds the donor out until the provider's window resets.
func (b *Broker) applyCooldown(ctx context.Context, pledgeID string, until, now time.Time) {
	if !until.IsZero() {
		// Come back just after the reset, not exactly on it.
		until = until.Add(time.Minute)
	}
	// Also covers a reset instant that parses into the past — the provider's
	// notice may name a zone the parser read as UTC, and a cooldown already
	// elapsed is no cooldown at all.
	if until.IsZero() || !until.After(now) {
		until = now.Add(blindCooldown)
	}
	p, gerr := b.pledges.Get(ctx, pledgeID)
	if gerr != nil {
		b.logger.Warn("credpool: cannot apply cooldown to pledge %s: %v", pledgeID, gerr)
		return
	}
	p.CooldownUntil = &until
	if uerr := b.pledges.Upsert(ctx, p); uerr != nil {
		b.logger.Warn("credpool: cannot persist cooldown for pledge %s: %v", pledgeID, uerr)
		return
	}
	b.logger.Info("credpool: donor %s hit their provider quota window — resting until %s", p.UserID, until.Format(time.RFC3339))
}

// noteAuthFailure counts a rejected credential and parks the pledge once
// the failures are no longer plausibly transient.
func (b *Broker) noteAuthFailure(ctx context.Context, pledgeID string) {
	p, err := b.pledges.Get(ctx, pledgeID)
	if err != nil {
		return
	}
	p.ConsecutiveAuthFailures++
	if p.ConsecutiveAuthFailures >= authFailureThreshold {
		p.Health = HealthAuthFailed
		p.HealthDetail = "the provider rejected this subscription on consecutive runs — reconnect it to resume sharing"
		b.logger.Warn("credpool: donor %s's credential was rejected %d times — held out of the pool", p.UserID, p.ConsecutiveAuthFailures)
	}
	if err := b.pledges.Upsert(ctx, p); err != nil {
		b.logger.Warn("credpool: cannot persist auth failure for pledge %s: %v", pledgeID, err)
	}
}

// clearAuthFailures resets the counter after a run that authenticated
// fine, so unrelated failures spread over weeks never accumulate into an
// eviction.
func (b *Broker) clearAuthFailures(ctx context.Context, pledgeID string) {
	p, err := b.pledges.Get(ctx, pledgeID)
	if err != nil || p.ConsecutiveAuthFailures == 0 {
		return
	}
	p.ConsecutiveAuthFailures = 0
	if err := b.pledges.Upsert(ctx, p); err != nil {
		b.logger.Warn("credpool: cannot reset auth failures for pledge %s: %v", pledgeID, err)
	}
}

func (b *Broker) markUnhealthy(ctx context.Context, p Pledge, h Health, detail string) {
	p.Health = h
	p.HealthDetail = detail
	if err := b.pledges.Upsert(ctx, p); err != nil {
		b.logger.Warn("credpool: cannot mark pledge %s as %s: %v", p.ID, h, err)
	}
}
