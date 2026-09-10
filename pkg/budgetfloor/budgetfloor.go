// Package budgetfloor reserves capacity for a named workload — the one thing
// iterion's budget machinery could not express.
//
// Every existing mechanism is a CEILING. `max_cost_usd`, the org monthly caps
// (pkg/orgusage), the per-credential meters (pkg/credusage), the window caps
// (pkg/usagecap), the team concurrency and launch-rate caps, a pool pledge's
// spend/day and runs/day — all of them answer *"how much may this stop at?"*.
// None answers *"how much is reserved for this, whatever else runs?"*.
//
// The difference is not academic. Measured 2026-09-08: campaign bots and the
// PR reviewer shared one Anthropic subscription; when its five-hour window
// closed at 06:14Z every claude_code run was refused, eight runs parked in six
// minutes and fourteen within the hour. No cap had been exceeded and no quota
// breached — each individual run stayed under its own ceiling all the way
// down. Review simply had nothing held for it.
//
// # Why the reserve lives on the provider's window by default
//
// On a subscription the provider bills nothing per call: pkg/credusage types
// those dollar figures `estimate` precisely because they are not an invoice.
// Reserving "$X for the reviewer" on a forfait reserves a fiction — the run
// that dies does so because the five-hour window is spent, not because a
// dollar figure was reached. The scarce thing is the window, so that is the
// axis the default reservation holds.
//
// The other two axes are offered because they are the honest answer in cases
// the window is not: MonthlyUSD is real money on a metered key, and
// ConcurrentRuns protects responsiveness rather than quota. An operator may
// set any subset; each is enforced independently, at the gate that already
// enforces its ceiling.
//
// # Composition
//
// A workload's ceiling is the deployment cap MINUS the reserves of every
// OTHER workload. With reserves A=20 and B=10 under a cap of 80: ordinary
// work stops at 50, A may reach 70, B may reach 60. Each workload is
// protected from all the others and from none of itself — which is what makes
// two reservations compose instead of one silently voiding the other.
//
// The package is a LEAF (stdlib only) so the gates that consult it —
// cloudpublisher's credential walk, the launch gate — can import it without
// inverting the graph, the same rule pkg/credusage and pkg/modelspecs follow.
package budgetfloor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Window names a provider usage window a reserve can be expressed against.
// The strings match pkg/usagecap's own, so an operator reads one vocabulary.
type Window string

const (
	WindowFiveHour Window = "five_hour"
	WindowWeek     Window = "week"
)

// Reserve is the capacity held back for one workload, on any subset of three
// axes. Every axis zero means "no reservation" — the zero value reserves
// NOTHING, so a policy an operator has not filled in cannot start refusing
// work by accident.
type Reserve struct {
	// FiveHourPercent / WeekPercent are points of the provider's own window
	// held back from every other workload. This is the DEFAULT axis: on a
	// subscription it is the only one that measures the thing that actually
	// runs out.
	FiveHourPercent int `bson:"five_hour_percent,omitempty" json:"five_hour_percent,omitempty"`
	WeekPercent     int `bson:"week_percent,omitempty" json:"week_percent,omitempty"`
	// MonthlyUSD holds a slice of the tenant's monthly cost cap. Real money
	// on a metered key; an estimate on a forfait — where the window axis
	// above is the one that bites.
	MonthlyUSD float64 `bson:"monthly_usd,omitempty" json:"monthly_usd,omitempty"`
	// ConcurrentRuns holds simultaneous-run slots. Protects responsiveness
	// (the reviewer answers a PR while a campaign runs), not quota: slots on
	// an exhausted window buy nothing.
	ConcurrentRuns int `bson:"concurrent_runs,omitempty" json:"concurrent_runs,omitempty"`
}

// Empty reports a reserve that holds nothing on any axis.
func (r Reserve) Empty() bool {
	return r.FiveHourPercent == 0 && r.WeekPercent == 0 && r.MonthlyUSD == 0 && r.ConcurrentRuns == 0
}

// WindowPercent returns the points held on one window.
func (r Reserve) WindowPercent(w Window) int {
	switch w {
	case WindowFiveHour:
		return r.FiveHourPercent
	case WindowWeek:
		return r.WeekPercent
	}
	return 0
}

// Countable reports whether the reserve holds anything a per-repo share can
// be computed against. A window percentage is NOT countable: slicing a live
// five-hour window between repositories would need real-time arbitration
// across replicas, and a share of it computed locally would be a number two
// pods disagree about.
func (r Reserve) Countable() bool { return r.MonthlyUSD > 0 || r.ConcurrentRuns > 0 }

func (r Reserve) Validate() error {
	for _, f := range []struct {
		name string
		pct  int
	}{{"five_hour_percent", r.FiveHourPercent}, {"week_percent", r.WeekPercent}} {
		if f.pct < 0 || f.pct > 100 {
			return fmt.Errorf("budgetfloor: %s must be in [0,100], got %d", f.name, f.pct)
		}
	}
	if r.MonthlyUSD < 0 {
		return fmt.Errorf("budgetfloor: monthly_usd cannot be negative")
	}
	if r.ConcurrentRuns < 0 {
		return fmt.Errorf("budgetfloor: concurrent_runs cannot be negative")
	}
	return nil
}

// Reservation binds a reserve to the workload it protects.
//
// The workload is a BOT ID — the thing that actually spends, already carried
// on every run (RunMessage.BotID) and already the vocabulary a credential-pool
// pledge uses for its allow-list. No indirection to resolve at admission, and
// no new concept to document.
//
// The cost of that choice, stated because it is silent: renaming or replacing
// the bot leaves the reservation pointing at an id nothing launches, and it
// then protects nothing. Re-point it by hand.
type Reservation struct {
	BotID   string  `bson:"bot_id" json:"bot_id"`
	Reserve Reserve `bson:"reserve" json:"reserve"`
	// Note is the operator's own words for why this exists, surfaced wherever
	// a refusal cites the reservation.
	Note string `bson:"note,omitempty" json:"note,omitempty"`
}

func (r Reservation) Validate() error {
	if strings.TrimSpace(r.BotID) == "" {
		return fmt.Errorf("budgetfloor: a reservation names no bot")
	}
	return r.Reserve.Validate()
}

// RepoQuota caps ONE repository inside the global budget — the other half of
// the ask, and a ceiling rather than a floor: a floor answers "what is held
// for this", a quota answers "how far may this one go".
//
// MonthlyUSD is the default axis: it reads directly off the repository
// dimension in pkg/credusage and answers "this repo is eating the
// subscription" with a number.
type RepoQuota struct {
	// Repo is the forge slug (store.Run.ProjectPath) — the same identity the
	// meter keys on, never a second one derived from a clone URL.
	Repo string `bson:"repo" json:"repo"`
	// MonthlyUSD caps the repository's metered + estimated spend for the
	// month. Default axis.
	MonthlyUSD float64 `bson:"monthly_usd,omitempty" json:"monthly_usd,omitempty"`
	// RunsPerMonth caps attempts instead of amount — insensitive to a forfait
	// billing nothing, at the price of counting a trivial run like an
	// expensive one.
	RunsPerMonth int `bson:"runs_per_month,omitempty" json:"runs_per_month,omitempty"`
	// ReserveSharePercent expresses the cap as a share of a workload's
	// reservation instead of an absolute. Resolved against that reservation's
	// COUNTABLE axes; a window-only reserve cannot serve it (see
	// Reserve.Countable) and Validate refuses the pair rather than silently
	// resolving to zero — which would read as "this repo may spend nothing".
	ReserveSharePercent int `bson:"reserve_share_percent,omitempty" json:"reserve_share_percent,omitempty"`
	// ShareOfBot names which reservation ReserveSharePercent slices.
	ShareOfBot string `bson:"share_of_bot,omitempty" json:"share_of_bot,omitempty"`
}

func (q RepoQuota) Validate() error {
	if strings.TrimSpace(q.Repo) == "" {
		return fmt.Errorf("budgetfloor: a repo quota names no repository")
	}
	if q.MonthlyUSD < 0 || q.RunsPerMonth < 0 {
		return fmt.Errorf("budgetfloor: repo quota ceilings cannot be negative")
	}
	if q.ReserveSharePercent < 0 || q.ReserveSharePercent > 100 {
		return fmt.Errorf("budgetfloor: reserve_share_percent must be in [0,100], got %d", q.ReserveSharePercent)
	}
	if q.ReserveSharePercent > 0 && strings.TrimSpace(q.ShareOfBot) == "" {
		return fmt.Errorf("budgetfloor: repo %q takes a share of a reservation but names no bot (share_of_bot)", q.Repo)
	}
	return nil
}

// Empty reports a quota that caps nothing.
func (q RepoQuota) Empty() bool {
	return q.MonthlyUSD == 0 && q.RunsPerMonth == 0 && q.ReserveSharePercent == 0
}

// Policy is the deployment's whole set of reservations and repo quotas. Its
// zero value reserves and caps nothing, so a deployment that never configured
// one behaves exactly as before.
type Policy struct {
	Reservations []Reservation `bson:"reservations,omitempty" json:"reservations,omitempty"`
	RepoQuotas   []RepoQuota   `bson:"repo_quotas,omitempty" json:"repo_quotas,omitempty"`
	// UpdatedAt is the compare-and-set token the platform-settings store
	// writes on every save. Present because the whole policy is replaced as
	// a unit: without it two admins editing at once would silently drop one
	// another's reservations under ReplaceOne semantics.
	UpdatedAt time.Time `bson:"updated_at,omitempty" json:"updated_at,omitempty"`
}

// Validate checks the whole policy, including the cross-references a single
// record cannot see.
func (p Policy) Validate() error {
	seen := map[string]bool{}
	byBot := map[string]Reservation{}
	for _, r := range p.Reservations {
		if err := r.Validate(); err != nil {
			return err
		}
		bot := strings.TrimSpace(r.BotID)
		if seen[bot] {
			// Two reservations for one bot would compose against each other
			// through OtherReserved below — the workload would be refused on
			// its own band.
			return fmt.Errorf("budgetfloor: bot %q has more than one reservation", bot)
		}
		seen[bot] = true
		byBot[bot] = r
	}
	// A reserve cannot hold more of a window than the deployment could ever
	// use. Checked across ALL reservations, because it is their SUM that
	// ordinary work is refused against.
	for _, w := range []Window{WindowFiveHour, WindowWeek} {
		total := 0
		for _, r := range p.Reservations {
			total += r.Reserve.WindowPercent(w)
		}
		if total > 100 {
			return fmt.Errorf("budgetfloor: reservations hold %d%% of the %s window between them, which leaves ordinary work a negative ceiling", total, w)
		}
	}
	repos := map[string]bool{}
	for _, q := range p.RepoQuotas {
		if err := q.Validate(); err != nil {
			return err
		}
		repo := strings.TrimSpace(q.Repo)
		if repos[repo] {
			return fmt.Errorf("budgetfloor: repository %q has more than one quota", repo)
		}
		repos[repo] = true
		if q.ReserveSharePercent > 0 {
			res, ok := byBot[strings.TrimSpace(q.ShareOfBot)]
			if !ok {
				return fmt.Errorf("budgetfloor: repository %q takes a share of bot %q, which has no reservation", repo, q.ShareOfBot)
			}
			if !res.Reserve.Countable() {
				// Refused rather than resolved to zero: a share of a window
				// reserve is not a small number, it is an undefined one.
				return fmt.Errorf("budgetfloor: repository %q takes %d%% of %q's reservation, but that reservation holds only window percentages — "+
					"a share of a live provider window cannot be computed without real-time arbitration between replicas. "+
					"Give %q a monthly_usd or concurrent_runs reserve, or cap the repository with monthly_usd / runs_per_month directly",
					repo, q.ReserveSharePercent, q.ShareOfBot, q.ShareOfBot)
			}
		}
	}
	return nil
}

// Reserved returns the reservation protecting a bot, if any.
func (p Policy) Reserved(botID string) (Reservation, bool) {
	bot := strings.TrimSpace(botID)
	if bot == "" {
		return Reservation{}, false
	}
	for _, r := range p.Reservations {
		if r.BotID == bot {
			return r, true
		}
	}
	return Reservation{}, false
}

// OtherReserved sums what every workload OTHER than botID holds on a window.
//
// This is the whole composition rule: a workload is protected from all the
// others and from none of itself. Summing every reservation instead would
// refuse a workload on its own band — the reservation would make its holder
// stop earlier, which is precisely backwards.
//
// An empty botID (a run with no bot: a plain .bot launch) is nobody's
// workload, so it faces the full sum.
func (p Policy) OtherReserved(botID string, w Window) int {
	bot := strings.TrimSpace(botID)
	total := 0
	for _, r := range p.Reservations {
		if r.BotID == bot {
			continue
		}
		total += r.Reserve.WindowPercent(w)
	}
	return total
}

// OtherReservedSlots sums the concurrency slots every workload OTHER than
// botID holds. Same composition rule as OtherReserved, on the axis that
// protects responsiveness rather than quota.
func (p Policy) OtherReservedSlots(botID string) int {
	bot := strings.TrimSpace(botID)
	total := 0
	for _, r := range p.Reservations {
		if r.BotID == bot {
			continue
		}
		total += r.Reserve.ConcurrentRuns
	}
	return total
}

// OtherReservedUSD sums the monthly spend every workload OTHER than botID
// holds — the axis that is real money on a metered key and an estimate on a
// forfait (see the package doc).
func (p Policy) OtherReservedUSD(botID string) float64 {
	bot := strings.TrimSpace(botID)
	total := 0.0
	for _, r := range p.Reservations {
		if r.BotID == bot {
			continue
		}
		total += r.Reserve.MonthlyUSD
	}
	return total
}

// WindowCeiling is the utilisation percentage at which THIS bot must stop
// drawing on a credential, given the deployment's own cap for that window.
//
// `cap` is pkg/usagecap's MaxPercent — 0 meaning the family is not enforced,
// in which case there is nothing to subtract a reserve from and the answer is
// 0 (unenforced) rather than a negative ceiling that would refuse everything.
// That case is the reason this returns the cap untouched instead of
// `cap - reserved`: a deployment that never set a usage cap must not acquire
// one by configuring a reservation.
func (p Policy) WindowCeiling(botID string, w Window, cap int) int {
	if cap <= 0 {
		return cap
	}
	ceiling := cap - p.OtherReserved(botID, w)
	if ceiling < 0 {
		// Validate refuses a policy that sums past 100, but a cap LOWER than
		// the reserves is a legitimate runtime combination (an operator drops
		// the cap without touching the reservations). Clamp at zero: the
		// reserved workload keeps the whole remaining cap, everyone else is
		// held off entirely, which is what the reservation asked for.
		return 0
	}
	return ceiling
}

// RepoCap resolves a repository's effective ceilings for the month. The
// returned amounts are zero when unlimited on that axis.
//
// A share-based quota is resolved HERE rather than stored pre-multiplied, so
// raising a reservation raises every repository that takes a share of it —
// the property that makes shares worth having.
func (p Policy) RepoCap(repo string) (monthlyUSD float64, runsPerMonth int) {
	name := strings.TrimSpace(repo)
	if name == "" {
		// Spend that named no repository is not "every repository": a run
		// with no repo cannot be capped by one, and folding it into some
		// default bucket would charge it to a repository that never ran it.
		return 0, 0
	}
	for _, q := range p.RepoQuotas {
		if q.Repo != name {
			continue
		}
		monthlyUSD, runsPerMonth = q.MonthlyUSD, q.RunsPerMonth
		if q.ReserveSharePercent > 0 {
			if res, ok := p.Reserved(q.ShareOfBot); ok {
				if share := res.Reserve.MonthlyUSD * float64(q.ReserveSharePercent) / 100; share > 0 {
					// The tighter of the two wins when both are set: an
					// explicit absolute is an operator's deliberate floor
					// under a share, not a second opinion to average.
					if monthlyUSD == 0 || share < monthlyUSD {
						monthlyUSD = share
					}
				}
			}
		}
		return monthlyUSD, runsPerMonth
	}
	return 0, 0
}

// Static is a Policy that never changes — the shape a deployment configured
// from the environment has, and the one a test can state outright. The
// runtime-mutable form is a platformcfg resolver over the same type.
type Static Policy

// Get matches the platformcfg.Resolver shape every consumer reads, so a
// static policy and a runtime-mutable one are interchangeable at the seam.
func (s Static) Get(context.Context) *Policy { p := Policy(s); return &p }

// Bots lists every reserved bot id, sorted — for the operator views, which
// must not reorder between two reads.
func (p Policy) Bots() []string {
	out := make([]string, 0, len(p.Reservations))
	for _, r := range p.Reservations {
		out = append(out, r.BotID)
	}
	sort.Strings(out)
	return out
}
