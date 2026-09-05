package usagecap

import (
	"strings"
	"testing"
	"time"
)

func reading(w Window, util float64, resets time.Time) Reading {
	return Reading{
		Window:      w,
		Utilization: util,
		Status:      StatusAllowed,
		ResetsAt:    resets,
		ObservedAt:  time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC),
	}
}

// Jo's shipped posture: 85% soft on the five-hour window, 75% hard on the
// weekly one.
func joPolicy() Policy {
	return Policy{
		FiveHour: WindowPolicy{MaxPercent: 85, Mode: ModeSoft},
		Week:     WindowPolicy{MaxPercent: 75, Mode: ModeHard},
	}
}

func TestFamilyOf(t *testing.T) {
	tests := []struct {
		window Window
		want   Family
	}{
		{WindowFiveHour, FamilyFiveHour},
		{WindowSevenDay, FamilyWeek},
		// The per-model weekly sub-limits are real walls: a run refused on
		// the opus weekly limit is refused, whatever the all-models number
		// says. They belong to the weekly cap.
		{WindowSevenDayOpus, FamilyWeek},
		{WindowSevenDaySonnet, FamilyWeek},
		{WindowSevenDayOverageIncluded, FamilyWeek},
		// Overage is money, not subscription quota — the budget flags bound
		// it, not this package.
		{WindowOverage, FamilyNone},
		{Window("something_new"), FamilyNone},
	}
	for _, tt := range tests {
		if got := FamilyOf(tt.window); got != tt.want {
			t.Errorf("FamilyOf(%q) = %q, want %q", tt.window, got, tt.want)
		}
	}
}

func TestObserve_SoftAndHardPostures(t *testing.T) {
	resets := time.Date(2026, 8, 18, 21, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		reading     Reading
		wantBlocked bool
		wantStop    bool
	}{
		{"5h under its cap", reading(WindowFiveHour, 0.80, resets), false, false},
		{"5h at its cap blocks softly", reading(WindowFiveHour, 0.85, resets), true, false},
		{"5h far over stays soft", reading(WindowFiveHour, 0.99, resets), true, false},
		{"week under its cap", reading(WindowSevenDay, 0.74, resets), false, false},
		{"week at its cap stops the run", reading(WindowSevenDay, 0.75, resets), true, true},
		{"weekly opus sub-limit stops too", reading(WindowSevenDayOpus, 0.92, resets), true, true},
		{"overage is not capped here", reading(WindowOverage, 1.0, resets), false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGuard(joPolicy(), nil)
			d := g.Observe(tt.reading)
			if d.Blocked != tt.wantBlocked || d.Stop != tt.wantStop {
				t.Fatalf("blocked=%v stop=%v, want blocked=%v stop=%v (reason %q)",
					d.Blocked, d.Stop, tt.wantBlocked, tt.wantStop, d.Reason)
			}
			if d.Stop != g.Fired() {
				t.Errorf("Fired() = %v, want %v", g.Fired(), d.Stop)
			}
			if d.Blocked {
				if d.ResetsAt != resets {
					t.Errorf("ResetsAt = %v, want %v — the retry instant must survive the decision", d.ResetsAt, resets)
				}
				if !strings.Contains(d.Reason, "usage cap") {
					t.Errorf("Reason = %q, want it to name the cap", d.Reason)
				}
			}
		})
	}
}

// A rejected status carries no utilization number of its own. Reading it as
// 0% would let the guard wave through the one case it exists for.
func TestObserve_RejectedWithoutUtilization(t *testing.T) {
	g := NewGuard(joPolicy(), nil)
	d := g.Observe(Reading{
		Window:     WindowSevenDay,
		Status:     StatusRejected,
		ObservedAt: time.Now().UTC(),
	})
	if !d.Blocked || !d.Stop {
		t.Fatalf("blocked=%v stop=%v, want both true for a provider rejection", d.Blocked, d.Stop)
	}
}

func TestObserve_DisabledPolicyNeverBlocks(t *testing.T) {
	for _, pol := range []Policy{
		{},
		{Week: WindowPolicy{MaxPercent: 75, Mode: ModeOff}},
		{Week: WindowPolicy{MaxPercent: 0, Mode: ModeHard}},
	} {
		g := NewGuard(pol, nil)
		if d := g.Observe(reading(WindowSevenDay, 1.0, time.Time{})); d.Blocked {
			t.Fatalf("policy %+v blocked at 100%% — an unconfigured cap must be inert", pol)
		}
	}
}

func TestObserve_PublishesEveryReading(t *testing.T) {
	var got []Reading
	g := NewGuard(joPolicy(), func(r Reading) { got = append(got, r) })
	g.Observe(reading(WindowFiveHour, 0.10, time.Time{}))
	g.Observe(reading(WindowSevenDay, 0.99, time.Time{}))
	if len(got) != 2 {
		t.Fatalf("published %d readings, want 2 — the one that stops a run is the one the next pod needs", len(got))
	}
}

func TestObserve_StampsObservedAt(t *testing.T) {
	g := NewGuard(Policy{}, nil)
	g.Observe(Reading{Window: WindowFiveHour, Utilization: 0.1})
	latest := g.Latest()
	if len(latest) != 1 || latest[0].ObservedAt.IsZero() {
		t.Fatalf("latest = %+v, want one reading with a non-zero ObservedAt", latest)
	}
}

func TestReadingFresh(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		r    Reading
		want bool
	}{
		{"never observed", Reading{}, false},
		{"window still open", Reading{ObservedAt: now.Add(-time.Hour), ResetsAt: now.Add(time.Hour)}, true},
		// Past its reset the window has rolled over: the number describes a
		// window that no longer exists, so it must stop blocking by itself.
		{"window rolled over", Reading{ObservedAt: now.Add(-2 * time.Hour), ResetsAt: now.Add(-time.Minute)}, false},
		{"undated but recent", Reading{ObservedAt: now.Add(-time.Minute)}, true},
		{"undated and stale", Reading{ObservedAt: now.Add(-2 * time.Hour)}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.r.Fresh(now, DefaultTrust()); got != tt.want {
				t.Errorf("Fresh = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPreflight(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	soon := now.Add(2 * time.Hour)
	late := now.Add(30 * time.Hour)

	t.Run("nothing measured yet", func(t *testing.T) {
		if d := Preflight(nil, joPolicy(), now, DefaultTrust()); d.Blocked {
			t.Fatal("blocked with no readings — an unmeasured deployment must not be stranded")
		}
	})

	t.Run("a soft cap also stops new work", func(t *testing.T) {
		d := Preflight([]Reading{reading(WindowFiveHour, 0.90, soon)}, joPolicy(), now, DefaultTrust())
		if !d.Blocked || d.Stop {
			t.Fatalf("blocked=%v stop=%v, want blocked without stop: soft means 'start nothing new'", d.Blocked, d.Stop)
		}
	})

	t.Run("stale readings are ignored", func(t *testing.T) {
		rolled := reading(WindowSevenDay, 0.99, now.Add(-time.Minute))
		if d := Preflight([]Reading{rolled}, joPolicy(), now, DefaultTrust()); d.Blocked {
			t.Fatal("blocked on a window that already rolled over")
		}
	})

	t.Run("the latest reopening wins", func(t *testing.T) {
		d := Preflight([]Reading{
			reading(WindowFiveHour, 0.90, soon),
			reading(WindowSevenDay, 0.99, late),
		}, joPolicy(), now, DefaultTrust())
		if !d.Blocked {
			t.Fatal("want blocked")
		}
		// Coming back at the 5h reset would just park the run again on the
		// weekly window.
		if !d.ResetsAt.Equal(late) {
			t.Fatalf("ResetsAt = %v, want the last window to reopen (%v)", d.ResetsAt, late)
		}
	})
}

func TestPolicyString(t *testing.T) {
	if got := (Policy{}).String(); got != "usage caps off" {
		t.Errorf("String() = %q", got)
	}
	got := joPolicy().String()
	if !strings.Contains(got, "5h=85%/soft") || !strings.Contains(got, "week=75%/hard") {
		t.Errorf("String() = %q, want both caps and their modes", got)
	}
}

func TestNilGuardIsInert(t *testing.T) {
	var g *Guard
	if d := g.Observe(reading(WindowSevenDay, 1.0, time.Time{})); d.Blocked {
		t.Fatal("a nil guard blocked")
	}
	if g.Fired() || g.Latest() != nil || g.Policy().Enabled() {
		t.Fatal("a nil guard must answer as an absent cap, so callers need no nil check")
	}
}

// WindowAuth is credential-tier evidence, never an operator cap: FamilyOf
// must expose it to the evidence consumers (non-None) while an ordinary
// policy — which configures no "credential" family — must stay inert on
// it, so an auth-dead credential parks NOTHING through the guard.
func TestWindowAuth_evidenceNotCap(t *testing.T) {
	if FamilyOf(WindowAuth) == FamilyNone {
		t.Fatal("FamilyOf(WindowAuth) = FamilyNone — the evidence consumers would filter it out")
	}
	pol := Policy{FiveHour: WindowPolicy{MaxPercent: 85, Mode: ModeSoft}, Week: WindowPolicy{MaxPercent: 85, Mode: ModeHard}}
	d := Preflight([]Reading{{Window: WindowAuth, Status: StatusRejected, ObservedAt: time.Now()}}, pol, time.Now(), DefaultTrust())
	if d.Blocked {
		t.Fatal("an auth refusal must never block a launch through the usage cap — it is routing evidence, not quota")
	}
}

// WindowSpend exists for ONE consumer: the credential-tier skip, which is
// gated on FamilyOf(window) != FamilyNone. Drop the case (or let the
// window fall to default) and every spend refusal is filtered out at that
// consumer — the feature goes inert with nothing failing. Sibling of
// TestWindowAuth_evidenceNotCap, for the same reason.
func TestWindowSpend_evidenceNotCap(t *testing.T) {
	if got := FamilyOf(WindowSpend); got != FamilyAccount {
		t.Fatalf("FamilyOf(WindowSpend) = %q, want %q — FamilyNone would make the evidence consumers drop it", got, FamilyAccount)
	}
	// It governs no operator cap: an account ceiling is the provider's
	// wall, not a percentage iterion stops itself at.
	if p := (Policy{}).For(FamilyAccount); p.Enabled() {
		t.Fatalf("FamilyAccount must stay uncapped (the provider's wall, not a percentage we stop at), got %+v", p)
	}
}

// #690 — a dated reading is trusted for a bounded time, not for its whole
// window. The provider reset every window early on 2026-09-04; readings
// taken before it (95%, resets three days out) kept every walk skipping
// both forfaits for four days, and the only writer of a fresh reading is a
// live session, which the skip itself prevented. Past the trust window a
// reading is suggestive, not authoritative: the walk lets the tier through
// and the next session's rate_limit_event re-establishes the truth.
func TestReadingFresh_TrustWindowBoundsADatedReading(t *testing.T) {
	now := time.Date(2026, 9, 4, 20, 20, 0, 0, time.UTC)
	trust := Trust{MaxAge: time.Hour, Window: 2 * time.Hour}
	threeDays := now.Add(72 * time.Hour)

	// Blind judge: 95%, reset three days out, observed six hours ago —
	// older than the trust window, so NOT fresh.
	stale := Reading{Window: WindowSevenDay, Utilization: 0.95, Status: StatusAllowed, ResetsAt: threeDays, ObservedAt: now.Add(-6 * time.Hour)}
	if stale.Fresh(now, trust) {
		t.Fatal("a reading older than the trust window is still trusted — the self-sustaining lock of #690")
	}
	// No hysteria: the same reading observed ten minutes ago still holds.
	recent := stale
	recent.ObservedAt = now.Add(-10 * time.Minute)
	if !recent.Fresh(now, trust) {
		t.Fatal("a recent reading inside its window must stay fresh")
	}
	// The reset instant still ends a reading early, whatever its age.
	rolled := recent
	rolled.ResetsAt = now.Add(-time.Minute)
	if rolled.Fresh(now, trust) {
		t.Fatal("a reading past its reset instant must not be fresh")
	}
	// A zero Trust means the defaults (MaxAge 1h, Window 3h), so every
	// caller that has no operator value keeps a bounded trust.
	if !recent.Fresh(now, Trust{}) {
		t.Fatal("zero Trust must normalise to the defaults, not to 'never fresh'")
	}
	if (Reading{Window: WindowSevenDay, Utilization: 0.95, ResetsAt: threeDays, ObservedAt: now.Add(-(DefaultTrustWindow + time.Minute))}).Fresh(now, Trust{}) {
		t.Fatalf("zero Trust must bound a dated reading at DefaultTrustWindow (%s)", DefaultTrustWindow)
	}
}

// The same two cases through Preflight — the gate the runner and the
// publisher's walk actually consult.
func TestPreflight_StaleDatedReadingDoesNotBlock(t *testing.T) {
	now := time.Date(2026, 9, 4, 20, 20, 0, 0, time.UTC)
	trust := Trust{MaxAge: time.Hour, Window: 2 * time.Hour}
	pol := Policy{Week: WindowPolicy{MaxPercent: 85, Mode: ModeHard}}
	threeDays := now.Add(72 * time.Hour)

	stale := Reading{Window: WindowSevenDay, Utilization: 0.95, Status: StatusAllowed, ResetsAt: threeDays, ObservedAt: now.Add(-6 * time.Hour)}
	if d := Preflight([]Reading{stale}, pol, now, trust); d.Blocked {
		t.Fatalf("blocked on a six-hour-old reading under a 2h trust window: %s", d.Reason)
	}
	recent := stale
	recent.ObservedAt = now.Add(-10 * time.Minute)
	d := Preflight([]Reading{recent}, pol, now, trust)
	if !d.Blocked || !d.Stop {
		t.Fatalf("a ten-minute-old 95%% reading must still block (hard): blocked=%v stop=%v", d.Blocked, d.Stop)
	}
	if !d.ResetsAt.Equal(threeDays) {
		t.Fatalf("ResetsAt = %v, want %v", d.ResetsAt, threeDays)
	}
}

// The operator knob: ITERION_USAGE_CAP_TRUST_WINDOW. Unset → the default;
// a malformed or non-positive value is an error — the package's standing
// rule that a typo must never silently change enforcement — and FromEnv
// refuses to start on it like on every other cap variable, kill switch
// included.
func TestTrustFromEnv(t *testing.T) {
	got, err := TrustFromEnv()
	if err != nil || got != DefaultTrust() {
		t.Fatalf("unset: got %+v, %v; want the defaults", got, err)
	}
	t.Setenv(EnvTrustWindow, "90m")
	got, err = TrustFromEnv()
	if err != nil || got.Window != 90*time.Minute || got.MaxAge != DefaultMaxAge {
		t.Fatalf("90m: got %+v, %v", got, err)
	}
	for _, bad := range []string{"soon", "0", "-1h", "3"} {
		t.Setenv(EnvTrustWindow, bad)
		if _, err := TrustFromEnv(); err == nil {
			t.Errorf("TrustFromEnv accepted %q", bad)
		}
		if _, err := FromEnv(); err == nil {
			t.Errorf("FromEnv accepted %s=%q — a malformed trust window must refuse to start", EnvTrustWindow, bad)
		}
		t.Setenv(EnvEnabled, "off")
		if _, err := FromEnv(); err == nil {
			t.Errorf("FromEnv accepted %s=%q under the kill switch — the trust window also governs the credential-skip evidence, which the switch does not disarm", EnvTrustWindow, bad)
		}
		t.Setenv(EnvEnabled, "")
	}
}
