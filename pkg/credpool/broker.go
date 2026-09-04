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
//
// Concrete abstentions arrive as *NoDonorError, which unwraps to this
// sentinel so a caller using errors.Is(err, ErrNoDonor) keeps working;
// use errors.As to extract the reason for a Warn log.
var ErrNoDonor = errors.New("credpool: no eligible donor")

// NoDonorReason names WHY the pool abstained on this request. The four
// values match the four return sites in Acquire/resolvePools, so a caller
// can distinguish "the deployment has no pool at all" from "every donor
// declined this request" without parsing prose.
type NoDonorReason string

const (
	// ReasonPoolDisabled: nil *Broker — the deployment never wired a pool,
	// or a store required to serve one is missing.
	ReasonPoolDisabled NoDonorReason = "pool_disabled"
	// ReasonNoEnabledPool: pools.ListEnabled returned nothing — an operator
	// created no pool, or every pool is off (kill switch).
	ReasonNoEnabledPool NoDonorReason = "no_enabled_pool"
	// ReasonAudienceRejected: some pool(s) exist, but none opened its
	// audience to this requester (wrong org / no reciprocity / not on the
	// team allowlist).
	ReasonAudienceRejected NoDonorReason = "audience_rejected"
	// ReasonNoEligiblePledge: pools admitted the request, candidate pledges
	// were walked, but every one declined (parked, out of its sharing
	// window, ceiling reached, bot allow-list, concurrency, ledger refusal,
	// donor is the requester, credential vanished).
	ReasonNoEligiblePledge NoDonorReason = "no_eligible_pledge"
)

// PledgeSkip records why one candidate pledge did not serve the
// request. The per-pledge Status the ticket asks for — pause, unhealthy,
// out_of_hours, bot_filtered, cooling, no_credential — collapsed to
// `no_eligible_pledge` before round 1 G5.
type PledgeSkip struct {
	PledgeID string
	Status   Status
}

// NoDonorError is a typed ErrNoDonor: it carries the reason for the
// abstention plus the exact counts the walk saw and the per-pledge
// skips, so an operator investigating a run that landed on the platform
// tier can tell whether the pool was mute because no pool exists,
// because the audience refused them, or because every donor was cooling
// down — and specifically which donor was in which state.
//
// PoolsEnabled counts every pool `pools.ListEnabled` returned;
// PoolsAdmitted counts the subset whose audience opened to this
// request. On `audience_rejected` PoolsAdmitted is 0 by construction;
// on `no_eligible_pledge` PoolsAdmitted > 0 and PoolsEnabled ≥
// PoolsAdmitted. PledgesConsidered counts pledges the walk actually
// looked at (source+ref matches a wanted credential) — a pool full of
// kinds the run cannot spend no longer inflates the count (round 1 G6).
//
// Unwrap returns ErrNoDonor so callers using errors.Is keep working;
// use errors.As(&NoDonorError{}) to read Reason / Skips.
type NoDonorError struct {
	Reason NoDonorReason
	// PoolsEnabled is every enabled pool the walk saw.
	PoolsEnabled int
	// PoolsAdmitted is the subset whose audience admitted this request.
	PoolsAdmitted int
	// PledgesConsidered counts pledges of a wanted (Source, Ref) —
	// pledges of a kind the run cannot spend are never counted.
	PledgesConsidered int
	// Skips names every pledge the walk considered and the reason it
	// declined. Empty at reasons other than `no_eligible_pledge`.
	Skips []PledgeSkip
}

// Error renders the sentinel prose followed by the reason and counts.
// Kept short — the caller usually logs it inside a longer sentence.
func (e *NoDonorError) Error() string {
	if e == nil {
		return ErrNoDonor.Error()
	}
	return fmt.Sprintf("%s (reason=%s pools_enabled=%d pools_admitted=%d pledges_considered=%d skips=%d)",
		ErrNoDonor.Error(), e.Reason, e.PoolsEnabled, e.PoolsAdmitted, e.PledgesConsidered, len(e.Skips))
}

// Unwrap keeps `errors.Is(err, ErrNoDonor)` true for every abstention.
func (e *NoDonorError) Unwrap() error { return ErrNoDonor }

// noDonor builds a typed abstention. Small helper so a new site doesn't
// forget to wrap the sentinel (which would leave `errors.As` failing
// silently at the caller — the same class of defect #654 fixes).
func noDonor(reason NoDonorReason, poolsEnabled, poolsAdmitted, pledgesConsidered int, skips []PledgeSkip) error {
	return &NoDonorError{
		Reason:            reason,
		PoolsEnabled:      poolsEnabled,
		PoolsAdmitted:     poolsAdmitted,
		PledgesConsidered: pledgesConsidered,
		Skips:             skips,
	}
}

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
	apiKeys secrets.ApiKeyStore
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
	OAuth secrets.OAuthStore
	// APIKeys resolves a lent BYOK key. Nil simply means the deployment
	// only pools subscriptions.
	APIKeys secrets.ApiKeyStore
	Sealer  secrets.Sealer
	Logger  *iterlog.Logger
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
		oauth: cfg.OAuth, apiKeys: cfg.APIKeys, sealer: cfg.Sealer, logger: cfg.Logger,
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
	// Wants lists the credentials that would serve this run, in preference
	// order. A run asks once for all of them: they share the same pool,
	// audience and candidate list, so re-resolving per entry would be the
	// same three queries again for a different in-memory filter.
	//
	// Order is the caller's policy, not the pool's — typically the
	// subscription paths first, so a metered key is only spent when no
	// already-paid-for plan can serve.
	Wants []Credential
}

// Grant is a served donation.
type Grant struct {
	PledgeID string
	DonorID  string
	// Credential says WHAT was lent, so the caller knows which slot of the
	// run bundle it belongs in — and whether the money is metered.
	Credential
	// Payload is the verbatim credentials blob, unsealed. The caller seals
	// it into the run bundle exactly like a personal or org forfait — it
	// must never be logged, persisted in the clear, or returned over an
	// API.
	Payload []byte
	// Fingerprint is the donor credential's stable audit identity — not
	// the payload's, which token refresh rewrites every few hours for the
	// same subscription. It is what the borrower's usage-cap meter keys
	// on, so a donor who reconnects a FRESH subscription opens a fresh
	// meter instead of inheriting the exhausted readings of the one it
	// replaced. Empty for a lent API key (the runner hashes the plaintext
	// it holds) and for donor records that predate stamping.
	Fingerprint string
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
		return nil, noDonor(ReasonPoolDisabled, 0, 0, 0, nil)
	}
	if req.RunID == "" {
		return nil, fmt.Errorf("credpool: acquire without a run id")
	}
	if len(req.Wants) == 0 {
		return nil, fmt.Errorf("credpool: acquire without a credential to look for")
	}
	for _, w := range req.Wants {
		if !w.Source.Valid() || w.Ref == "" {
			return nil, fmt.Errorf("credpool: malformed wanted credential %q", w)
		}
	}
	now := b.now()

	// Close whatever was still marked as serving this run BEFORE judging the
	// admission. A pod killed without reporting leaves its lease open, and an
	// open lease with no outcome reads as "this pledge already admitted this
	// run" — which would renew the attempt for free, consuming no unit of the
	// donor's daily runs and re-granting their whole remaining allowance,
	// against a ledger that learned nothing about what the killed attempt
	// spent. Closing first turns it into the superseded non-admission it is,
	// so the retry is admitted as new.
	b.supersedeOpenLeases(ctx, req.RunID, now)

	poolsEnabled, allowed, err := b.resolvePools(ctx, req)
	if err != nil {
		return nil, err
	}

	// Want-major: the preference between credentials is the caller's
	// (subscriptions before metered keys), and outranks which pool holds
	// them. Within a want, the requester's own org pool was sorted first.
	//
	// A pledge is counted only when it matches one of the requested
	// wants AND could ever serve this requester — a pool full of kinds
	// the run cannot spend never inflates the number an operator reads
	// (round 1 G6), and neither does the requester's own pledge, which
	// the walk drops without a skip (a donor never serves their own
	// run). Counting it left the line reading "one donor considered,
	// none declined", which is the silence #654 exists to end. Skips
	// accumulate every
	// pledge that DID match and yet declined, with its per-pledge
	// status (round 1 G5): "no_eligible_pledge" now discloses which
	// donor was paused, unhealthy, out-of-hours, bot-filtered, cooling.
	pledgesConsidered := 0
	seenPledge := map[string]bool{}
	for _, pc := range allowed {
		for _, p := range pc.candidates {
			if !seenPledge[p.ID] && p.UserID != req.UserID && wantMatchesPledge(req.Wants, p) {
				seenPledge[p.ID] = true
				pledgesConsidered++
			}
		}
	}
	var allSkips []PledgeSkip
	seenSkip := map[string]bool{}
	for _, want := range req.Wants {
		for _, pc := range allowed {
			grant, skips, err := b.acquireKind(ctx, pc.pool, pc.candidates, req, want, now)
			if err != nil {
				return nil, err
			}
			for _, s := range skips {
				if !seenSkip[s.PledgeID] {
					seenSkip[s.PledgeID] = true
					allSkips = append(allSkips, s)
				}
			}
			if grant != nil {
				return grant, nil
			}
		}
	}
	return nil, noDonor(ReasonNoEligiblePledge, poolsEnabled, len(allowed), pledgesConsidered, allSkips)
}

// wantMatchesPledge reports whether the pledge is of a kind any of the
// requested wants would take — the eligibility filter for the "how many
// pledges did the walk actually consider?" count.
func wantMatchesPledge(wants []Credential, p Pledge) bool {
	for _, w := range wants {
		if p.Source == w.Source && p.Ref == w.Ref {
			return true
		}
	}
	return false
}

// acquireKind tries one credential kind against an already-resolved
// candidate list. Returns (nil, skips, nil) when no donor of that kind
// can serve — the caller falls through to the next kind. `skips` names
// every candidate the walk did NOT admit and the per-pledge status the
// caller can carry on NoDonorError.
func (b *Broker) acquireKind(ctx context.Context, pool Pool, candidates []Pledge, req Request, want Credential, now time.Time) (*Grant, []PledgeSkip, error) {
	eligible := make([]Pledge, 0, len(candidates))
	var skips []PledgeSkip
	for _, p := range candidates {
		if p.Source != want.Source || p.Ref != want.Ref {
			continue
		}
		// A donor never serves their own run: they would be lending to
		// themselves, consuming pool bookkeeping for nothing and skewing
		// the fairness ranking against the other donors. Not a "skip"
		// worth reporting: no operator investigation ever asks that.
		if p.UserID == req.UserID {
			continue
		}
		if ok, status := p.AvailableForLaunch(now, req.BotID); ok {
			eligible = append(eligible, p)
		} else {
			skips = append(skips, PledgeSkip{PledgeID: p.ID, Status: status})
		}
	}
	if len(eligible) == 0 {
		return nil, skips, nil
	}

	ranked, err := b.rank(ctx, eligible, now)
	if err != nil {
		return nil, skips, err
	}

	// Walk the ranking: a donor whose ledger refuses (raced to their cap,
	// concurrency full) or whose credential is gone simply yields to the
	// next — and lands on the skips list under the status tryPledge
	// decided, so the operator can tell "exhausted" from "serving" from
	// "unhealthy" from "paused".
	for _, p := range ranked {
		grant, status, err := b.tryPledge(ctx, pool, p, req, now)
		if err != nil {
			b.logger.Warn("credpool: pledge %s unusable for run %s: %v", p.ID, req.RunID, err)
			skips = append(skips, PledgeSkip{PledgeID: p.ID, Status: StatusUnhealthy})
			continue
		}
		if grant != nil {
			return grant, skips, nil
		}
		skips = append(skips, PledgeSkip{PledgeID: p.ID, Status: status})
	}
	return nil, skips, nil
}

// skipStatus names why a ledger decline held a pledge out, for the skips
// an abstention reports. A full slot set is "serving" by definition. A
// SPEND ceiling met while the donor has runs in flight also reads
// "serving": part of what tripped it is allowance PROMISED to those runs,
// which clears as they end — the same distinction the pledge view draws
// for the donor, approximated here without the second ledger read that
// view pays (an operator reading a log line, not a contributor reading
// their own page). With nothing in flight the ceiling was really spent.
//
// A runs-per-day ceiling is NOT that shape: the count is consumed at
// admission and never given back, so it reads "exhausted" whether or not
// something is in flight — the same cause must not name itself two ways
// depending on when the operator looked.
func (d DenyReason) skipStatus(live LiveCommitment) Status {
	if d == DenyRunsPerDay {
		return StatusExhausted
	}
	if d == DenyConcurrency || live.Runs > 0 {
		return StatusServing
	}
	return StatusExhausted
}

// poolCandidates is one pool this requester may draw on, with its pledges.
type poolCandidates struct {
	pool       Pool
	candidates []Pledge
}

// resolvePools finds EVERY pool whose audience admits this request, own-org
// first, each with its pledges.
//
// Every one, not the first match: an org that runs its own pool must still
// reach a community pool that opened itself to it once its own donors are
// all cooling, exhausted or out-of-hours — otherwise having a pool of your
// own silently excludes you from everyone else's.
//
// Selection scans the ENABLED pools and applies each one's audience,
// rather than looking up the requester's own org: a pool that opens itself
// to other teams/orgs — or to contributors wherever they launch from — must
// be reachable FROM those places, otherwise every audience dial beyond the
// owning org is unreachable configuration. A deployment runs a handful of
// pools, so this is a small list, and the requester's own org pool wins
// when several would serve.
func (b *Broker) resolvePools(ctx context.Context, req Request) (poolsEnabled int, allowed []poolCandidates, err error) {
	pools, err := b.pools.ListEnabled(ctx)
	if err != nil {
		return 0, nil, err
	}
	poolsEnabled = len(pools)
	if poolsEnabled == 0 {
		return 0, nil, noDonor(ReasonNoEnabledPool, 0, 0, 0, nil)
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
	allowed = make([]poolCandidates, 0, len(pools))
	for _, pool := range pools {
		candidates, lerr := b.pledges.ListByPool(ctx, pool.ID)
		if lerr != nil {
			return 0, nil, lerr
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
			allowed = append(allowed, poolCandidates{pool: pool, candidates: candidates})
		}
	}
	if len(allowed) == 0 {
		return poolsEnabled, nil, noDonor(ReasonAudienceRejected, poolsEnabled, 0, 0, nil)
	}
	return poolsEnabled, allowed, nil
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
// tryPledge admits one eligible pledge or explains why not: a nil grant
// with a Status names the decline (exhausted, serving, unhealthy) for the
// abstention's skips; an error is a store failure the caller reports.
func (b *Broker) tryPledge(ctx context.Context, pool Pool, p Pledge, req Request, now time.Time) (*Grant, Status, error) {
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
		return nil, "", err
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
		return nil, "", err
	}

	var remaining float64
	var deny DenyReason
	if readmitting { //nolint:staticcheck // reads better than an inverted condition
		remaining, deny, err = b.ledger.Renew(ctx, p.ID, now, p.Limits, live)
	} else {
		remaining, deny, err = b.ledger.Reserve(ctx, p.ID, now, p.Limits, live)
	}
	if err != nil {
		return nil, "", err
	}
	if deny != DenyNone {
		b.logger.Debug("credpool: pledge %s declined run %s (%s)", p.ID, req.RunID, deny)
		return nil, deny.skipStatus(live), nil
	}

	// From here on, any failure must give back whatever was consumed.
	// Renew consumed nothing, so it has nothing to return.
	release := func() {
		if !readmitting {
			b.releaseReservation(ctx, p.ID, now)
		}
	}

	payload, fingerprint, gone, err := b.openCredential(ctx, p, now)
	if err != nil {
		release()
		return nil, "", err
	}
	if gone != "" {
		release()
		// The donor pledged a credential that is no longer usable. Park the
		// pledge rather than re-discovering this on every launch.
		b.markUnhealthy(ctx, p.ID, HealthTokenExpired, gone)
		return nil, StatusUnhealthy, nil
	}

	lease := Lease{
		ID:              uuid.NewString(),
		RunID:           req.RunID,
		PledgeID:        p.ID,
		PoolID:          pool.ID,
		DonorID:         p.UserID,
		TenantID:        req.TenantID,
		RequesterID:     req.UserID,
		BotID:           req.BotID,
		Credential:      p.Credential,
		GrantedCostUSD:  remaining,
		ConsumedRunUnit: !readmitting,
		AcquiredAt:      now,
		ExpiresAt:       now.Add(b.leaseTTL),
	}
	if err := b.leases.Put(ctx, lease); err != nil {
		release()
		return nil, "", fmt.Errorf("credpool: record lease: %w", err)
	}

	// Best-effort fairness bookkeeping; a miss only costs tie-break
	// precision on the next selection.
	if err := b.pledges.TouchLastServed(ctx, p.ID, now); err != nil {
		b.logger.Warn("credpool: could not stamp last_served_at on pledge %s: %v", p.ID, err)
	}

	b.logger.Info("credpool: run %s served by donor %s (%s, allowance=$%.2f%s)",
		req.RunID, p.UserID, p.Credential, remaining, meteredNote(p.Source))
	return &Grant{
		PledgeID:     p.ID,
		DonorID:      p.UserID,
		Credential:   p.Credential,
		Payload:      payload,
		Fingerprint:  fingerprint,
		RemainingUSD: remaining,
	}, "", nil
}

// meteredNote flags, in the launch log, the case where the allowance is
// real money rather than a slice of an already-paid-for plan.
func meteredNote(src CredentialSource) string {
	if src.Metered() {
		return ", metered"
	}
	return ""
}

// openCredential unseals whatever the pledge lends. It returns a non-empty
// `gone` reason — rather than an error — when the credential has vanished
// or died in a way only the donor can fix, so the caller parks the pledge
// instead of rediscovering it on every launch.
//
// fingerprint is the donor record's stable audit identity, which the
// borrower's usage-cap meter keys on so a donor who reconnects a fresh
// subscription is not metered against the one it replaced. Only the OAuth
// case carries one: an API key IS its own identity, so the runner hashes
// the plaintext it already holds rather than being told.
func (b *Broker) openCredential(ctx context.Context, p Pledge, now time.Time) (payload []byte, fingerprint, gone string, err error) {
	switch p.Source {
	case SourceOAuth:
		rec, gerr := b.oauth.Get(ctx, p.UserID, secrets.OAuthKind(p.Ref))
		if gerr != nil {
			if errors.Is(gerr, secrets.ErrOAuthNotFound) {
				return nil, "", "the connected subscription was disconnected — reconnect it to resume sharing", nil
			}
			return nil, "", "", gerr
		}
		// An expired token with no way to renew is dead: the refresh worker
		// skips it, so it will never come back on its own.
		if rec.NotRefreshable && rec.AccessTokenExpiresAt != nil && !now.Before(*rec.AccessTokenExpiresAt) {
			return nil, "", "the connected subscription expired and carries no refresh token — reconnect it to resume sharing", nil
		}
		pt, oerr := secrets.OpenOAuthPayload(b.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
		if oerr != nil {
			return nil, "", "", fmt.Errorf("credpool: unseal donated subscription: %w", oerr)
		}
		return pt, rec.Fingerprint, "", nil

	case SourceAPIKey:
		if b.apiKeys == nil {
			return nil, "", "", fmt.Errorf("credpool: no api-key store wired; cannot serve %s", p.Credential)
		}
		// GetOwned, not Get: this read runs on the BORROWER's context, in
		// another tenant than the donor's, and the tenant-scoped Get would
		// find nothing — parking an innocent donor's pledge with "the lent
		// API key was deleted" on every cross-team draw, which is every
		// draw a pool exists for.
		k, gerr := b.apiKeys.GetOwned(ctx, p.KeyID, p.UserID)
		if gerr != nil {
			if errors.Is(gerr, secrets.ErrApiKeyNotFound) {
				return nil, "", "the lent API key was deleted — pledge another to resume sharing", nil
			}
			return nil, "", "", gerr
		}
		// A donor lends THEIR OWN key. A team-wide key is the team's to
		// spend, not one member's to hand to the pool, and a pledge must
		// never become a way to re-scope somebody else's credential.
		if k.ScopeUserID == "" || k.ScopeUserID != p.UserID {
			return nil, "", "that API key is not yours to lend — pledge a personal key instead", nil
		}
		if k.Provider != secrets.Provider(p.Ref) {
			return nil, "", "the lent API key no longer matches the pledged provider — pledge it again", nil
		}
		if k.ExpiresAt != nil && !now.Before(*k.ExpiresAt) {
			return nil, "", "the lent API key has expired — pledge a current one to resume sharing", nil
		}
		pt, oerr := secrets.OpenApiKey(b.sealer, k)
		if oerr != nil {
			return nil, "", "", fmt.Errorf("credpool: unseal donated api key: %w", oerr)
		}
		return pt, "", "", nil
	}
	return nil, "", "", fmt.Errorf("credpool: unknown credential source %q", p.Source)
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
		// Zero, not l.CostUSD: Close ADDS, and whatever this lease already
		// carries was recorded when it was charged.
		if _, cerr := b.leases.Close(ctx, l.ID, 0, OutcomeSuperseded, now); cerr != nil {
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

// ReleaseGuard pins the exact lease that was open before a caller performed
// an atomic state transition. A later attempt of the same run gets a new
// lease id, so releasing through this guard can never close that successor.
// Its fields stay private so callers cannot manufacture or retarget one.
type ReleaseGuard struct {
	lease Lease
}

// CaptureRelease snapshots the currently-open lease of runID. Callers that
// may release after making a run resumable must capture BEFORE that state
// transition; otherwise a concurrent resume could replace the open lease
// before the lookup. A nil guard means there was nothing safe to release.
func (b *Broker) CaptureRelease(ctx context.Context, runID string) *ReleaseGuard {
	if b == nil || runID == "" {
		return nil
	}
	lease, err := b.leases.GetOpenByRun(context.WithoutCancel(ctx), runID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			b.logger.Warn("credpool: could not capture the lease of run %s for release: %v", runID, err)
		}
		return nil
	}
	return &ReleaseGuard{lease: lease}
}

// ReleaseCaptured releases only the lease pinned by guard. If a new attempt
// has already superseded it, Close loses its CAS and the new open lease is
// left untouched.
func (b *Broker) ReleaseCaptured(ctx context.Context, guard *ReleaseGuard) {
	if b == nil || guard == nil {
		return
	}
	b.releaseLease(context.WithoutCancel(ctx), guard.lease)
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
	b.ReleaseCaptured(ctx, b.CaptureRelease(ctx, runID))
}

func (b *Broker) releaseLease(ctx context.Context, lease Lease) {
	won, err := b.leases.Close(ctx, lease.ID, 0, OutcomeNotLaunched, b.now())
	if err != nil {
		b.logger.Warn("credpool: could not release lease %s of abandoned run %s: %v", lease.ID, lease.RunID, err)
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
	b.logger.Info("credpool: run %s never launched — returned donor %s's admission", lease.RunID, lease.DonorID)
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
	// Interim marks an attempt that spent the credential but does NOT
	// settle the run: the queue will redeliver the same sealed bundle, so
	// the next attempt executes on the same lease. The spend is charged,
	// the lease stays open. Without it a flapping backend could run a
	// contributor's subscription once per delivery while their ledger
	// recorded one attempt — and their ceilings would never see the rest.
	Interim bool
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

	// An INTERIM report: this attempt spent the donor's credential but the
	// run keeps the same lease, because the queue will redeliver the very
	// same sealed bundle to another pod. Closing here would make every
	// redelivered attempt's spend invisible — no open lease left to report
	// against — so a run could burn a contributor's quota once per delivery
	// while their ledger recorded a single attempt. Charge now; the
	// attempt that finally settles the run closes.
	if out.Interim {
		// Onto the lease as well as the ledger: the lease IS the donor's
		// audit trail, and a redelivered run whose trail showed only the
		// last attempt would contradict the ceilings it was charged against.
		if err := b.leases.AddCost(ctx, lease.ID, out.CostUSD); err != nil {
			b.logger.Warn("credpool: could not record the interim spend of run %s on lease %s: %v", runID, lease.ID, err)
		}
		return b.chargeAndReact(ctx, lease, out, now, runID)
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

	return b.chargeAndReact(ctx, lease, out, now, runID)
}

// chargeAndReact debits the donor for what an attempt consumed and applies
// what that attempt says about their credential. Shared by the closing and
// interim report paths.
func (b *Broker) chargeAndReact(ctx context.Context, lease Lease, out Outcome, now time.Time, runID string) error {
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

// markUnhealthy parks a pledge whose credential no longer authenticates.
//
// It re-reads first, like its siblings above: the copy an acquisition holds
// was listed several round trips earlier, and Upsert INSERTS when absent —
// a blind write would resurrect a pledge the donor withdrew (or whose
// credential they disconnected) in between, which is precisely the sequence
// that lands here, since disconnecting deletes the pledge AND the OAuth
// record the caller just failed to read.
func (b *Broker) markUnhealthy(ctx context.Context, pledgeID string, h Health, detail string) {
	p, err := b.pledges.Get(ctx, pledgeID)
	if err != nil {
		return
	}
	p.Health = h
	p.HealthDetail = detail
	if err := b.pledges.Upsert(ctx, p); err != nil {
		b.logger.Warn("credpool: cannot mark pledge %s as %s: %v", pledgeID, h, err)
	}
}
