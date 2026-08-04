// Package credpool mutualises LLM subscription credentials that
// individual developers lend to a deployment.
//
// A developer connects their Claude Code / Codex subscription through the
// ordinary personal OAuth flow (pkg/secrets, /api/me/oauth/…), then makes a
// Pledge: the standing offer of that credential, bounded by limits THEY set
// (spend per day and per week, runs per day, concurrent runs, an optional
// time window, an optional bot allow-list) and revocable at any moment.
//
// At launch, a run that has neither its own BYOK key nor a personal/org
// forfait asks the Broker for a donor. The Broker picks the least-consumed
// eligible pledge, records a Lease against the run, and hands back the
// verbatim credential blob — which the caller seals into the run bundle
// exactly like the two other credential tiers. When the attempt ends, the
// runner reports what it spent; that closes the lease and decrements the
// donor's allowance.
//
// # What this package deliberately does not pretend
//
// The dollar figures are ESTIMATES. On a subscription the CLI bills nothing
// per call, so the cost of a delegation is derived from its token counts
// (pkg/backend/cost). They are the right unit for sharing fairly between
// donors; they are not an invoice. The hard signal is the provider's own
// quota window — an ErrRateLimited(usage_window) puts the donor on cooldown
// until its reset, which is the guard that actually protects a lender.
//
// # Trust posture
//
// A subscription is an individual licence. Pooling one across people is a
// deployment owner's decision, not a neutral technical detail, so every
// mechanism here is built to keep the lender in control and informed:
// nothing is shared without an explicit pledge, Enabled=false takes effect
// at the next acquisition, limits are the lender's own numbers, and every
// lease is attributable (which run, which bot, which requester).
package credpool

import (
	"fmt"
	"strings"
	"time"

	"github.com/SocialGouv/iterion/pkg/clock"
	"github.com/SocialGouv/iterion/pkg/orgusage"
)

// ---------------------------------------------------------------------------
// Pool
// ---------------------------------------------------------------------------

// Pool is one org's mutualised credential pool. A deployment normally runs
// exactly one (id == the owning org id); the type carries an explicit ID so
// a deployment can run several without a schema change.
type Pool struct {
	ID    string `bson:"_id" json:"id"`
	OrgID string `bson:"org_id" json:"org_id"`
	Name  string `bson:"name,omitempty" json:"name,omitempty"`
	// Enabled is the operator-side master switch, independent of the
	// per-donor one. Off = the pool tier is skipped entirely.
	Enabled   bool      `bson:"enabled" json:"enabled"`
	Audience  Audience  `bson:"audience" json:"audience"`
	CreatedAt time.Time `bson:"created_at" json:"created_at"`
	UpdatedAt time.Time `bson:"updated_at" json:"updated_at"`
}

// Audience decides who may draw on a pool. The four predicates are
// independent and evaluated as a UNION, so a deployment composes the policy
// it wants instead of picking from a fixed menu — and every field left at
// its zero value narrows rather than widens.
//
// The zero Audience means "only teams of the owning org", which is the
// strictest useful policy and therefore the default.
type Audience struct {
	// Teams is an explicit allow-list of team ids.
	Teams []string `bson:"teams,omitempty" json:"teams,omitempty"`
	// Orgs admits every team under these org ids. The pool's own OrgID is
	// always admitted and need not be repeated here.
	Orgs []string `bson:"orgs,omitempty" json:"orgs,omitempty"`
	// Contributors admits any user who is themselves an active donor to
	// this pool, whichever team they launch from — the reciprocity dial
	// ("lend to borrow").
	Contributors bool `bson:"contributors,omitempty" json:"contributors,omitempty"`
	// AllTeams admits every team on the instance. The widest setting;
	// meaningful for a deployment that IS a shared community instance.
	AllTeams bool `bson:"all_teams,omitempty" json:"all_teams,omitempty"`
}

// Allows reports whether a run launched by userID, from teamID under orgID,
// may draw on a pool owned by poolOrgID. hasActivePledge is the caller's
// answer to "is this user themselves a donor", consulted only when the
// Contributors dial is on (so the caller can skip that lookup otherwise).
func (a Audience) Allows(poolOrgID, orgID, teamID, userID string, hasActivePledge bool) bool {
	if a.AllTeams {
		return true
	}
	// The owning org is implicit: a pool always serves its own org.
	if orgID != "" && orgID == poolOrgID {
		return true
	}
	for _, id := range a.Orgs {
		if id != "" && id == orgID {
			return true
		}
	}
	for _, id := range a.Teams {
		if id != "" && id == teamID {
			return true
		}
	}
	if a.Contributors && userID != "" && hasActivePledge {
		return true
	}
	return false
}

// NeedsPledgeLookup reports whether evaluating this audience requires
// knowing if the requester is a donor. Lets the caller skip that query for
// the common (reciprocity-off) case.
func (a Audience) NeedsPledgeLookup() bool { return a.Contributors }

// ---------------------------------------------------------------------------
// Pledge
// ---------------------------------------------------------------------------

// Health is a pledge's sticky operational state. Transient unavailability
// (a cooldown, a closed time window) is NOT stored here — it is derived, so
// a pledge never needs a background job to come back to life.
type Health string

const (
	// HealthOK — usable.
	HealthOK Health = "ok"
	// HealthTokenExpired — the stored credential expired and carries no
	// refresh token; only the donor reconnecting can revive it.
	HealthTokenExpired Health = "token_expired"
	// HealthAuthFailed — the provider rejected the credential on
	// consecutive runs. Held out of the pool until the donor reconnects,
	// so a dead token cannot poison every launch.
	HealthAuthFailed Health = "auth_failed"
)

// Status is the display-level state of a pledge: Health, refined by the
// transient conditions.
type Status string

const (
	StatusActive     Status = "active"
	StatusPaused     Status = "paused"  // donor switched sharing off
	StatusCooling    Status = "cooling" // provider quota window exhausted
	StatusOutOfHours Status = "out_of_hours"
	StatusExhausted  Status = "exhausted" // limits reached for the period
	// StatusServing — every slot the donor allowed is busy right now. Not
	// exhausted: they have given nothing yet beyond what those runs spend,
	// and the state clears as they finish.
	StatusServing   Status = "serving"
	StatusUnhealthy Status = "unhealthy" // token expired / auth failing
	// StatusBotFiltered — the donor is sharing, but not with the bot that
	// asked. Distinct from paused so the UI never tells a willing
	// contributor their contribution is off.
	StatusBotFiltered Status = "bot_filtered"
)

// Limits are the ceilings a donor sets on their own contribution. Every
// field is optional; zero means "no limit on this axis".
type Limits struct {
	// MaxUSDPerDay / MaxUSDPerWeek cap ESTIMATED spend (see the package
	// doc). The remaining daily allowance also becomes the launched run's
	// own cost budget, so a single run cannot blow past the pledge.
	MaxUSDPerDay  float64 `bson:"max_usd_per_day,omitempty" json:"max_usd_per_day,omitempty"`
	MaxUSDPerWeek float64 `bson:"max_usd_per_week,omitempty" json:"max_usd_per_week,omitempty"`
	// MaxRunsPerDay caps how many runs may be served, independent of cost.
	MaxRunsPerDay int `bson:"max_runs_per_day,omitempty" json:"max_runs_per_day,omitempty"`
	// MaxConcurrentRuns caps simultaneously-served runs — the dial that
	// keeps a donor's own interactive session responsive.
	MaxConcurrentRuns int `bson:"max_concurrent_runs,omitempty" json:"max_concurrent_runs,omitempty"`
}

// Validate rejects negative ceilings and a weekly cap below the daily one
// (which would make the daily cap a lie).
func (l Limits) Validate() error {
	if l.MaxUSDPerDay < 0 || l.MaxUSDPerWeek < 0 {
		return fmt.Errorf("credpool: spend limits cannot be negative")
	}
	if l.MaxRunsPerDay < 0 || l.MaxConcurrentRuns < 0 {
		return fmt.Errorf("credpool: run limits cannot be negative")
	}
	if l.MaxUSDPerDay > 0 && l.MaxUSDPerWeek > 0 && l.MaxUSDPerWeek < l.MaxUSDPerDay {
		return fmt.Errorf("credpool: weekly cap ($%.2f) is below the daily cap ($%.2f)", l.MaxUSDPerWeek, l.MaxUSDPerDay)
	}
	return nil
}

// Window restricts sharing to certain local hours and weekdays — the
// "borrow it while I sleep" dial.
type Window struct {
	// Timezone is an IANA location name; empty means UTC. An unresolvable
	// name is treated as UTC rather than closing the window, so a typo
	// cannot silently withdraw a donor's contribution.
	Timezone string `bson:"timezone,omitempty" json:"timezone,omitempty"`
	// StartHour is inclusive, EndHour exclusive, both in local time.
	// Equal values mean "all day". StartHour > EndHour wraps midnight
	// (19→8 is the evening-through-morning case donors actually want).
	StartHour int `bson:"start_hour" json:"start_hour"`
	EndHour   int `bson:"end_hour" json:"end_hour"`
	// Weekdays restricts to these days (time.Weekday values, Sunday=0).
	// Empty means every day.
	Weekdays []int `bson:"weekdays,omitempty" json:"weekdays,omitempty"`
}

// Validate bounds the hour and weekday values.
func (w *Window) Validate() error {
	if w == nil {
		return nil
	}
	if w.StartHour < 0 || w.StartHour > 23 || w.EndHour < 0 || w.EndHour > 23 {
		return fmt.Errorf("credpool: window hours must be in 0..23")
	}
	for _, d := range w.Weekdays {
		if d < 0 || d > 6 {
			return fmt.Errorf("credpool: weekday %d out of range (0=Sunday..6=Saturday)", d)
		}
	}
	if w.Timezone != "" {
		if _, err := time.LoadLocation(w.Timezone); err != nil {
			return fmt.Errorf("credpool: unknown timezone %q", w.Timezone)
		}
	}
	return nil
}

// Open reports whether now falls inside the window. A nil Window is always
// open.
func (w *Window) Open(now time.Time) bool {
	if w == nil {
		return true
	}
	loc := time.UTC
	if w.Timezone != "" {
		if l, err := time.LoadLocation(w.Timezone); err == nil {
			loc = l
		}
	}
	local := now.In(loc)
	if len(w.Weekdays) > 0 {
		match := false
		for _, d := range w.Weekdays {
			if time.Weekday(d) == local.Weekday() {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}
	if w.StartHour == w.EndHour {
		return true // all day
	}
	h := local.Hour()
	if w.StartHour < w.EndHour {
		return h >= w.StartHour && h < w.EndHour
	}
	// Wraps midnight.
	return h >= w.StartHour || h < w.EndHour
}

// CredentialSource selects which store holds a lent credential — and, more
// importantly, what the money means.
type CredentialSource string

const (
	// SourceOAuth is a subscription connected through the OAuth flow. The
	// provider bills nothing per call, so the pool's dollar figures are
	// ESTIMATES derived from tokens, and the real ceiling is the provider's
	// own usage window.
	SourceOAuth CredentialSource = "oauth"
	// SourceAPIKey is a metered provider key. Every token is real money on
	// the lender's invoice: the figures stop being estimates and the
	// ceilings stop being a proxy — they are the financial control.
	SourceAPIKey CredentialSource = "api_key"
)

func (s CredentialSource) Valid() bool {
	return s == SourceOAuth || s == SourceAPIKey
}

// Metered reports whether spending this credential costs its lender real
// money per token, rather than drawing on a subscription they already pay
// for. Callers use it to decide whether to present a figure as exact.
func (s CredentialSource) Metered() bool { return s == SourceAPIKey }

// Credential names one lendable credential.
type Credential struct {
	Source CredentialSource `bson:"source" json:"source"`
	// Ref is the OAuth kind ("claude_code", "codex") for a subscription, or
	// the provider ("anthropic", "openai", …) for a key. Held as a string
	// so this package stays independent of the secrets enums; the broker
	// converts at the boundary.
	Ref string `bson:"ref" json:"ref"`
	// KeyID selects WHICH of the donor's keys, for api_key only. A donor
	// may hold several per provider, and lends one deliberately rather than
	// whichever the resolver would have picked.
	KeyID string `bson:"key_id,omitempty" json:"key_id,omitempty"`
}

func (c Credential) String() string {
	return string(c.Source) + "/" + c.Ref
}

// Pledge is one donor's standing offer of one credential they hold.
// Keyed by (UserID, Source, Ref), so a pledge can never point at a
// credential that is not the donor's own.
type Pledge struct {
	ID     string `bson:"_id" json:"id"`
	PoolID string `bson:"pool_id" json:"pool_id"`
	UserID string `bson:"user_id" json:"user_id"`
	// Credential is what is lent — a subscription or a metered key.
	Credential `bson:",inline" json:",inline"`
	// Enabled is the donor's kill switch. Effective at the next
	// acquisition; runs already in flight finish on the credential they
	// were granted (killing them mid-way would waste the spend already
	// incurred without giving the donor anything back).
	Enabled bool    `bson:"enabled" json:"enabled"`
	Limits  Limits  `bson:"limits" json:"limits"`
	Window  *Window `bson:"window,omitempty" json:"window,omitempty"`
	// Bots optionally restricts which bot ids this credential may run.
	// Empty means any bot the pool serves.
	Bots []string `bson:"bots,omitempty" json:"bots,omitempty"`
	// CooldownUntil holds the pledge out of the pool until the provider's
	// quota window resets. Set from ErrRateLimited.ResetAt.
	CooldownUntil *time.Time `bson:"cooldown_until,omitempty" json:"cooldown_until,omitempty"`
	Health        Health     `bson:"health" json:"health"`
	// HealthDetail carries the last diagnostic, shown to the donor so an
	// unhealthy pledge is actionable instead of merely absent.
	HealthDetail string `bson:"health_detail,omitempty" json:"health_detail,omitempty"`
	// ConsecutiveAuthFailures drives the flip to HealthAuthFailed. Reset
	// by any successful run, so one transient blip does not evict a donor.
	ConsecutiveAuthFailures int        `bson:"consecutive_auth_failures,omitempty" json:"consecutive_auth_failures,omitempty"`
	LastServedAt            *time.Time `bson:"last_served_at,omitempty" json:"last_served_at,omitempty"`
	CreatedAt               time.Time  `bson:"created_at" json:"created_at"`
	UpdatedAt               time.Time  `bson:"updated_at" json:"updated_at"`
}

// PledgeID is the deterministic id for one donor's offer of one
// credential.
func PledgeID(userID string, src CredentialSource, ref string) string {
	return userID + "|" + string(src) + "|" + ref
}

// Validate checks a donor-supplied pledge before it is stored.
func (p Pledge) Validate() error {
	if strings.TrimSpace(p.UserID) == "" {
		return fmt.Errorf("credpool: pledge has no user")
	}
	if !p.Source.Valid() {
		return fmt.Errorf("credpool: pledge has an unknown credential source %q", p.Source)
	}
	if strings.TrimSpace(p.Ref) == "" {
		return fmt.Errorf("credpool: pledge names no credential")
	}
	// A metered key is lent by identity, not by "whichever anthropic key the
	// resolver picks" — the donor chooses which of their keys is exposed.
	if p.Source == SourceAPIKey && strings.TrimSpace(p.KeyID) == "" {
		return fmt.Errorf("credpool: an api_key pledge must name the key it lends")
	}
	if err := p.Limits.Validate(); err != nil {
		return err
	}
	return p.Window.Validate()
}

// Cooling reports whether the pledge is inside a provider-quota cooldown.
func (p Pledge) Cooling(now time.Time) bool {
	return p.CooldownUntil != nil && now.Before(*p.CooldownUntil)
}

// Available reports whether the pledge can be considered for selection at
// all, ignoring consumption (which the ledger owns). The reason is a stable
// token, surfaced to the donor and in the acquisition trace so "why did
// nobody serve this run" is answerable.
func (p Pledge) Available(now time.Time, botID string) (bool, Status) {
	if !p.Enabled {
		return false, StatusPaused
	}
	if p.Health != HealthOK && p.Health != "" {
		return false, StatusUnhealthy
	}
	if p.Cooling(now) {
		return false, StatusCooling
	}
	if !p.Window.Open(now) {
		return false, StatusOutOfHours
	}
	if len(p.Bots) > 0 && botID != "" && !p.servesBot(botID) {
		return false, StatusBotFiltered
	}
	return true, StatusActive
}

// AvailableForLaunch is Available as the ACQUISITION path must ask it.
//
// The difference is the empty bot id. Available treats it as "ignore the
// allow-list", which is what the donor-facing status views want. On a
// launch it means the opposite: `LaunchSpec.BotID` is empty for every
// plain `.bot` run — an inline workflow the requester uploaded, i.e. the
// arbitrary-code case — so skipping the filter there would hand a donor
// who pledged `bots: [review-pr]` to any file a requester cares to submit.
// The one input the requester fully controls must fail CLOSED.
func (p Pledge) AvailableForLaunch(now time.Time, botID string) (bool, Status) {
	if ok, status := p.Available(now, botID); !ok {
		return false, status
	}
	if len(p.Bots) > 0 && !p.servesBot(botID) {
		return false, StatusBotFiltered
	}
	return true, StatusActive
}

func (p Pledge) servesBot(botID string) bool {
	for _, b := range p.Bots {
		if b == botID {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Lease
// ---------------------------------------------------------------------------

// Lease records ONE ATTEMPT of a run drawing on a pledge. It is the
// concurrency unit (live leases per pledge), the in-flight spend commitment,
// AND the donor's audit trail (which run, which bot, for whom, what it cost).
//
// One document per attempt, never reused. Two earlier designs failed:
// keying by run alone let a resume onto a different donor erase the first
// donor's record — money on their ledger with nothing to explain it — and
// keying by (run, pledge) let a re-admission overwrite a CLOSED lease back
// to open, which both wiped the finished attempt's cost and re-armed the
// close CAS so a redelivered report could charge again.
//
// The invariant is enforced elsewhere instead: acquiring supersedes any
// still-open lease of the same run, so a run never holds two open leases
// and "who is serving this run" is never ambiguous.
type Lease struct {
	ID       string `bson:"_id" json:"id"`
	RunID    string `bson:"run_id" json:"run_id"`
	PledgeID string `bson:"pledge_id" json:"pledge_id"`
	PoolID   string `bson:"pool_id" json:"pool_id"`
	// DonorID is the lending user; denormalised so the donor's "what did
	// my quota run" view is a single indexed query.
	DonorID    string `bson:"donor_id" json:"donor_id"`
	Credential `bson:",inline" json:",inline"`
	// TenantID / RequesterID / BotID are the accountability triple: which
	// team, which person, which bot consumed the donation.
	TenantID    string `bson:"tenant_id,omitempty" json:"tenant_id,omitempty"`
	RequesterID string `bson:"requester_id,omitempty" json:"requester_id,omitempty"`
	BotID       string `bson:"bot_id,omitempty" json:"bot_id,omitempty"`
	// GrantedCostUSD is the allowance handed to this run (what remained of
	// the donor's daily cap). It is not only for display: while the lease
	// is open this is the donor's COMMITTED but unspent exposure, and the
	// next admission subtracts it — without that, N runs launched together
	// would each be granted the same remaining allowance and could spend
	// it N times over.
	GrantedCostUSD float64 `bson:"granted_cost_usd,omitempty" json:"granted_cost_usd,omitempty"`
	// ConsumedRunUnit records whether admitting this attempt took a unit of
	// the donor's daily run quota. A re-admission (a resume) does not, so
	// releasing it must not hand one back — that would mint quota out of a
	// failed launch and let the donor exceed the ceiling they set.
	ConsumedRunUnit bool `bson:"consumed_run_unit,omitempty" json:"consumed_run_unit,omitempty"`
	// Closed marks a lease whose run reported back. Closed leases are kept
	// (they are the donor's history) but stop counting toward concurrency.
	Closed  bool    `bson:"closed" json:"closed"`
	CostUSD float64 `bson:"cost_usd,omitempty" json:"cost_usd,omitempty"`
	// Outcome is a short token describing how the run ended for the pool
	// ("ok", "usage_window", "auth_failed"), for the donor's history view.
	Outcome    string     `bson:"outcome,omitempty" json:"outcome,omitempty"`
	AcquiredAt time.Time  `bson:"acquired_at" json:"acquired_at"`
	ClosedAt   *time.Time `bson:"closed_at,omitempty" json:"closed_at,omitempty"`
	// ExpiresAt bounds an abandoned lease. A pod killed mid-run never
	// reports, so without this the donor would lose a concurrency slot
	// permanently; the sweeper closes anything past this instant.
	ExpiresAt time.Time `bson:"expires_at" json:"expires_at"`
}

// Lease outcomes. Free-form for display, but these three are load-bearing:
// the pool reads them back.
const (
	// OutcomeAbandoned — the run never reported; the sweeper closed it.
	// Its spend is unknown, so a later attempt is admitted as new rather
	// than renewing an accounting record that means nothing.
	OutcomeAbandoned = "abandoned"
	// OutcomeSuperseded — a later attempt of the same run took over.
	OutcomeSuperseded = "superseded"
	// OutcomeNotLaunched — the grant was returned; the run never started.
	OutcomeNotLaunched = "not_launched"
)

// nonAdmissionOutcomes are the closes that must NOT count as "this pledge
// already admitted this run".
//
//   - abandoned: nothing ever learned what it spent, so renewing against it
//     forever would let a crash-looping run draw unmetered.
//   - not_launched: Release already gave its run unit back, so treating it
//     as an admission would let the next attempt renew against a unit that
//     no longer exists — and slip past the donor's daily ceiling.
//   - superseded: the attempt it belonged to was still open when the next
//     one asked, i.e. it died without reporting. Same reasoning as
//     abandoned, minus the wait for the sweeper.
var nonAdmissionOutcomes = []string{OutcomeAbandoned, OutcomeNotLaunched, OutcomeSuperseded}

func isNonAdmission(outcome string) bool {
	for _, o := range nonAdmissionOutcomes {
		if outcome == o {
			return true
		}
	}
	return false
}

// DefaultLeaseTTL bounds how long an unreported lease keeps consuming a
// donor's concurrency slot. Generous enough for a long agent run, short
// enough that a lost pod frees the slot the same day.
const DefaultLeaseTTL = 12 * time.Hour

// LeaseRetention is how long closed leases are kept as donor-visible
// history before Mongo's TTL evicts them.
const LeaseRetention = 90 * 24 * time.Hour

// ---------------------------------------------------------------------------
// Period keys
// ---------------------------------------------------------------------------

// dayKey buckets an instant into its UTC day — the same partition key
// the runtime's daily spend ledger uses.
func dayKey(when time.Time) string { return clock.DayKey(when) }

// weekKey buckets an instant into its ISO year-week, so a "per week" cap
// resets on a stable boundary regardless of month length.
func weekKey(when time.Time) string {
	y, w := when.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// dayStart / weekStart are stored on each ledger document so a TTL index
// can evict stale periods.
func dayStart(when time.Time) time.Time {
	u := when.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func weekStart(when time.Time) time.Time {
	u := dayStart(when)
	// Monday-anchored, matching ISOWeek.
	offset := (int(u.Weekday()) + 6) % 7
	return u.AddDate(0, 0, -offset)
}

// ledgerKey is the document id for one (pledge, period) bucket.
func ledgerKey(pledgeID, period, key string) string {
	return pledgeID + "|" + period + "|" + key
}

const (
	periodDay  = "d"
	periodWeek = "w"
)

// CostToMillis converts USD to integer thousandths so Mongo $inc stays
// integral (a float $inc accumulates drift). Shared with orgusage rather
// than mirrored: the two quota systems must round money identically, and a
// second copy is a place for that to quietly stop being true.
func CostToMillis(usd float64) int64 { return orgusage.CostToMillis(usd) }

func millisToCost(m int64) float64 { return float64(m) / 1000 }

// LedgerRetentionDays bounds how long period documents are kept.
const LedgerRetentionDays = 400
