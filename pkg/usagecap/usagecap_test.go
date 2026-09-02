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
			if got := tt.r.Fresh(now, DefaultMaxAge); got != tt.want {
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
		if d := Preflight(nil, joPolicy(), now, DefaultMaxAge); d.Blocked {
			t.Fatal("blocked with no readings — an unmeasured deployment must not be stranded")
		}
	})

	t.Run("a soft cap also stops new work", func(t *testing.T) {
		d := Preflight([]Reading{reading(WindowFiveHour, 0.90, soon)}, joPolicy(), now, DefaultMaxAge)
		if !d.Blocked || d.Stop {
			t.Fatalf("blocked=%v stop=%v, want blocked without stop: soft means 'start nothing new'", d.Blocked, d.Stop)
		}
	})

	t.Run("stale readings are ignored", func(t *testing.T) {
		rolled := reading(WindowSevenDay, 0.99, now.Add(-time.Minute))
		if d := Preflight([]Reading{rolled}, joPolicy(), now, DefaultMaxAge); d.Blocked {
			t.Fatal("blocked on a window that already rolled over")
		}
	})

	t.Run("the latest reopening wins", func(t *testing.T) {
		d := Preflight([]Reading{
			reading(WindowFiveHour, 0.90, soon),
			reading(WindowSevenDay, 0.99, late),
		}, joPolicy(), now, DefaultMaxAge)
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
	d := Preflight([]Reading{{Window: WindowAuth, Status: StatusRejected, ObservedAt: time.Now()}}, pol, time.Now(), DefaultMaxAge)
	if d.Blocked {
		t.Fatal("an auth refusal must never block a launch through the usage cap — it is routing evidence, not quota")
	}
}
