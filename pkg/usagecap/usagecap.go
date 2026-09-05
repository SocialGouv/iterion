// Package usagecap stops a deployment from spending its LLM subscription
// down to the provider's own wall.
//
// A subscription ("forfait") meters two rolling windows — five hours and
// seven days — and refuses every call once one is exhausted. iterion already
// survives that refusal: the run parks and a durable retry resumes it when
// the window reopens (pkg/runner/usage_retry.go). What it could not do is
// stop BEFORE the wall, and the wall is not where an operator wants to be:
// the same subscription usually pays for their own interactive work, so a
// fleet of bots that drives it to 100% takes the human down with it.
//
// So this package reads the provider's own telemetry — the utilization each
// window reports on every call — and enforces a percentage the operator
// chose. Two enforcement postures, because the two windows fail differently:
//
//   - SOFT (the five-hour window's default): never interrupt work in flight,
//     but start nothing new. A five-hour window refills soon; killing a
//     half-finished run to save minutes of quota trades a lot for a little.
//   - HARD (the weekly window's default): stop the run where it stands. A
//     weekly window that runs out on a Tuesday is a dead week — the run that
//     would have finished is worth less than the four days of headroom it
//     would have eaten.
//
// Both postures end the same way for the run: the caller turns a Stop
// decision into the same usage-window error the provider itself would have
// produced, so the existing park-and-resume machinery carries it home
// untouched. A capped run is not a lost run; it is a run that waits.
//
// Nothing here talks to a provider. Readings are pushed in by whoever
// observes them (the claude_code stream carries a rate_limit_event), and
// pushed out to a Store so a pod that is about to start work can consult
// what another pod learned.
package usagecap

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Window is one of the provider's metered usage windows, named as the
// provider names it on the wire.
type Window string

const (
	// WindowFiveHour is the rolling five-hour session window.
	WindowFiveHour Window = "five_hour"
	// WindowSevenDay is the rolling weekly window, all models.
	WindowSevenDay Window = "seven_day"
	// WindowSevenDayOpus / WindowSevenDaySonnet are per-model weekly
	// sub-limits: a run can be refused on one of them while the
	// all-models window still has room, so they are capped as members of
	// the weekly family rather than ignored.
	WindowSevenDayOpus   Window = "seven_day_opus"
	WindowSevenDaySonnet Window = "seven_day_sonnet"
	// WindowSevenDayOverageIncluded is the weekly window as measured with
	// the paid overage channel folded in.
	WindowSevenDayOverageIncluded Window = "seven_day_overage_included"
	// WindowOverage is the metered pay-as-you-go channel that some plans
	// can spill into. Deliberately NOT capped here: it is money, not
	// subscription quota, and the budget flags (--max-cost-usd) are what
	// bound money.
	WindowOverage Window = "overage"
	// WindowFrequency is not a provider window at all: it is an
	// account-level refusal of the REQUEST RATE (a fair-usage policy, a
	// frequency restriction) relayed as an error rather than as window
	// telemetry, so it never carries a reset instant. It exists as a
	// window name so the refusal can be recorded as meter evidence: the
	// credential-tier skip needs a fresh StatusRejected reading to route
	// around a credential the provider will not serve, and freshness for
	// a reading with no reset instant is already bounded by ObservedAt.
	WindowFrequency Window = "frequency"
	// WindowSpend is not a provider window either: it is the ACCOUNT's
	// money ceiling, set by its own admin ("You've hit your org's monthly
	// spend limit · ask your admin to raise it"), relayed as text rather
	// than as window telemetry. Distinct from WindowOverage — that is a
	// channel the plan may spill INTO, this is the wall it stops at — and
	// from WindowFrequency, which refuses the request RATE while the
	// budget is intact. It carries no reset instant (a human raises the
	// ceiling, or the calendar month rolls), so freshness is bounded by
	// ObservedAt like the other two refusals, and the credential-tier
	// skip is the consumer: a credential whose org budget is spent serves
	// NOTHING, so the resolver must route around it instead of feeding
	// every node of every run into the same wall.
	WindowSpend Window = "spend"
	// WindowAuth is not a provider window either: it is the provider
	// REJECTING THE CREDENTIAL ITSELF — a dead token, an expired OAuth
	// record, a malformed secret. Like WindowFrequency it exists as a
	// window name so the refusal can be recorded as meter evidence: the
	// credential-tier skip is the only consumer, and without this
	// evidence a structurally-broken credential keeps filling its slot
	// on every re-resolution, gating the pool and platform tiers off
	// behind a credential that can never serve (five consecutive
	// dead-on-arrival fleets were the lived cost). No reset instant —
	// a dead credential does not heal on a schedule — so freshness is
	// bounded by ObservedAt, giving a cheap periodic re-probe in case
	// an operator rotated the secret in place.
	WindowAuth Window = "auth"
)

// Family groups the windows that share one operator-facing cap. An
// operator thinks in "my 5h" and "my week", not in six wire names.
type Family string

const (
	FamilyFiveHour Family = "5h"
	FamilyWeek     Family = "week"
	// FamilyAccount governs no operator cap today (Policy.For of an
	// unconfigured family is inert), but it is NOT FamilyNone: a
	// frequency refusal is real provider evidence, and the consumers
	// that filter evidence on "does a cap family govern this window"
	// must see it.
	FamilyAccount Family = "account"
	// FamilyCredential mirrors FamilyAccount for WindowAuth: no
	// operator cap governs it (an unconfigured family is inert in the
	// guard), but it is real provider evidence the credential-tier
	// skip must see — FamilyNone would filter it out at the consumer.
	FamilyCredential Family = "credential"
	// FamilyNone marks a window no cap applies to.
	FamilyNone Family = ""
)

// FamilyOf maps a wire window onto the cap that governs it.
func FamilyOf(w Window) Family {
	switch w {
	case WindowFiveHour:
		return FamilyFiveHour
	case WindowSevenDay, WindowSevenDayOpus, WindowSevenDaySonnet, WindowSevenDayOverageIncluded:
		return FamilyWeek
	case WindowFrequency, WindowSpend:
		return FamilyAccount
	case WindowAuth:
		return FamilyCredential
	default:
		// Includes WindowOverage and any window a future CLI adds: an
		// unknown window is not silently folded into a cap that was never
		// meant to govern it.
		return FamilyNone
	}
}

// Mode is how hard a cap bites.
type Mode string

const (
	// ModeOff records readings but never blocks. The zero value, so a
	// zero Policy is inert — a cap nobody configured must never surprise
	// anyone.
	ModeOff Mode = "off"
	// ModeSoft blocks work that has not started; work in flight runs on.
	ModeSoft Mode = "soft"
	// ModeHard additionally stops work in flight at the next observation.
	ModeHard Mode = "hard"
)

// ParseMode reads an operator-supplied mode, case- and space-insensitive.
// An empty string yields the supplied fallback; anything unrecognised is an
// error rather than a silent downgrade — a typo'd "hrad" that quietly
// disabled the guard is exactly the failure this package exists to prevent.
func ParseMode(s string, fallback Mode) (Mode, error) {
	switch v := Mode(strings.ToLower(strings.TrimSpace(s))); v {
	case "":
		return fallback, nil
	case ModeOff, ModeSoft, ModeHard:
		return v, nil
	default:
		return fallback, fmt.Errorf("usagecap: unknown mode %q (want off|soft|hard)", s)
	}
}

// WindowPolicy is the cap for one family.
type WindowPolicy struct {
	// MaxPercent is the utilization ceiling, 0–100. Zero means no cap,
	// whatever Mode says.
	MaxPercent float64
	// Mode is the enforcement posture at and above MaxPercent.
	Mode Mode
}

// Enabled reports whether this policy can ever block.
func (p WindowPolicy) Enabled() bool {
	return p.MaxPercent > 0 && (p.Mode == ModeSoft || p.Mode == ModeHard)
}

// Policy is the full operator configuration.
type Policy struct {
	FiveHour WindowPolicy
	Week     WindowPolicy
}

// For returns the policy governing a family (zero value for FamilyNone).
func (p Policy) For(f Family) WindowPolicy {
	switch f {
	case FamilyFiveHour:
		return p.FiveHour
	case FamilyWeek:
		return p.Week
	default:
		return WindowPolicy{}
	}
}

// Enabled reports whether any cap can block.
func (p Policy) Enabled() bool { return p.FiveHour.Enabled() || p.Week.Enabled() }

// String renders the policy for a log line.
func (p Policy) String() string {
	if !p.Enabled() {
		return "usage caps off"
	}
	part := func(name string, wp WindowPolicy) string {
		if !wp.Enabled() {
			return name + "=off"
		}
		return fmt.Sprintf("%s=%.0f%%/%s", name, wp.MaxPercent, wp.Mode)
	}
	return part("5h", p.FiveHour) + " " + part("week", p.Week)
}

// Reading is one observation of one window, as the provider reported it.
type Reading struct {
	// Window is the provider's own window name.
	Window Window
	// Utilization is the fraction consumed, 0..1 — the scale the provider
	// uses. Percent() is the operator-facing form.
	Utilization float64
	// Status is the provider's own verdict: allowed, allowed_warning or
	// rejected. Carried through so a "rejected" can block even when the
	// utilization number is missing.
	Status string
	// ResetsAt is when this window rolls over. Zero when the provider did
	// not say, which is what makes a reading unbounded in time — see
	// Fresh.
	ResetsAt time.Time
	// ObservedAt is when iterion saw the reading.
	ObservedAt time.Time
	// Source is the delegate's provider-routing label for the session
	// that produced this reading (providerFingerprint: "facade:<url>",
	// "anthropic-direct", "anthropic-oauth", "anthropic-env"). It lets
	// the publisher key the reading under the credential the node
	// ACTUALLY spent — a node pinned `provider: anthropic` must not
	// charge its refusal to the z.ai key sharing the bundle. Empty on
	// readings from older binaries; consumers fall back to the bundle's
	// default precedence then. Never carries a secret.
	Source string
	// Refusals is how many times IN A ROW this credential was refused on
	// this window, counted by the STORE — a caller is one pod that saw one
	// refusal; only the ledger can tell a blip from an account frozen for
	// days. Counted only for a refusal with NO reset instant (the
	// account-level ones: auth, frequency, spend), because those are the
	// readings nothing but the staleness bound ever expires. A served call
	// resets it to zero, which is what keeps the escalating rest
	// self-healing rather than a one-way lock. Zero on legacy readings.
	Refusals int
}

// Provider status values.
const (
	StatusAllowed  = "allowed"
	StatusWarning  = "allowed_warning"
	StatusRejected = "rejected"
)

// Percent is the utilization on the 0–100 scale operators configure in.
func (r Reading) Percent() float64 { return r.Utilization * 100 }

// Trust bounds how long a reading is BELIEVED, on two axes.
//
// A reading is a measurement of a window at one instant, and the premise
// "a window's utilization cannot drop before its reset" is FALSE for the
// provider: it has reset every window early, out of cycle. A reading
// trusted for its whole window then locks the credential out until a
// reset that already happened — and nothing can correct it, because the
// only writer of a fresh reading is a live session, which the lock itself
// prevents (measured: two forfaits at 0% skipped for four days, the merge
// gate blocked behind them). So a reading is authoritative only for a
// bounded time after it was observed; past that it is suggestive, the
// gate lets the credential through, and the next session's own
// rate_limit_event re-establishes the truth within one call.
type Trust struct {
	// MaxAge bounds a reading that carries no reset instant (a refusal
	// relayed as text, a dead credential): with no window end to expire
	// at, this is the only thing that stops it from being immortal.
	MaxAge time.Duration
	// Window bounds a DATED reading — the trust window. Past it the number
	// is a memory of the window, not a measurement of it. It does not
	// apply to a reading with no reset instant: the window exists because
	// a dated window can roll over early, which an account-level refusal
	// cannot do.
	Window time.Duration
	// MaxRefusalRest bounds how far MaxAge may be stretched for a
	// credential refused several times in a row (see Reading.RestBound).
	// Zero means DefaultMaxRefusalRest; NEGATIVE turns the escalation off
	// (ITERION_USAGE_CAP_REFUSAL_REST_MAX=off), which is why Normalized
	// only fills the zero value.
	MaxRefusalRest time.Duration
}

// DefaultMaxAge bounds how long a reading with no reset instant is trusted.
// Short enough that a wrong block heals within one five-hour window, long
// enough to cover the gap between two runs on a quiet deployment.
const DefaultMaxAge = time.Hour

// DefaultTrustWindow bounds how long any reading is trusted after it was
// observed (ITERION_USAGE_CAP_TRUST_WINDOW overrides it). Long enough that
// a deployment running a session every few hours never re-probes a window
// it just measured; short enough that an early provider reset costs hours
// of idle subscription, not days.
const DefaultTrustWindow = 3 * time.Hour

// DefaultMaxRefusalRest bounds the escalating rest a repeatedly-refused
// credential earns. Chosen so an account frozen for days is probed four
// times a day instead of twenty-four, while an operator who rotates a dead
// token IN PLACE (same fingerprint, so the meter does not reset) still sees
// it picked up within a working part of a day. `iterion remote admin
// usage-readings clear <fingerprint>` cuts the wait short on demand.
const DefaultMaxRefusalRest = 6 * time.Hour

// DefaultTrust is the bound every enforcement point applies when the
// operator set nothing.
func DefaultTrust() Trust {
	return Trust{MaxAge: DefaultMaxAge, Window: DefaultTrustWindow, MaxRefusalRest: DefaultMaxRefusalRest}
}

// Normalized fills the zero value with the defaults, so a caller holding
// no operator value (a test, an unset config) still applies a bound. A
// NEGATIVE MaxRefusalRest is preserved: it is how "escalation off" travels.
func (t Trust) Normalized() Trust {
	if t.MaxAge <= 0 {
		t.MaxAge = DefaultMaxAge
	}
	if t.Window <= 0 {
		t.Window = DefaultTrustWindow
	}
	if t.MaxRefusalRest == 0 {
		t.MaxRefusalRest = DefaultMaxRefusalRest
	}
	return t
}

// RestBound is how long THIS reading is believed when it carries no reset
// instant: the staleness bound, doubled once per consecutive refusal and
// capped at Trust.MaxRefusalRest.
//
// Without it a credential the provider has frozen — a dead token, an
// account whose fair-usage limiter is shut, an org past its spend ceiling —
// is re-probed every MaxAge forever: one wasted pod and one parked run per
// hour, for days, on a condition only a human can end. Escalating trades
// that for a slower re-probe, and the ceiling is what keeps it a REST and
// not a lock: the streak ends the moment the credential serves a call.
func (r Reading) RestBound(t Trust) time.Duration {
	t = t.Normalized()
	base := t.MaxAge
	if r.Status != StatusRejected || r.Refusals <= 1 || t.MaxRefusalRest <= base {
		return base
	}
	rest := base
	for i := 1; i < r.Refusals && rest < t.MaxRefusalRest; i++ {
		rest *= 2
	}
	if rest > t.MaxRefusalRest {
		rest = t.MaxRefusalRest
	}
	return rest
}

// Fresh reports whether a reading still describes the current window AND
// is recent enough to be believed (see Trust).
//
// A reading dies at its own reset instant: past it the window has rolled
// over and the old number describes a window that no longer exists. It
// also dies at the end of the trust window, however far its reset is:
// that is what keeps a pre-reset reading from locking a credential out
// through a reset the provider made early.
//
// A reading with no reset instant cannot expire at a rollover, so it falls
// back to RestBound (MaxAge, stretched by the refusal streak). Without that
// fallback an undated reading would be immortal — and the trust window does
// NOT bound it, because that window exists for readings whose provider
// window can reset early, which an account-level refusal cannot do; capping
// the rest at the trust window would make the escalation inert.
func (r Reading) Fresh(now time.Time, t Trust) bool {
	if r.ObservedAt.IsZero() {
		return false
	}
	t = t.Normalized()
	age := now.Sub(r.ObservedAt)
	if r.ResetsAt.IsZero() {
		return age < r.RestBound(t)
	}
	if age >= t.Window {
		return false
	}
	return now.Before(r.ResetsAt)
}

// Decision is what a policy says about a reading (or a set of them).
type Decision struct {
	// Blocked is true when a cap is met or exceeded: no NEW work should
	// start against this credential.
	Blocked bool
	// Stop is true when the cap that fired is HARD: work already in
	// flight must end too. Never true without Blocked.
	Stop bool
	// Window / Family / Percent / Cap describe the cap that fired.
	Window  Window
	Family  Family
	Percent float64
	Cap     float64
	// ResetsAt is when the blocking window reopens — the instant the
	// caller arms a retry for. Zero when the provider did not say.
	ResetsAt time.Time
	// Reason is a one-line human explanation, safe for a log line, a run
	// error and an event payload.
	Reason string
}

// evaluate applies a policy to a single reading.
func evaluate(r Reading, pol Policy) Decision {
	fam := FamilyOf(r.Window)
	wp := pol.For(fam)
	if !wp.Enabled() {
		return Decision{}
	}
	pct := r.Percent()
	// A provider that has already refused is over any cap the operator
	// could have set, whatever the utilization number says (it is
	// optional on the wire, so "rejected with no number" is a real shape).
	rejected := r.Status == StatusRejected
	if !rejected && pct < wp.MaxPercent {
		return Decision{}
	}
	d := Decision{
		Blocked:  true,
		Stop:     wp.Mode == ModeHard,
		Window:   r.Window,
		Family:   fam,
		Percent:  pct,
		Cap:      wp.MaxPercent,
		ResetsAt: r.ResetsAt,
	}
	switch {
	case rejected && pct == 0:
		d.Reason = fmt.Sprintf("usage cap: provider rejected on the %s window (%s cap %.0f%%, %s)",
			r.Window, fam, wp.MaxPercent, wp.Mode)
	default:
		d.Reason = fmt.Sprintf("usage cap: %s window at %.0f%% ≥ %.0f%% (%s, %s)",
			r.Window, pct, wp.MaxPercent, fam, wp.Mode)
	}
	if !r.ResetsAt.IsZero() {
		d.Reason += ", resets " + r.ResetsAt.UTC().Format(time.RFC3339)
	}
	return d
}

// Guard is the per-process enforcement point. It remembers the latest
// reading per window, evaluates each new one against the policy, and
// publishes what it learns so other processes can pre-flight against it.
//
// The policy is consulted through a PolicySource on EVERY evaluation, so a
// runtime settings change (the DB-backed record) reaches a guard already
// watching a long run within the source's TTL — no restart, no re-launch.
//
// Safe for concurrent use: readings arrive on a backend's stream goroutine
// while the run executes elsewhere.
type Guard struct {
	src   PolicySource
	sink  func(Reading)
	mu    sync.Mutex
	seen  map[Window]Reading
	fired bool
}

// NewGuard builds a guard over a fixed policy. sink, when non-nil,
// receives every reading — the seam the runner uses to publish into a
// shared Store. It is called on the observing goroutine and must not
// block.
func NewGuard(pol Policy, sink func(Reading)) *Guard {
	return NewGuardWithSource(StaticPolicy(pol), sink)
}

// NewGuardWithSource builds a guard that re-reads its policy per
// evaluation — the cloud runner passes the DB-backed Resolver here.
func NewGuardWithSource(src PolicySource, sink func(Reading)) *Guard {
	if src == nil {
		src = StaticPolicy(Policy{})
	}
	return &Guard{src: src, sink: sink, seen: map[Window]Reading{}}
}

// Policy returns the guard's current effective configuration.
func (g *Guard) Policy() Policy {
	if g == nil {
		return Policy{}
	}
	return g.src.Effective(context.Background())
}

// Observe records a reading and reports what the policy makes of it. A nil
// guard observes nothing and blocks nothing, so callers need no nil check.
func (g *Guard) Observe(r Reading) Decision {
	if g == nil {
		return Decision{}
	}
	if r.ObservedAt.IsZero() {
		r.ObservedAt = time.Now().UTC()
	}
	g.mu.Lock()
	g.seen[r.Window] = r
	g.mu.Unlock()

	// Publish before deciding: a reading that stops THIS run is exactly
	// the one the next pod most needs, and a Stop decision unwinds the
	// caller fast.
	if g.sink != nil {
		g.sink(r)
	}

	d := evaluate(r, g.src.Effective(context.Background()))
	if d.Stop {
		g.mu.Lock()
		g.fired = true
		g.mu.Unlock()
	}
	return d
}

// Fired reports whether a hard cap has stopped this guard's work. Lets a
// caller distinguish "the run failed" from "iterion stopped the run".
func (g *Guard) Fired() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fired
}

// Latest returns the readings seen so far, newest per window.
func (g *Guard) Latest() []Reading {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Reading, 0, len(g.seen))
	for _, r := range g.seen {
		out = append(out, r)
	}
	return out
}

// Preflight answers "may new work start against this credential" from
// previously stored readings. It is the cheap gate: no pod, no clone, no
// call. Stale readings are ignored (see Reading.Fresh and Trust), so a
// deployment that has not run in hours is never blocked by what it learned
// then — the next session re-measures.
//
// When several windows block, the one that reopens LAST wins: coming back
// before every blocking window has reopened would just park the run again.
func Preflight(readings []Reading, pol Policy, now time.Time, trust Trust) Decision {
	if !pol.Enabled() {
		return Decision{}
	}
	var worst Decision
	for _, r := range readings {
		if !r.Fresh(now, trust) {
			continue
		}
		d := evaluate(r, pol)
		if !d.Blocked {
			continue
		}
		switch {
		case !worst.Blocked:
			worst = d
		case d.ResetsAt.After(worst.ResetsAt):
			worst = d
		}
	}
	return worst
}
