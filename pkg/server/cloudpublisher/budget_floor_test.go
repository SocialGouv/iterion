package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// floorPublisher builds a walk with an operator cap and a set of reservations,
// over one credential whose window sits at `utilization`.
func floorPublisher(t *testing.T, utilization float64, capPct float64, res ...budgetfloor.Reservation) (*Publisher, secrets.ApiKey, string) {
	t.Helper()
	st := usagecap.NewMemStore()
	scope := usagecap.TenantScope("team")
	key := secrets.ApiKey{Provider: secrets.ProviderAnthropic, Name: "shared", Fingerprint: "fp-shared"}
	if err := st.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, scope, key.Fingerprint),
		usagecap.Reading{
			Window: usagecap.WindowFiveHour, Status: usagecap.StatusAllowed,
			Utilization: utilization, ObservedAt: time.Now(),
			ResetsAt: time.Now().Add(2 * time.Hour),
		}); err != nil {
		t.Fatalf("record: %v", err)
	}
	p := &Publisher{
		usageCaps: st,
		logger:    iterlog.New(iterlog.LevelError, nil),
		capPolicy: usagecap.StaticPolicy{
			FiveHour: usagecap.WindowPolicy{MaxPercent: capPct, Mode: usagecap.ModeHard},
		},
	}
	if len(res) > 0 {
		p.budgetFloor = budgetfloor.Static{Reservations: res}
	}
	return p, key, scope
}

// The whole feature, at the only place it can be observed: ONE credential, ONE
// set of readings, and two bots that get different answers.
//
// Without a reservation both bots see the same credential — which is exactly
// the production failure of 2026-09-08: campaign bots drove the shared
// subscription's five-hour window to the cap and the reviewer, holding
// nothing, stopped with them.
func TestBudgetFloor_TheReservedWorkloadKeepsACredentialTheOthersHaveLost(t *testing.T) {
	const cap, reserve = 80.0, 20 // ordinary work stops at 60
	reservation := budgetfloor.Reservation{
		BotID:   "review-pr",
		Reserve: budgetfloor.Reserve{FiveHourPercent: reserve},
	}

	t.Run("inside the reserved band, only the reserved bot may draw", func(t *testing.T) {
		// 65% — above the ordinary ceiling (60), below the cap (80).
		p, key, scope := floorPublisher(t, 0.65, cap, reservation)
		if !p.apiKeyUsable(context.Background(), scope, "run-a", "review-pr", nil)(key) {
			t.Fatal("the RESERVED bot was refused inside its own band — the reservation is protecting the workload from itself")
		}
		if p.apiKeyUsable(context.Background(), scope, "run-b", "feature-dev", nil)(key) {
			t.Fatal("an unreserved bot drew inside the reserved band — nothing is actually held back")
		}
		if p.apiKeyUsable(context.Background(), scope, "run-c", "", nil)(key) {
			t.Fatal("a run with no bot drew inside the reserved band — 'unknown' must not read as 'reserved'")
		}
	})

	t.Run("below the ordinary ceiling everyone draws", func(t *testing.T) {
		p, key, scope := floorPublisher(t, 0.30, cap, reservation)
		for _, bot := range []string{"review-pr", "feature-dev", ""} {
			if !p.apiKeyUsable(context.Background(), scope, "run", bot, nil)(key) {
				t.Fatalf("bot %q was refused at 30%% utilisation, far below every ceiling", bot)
			}
		}
	})

	t.Run("past the deployment cap nobody draws, reserved included", func(t *testing.T) {
		// A reservation holds capacity back from others; it never grants its
		// holder more than the deployment allows itself.
		p, key, scope := floorPublisher(t, 0.95, cap, reservation)
		if p.apiKeyUsable(context.Background(), scope, "run-a", "review-pr", nil)(key) {
			t.Fatal("the reserved bot drew past the deployment's own cap — a floor must not become a way to overspend")
		}
	})

	t.Run("with no reservation the walk is unchanged", func(t *testing.T) {
		// The regression guard: shipping this must not move a deployment that
		// configured nothing.
		p, key, scope := floorPublisher(t, 0.65, cap)
		for _, bot := range []string{"review-pr", "feature-dev", ""} {
			if !p.apiKeyUsable(context.Background(), scope, "run", bot, nil)(key) {
				t.Fatalf("bot %q refused at 65%% under an 80%% cap with NO reservation configured", bot)
			}
		}
	})
}

// A deployment that never set a usage cap must not acquire one by writing a
// reservation. Without this guard the subtraction produces a positive
// MaxPercent out of nothing, usagecap reads it as enforcement, and every run
// on a capless deployment starts being refused.
func TestBudgetFloor_AReservationDoesNotCreateACap(t *testing.T) {
	p, key, scope := floorPublisher(t, 0.99, 0, // cap 0 = the family is not enforced
		budgetfloor.Reservation{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 20}})
	for _, bot := range []string{"review-pr", "feature-dev", ""} {
		if !p.apiKeyUsable(context.Background(), scope, "run", bot, nil)(key) {
			t.Fatalf("bot %q was refused at 99%% on a deployment with NO usage cap — the reservation invented a ceiling", bot)
		}
	}
}

// Two reservations must compose rather than one voiding the other, and the
// arithmetic has to survive the trip through usagecap's policy.
func TestBudgetFloor_TwoReservationsCompose(t *testing.T) {
	res := []budgetfloor.Reservation{
		{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 20}},
		{BotID: "feature-dev", Reserve: budgetfloor.Reserve{FiveHourPercent: 10}},
	}
	// 65%: above ordinary (50) and above feature-dev's ceiling (60), below
	// review-pr's (70).
	p, key, scope := floorPublisher(t, 0.65, 80, res...)
	if !p.apiKeyUsable(context.Background(), scope, "r", "review-pr", nil)(key) {
		t.Error("review-pr refused at 65% though its ceiling is 70 (80 - feature-dev's 10)")
	}
	if p.apiKeyUsable(context.Background(), scope, "r", "feature-dev", nil)(key) {
		t.Error("feature-dev drew at 65% though its ceiling is 60 (80 - review-pr's 20)")
	}
	if p.apiKeyUsable(context.Background(), scope, "r", "docs-refresh", nil)(key) {
		t.Error("an unreserved bot drew at 65% though its ceiling is 50 (80 - 30 held between the two)")
	}
}
