package cloudpublisher

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/budgetfloor"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/secrets"
	"github.com/SocialGouv/iterion/pkg/store"
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

// The reserve that swallows the deployment's whole cap. This is where a
// clamped ceiling INVERTS the feature: usagecap reads MaxPercent 0 as "this
// window is not enforced" (WindowPolicy.Enabled), so lowering the ceiling to
// zero hands every unreserved bot an UNCAPPED credential — free to draw the
// shared subscription all the way to the provider wall, which is the exact
// starvation the reserve exists to prevent.
//
// Reachable with ordinary numbers: a 50% deployment cap and one 50% reserve
// (Validate only refuses reserves summing past 100), or an operator lowering
// the cap after the reservations were written.
func TestBudgetFloor_AReserveThatSwallowsTheCapHoldsOthersOffInsteadOfUncappingThem(t *testing.T) {
	res := budgetfloor.Reservation{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 50}}
	// 5% utilisation: far below every cap, so ONLY the reservation can be
	// what refuses — and a policy read as "unenforced" would admit everyone.
	p, key, scope := floorPublisher(t, 0.05, 50, res)
	for _, bot := range []string{"feature-dev", ""} {
		if p.apiKeyUsable(context.Background(), scope, "run", bot, nil)(key) {
			t.Errorf("bot %q drew on a credential whose whole window is reserved for review-pr — the cap was disabled, not lowered", bot)
		}
	}
	if !p.apiKeyUsable(context.Background(), scope, "run", "review-pr", nil)(key) {
		t.Fatal("the reserved bot was refused on its own reservation")
	}
	// And at a utilisation that IS over the deployment cap, the holder stops
	// with everyone else: the floor never becomes a way to overspend.
	over, key2, scope2 := floorPublisher(t, 0.95, 50, res)
	if over.apiKeyUsable(context.Background(), scope2, "run", "review-pr", nil)(key2) {
		t.Fatal("the reserved bot drew past the deployment's own cap")
	}
}

// A reserve holds a band of a provider WINDOW, so it can only hold back a
// credential whose window this deployment actually meters. An OpenAI key has
// no window ledger here (usageBackendForProvider maps it to ""), and refusing
// it because the ANTHROPIC five-hour band is reserved would apply an
// arithmetic that does not describe it — and would strand a run whose only
// other credential was never in the reserve's scope.
func TestBudgetFloor_AnUnmeteredProviderIsNeverHeldBack(t *testing.T) {
	p, _, scope := floorPublisher(t, 0.05, 50,
		budgetfloor.Reservation{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 50}})
	openai := secrets.ApiKey{Provider: secrets.ProviderOpenAI, Name: "metered", Fingerprint: "fp-openai"}
	if !p.apiKeyUsable(context.Background(), scope, "run", "feature-dev", nil)(openai) {
		t.Fatal("an OpenAI key was refused by a reserve on the Anthropic five-hour window")
	}
	// A key with no fingerprint names a slot, not an account: same rule.
	unstamped := secrets.ApiKey{Provider: secrets.ProviderAnthropic, Name: "unstamped"}
	if !p.apiKeyUsable(context.Background(), scope, "run", "feature-dev", nil)(unstamped) {
		t.Fatal("a key with no fingerprint was refused — nothing meters it, so nothing can hold it back")
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

// resolveFloorBundle runs the WHOLE credential walk for one bot and returns
// the sealed bundle a runner would open. The predicate-level tests above
// cannot see the walk's tail — and the tail is where a skip is undone.
func resolveFloorBundle(t *testing.T, p *Publisher, runID, tenant, botID string) secrets.RunBundle {
	t.Helper()
	rs, ok := p.runSecrets.(*secrets.MemoryRunSecretsStore)
	if !ok {
		t.Fatal("resolveFloorBundle needs a MemoryRunSecretsStore")
	}
	ctx := store.WithTenant(context.Background(), tenant)
	creds, err := p.resolveAndSealCredentials(ctx, runID, "", tenant, "owner1", botID, nil, nil, nil, model.ModelOverrides{}, nil)
	if err != nil {
		t.Fatalf("resolveAndSealCredentials: %v", err)
	}
	if creds.secretsRef == "" {
		return secrets.RunBundle{}
	}
	rec, err := rs.Get(ctx, creds.secretsRef)
	if err != nil {
		t.Fatalf("RunSecrets.Get: %v", err)
	}
	bundle, err := secrets.OpenRunBundle(p.sealer, runID, rec.SealedBundle)
	if err != nil {
		t.Fatalf("OpenRunBundle: %v", err)
	}
	return bundle
}

// The reserve has to survive the walk's TAIL, and this is where it nearly did
// not. A skipped credential is remembered and RESTORED when no other tier
// filled its wire — the right answer for a credential the provider refused
// (parking with a durable retry beats dying with no credential) and the exact
// wrong one for a credential a reservation holds back: with ONE shared
// subscription — the 2026-09-08 deployment this feature was built for —
// nothing else fills the wire, so the unreserved bot would be handed back the
// very credential held for the reviewer, and the whole feature would be inert
// while every unit test of the predicate stayed green.
func TestBudgetFloor_AHeldBackCredentialIsNotRestoredAtTheEndOfTheWalk(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	newPub := func(t *testing.T) *Publisher {
		t.Helper()
		keys := secrets.NewMemoryApiKeyStore()
		seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-shared", "fp-shared")
		caps := usagecap.NewMemStore()
		// 65% utilisation: over the ordinary ceiling (80 - 20), under the cap.
		if err := caps.Record(context.Background(),
			usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team1"), "fp-shared"),
			usagecap.Reading{Window: usagecap.WindowFiveHour, Status: usagecap.StatusAllowed,
				Utilization: 0.65, ObservedAt: time.Now(), ResetsAt: time.Now().Add(2 * time.Hour)}); err != nil {
			t.Fatalf("record: %v", err)
		}
		return &Publisher{
			apiKeys: keys, usageCaps: caps,
			runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
			logger: iterlog.New(iterlog.LevelError, nil),
			capPolicy: usagecap.StaticPolicy{
				FiveHour: usagecap.WindowPolicy{MaxPercent: 80, Mode: usagecap.ModeHard},
			},
			budgetFloor: budgetfloor.Static{Reservations: []budgetfloor.Reservation{
				{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 20}},
			}},
		}
	}

	if got := resolveFloorBundle(t, newPub(t), "run-ordinary", "team1", "feature-dev").APIKeys[secrets.ProviderAnthropic]; got != "" {
		t.Errorf("an unreserved bot was sealed the reserved credential (%q) — the skip was undone by the restore, and the reservation buys nothing", got)
	}
	if got := resolveFloorBundle(t, newPub(t), "run-reserved", "team1", "review-pr").APIKeys[secrets.ProviderAnthropic]; got != "sk-shared" {
		t.Errorf("the RESERVED bot was denied its own band: key = %q, want sk-shared", got)
	}
	// And with no reservation at all the tail is unchanged: a credential the
	// PROVIDER refused still comes back, because a parked run with a durable
	// retry beats one that dies on an empty wire.
	plain := newPub(t)
	plain.budgetFloor = nil
	recordRefusal(t, plain.usageCaps, usagecap.TenantScope("team1"), "fp-shared")
	if got := resolveFloorBundle(t, plain, "run-refused", "team1", "feature-dev").APIKeys[secrets.ProviderAnthropic]; got != "sk-shared" {
		t.Errorf("a provider-refused key was not restored (%q) — that restore is not the reserve's to cancel", got)
	}
}

// The mixed provider: one key the PROVIDER refused, one key the RESERVE holds
// back. The two skips are different answers and the restore owes each its own.
//
// The tracker's flag is what tells them apart, and keyed by provider it could
// not: the reserved key marked the whole provider held, so the DEAD key — the
// one actually queued for restore, and the one whose restore buys the run a
// park on a durable usage-window retry — was refused too, and the run went out
// with an empty wire to die on a no-credential auth error nothing retries. The
// reserve is entitled to withhold the credential it holds; it is not entitled
// to withhold a different one that happens to share a provider.
func TestBudgetFloor_ADeadSiblingKeyIsStillRestored(t *testing.T) {
	sealer, err := secrets.NewAESGCMSealer(make([]byte, 32))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	keys := secrets.NewMemoryApiKeyStore()
	// Seeded first ⇒ the older CreatedAt ⇒ the key an unfiltered resolve
	// prefers, so this is the one the restore step holds a candidate for.
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-dead", "fp-dead")
	seedKeyFP(t, keys, sealer, "team1", secrets.ProviderAnthropic, "sk-reserved", "fp-reserved")

	caps := usagecap.NewMemStore()
	// fp-dead: a fresh provider refusal — unusable by anyone, reserve or not.
	recordRefusal(t, caps, usagecap.TenantScope("team1"), "fp-dead")
	// fp-reserved: perfectly usable at 65%, but over the unreserved ceiling
	// (80 - 20) — held for review-pr, not spent.
	if err := caps.Record(context.Background(),
		usagecap.Key(delegate.BackendClaudeCode, usagecap.TenantScope("team1"), "fp-reserved"),
		usagecap.Reading{Window: usagecap.WindowFiveHour, Status: usagecap.StatusAllowed,
			Utilization: 0.65, ObservedAt: time.Now(), ResetsAt: time.Now().Add(2 * time.Hour)}); err != nil {
		t.Fatalf("record: %v", err)
	}

	p := &Publisher{
		apiKeys: keys, usageCaps: caps,
		runSecrets: secrets.NewMemoryRunSecretsStore(), sealer: sealer,
		logger: iterlog.New(iterlog.LevelError, nil),
		capPolicy: usagecap.StaticPolicy{
			FiveHour: usagecap.WindowPolicy{MaxPercent: 80, Mode: usagecap.ModeHard},
		},
		budgetFloor: budgetfloor.Static{Reservations: []budgetfloor.Reservation{
			{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 20}},
		}},
	}
	got := resolveFloorBundle(t, p, "run-mixed", "team1", "feature-dev").APIKeys[secrets.ProviderAnthropic]
	switch got {
	case "sk-dead":
		// Right: the dead key comes back, the run parks and retries.
	case "sk-reserved":
		t.Fatal("the RESERVED key was handed to an unreserved bot — the reserve leaked through the restore")
	default:
		t.Fatalf("anthropic wire = %q, want sk-dead restored: a key the PROVIDER refused is worth restoring, and the reserve holding a SIBLING key is not a reason to withhold it", got)
	}
}

// A window carrying a percentage but mode `off` is a guard the operator
// DISARMED — the shape `ITERION_USAGE_CAP=off` leaves behind over a stored
// percentage, and the one usagecap promises can never re-arm ("an overridden
// percentage can never re-arm a guard the operator explicitly disarmed").
// Reading MaxPercent instead of Enabled() would let a reservation re-arm
// exactly that guard and refuse every metered credential: a floor may hold
// work back, it may never invent a ceiling.
func TestBudgetFloor_ADisarmedWindowStaysDisarmed(t *testing.T) {
	p, key, scope := floorPublisher(t, 0.99, 80,
		budgetfloor.Reservation{BotID: "review-pr", Reserve: budgetfloor.Reserve{FiveHourPercent: 80}})
	p.capPolicy = usagecap.StaticPolicy{
		// A percentage the operator switched off: Preflight can never block.
		FiveHour: usagecap.WindowPolicy{MaxPercent: 80, Mode: usagecap.ModeOff},
	}
	for _, bot := range []string{"feature-dev", ""} {
		if !p.apiKeyUsable(context.Background(), scope, "run", bot, nil)(key) {
			t.Errorf("bot %q was refused on a window the operator disarmed — the reservation re-armed the kill switch", bot)
		}
	}
}
