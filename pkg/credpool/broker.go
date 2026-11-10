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
	// ReopenAt is the reset instant a NEAR_LIMIT skip arms the durable
	// usage-window retry on (the skipTracker's reopen). Zero elsewhere.
	ReopenAt time.Time
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
	// FallbackOnly restricts the candidates to pledges whose donor marked
	// `fallback_use` — the routing fallback door (ADR-121 § The
	// per-family pool fallback door). The whole-bundle tier never sets
	// it: a run with no credential of its own is served by ANY active
	// pledge, consenting or not, exactly as before the door existed.
	FallbackOnly bool
	// PoolIDs restricts the walk to NAMED pools (ADR-164): the launch's
	// resolved attachment. The attachment IS the consent — the audience
	// union is short-circuited, private pools are reachable, and each
	// named pool still counts in PoolsAdmitted. Empty keeps the legacy
	// ListEnabled+Audience walk, byte for byte.
	PoolIDs []string
	// AttachmentID names the scope whose att: ledger row the DRAW caps
	// reserve against (empty = no attachment — no borrower-side caps).
	// Stamped on the lease so a release refunds the right row.
	AttachmentID string
	// DrawLimits are the attachment's borrower-side caps (the att: row's
	// Limits). Zero fields = uncapped axes.
	DrawLimits DrawLimits
	// RankMode picks the entry ordering: the attachment's strategy mode.
	// Zero behaves exactly as the legacy ranking (least_consumed).
	RankMode StrategyMode
	// SideBySide marks a per-slot acquisition of the ATTACHED tier
	// (ADR-164): the run's earlier slot leases stay open — they are this
	// acquisition's siblings, never a takeover. The supersede machinery
	// (a newer attempt replacing a dead one) is skipped entirely, and the
	// attempt's report charges EVERY open lease its fingerprint slice.
	SideBySide bool
	// ClosedAtMargin, when set, is consulted per candidate during
	// eligibility: a non-empty reopenAt marks the entry NEAR LIMIT (its
	// freshest usage reading sits at or past the strategy margin with a
	// future reset) and the next entry serves instead. Built by ONE
	// publisher constructor so live and preview cannot diverge; the pool
	// package never reads usagecap itself (dependency direction).
	ClosedAtMargin func(p Pledge) (reopenAt time.Time, near bool)
	// HoldOnSwitch is the attachment's reset_hold (ADR-164): the lease
	// this acquisition opens is marked Held when the strategy SWITCHED to
	// its entry — a better-ranked entry of the kind did not serve — or
	// when it re-serves PreferPledgeID.
	HoldOnSwitch bool
	// PreferPledgeID is the entry the run holds for this kind (its newest
	// Held lease): ranked FIRST when it is eligible, so a resume re-acquires
	// it instead of returning to an entry whose window reset meanwhile.
	// Ineligible, it yields to the ordinary ranking.
	PreferPledgeID string

	// AttemptID names the credential resolution this acquisition belongs
	// to (one per launch or resume resolution): the leases sharing it are
	// one attempt's side-by-side siblings, settled together by its report.
	AttemptID string

	// holdLease is the per-candidate verdict the ranking walk hands to
	// tryPledge: mark the lease it opens as Held.
	holdLease bool
}

// Grant is a served donation.
type Grant struct {
	PledgeID string
	DonorID  string
	// AcquiredAt is the instant the lease this grant opened was acquired:
	// what the attempt's publication must follow at millisecond precision
	// (store.PublishAt), so the attempt's spend report can always tell its
	// own lease from a successor's.
	AcquiredAt time.Time
	// Credential says WHAT was lent, so the caller knows which slot of the
	// run bundle it belongs in — and whether the money is metered.
	Credential
	// Payload is the verbatim credentials blob, unsealed. The caller seals
	// it into the run bundle exactly like a personal or org forfait — it
	// must never be logged, persisted in the clear, or returned over an
	// API.
	Payload []byte
	// Fingerprint is the donor credential's stable audit identity — for a
	// subscription, NOT the payload's, which token refresh rewrites every
	// few hours for the same account; for a key, the record's own stamp,
	// which is the hash of the plaintext. It is what the borrower's
	// usage-cap meter keys on, so a donor who reconnects a FRESH
	// subscription opens a fresh meter instead of inheriting the exhausted
	// readings of the one it replaced. Empty only for donor records that
	// predate stamping.
	Fingerprint string
	// RecordID is the id of the donor's OAuth record a lent subscription was
	// read from — the record the server's refresh worker rotates. The
	// borrower's runner follows it instead of exchanging the refresh token
	// itself: that exchange would revoke the token the donor's record, and
	// every run spending it, still holds. Empty for a lent API key.
	RecordID string
	// RecordConnectedAt is that record's connect time (OAuthRecord.CreatedAt),
	// which the refresh worker's rotations never rewrite: the borrower's
	// runner stops following the record only when the donor re-connects it
	// with another subscription.
	RecordConnectedAt time.Time
	// KeyMaxConcurrentRuns is the lent key's own HARD concurrency cap
	// (secrets.ApiKey.MaxConcurrentRuns) and KeyName its label, read off the
	// donor's record: the borrower's launch reserves a slot of the key on
	// the same ledger row as the donor's own launches, so the cap holds
	// whoever runs on the key. Zero and empty for an uncapped key or a
	// subscription. The pledge's Limits.MaxConcurrentRuns is the donor's
	// lending dial, enforced on the lease, on top.
	KeyMaxConcurrentRuns int
	KeyName              string
	// RemainingUSD is what was left of the donor's tightest spend cap.
	// Zero means the donor set no spend cap, NOT "nothing left" — callers
	// clamping a run budget must treat zero as "no ceiling from the pool".
	RemainingUSD float64
	// Backend and Model are the entry's route binding (Pledge.Backend /
	// Pledge.Model): what the caller composes the entry's fallback rung
	// from (ADR-164). Empty when the entry carries no explicit binding —
	// the entry funds whatever its slot natively serves.
	Backend string
	Model   string
	// BaseURL is the lent key's canonical gateway endpoint (ADR-131): it
	// travels with the key so the caller seals the PAIR through one
	// chokepoint, and the seal's AAD binds the URL the entry names. Empty
	// for a subscription and for a vendor key — non-empty is a promise the
	// seal keeps (a gateway grant carrying none is refused and released by
	// the publisher).
	BaseURL string
	// AttachmentID names the attachment scope this grant was acquired
	// under; the att: ledger row consumed the draw unit and the lease is
	// stamped with the same id. Empty = legacy (unattached) grant.
	AttachmentID string
	// AttachmentRemainingUSD is what was left of the attachment's draw cap
	// after this admission — the borrower-side ceiling the run budget must
	// also clamp to. Zero means the attachment set no spend cap, NOT
	// "nothing left" (same convention as RemainingUSD).
	AttachmentRemainingUSD float64
	// Held reports the lease was marked as the run's held entry
	// (reset_hold — Request.HoldOnSwitch).
	Held bool
	// Bound when the lease is inserted, never looked up by run ID later:
	// concurrent acquisitions can hold different leases for the same run.
	releaseGuard *ReleaseGuard
	// superseded is what this acquisition closed of the run's previous
	// attempt: what ReopenSuperseded puts back when the takeover did not
	// happen.
	superseded []supersededLease
}

// supersededLease is a lease an acquisition closed as superseded, and the
// instant of that close: what undoing it reopens (LeaseStore.Reopen).
type supersededLease struct {
	id    string
	runID string
	at    time.Time
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
	// so the retry is admitted as new. A run id another team holds is
	// refused instead: its leases are that team's record of what their runs
	// consumed, not this requester's to close.
	// A SideBySide walk (the attached per-slot tier, ADR-164) keeps its
	// earlier slot leases OPEN: they are siblings of this acquisition, not
	// a takeover of it — superseding them would free the first donor's
	// slot and committed allowance mid-run while both credentials stay
	// sealed and spent. Only a non-side-by-side acquire replaces the
	// run's previous attempt leases.
	superseded, err := func() ([]supersededLease, error) {
		if req.SideBySide {
			return nil, nil
		}
		return b.supersedeOpenLeases(ctx, req, now, OutcomeSuperseded, nil)
	}()
	if err != nil {
		return nil, err
	}
	grant, err := b.admit(ctx, req, now)
	if grant == nil {
		// No takeover: the attempt those leases serve goes on — a resume
		// that found no donor may still be refused before its publication —
		// so they are reopened. A publication that goes ahead without a
		// grant supersedes them then (SupersedeRun).
		b.reopenSuperseded(ctx, superseded)
		return nil, err
	}
	grant.superseded = superseded
	return grant, nil
}

// admit is Acquire past the supersede: the pools, the walk of the pledges
// and the admission of the first that serves.
func (b *Broker) admit(ctx context.Context, req Request, now time.Time) (*Grant, error) {
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
// requested wants ∩ pool vocabulary — the eligibility filter for the "how
// many pledges did the walk actually consider?" count.
func wantMatchesPledge(wants []Credential, p Pledge) bool {
	for _, w := range wants {
		if p.Source == w.Source && p.Ref == w.Ref {
			return true
		}
	}
	return false
}

// poolIDsContain reports whether id is among the named pools.
func poolIDsContain(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
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
	eligible, skips := eligiblePledges(candidates, req, want, now)
	if len(eligible) == 0 {
		return nil, skips, nil
	}

	ranked, err := b.rank(ctx, eligible, now, req.RankMode)
	if err != nil {
		return nil, skips, err
	}
	// The run's held entry (reset_hold) leads when it is eligible: the
	// strategy switched to it earlier, and a reset window elsewhere must
	// not pull the run back until the operator resets the hold.
	preferredMoved := false
	if req.PreferPledgeID != "" {
		for i, p := range ranked {
			if p.ID == req.PreferPledgeID && i > 0 {
				ranked = append([]Pledge{p}, append(ranked[:i:i], ranked[i+1:]...)...)
				preferredMoved = true
				break
			}
		}
	}
	// A candidate the eligibility filter dropped with a priority AT LEAST
	// as good as the one that serves means the strategy moved off an entry
	// that was in play (an equal-priority peer near its margin included —
	// holding an equivalent entry is harmless, flipping back is the thrash
	// reset_hold exists to stop).
	// Only the ELIGIBILITY filter's skips (taken before the walk): a skip
	// the walk itself appends below — a store blip read as unhealthy — is
	// not an entry that declined.
	eligibilitySkips := skips[:len(skips):len(skips)]
	bestSkipped := func(prio int) bool {
		for _, s := range eligibilitySkips {
			if !switchStatus(s.Status) {
				continue
			}
			for _, c := range candidates {
				if c.ID == s.PledgeID && c.Priority <= prio {
					return true
				}
			}
		}
		return false
	}

	// Walk the ranking: a donor whose ledger refuses (raced to their cap,
	// concurrency full) or whose credential is gone simply yields to the
	// next — and lands on the skips list under the status tryPledge
	// decided, so the operator can tell "exhausted" from "serving" from
	// "unhealthy" from "paused".
	// rankedDeclined: a better-ranked candidate was REFUSED with an
	// exhaustion-class status (a store blip is not a switch — the entry did
	// not decline, the read failed).
	rankedDeclined := false
	for _, p := range ranked {
		// reset_hold: hold when this candidate serves AFTER a better one
		// declined for exhaustion (ranked ahead of it, or filtered out with
		// a priority at least as good), or when it is the run's held entry.
		req.holdLease = req.HoldOnSwitch && (p.ID == req.PreferPledgeID || rankedDeclined || bestSkipped(p.Priority))
		grant, status, capped, err := b.tryPledge(ctx, pool, p, req, now)
		if err != nil {
			b.logger.Warn("credpool: pledge %s unusable for run %s: %v", p.ID, req.RunID, err)
			skips = append(skips, PledgeSkip{PledgeID: p.ID, Status: StatusUnhealthy})
			continue
		}
		if grant != nil {
			return grant, skips, nil
		}
		skips = append(skips, PledgeSkip{PledgeID: p.ID, Status: status})
		// The held entry moved ahead of the ordinary order declining is no
		// switch OFF the natural first choice that serves next.
		movedPreferred := preferredMoved && p.ID == req.PreferPledgeID
		if (capped || switchStatus(status)) && !movedPreferred {
			rankedDeclined = true
		}
	}
	return nil, skips, nil
}

// eligiblePledges is shared by admission and preview; it performs no writes.
func eligiblePledges(candidates []Pledge, req Request, want Credential, now time.Time) ([]Pledge, []PledgeSkip) {
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
		// The fallback door serves only donors who consented to it: a
		// pledge without the mark is whole-bundle currency, never a
		// fallback rung's. Named so "why did nobody serve this door
		// consult" reads as consent, not as paused or cooling.
		if req.FallbackOnly && !p.FallbackUse {
			skips = append(skips, PledgeSkip{PledgeID: p.ID, Status: StatusNoFallbackConsent})
			continue
		}
		if ok, status := p.AvailableForLaunch(now, req.BotID); ok {
			// The margin skip fires HERE — the single site both the live
			// walk (acquireKind) and the preview walk share, so they cannot
			// diverge on who is near limit. A near-limit entry yields to
			// the next candidate with a reset instant the publisher arms
			// its durable retry on.
			if req.ClosedAtMargin != nil {
				reopenAt, near := req.ClosedAtMargin(p)
				if near {
					skips = append(skips, PledgeSkip{PledgeID: p.ID, Status: StatusNearLimit, ReopenAt: reopenAt})
					continue
				}
			}
			eligible = append(eligible, p)
		} else {
			skips = append(skips, PledgeSkip{PledgeID: p.ID, Status: status})
		}
	}
	return eligible, skips
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
	// A private pool answers NOTHING without an explicit pool attachment —
	// no audience walk, no door probe, no whole-bundle tier. It is filtered
	// here, before the count, so the abstention's numbers stay honest: the
	// attachment path reaches it by explicit PoolIDs, where the attachment
	// itself is the consent (ADR-164).
	visible := make([]Pool, 0, len(pools))
	for _, p := range pools {
		if p.EffectiveVisibility() == PoolVisibilityPrivate && !poolIDsContain(req.PoolIDs, p.ID) {
			continue
		}
		visible = append(visible, p)
	}
	pools = visible
	poolsEnabled = len(pools)
	if len(req.PoolIDs) > 0 {
		// The attachment path: ONLY the named pools, admitted whatever
		// their audience says — the attachment is the consent. The
		// own-org sort and the reciprocity lookup are the legacy walk's
		// concerns and stay out of it.
		allowed = make([]poolCandidates, 0, len(pools))
		for _, pool := range pools {
			if !poolIDsContain(req.PoolIDs, pool.ID) {
				continue
			}
			candidates, lerr := b.pledges.ListByPool(ctx, pool.ID)
			if lerr != nil {
				return 0, nil, lerr
			}
			allowed = append(allowed, poolCandidates{pool: pool, candidates: candidates})
		}
		if len(allowed) == 0 {
			// Every named pool is disabled or unknown — the attachment
			// resolves to nothing; the abstention says so honestly.
			return poolsEnabled, nil, noDonor(ReasonNoEnabledPool, poolsEnabled, 0, 0, nil)
		}
		return poolsEnabled, allowed, nil
	}
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

// HasPool reports whether a named pool exists AND is enabled — the fold's
// refuse-at-launch check (ADR-164): an attachment naming a pool that has
// vanished or been switched off must refuse the launch naming the level,
// never fold around it. An unknown or disabled pool is the same answer:
// the attachment resolves to nothing the run may draw on.
func (b *Broker) HasPool(ctx context.Context, poolID string) (bool, error) {
	if b == nil || poolID == "" {
		return false, nil
	}
	pools, err := b.pools.ListEnabled(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range pools {
		if p.ID == poolID {
			return true, nil
		}
	}
	return false, nil
}

// HasFallbackDonors reports whether ANY audience-admitted pool holds a
// pledge the door could consider: matching one of the wants, marked
// `fallback_use`, not the requester. It is the door's AMORTIZATION probe
// (ADR-121 § Delivery 2) — the publisher asks it before any consult, so
// a deployment with zero opted-in donors pays no walk, no consult log and
// no grant for the door. Shaped exactly like resolvePools (same
// ListEnabled walk, same audience admission including the reciprocity
// lookup) so the probe and the consult cannot diverge on who is
// reachable; a community pool that opened itself to the requester is
// seen by both or neither.
func (b *Broker) HasFallbackDonors(ctx context.Context, req Request) (bool, error) {
	_, allowed, err := b.resolvePools(ctx, req)
	if err != nil {
		// Nothing admitted the requester at all: no donor, fallback or
		// otherwise. The typed abstention is the consult's answer, not a
		// probe failure.
		return false, nil
	}
	for _, pc := range allowed {
		for _, p := range pc.candidates {
			if !p.FallbackUse || p.UserID == req.UserID {
				continue
			}
			for _, w := range req.Wants {
				if p.Source == w.Source && p.Ref == w.Ref {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// rank orders eligible pledges. least_consumed (the legacy ranking, and the
// zero RankMode): least consumed today first — as a fraction of what the
// donor offered, so a small pledge is not drained before a large one — ties
// broken by least-recently served. priority (ADR-164): the operator's
// explicit order wins — lower Priority first; equal priorities keep
// forfaits before metered keys (a free forfait rides ahead of a paid key —
// the poolWantOrder philosophy), then the legacy fairness comparison.
func (b *Broker) rank(ctx context.Context, eligible []Pledge, now time.Time, mode StrategyMode) ([]Pledge, error) {
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
		if mode == StrategyPriority {
			// The operator's explicit order outranks the fairness score;
			// equal priorities fall through to forfaits-before-metered and
			// then the legacy comparison, so a pool of equal-priority
			// entries still rotates fairly.
			if ranked[i].Priority != ranked[j].Priority {
				return ranked[i].Priority < ranked[j].Priority
			}
			mi, mj := ranked[i].Source.Metered(), ranked[j].Source.Metered()
			if mi != mj {
				return !mi
			}
		}
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
func (b *Broker) tryPledge(ctx context.Context, pool Pool, p Pledge, req Request, now time.Time) (*Grant, Status, bool, error) {
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
		return nil, "", false, err
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
		return nil, "", false, err
	}

	// The ATTACHMENT row first (ADR-164): the borrower scope's draw caps are
	// the cheaper refusal, and denying here consumes nothing of the donor's.
	// The run's leases are read once, here, for both the attachment's
	// readmission test and the acquisition floor below.
	prev, err := b.leases.ListByRun(ctx, req.RunID)
	if err != nil {
		return nil, "", false, fmt.Errorf("credpool: read the run's leases for the acquisition floor: %w", err)
	}
	var (
		attRemaining float64
		attDeny      DenyReason
		attReadmit   bool
	)
	if req.AttachmentID != "" {
		for _, l := range prev {
			if l.AttachmentID == req.AttachmentID {
				attReadmit = true
				break
			}
		}
		attRuns, attCommitted, aerr := b.leases.LiveCommitmentByAttachment(ctx, req.AttachmentID, req.RunID, now)
		if aerr != nil {
			return nil, "", false, aerr
		}
		attLive := LiveCommitment{Runs: attRuns, CommittedUSD: attCommitted}
		if attReadmit {
			attRemaining, attDeny, aerr = b.ledger.Renew(ctx, AttachmentLedgerID(req.AttachmentID), now, req.DrawLimits.AsLimits(), attLive)
		} else {
			attRemaining, attDeny, aerr = b.ledger.Reserve(ctx, AttachmentLedgerID(req.AttachmentID), now, req.DrawLimits.AsLimits(), attLive)
		}
		if aerr != nil {
			return nil, "", false, aerr
		}
		if attDeny != DenyNone {
			// The SCOPE is capped out — honest display state: exhausted, the
			// borrower's own ceiling, not the donor's.
			return nil, StatusExhausted, true, nil
		}
	}

	var remaining float64
	var deny DenyReason
	if readmitting { //nolint:staticcheck // reads better than an inverted condition
		remaining, deny, err = b.ledger.Renew(ctx, p.ID, now, p.Limits, live)
	} else {
		remaining, deny, err = b.ledger.Reserve(ctx, p.ID, now, p.Limits, live)
	}
	// The donor's row refusing or erroring must give the ATT: row back
	// BEFORE the walk moves to the next candidate — otherwise every
	// capped-out donor in the ranking burns the borrower scope another
	// run unit (a K-donor walk costs K+1 draws for one run), and the
	// refusal's early return keeps the att: reservation of an acquisition
	// that never happened.
	if err != nil {
		if req.AttachmentID != "" && !attReadmit {
			b.releaseReservation(ctx, AttachmentLedgerID(req.AttachmentID), now)
		}
		return nil, "", false, err
	}
	if deny != DenyNone {
		if req.AttachmentID != "" && !attReadmit {
			b.releaseReservation(ctx, AttachmentLedgerID(req.AttachmentID), now)
		}
		b.logger.Debug("credpool: pledge %s declined run %s (%s)", p.ID, req.RunID, deny)
		// capped: the entry declined on its own allowance (runs/day, $/day,
		// $/week) — whatever the display status says about promised spend —
		// as opposed to a full concurrency slot set, which only means busy.
		return nil, deny.skipStatus(live), deny != DenyConcurrency, nil
	}

	// From here on, any failure must give back whatever was consumed.
	// Renew consumed nothing, so it has nothing to return — on either row.
	release := func() {
		if !readmitting {
			b.releaseReservation(ctx, p.ID, now)
		}
		if req.AttachmentID != "" && !attReadmit {
			b.releaseReservation(ctx, AttachmentLedgerID(req.AttachmentID), now)
		}
	}

	cred, gone, err := b.openCredential(ctx, p, now)
	if err != nil {
		release()
		return nil, "", false, err
	}
	if gone != "" {
		release()
		// The donor pledged a credential that is no longer usable. Park the
		// pledge rather than re-discovering this on every launch.
		b.markUnhealthy(ctx, p.ID, HealthTokenExpired, gone)
		return nil, StatusUnhealthy, false, nil
	}

	// The run's leases stay strictly ordered at the stores' precision, so
	// an attempt's report can tell its own lease from a successor's.
	// Advisory under concurrency (two acquisitions read before either
	// writes): the tie then resolves by lease id, deterministically.
	acquiredAt := now
	for i := range prev {
		acquiredAt = nextAcquiredAt(acquiredAt, &prev[i].AcquiredAt)
	}
	lease := Lease{
		ID:                 uuid.NewString(),
		RunID:              req.RunID,
		PledgeID:           p.ID,
		PoolID:             pool.ID,
		DonorID:            p.UserID,
		Fingerprint:        cred.fingerprint,
		TenantID:           req.TenantID,
		RequesterID:        req.UserID,
		BotID:              req.BotID,
		Credential:         p.Credential,
		GrantedCostUSD:     remaining,
		ConsumedRunUnit:    !readmitting,
		AcquiredAt:         acquiredAt,
		ExpiresAt:          acquiredAt.Add(b.leaseTTL),
		AttachmentID:       req.AttachmentID,
		AttConsumedRunUnit: req.AttachmentID != "" && !attReadmit,
		Held:               req.holdLease,
		AttemptID:          req.AttemptID,
	}
	if err := b.leases.Put(ctx, lease); err != nil {
		release()
		return nil, "", false, fmt.Errorf("credpool: record lease: %w", err)
	}

	// Best-effort fairness bookkeeping; a miss only costs tie-break
	// precision on the next selection.
	if err := b.pledges.TouchLastServed(ctx, p.ID, now); err != nil {
		b.logger.Warn("credpool: could not stamp last_served_at on pledge %s: %v", p.ID, err)
	}

	b.logger.Info("credpool: run %s served by donor %s (%s, allowance=$%.2f%s)",
		req.RunID, p.UserID, p.Credential, remaining, meteredNote(p.Source))
	return &Grant{
		PledgeID:               p.ID,
		DonorID:                p.UserID,
		Credential:             p.Credential,
		Payload:                cred.payload,
		Fingerprint:            cred.fingerprint,
		RecordID:               cred.recordID,
		RecordConnectedAt:      cred.connectedAt,
		KeyMaxConcurrentRuns:   cred.maxConcurrentRuns,
		KeyName:                cred.name,
		BaseURL:                cred.baseURL,
		Backend:                p.Backend,
		Model:                  p.Model,
		AttachmentID:           req.AttachmentID,
		AttachmentRemainingUSD: attRemaining,
		RemainingUSD:           remaining,
		AcquiredAt:             lease.AcquiredAt,
		Held:                   lease.Held,
		releaseGuard:           &ReleaseGuard{lease: lease},
	}, "", false, nil
}

// HeldPledge returns the entry the run holds for the wanted kind under
// reset_hold — the pledge of its NEWEST Held lease of that credential, any
// state (the previous attempt's lease is closed by its report) — or "".
func (b *Broker) HeldPledge(ctx context.Context, runID string, want Credential) (string, error) {
	if b == nil || runID == "" {
		return "", nil
	}
	leases, err := b.leases.ListByRun(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("credpool: read the run's held entry: %w", err)
	}
	// The run's NEWEST lease of the kind decides (a lease returned unused
	// is no switch and is skipped): held, it names the entry; not held, the
	// run already left the held entry — an older hold must not resurrect.
	for _, l := range leases {
		if l.Source != want.Source || l.Ref != want.Ref || (l.Closed && l.Outcome == OutcomeNotLaunched) {
			continue
		}
		if l.Held {
			return l.PledgeID, nil
		}
		return "", nil
	}
	return "", nil
}

// switchStatus reports whether a declining candidate's status means the
// strategy MOVED OFF it for exhaustion — the reset_hold trigger: at its
// margin, cooling on a provider refusal, failing auth, or out of its
// allowance. A paused, bot-filtered, out-of-hours or busy entry was never
// in play for this run, or is only momentarily unavailable.
func switchStatus(s Status) bool {
	switch s {
	case StatusNearLimit, StatusCooling, StatusUnhealthy, StatusExhausted:
		return true
	}
	return false
}

// ClearHolds lifts every reset_hold naming the entry — the manual reset's
// hold half: the runs holding it re-rank at their next resume.
func (b *Broker) ClearHolds(ctx context.Context, pledgeID string) (int, error) {
	if b == nil {
		return 0, nil
	}
	return b.leases.ClearHeld(ctx, pledgeID)
}

// VerifyEntry checks a pledge names a credential its lender actually
// holds — the SAME open path an acquisition runs, without granting: an
// oauth record under the lender's key (or a key row in the pledged
// scope), unsealable, not gone. One truth for what "lending requires
// holding" means; the management routes refuse on its answer.
func (b *Broker) VerifyEntry(ctx context.Context, p Pledge) error {
	if b == nil {
		return nil
	}
	_, gone, err := b.openCredential(ctx, p, b.now())
	if err != nil {
		return err
	}
	if gone != "" {
		return errors.New(gone)
	}
	return nil
}

// meteredNote flags, in the launch log, the case where the allowance is
// real money rather than a slice of an already-paid-for plan.
func meteredNote(src CredentialSource) string {
	if src.Metered() {
		return ", metered"
	}
	return ""
}

// openedCredential is what a pledge lends, unsealed for one grant.
type openedCredential struct {
	payload     []byte
	fingerprint string
	recordID    string
	connectedAt time.Time
	// maxConcurrentRuns and name are the lent key's own hard concurrency
	// cap and label (Grant.KeyMaxConcurrentRuns, Grant.KeyName); zero and
	// empty for a subscription.
	maxConcurrentRuns int
	name              string
	// baseURL is the lent gateway key's canonical endpoint
	// (Grant.BaseURL); empty for a subscription and a vendor key.
	baseURL string
}

// openCredential unseals whatever the pledge lends. It returns a non-empty
// `gone` reason — rather than an error — when the credential has vanished
// or died in a way only the donor can fix, so the caller parks the pledge
// instead of rediscovering it on every launch.
//
// fingerprint is the donor record's stable audit identity, which the
// borrower's usage-cap meter keys on so a donor who reconnects a fresh
// subscription is not metered against the one it replaced. Both sources
// carry one: an OAuth record's connect-time stamp, and an API key's own
// (the hash of the plaintext the runner would derive anyway — carried so
// the grant NAMES the account instead of leaving every consumer to
// re-derive it or report nothing).
//
// recordID and connectedAt name the donor's OAuth record a subscription was
// read from, so the borrower's runner can follow its rotations
// (Grant.RecordID, Grant.RecordConnectedAt); empty for a key.
func (b *Broker) openCredential(ctx context.Context, p Pledge, now time.Time) (cred openedCredential, gone string, err error) {
	switch p.Source {
	case SourceOAuth:
		rec, gerr := b.oauth.Get(ctx, p.UserID, secrets.OAuthKind(p.Ref))
		if gerr != nil {
			if errors.Is(gerr, secrets.ErrOAuthNotFound) {
				return openedCredential{}, "the connected subscription was disconnected — reconnect it to resume sharing", nil
			}
			return openedCredential{}, "", gerr
		}
		// An expired token with no way to renew is dead: the refresh worker
		// skips it, so it will never come back on its own.
		if gone := oauthCredentialGone(rec, now); gone != "" {
			return openedCredential{}, gone, nil
		}
		pt, oerr := secrets.OpenOAuthPayload(b.sealer, rec.UserID, rec.Kind, rec.SealedPayload)
		if oerr != nil {
			return openedCredential{}, "", fmt.Errorf("credpool: unseal donated subscription: %w", oerr)
		}
		return openedCredential{payload: pt, fingerprint: rec.Fingerprint, recordID: rec.ID, connectedAt: rec.CreatedAt}, "", nil

	case SourceAPIKey:
		if b.apiKeys == nil {
			return openedCredential{}, "", fmt.Errorf("credpool: no api-key store wired; cannot serve %s", p.Credential)
		}
		// Operator-pushed entries (LendScope set) read by the RESERVED
		// scope the pledge names — the org/platform tiers' sentinel-tenant
		// rows, which carry no personal owner. Personal lenders keep the
		// GetOwned read.
		var k secrets.ApiKey
		var gerr error
		if p.LendScope != "" {
			k, gerr = b.apiKeys.GetForLend(ctx, p.KeyID, p.LendScope)
		} else {
			// GetOwned, not Get: this read runs on the BORROWER's context, in
			// another tenant than the donor's, and the tenant-scoped Get would
			// find nothing — parking an innocent donor's pledge with "the lent
			// API key was deleted" on every cross-team draw, which is every
			// draw a pool exists for.
			k, gerr = b.apiKeys.GetOwned(ctx, p.KeyID, p.UserID)
		}
		if gerr != nil {
			if errors.Is(gerr, secrets.ErrApiKeyNotFound) {
				return openedCredential{}, "the lent API key was deleted or moved out of its pledged scope — pledge another to resume sharing", nil
			}
			return openedCredential{}, "", gerr
		}
		// A donor lends THEIR OWN key. A team-wide key is the team's to
		// spend, not one member's to hand to the pool, and a pledge must
		// never become a way to re-scope somebody else's credential.
		if gone := apiKeyCredentialGone(k, p, now); gone != "" {
			return openedCredential{}, gone, nil
		}
		pt, oerr := secrets.OpenApiKey(b.sealer, k)
		if oerr != nil {
			return openedCredential{}, "", fmt.Errorf("credpool: unseal donated api key: %w", oerr)
		}
		// The donor record's own stamp. A key IS its own identity, so a
		// borrower could re-derive this from the plaintext — but only if
		// it thinks to, and an empty grant fingerprint is what left the
		// GRANTED line and the run-doc stamp naming a slot instead of the
		// account paying for it. Empty on keys stored before stamping;
		// the derivation from the plaintext stays the fallback. The key's
		// own concurrency cap rides with it: the borrower's launch is
		// admitted against it like the donor's own.
		return openedCredential{payload: pt, fingerprint: k.Fingerprint, maxConcurrentRuns: k.MaxConcurrentRuns, name: k.Name, baseURL: k.BaseURL}, "", nil
	}
	return openedCredential{}, "", fmt.Errorf("credpool: unknown credential source %q", p.Source)
}

// ErrRunHeldElsewhere reports an acquire on a run id another team's open
// lease already holds.
var ErrRunHeldElsewhere = errors.New("credpool: run id held by another team's open lease")

// supersedeOpenLeases closes every open lease the requesting team holds on
// this run, and returns those it closed. Their donors get their slot and
// committed allowance back; the run unit stays consumed, because those
// attempts did run. An open lease of ANOTHER team refuses the acquire:
// closing it would free the donor's slot and erase the charge while the run
// still uses the credential. Best-effort: a store miss only costs a slot
// until the lease TTL.
func (b *Broker) supersedeOpenLeases(ctx context.Context, req Request, now time.Time, outcome string, supersededByPublishedAt *time.Time) ([]supersededLease, error) {
	open, err := b.leases.ListOpenByRun(ctx, req.RunID)
	if err != nil {
		b.logger.Warn("credpool: could not check the open leases of run %s: %v", req.RunID, err)
		return nil, nil
	}
	for _, l := range open {
		// An unstamped side of the pair is legacy (pre-tenancy leases,
		// bounded by the lease TTL): it supersedes rather than refuses,
		// so an old lease cannot block the owning team's own retry.
		if req.TenantID != "" && l.TenantID != "" && l.TenantID != req.TenantID {
			return nil, fmt.Errorf("%w: run %q is held by team %q", ErrRunHeldElsewhere, req.RunID, l.TenantID)
		}
	}
	var closed []supersededLease
	for _, l := range open {
		// Zero, not l.CostUSD: Close ADDS, and whatever this lease already
		// carries was recorded when it was charged. The cause gets its own
		// outcome (outcome): an acquisition's plain supersede is the window
		// its own attempt's report stamps through; SupersedeRun's grantless
		// takeover must never be charged by a report of the takeover
		// (ReportAttempt stays silent on superseded_grantless).
		won, cerr := b.leases.Close(ctx, l.ID, 0, outcome, now)
		if cerr != nil {
			b.logger.Warn("credpool: could not supersede lease %s of run %s: %v", l.ID, req.RunID, cerr)
			continue
		}
		if won {
			if supersededByPublishedAt != nil {
				if serr := b.leases.SetSupersededByPublication(ctx, l.ID, *supersededByPublishedAt); serr != nil {
					b.logger.Warn("credpool: could not carry the superseding publication on lease %s: %v", l.ID, serr)
				}
			}
			closed = append(closed, supersededLease{id: l.ID, runID: l.RunID, at: now})
		}
	}
	return closed, nil
}

// reopenSuperseded undoes a supersede whose takeover did not happen: the
// attempt those leases serve goes on, and reports against them. A lease that
// attempt has reported since is left alone (LeaseStore.Reopen). Best-effort
// and cancellation-immune, like the release it follows.
func (b *Broker) reopenSuperseded(ctx context.Context, superseded []supersededLease) {
	ctx = context.WithoutCancel(ctx)
	for _, l := range superseded {
		if _, err := b.leases.Reopen(ctx, l.id, l.at); err != nil {
			b.logger.Warn("credpool: could not reopen lease %s of run %s after a takeover that did not happen: %v — the attempt it serves reports against no lease, and its donor's slot is free until it ends", l.id, l.runID, err)
		}
	}
}

// SupersedeRun closes the run's open leases as superseded by a GRANTLESS
// publication (OutcomeSupersededGrantless): a newer attempt of the run was
// published without a pool grant, so the attempt they serve is over — and
// the takeover holds no lease of its own, so no report of the takeover may
// charge these. An acquisition supersedes them itself (Acquire, the plain
// supersede its own report stamps through), and reopens them when it grants
// nothing — which leaves them to this, after the publication. Best-effort,
// like that supersede; ErrRunHeldElsewhere when another team's lease holds
// the run id.
func (b *Broker) SupersedeRun(ctx context.Context, runID, tenantID string, supersedingPublishedAt time.Time) error {
	if b == nil || runID == "" {
		return nil
	}
	var supersededBy *time.Time
	if !supersedingPublishedAt.IsZero() {
		t := supersedingPublishedAt
		supersededBy = &t
	}
	_, err := b.supersedeOpenLeases(ctx, Request{RunID: runID, TenantID: tenantID}, b.now(), OutcomeSupersededGrantless, supersededBy)
	return err
}

// releaseReservation gives back one admitted-but-unused run unit.
func (b *Broker) releaseReservation(ctx context.Context, pledgeID string, when time.Time) {
	if rerr := b.ledger.ReleaseRun(context.WithoutCancel(ctx), pledgeID, when); rerr != nil {
		b.logger.Warn("credpool: could not release the reserved unit of pledge %s: %v (its daily run count over-reports by one)", pledgeID, rerr)
	}
}

// ReleaseGuard pins an exact lease at acquisition or before a caller performs
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

// ReleaseGrant undoes only the acquisition that produced grant. Launchers
// must use it instead of Release: another acquisition for the same run may
// have inserted its lease even before the original Acquire returned.
// A nil grant, or one not produced by Acquire, has nothing to release.
// Like ReleaseCaptured, it is cancellation-immune and idempotent.
func (b *Broker) ReleaseGrant(ctx context.Context, grant *Grant) {
	if grant == nil {
		return
	}
	b.ReleaseCaptured(ctx, grant.releaseGuard)
}

// ReopenSuperseded undoes what grant's acquisition superseded, once the
// caller knows that takeover did not happen: the attempt it superseded goes
// on — a resume refused before its publication, whose flip to queued was
// reverted — and reports against its lease. Release the grant first
// (ReleaseGrant), so the run never holds two open leases. A resume that lost
// its run to a newer one does not call it: that one took the run over.
func (b *Broker) ReopenSuperseded(ctx context.Context, grant *Grant) {
	if b == nil || grant == nil {
		return
	}
	b.reopenSuperseded(ctx, grant.superseded)
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
// Callers holding the original grant should use ReleaseGrant, which cannot
// release a different acquisition for the same run.
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
	// The att: row refunds on ITS OWN admission truth — gating on the
	// donor's ConsumedRunUnit would under-count when a resume renews the
	// att: row while consuming a NEW donor's unit (a different pledge
	// serving the same attachment).
	if lease.AttachmentID != "" && lease.AttConsumedRunUnit {
		b.releaseReservation(ctx, AttachmentLedgerID(lease.AttachmentID), lease.AcquiredAt)
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
	// ByFingerprint carries the attempt's spend split per credential
	// fingerprint (the runner's by-route table grouped by the credential
	// that served each route). NON-NIL means the runner does fine
	// attribution: the lease books only the slice that ran on ITS
	// credential's fingerprint, and the rest of the attempt — spend on the
	// run's own credentials — stays off the donor's ledger. NIL (an older
	// runner) keeps the coarse whole-attempt booking.
	ByFingerprint []FingerprintSpend
	CostUSD       float64
	// InputTokens / OutputTokens carry an observed split; AggregateTokens
	// carries a CLI delegate's unsplittable total. A donor's ledger reads
	// zero as "not observed", so the aggregate must not be filed under a
	// direction it was never measured in.
	InputTokens     int64
	OutputTokens    int64
	AggregateTokens int64
	Condition       Condition
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

// attemptLease picks the lease the attempt published at attemptPublishedAt
// executed under: the run's newest lease acquired at or before it,
// compared at the millisecond both stores keep. The publication carries
// sub-millisecond precision and the acquisition does not, so a lease the
// store recorded inside the publication's own millisecond is this
// attempt's, and one recorded in a later millisecond is a successor's.
// Within one millisecond the broker's acquisition floor keeps a run's
// leases strictly ordered; a legacy tie (rows written before the floor)
// sorts by lease id, descending, so the pick is deterministic. A run with
// no such lease — an attempt older than lease stamping, or a publication
// that could not be read — falls back to the open-lease lookup, the
// identity a report had before this rule; nil when the run holds no lease
// at all.
func attemptLease(ctx context.Context, leases LeaseStore, runID string, attemptPublishedAt time.Time) (*Lease, error) {
	if attemptPublishedAt.IsZero() {
		l, err := leases.GetOpenByRun(ctx, runID)
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return &l, err
	}
	pub := attemptPublishedAt.Truncate(time.Millisecond)
	all, err := leases.ListByRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		l, err := leases.GetOpenByRun(ctx, runID)
		if errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return &l, err
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i].AcquiredAt.Truncate(time.Millisecond), all[j].AcquiredAt.Truncate(time.Millisecond)
		if !a.Equal(b) {
			return a.After(b)
		}
		return all[i].ID > all[j].ID
	})
	for i := range all {
		// Strictly earlier at the stores' precision: the publication is
		// never inside its own lease's acquisition millisecond
		// (store.PublishAt), so the attempt's own lease always clears this,
		// and a successor acquired after it never does.
		if all[i].AcquiredAt.Truncate(time.Millisecond).Before(pub) {
			return &all[i], nil
		}
	}
	// The attempt predates every lease the run carries: fall back to the
	// open lease, the identity the report had before this rule.
	l, err := leases.GetOpenByRun(ctx, runID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return &l, err
}

// nextAcquiredAt is a new lease's acquisition instant for a run whose
// previous lease was acquired at prev: now, but never inside the previous
// attempt's millisecond — a run's leases stay strictly ordered at the
// precision the stores keep, so a report can always tell its own lease
// from a successor's, even when a store keeps only milliseconds and a
// double resume races inside one millisecond.
func nextAcquiredAt(now time.Time, prev *time.Time) time.Time {
	if prev == nil {
		return now
	}
	if floor := prev.Truncate(time.Millisecond).Add(time.Millisecond); floor.After(now) {
		return floor
	}
	return now
}

// ReportAttempt is Report carrying the reporting attempt's identity — its
// publication, which the runner's recordPoolSpend reads off the delivery —
// so the spend is booked on the lease that attempt executed under, never on
// a lease a later resume's acquisition opened or superseded inside the
// previous attempt's unwind window.
//
// A superseded lease its attempt reports against still charges the donor —
// the supersede that closed it charged nothing — once, through the stamp's
// CAS: a redelivered report of the same attempt loses the CAS. A lease
// reported, or closed any other way, stays silent, as in Report.
func (b *Broker) ReportAttempt(ctx context.Context, runID string, attemptPublishedAt time.Time, out Outcome) error {
	if b == nil || runID == "" {
		return nil
	}
	lease, err := attemptLease(ctx, b.leases, runID, attemptPublishedAt)
	if err != nil {
		return err // a store blip must not silently drop the donor's charge
	}
	if lease == nil {
		return nil
	}
	now := b.now()

	// The SIDE-BY-SIDE shape (ADR-164 per-slot sealing): one attempt seals
	// several leases (forfait and gateway), sharing its AttemptID. The
	// report settles EXACTLY that attempt's siblings, each on its own
	// fingerprint slice and in whatever state it is — open (closed now, or
	// charged without closing on an interim report), superseded by the
	// next attempt's takeover (charged once through the stamp's CAS) — and
	// never a lease of another attempt of the same run: a late report of a
	// superseded attempt must not settle the resume's open leases. A lease
	// without an AttemptID (single-lease acquisitions) has no siblings.
	var errs []error
	settling := []Lease{*lease}
	if lease.AttemptID != "" {
		all, lerr := b.leases.ListByRun(ctx, runID)
		if lerr != nil {
			errs = append(errs, fmt.Errorf("credpool: read the attempt's sibling leases: %w", lerr))
		}
		for _, l := range all {
			if l.ID != lease.ID && l.AttemptID == lease.AttemptID {
				settling = append(settling, l)
			}
		}
	}
	// A COARSE share (no per-credential split, or a lease with no
	// fingerprint to slice by) cannot attribute the attempt: ONE lease
	// that served books it whole — the attempt's own, or, when that one
	// was returned unused, the first sibling that was not — and every
	// other lease settles its state with no spend and no condition.
	// Booking the whole attempt on every sibling charged each donor and the
	// borrower's att: row once per sealed slot.
	booker := 0
	if returnedUnused(settling[0]) {
		for i := 1; i < len(settling); i++ {
			if !returnedUnused(settling[i]) {
				booker = i
				break
			}
		}
	}
	for i, l := range settling {
		lo := out
		if i != booker && (out.ByFingerprint == nil || l.Fingerprint == "") {
			lo = out.withoutSpend()
		}
		if err := b.settleLease(ctx, l, lo, attemptPublishedAt, now, runID); err != nil {
			if i > 0 {
				err = fmt.Errorf("credpool: settle sibling lease %s: %w", l.ID, err)
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// settleLease books one lease's share of an attempt's report — the
// semantics every lease of the attempt shares: the fine attribution
// resolved against THIS lease's credential, a superseded lease charged
// once through the stamp's CAS, an interim report charged without
// closing, a final report closing the lease (a redelivery loses the CAS).
// A lease reported, released or abandoned elsewhere stays silent.
func (b *Broker) settleLease(ctx context.Context, lease Lease, rawOut Outcome, attemptPublishedAt, now time.Time, runID string) error {
	// The fine attribution resolves ONCE, against THIS lease's credential:
	// everything below - the superseded stamp, the interim add, the close,
	// the ledger debit - books the resolved slice, never the raw aggregate.
	out := rawOut.effectiveCharge(lease.Fingerprint)

	outcome := "ok"
	switch out.Condition {
	case ConditionUsageWindow:
		outcome = string(ConditionUsageWindow)
	case ConditionAuthFailed:
		outcome = string(ConditionAuthFailed)
	}

	// The lease is superseded: charge through the stamp's CAS. The
	// supersede added nothing, so this is the attempt's only charge on it,
	// and a redelivered report of the same attempt loses the CAS.
	if lease.Closed {
		switch lease.Outcome {
		case OutcomeSuperseded:
			// An acquisition's supersede: the takeover's own lease is the
			// one the pin would pick for the takeover's report — this is
			// the superseded attempt's own report, and its spend is the
			// donor's, once, through the stamp's CAS.
		case OutcomeSupersededGrantless:
			// A grantless takeover holds no lease of its own: the report of
			// the publication the close carried — the takeover's own,
			// stable across redeliveries and replays — is a no-op, or the
			// donor would be charged for an attempt their credential
			// never served. Any other publication (the superseded
			// attempt's own, earlier; a later attempt's, gated at the
			// runner) stamps through.
			if lease.SupersededByPublishedAt != nil &&
				lease.SupersededByPublishedAt.Truncate(time.Millisecond).Equal(attemptPublishedAt.Truncate(time.Millisecond)) {
				return nil
			}
		default:
			return nil // reported, released or abandoned elsewhere — as in Report
		}
		won, err := b.leases.StampSupersededReport(ctx, lease.ID, out.CostUSD, now)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil
			}
			return err
		}
		if !won {
			return nil
		}
		return b.chargeAndReact(ctx, lease, out, now, runID)
	}

	// Interim: the attempt spent but the queue will redeliver the same
	// bundle — charge now, the lease stays open.
	if out.Interim {
		if err := b.leases.AddCost(ctx, lease.ID, out.CostUSD); err != nil {
			b.logger.Warn("credpool: could not record the interim spend of run %s on lease %s: %v", runID, lease.ID, err)
		}
		return b.chargeAndReact(ctx, lease, out, now, runID)
	}

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
	if out.CostUSD > 0 || out.InputTokens > 0 || out.OutputTokens > 0 || out.AggregateTokens > 0 {
		// Charged against the lease's ACQUISITION instant, not now: a run
		// that starts at 23:50 and ends at 00:10 must debit the day whose
		// allowance admitted it, or the donor's daily cap silently leaks
		// across the boundary.
		if err := b.ledger.AddSpend(ctx, lease.PledgeID, lease.AcquiredAt, out.CostUSD, out.InputTokens, out.OutputTokens, out.AggregateTokens); err != nil {
			// The lease is already closed, so this spend will never be
			// retried — say so loudly rather than under-reporting a
			// donation in silence.
			b.logger.Error("credpool: run %s spent $%.4f on donor %s but the charge failed: %v (their ledger under-reports it)", runID, out.CostUSD, lease.DonorID, err)
			return fmt.Errorf("credpool: charge donor: %w", err)
		}
		// The borrower's own draw caps book the same slice on the att: row,
		// at the same acquisition instant. A failed booking only LOOSENS a
		// borrower cap (no donor is cheated), so it warns instead of
		// failing the report.
		if lease.AttachmentID != "" {
			if err := b.ledger.AddSpend(ctx, AttachmentLedgerID(lease.AttachmentID), lease.AcquiredAt, out.CostUSD, out.InputTokens, out.OutputTokens, out.AggregateTokens); err != nil {
				b.logger.Error("credpool: run %s spent on attachment %s but the draw charge failed: %v (the borrower's att: row under-reports it)", runID, lease.AttachmentID, err)
			}
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

// FingerprintSpend is one credential's slice of an attempt's consumption
// (ADR-121 § Delivery 2, #2255): what the routes served by that
// fingerprint consumed.
type FingerprintSpend struct {
	Fingerprint     string
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	AggregateTokens int64
}

// effectiveCharge resolves what THIS lease books from the report: with a
// fine table, only the slice matching the lease credential's fingerprint
// (zero when the fallback never fired on it); without one, the coarse
// whole-attempt charge. The condition and cooldown fields ride unchanged —
// they describe the credential's health, not its bill.
// returnedUnused reports a lease closed because its grant was given back
// before the run launched on it.
func returnedUnused(l Lease) bool {
	return l.Closed && l.Outcome == OutcomeNotLaunched
}

// withoutSpend is the outcome a sibling of a coarse report settles with:
// its lifecycle (interim or final) kept, its spend and its condition
// dropped — the report cannot say which credential spent or failed.
func (out Outcome) withoutSpend() Outcome {
	return Outcome{Interim: out.Interim}
}

func (out Outcome) effectiveCharge(fingerprint string) Outcome {
	if out.ByFingerprint == nil {
		return out
	}
	if fingerprint == "" {
		// A lease stamped before the field existed carries no identity to
		// slice by: keep the coarse booking it was acquired under, never
		// zero a donor's charge on a missing stamp.
		return out
	}
	eff := out
	eff.CostUSD, eff.InputTokens, eff.OutputTokens, eff.AggregateTokens = 0, 0, 0, 0
	for _, fs := range out.ByFingerprint {
		if fs.Fingerprint == fingerprint {
			eff.CostUSD = fs.CostUSD
			eff.InputTokens = fs.InputTokens
			eff.OutputTokens = fs.OutputTokens
			eff.AggregateTokens = fs.AggregateTokens
			break
		}
	}
	return eff
}
