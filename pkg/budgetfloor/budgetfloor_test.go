package budgetfloor

import (
	"strings"
	"testing"
)

// The composition rule is the whole design, and it is the one an
// implementation gets backwards for free: summing EVERY reservation (instead
// of every OTHER) refuses a workload on its own band, so the reservation makes
// its holder stop EARLIER than before it existed — the exact opposite of a
// floor, and green under any test that only checks ordinary work.
func TestWindowCeiling_AWorkloadIsProtectedFromOthersAndNotFromItself(t *testing.T) {
	p := Policy{Reservations: []Reservation{
		{BotID: "review-pr", Reserve: Reserve{FiveHourPercent: 20}},
		{BotID: "feature-dev", Reserve: Reserve{FiveHourPercent: 10}},
	}}
	if err := p.Validate(); err != nil {
		t.Fatalf("policy: %v", err)
	}
	for _, tc := range []struct {
		bot  string
		want float64
		why  string
	}{
		{"review-pr", 70, "its own 20 points stay available to it; only feature-dev's 10 are held back"},
		{"feature-dev", 60, "review-pr's 20 are held back from it"},
		{"whole-improve-loop", 50, "an unreserved bot faces the sum of both bands"},
		{"", 50, "a run with no bot is nobody's workload and faces the full sum"},
	} {
		got, held := p.WindowCeiling(tc.bot, WindowFiveHour, 80)
		if held {
			t.Errorf("WindowCeiling(%q) reported the window entirely reserved under an 80%% cap — %s", tc.bot, tc.why)
			continue
		}
		if got != tc.want {
			t.Errorf("WindowCeiling(%q) = %.0f, want %.0f — %s", tc.bot, got, tc.want, tc.why)
		}
	}
}

// A deployment that never set a usage cap must not acquire one by configuring
// a reservation: there is nothing to subtract from, and returning `0 - reserve`
// would refuse every run on a deployment that had no cap at all.
func TestWindowCeiling_NoCapMeansNoCeiling(t *testing.T) {
	p := Policy{Reservations: []Reservation{{BotID: "review-pr", Reserve: Reserve{FiveHourPercent: 20}}}}
	got, held := p.WindowCeiling("anything", WindowFiveHour, 0)
	if got != 0 || held {
		t.Fatalf("WindowCeiling with no cap = %.0f (held=%v), want 0/false (unenforced) — a reservation must not create a cap", got, held)
	}
}

// An operator can lower the cap below the reserves without touching them (and
// Validate cannot catch it — it knows the 100% window, never the deployment's
// own cap). The unreserved workload is then held off ENTIRELY, and that is the
// one answer a ceiling cannot carry: usagecap reads MaxPercent 0 as "this
// window is not enforced", so returning 0 would UNCAP exactly the workloads
// the reserve holds back. It comes back as held=true, and the caller refuses
// the credential instead of lowering a ceiling.
func TestWindowCeiling_CapBelowTheReservesIsHeldNotZero(t *testing.T) {
	p := Policy{Reservations: []Reservation{{BotID: "review-pr", Reserve: Reserve{FiveHourPercent: 40}}}}
	if _, held := p.WindowCeiling("feature-dev", WindowFiveHour, 30); !held {
		t.Fatal("unreserved work under a cap below the reserve was handed a ceiling — 0 means UNENFORCED downstream, so this must be reported as held")
	}
	// Exactly equal is the same answer: nothing is left for anyone else.
	if _, held := p.WindowCeiling("feature-dev", WindowFiveHour, 40); !held {
		t.Fatal("a reserve exactly equal to the cap left the window enforced-at-0 instead of held")
	}
	got, held := p.WindowCeiling("review-pr", WindowFiveHour, 30)
	if held || got != 30 {
		t.Fatalf("the reserved workload's ceiling = %.0f (held=%v), want the whole remaining cap 30 — a reservation never costs its own holder", got, held)
	}
}

// The zero value must reserve NOTHING: a deployment that never configured a
// policy cannot start refusing work because the feature shipped.
func TestZeroPolicyReservesNothing(t *testing.T) {
	var p Policy
	if err := p.Validate(); err != nil {
		t.Fatalf("the zero policy must be valid: %v", err)
	}
	got, held := p.WindowCeiling("review-pr", WindowFiveHour, 80)
	if held || got != 80 {
		t.Fatalf("zero policy ceiling = %.0f (held=%v), want the cap 80 untouched", got, held)
	}
	usd, runs := p.RepoCap("SocialGouv/iterion")
	if usd != 0 || runs != 0 {
		t.Fatalf("zero policy repo cap = $%.2f / %d runs, want unlimited on both", usd, runs)
	}
}

func TestValidate_RefusesPoliciesThatCannotMeanAnything(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Policy
	}{
		{"a reservation with no bot", Policy{Reservations: []Reservation{{Reserve: Reserve{FiveHourPercent: 10}}}}},
		{"two reservations for one bot", Policy{Reservations: []Reservation{
			{BotID: "review-pr", Reserve: Reserve{FiveHourPercent: 10}},
			{BotID: "review-pr", Reserve: Reserve{WeekPercent: 10}},
		}}},
		{"reserves summing past the window", Policy{Reservations: []Reservation{
			{BotID: "a", Reserve: Reserve{FiveHourPercent: 60}},
			{BotID: "b", Reserve: Reserve{FiveHourPercent: 50}},
		}}},
		{"a percentage above 100", Policy{Reservations: []Reservation{{BotID: "a", Reserve: Reserve{WeekPercent: 101}}}}},
		{"a negative amount", Policy{Reservations: []Reservation{{BotID: "a", Reserve: Reserve{MonthlyUSD: -1}}}}},
		{"a quota with no repository", Policy{RepoQuotas: []RepoQuota{{MonthlyUSD: 10}}}},
		{"two quotas for one repository", Policy{RepoQuotas: []RepoQuota{
			{Repo: "o/r", MonthlyUSD: 10}, {Repo: "o/r", RunsPerMonth: 5},
		}}},
		{"a share of a bot that has no reservation", Policy{RepoQuotas: []RepoQuota{
			{Repo: "o/r", ReserveSharePercent: 50, ShareOfBot: "nobody"},
		}}},
		{"a share naming no bot", Policy{RepoQuotas: []RepoQuota{
			{Repo: "o/r", ReserveSharePercent: 50},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.p.Validate(); err == nil {
				t.Fatal("accepted a policy that cannot mean anything")
			}
		})
	}
}

// A share of a WINDOW reserve is not a small number, it is an undefined one:
// slicing a live five-hour window between repositories needs real-time
// arbitration across replicas. Refused at configuration time, where the
// operator can still choose another axis — resolving it to zero would read as
// "this repository may spend nothing" and silently stop its runs.
func TestValidate_RefusesAShareOfAWindowOnlyReserve(t *testing.T) {
	p := Policy{
		Reservations: []Reservation{{BotID: "review-pr", Reserve: Reserve{FiveHourPercent: 20}}},
		RepoQuotas:   []RepoQuota{{Repo: "o/r", ReserveSharePercent: 50, ShareOfBot: "review-pr"}},
	}
	err := p.Validate()
	if err == nil {
		t.Fatal("accepted a repository share of a window-only reserve")
	}
	// The message has to name the way OUT, not only the refusal.
	for _, want := range []string{"monthly_usd", "runs_per_month"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not point at %q as an alternative: %v", want, err)
		}
	}

	// The same share against a countable reserve is fine, and resolves.
	p.Reservations[0].Reserve.MonthlyUSD = 400
	if err := p.Validate(); err != nil {
		t.Fatalf("a share of a countable reserve must be accepted: %v", err)
	}
	if usd, _ := p.RepoCap("o/r"); usd != 200 {
		t.Fatalf("50%% of a $400 reserve = $%.2f, want $200", usd)
	}
}

// Raising the reservation raises every repository that takes a share of it —
// the property that makes a share worth having over a pre-multiplied absolute.
func TestRepoCap_ASharedQuotaFollowsItsReservation(t *testing.T) {
	p := Policy{
		Reservations: []Reservation{{BotID: "review-pr", Reserve: Reserve{MonthlyUSD: 100}}},
		RepoQuotas:   []RepoQuota{{Repo: "o/r", ReserveSharePercent: 25, ShareOfBot: "review-pr"}},
	}
	if usd, _ := p.RepoCap("o/r"); usd != 25 {
		t.Fatalf("25%% of $100 = $%.2f, want $25", usd)
	}
	p.Reservations[0].Reserve.MonthlyUSD = 400
	if usd, _ := p.RepoCap("o/r"); usd != 100 {
		t.Fatalf("after raising the reserve to $400, the share = $%.2f, want $100", usd)
	}
}

// An absolute set beside a share is a deliberate floor under it, not a second
// opinion: the tighter of the two wins.
func TestRepoCap_AnAbsoluteBesideAShareTakesTheTighter(t *testing.T) {
	p := Policy{
		Reservations: []Reservation{{BotID: "review-pr", Reserve: Reserve{MonthlyUSD: 400}}},
		RepoQuotas: []RepoQuota{
			{Repo: "tight", MonthlyUSD: 50, ReserveSharePercent: 50, ShareOfBot: "review-pr"},  // share=200, absolute=50
			{Repo: "loose", MonthlyUSD: 500, ReserveSharePercent: 25, ShareOfBot: "review-pr"}, // share=100, absolute=500
		},
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("policy: %v", err)
	}
	if usd, _ := p.RepoCap("tight"); usd != 50 {
		t.Fatalf("tight = $%.2f, want the absolute $50", usd)
	}
	if usd, _ := p.RepoCap("loose"); usd != 100 {
		t.Fatalf("loose = $%.2f, want the share $100", usd)
	}
}

// Spend that named no repository is not "every repository": a run with no repo
// cannot be capped by one, and folding it into a default bucket would charge it
// to a repository that never ran it.
func TestRepoCap_TheEmptyRepositoryIsNotAWildcard(t *testing.T) {
	p := Policy{RepoQuotas: []RepoQuota{{Repo: "o/r", MonthlyUSD: 10}}}
	if usd, runs := p.RepoCap(""); usd != 0 || runs != 0 {
		t.Fatalf("RepoCap(\"\") = $%.2f / %d, want unlimited — it must not inherit another repo's quota", usd, runs)
	}
	if usd, _ := p.RepoCap("o/other"); usd != 0 {
		t.Fatalf("an unquotaed repository = $%.2f, want unlimited", usd)
	}
}
