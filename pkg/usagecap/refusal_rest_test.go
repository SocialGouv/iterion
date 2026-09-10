package usagecap

import (
	"testing"
	"time"
)

// A refusal with no reset instant re-probes on the staleness bound alone,
// so a credential the provider has frozen for days was asked again every
// hour, forever. The rest escalates with the streak instead.
func TestReading_RestBoundEscalatesWithConsecutiveRefusals(t *testing.T) {
	trust := DefaultTrust()
	cases := []struct {
		refusals int
		want     time.Duration
	}{
		{0, DefaultMaxAge},     // legacy readings carry no count
		{1, DefaultMaxAge},     // one refusal: the credential may just have blipped
		{2, 2 * DefaultMaxAge}, // twice in a row: rest longer
		{3, 4 * DefaultMaxAge},
		{4, DefaultMaxRefusalRest}, // bounded — an operator rotating in place must still be noticed
		{50, DefaultMaxRefusalRest},
	}
	for _, c := range cases {
		r := Reading{Window: WindowAuth, Status: StatusRejected, Refusals: c.refusals}
		if got := r.RestBound(trust); got != c.want {
			t.Errorf("RestBound(refusals=%d) = %s, want %s", c.refusals, got, c.want)
		}
	}
	// Only a REFUSAL rests: an allowed reading carrying a stale count is
	// bounded by the ordinary staleness age.
	ok := Reading{Window: WindowFiveHour, Status: StatusAllowed, Refusals: 9}
	if got := ok.RestBound(trust); got != DefaultMaxAge {
		t.Errorf("RestBound(allowed) = %s, want the plain staleness bound %s", got, DefaultMaxAge)
	}
}

// The escalation has to reach Fresh, or nothing changes for the consumers.
func TestReading_FreshHonoursTheEscalatedRest(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	trust := DefaultTrust()
	// Four hours old, refused four times in a row: still believed.
	frozen := Reading{
		Window: WindowAuth, Status: StatusRejected, Refusals: 4,
		ObservedAt: now.Add(-4 * time.Hour),
	}
	if !frozen.Fresh(now, trust) {
		t.Fatal("a credential refused four times in a row must rest longer than one hour")
	}
	// Past the ceiling it lapses, so a rotation in place is noticed.
	if frozen.Fresh(now.Add(3*time.Hour), trust) {
		t.Fatal("the rest must be bounded: a re-probe has to happen eventually")
	}
	// A single refusal keeps the one-hour bound.
	blip := Reading{
		Window: WindowFrequency, Status: StatusRejected, Refusals: 1,
		ObservedAt: now.Add(-90 * time.Minute),
	}
	if blip.Fresh(now, trust) {
		t.Fatal("a single refusal must still lapse at the staleness bound")
	}
}

// The trust window bounds readings that could reset early — i.e. DATED
// ones. A refusal with no reset instant does not roll over, so capping its
// rest at the trust window would make the escalation inert.
func TestReading_FreshTrustWindowDoesNotCapAResetlessRefusal(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	trust := Trust{MaxAge: DefaultMaxAge, Window: time.Hour, MaxRefusalRest: DefaultMaxRefusalRest}
	r := Reading{
		Window: WindowAuth, Status: StatusRejected, Refusals: 4,
		ObservedAt: now.Add(-2 * time.Hour),
	}
	if !r.Fresh(now, trust) {
		t.Fatal("a reset-less refusal is bounded by its own rest, not by the trust window")
	}
	// A DATED reading still dies at the trust window — #690's whole point.
	dated := Reading{
		Window: WindowSevenDay, Utilization: 0.99, Status: StatusWarning,
		ResetsAt: now.Add(72 * time.Hour), ObservedAt: now.Add(-2 * time.Hour),
	}
	if dated.Fresh(now, trust) {
		t.Fatal("a dated reading past the trust window must lapse (#690)")
	}
}

// The escape hatch: an operator can turn the escalation off.
func TestTrust_EscalationCanBeDisabled(t *testing.T) {
	trust := Trust{MaxAge: DefaultMaxAge, Window: DefaultTrustWindow, MaxRefusalRest: -1}.Normalized()
	r := Reading{Window: WindowAuth, Status: StatusRejected, Refusals: 8}
	if got := r.RestBound(trust); got != DefaultMaxAge {
		t.Fatalf("RestBound with the escalation off = %s, want %s", got, DefaultMaxAge)
	}
}

func TestTrustFromEnv_RefusalRestMax(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		got, err := TrustFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if got.MaxRefusalRest != DefaultMaxRefusalRest {
			t.Fatalf("MaxRefusalRest = %s, want %s", got.MaxRefusalRest, DefaultMaxRefusalRest)
		}
	})
	t.Run("set", func(t *testing.T) {
		t.Setenv(EnvRefusalRestMax, "90m")
		got, err := TrustFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if got.MaxRefusalRest != 90*time.Minute {
			t.Fatalf("MaxRefusalRest = %s, want 90m", got.MaxRefusalRest)
		}
	})
	t.Run("off", func(t *testing.T) {
		t.Setenv(EnvRefusalRestMax, "off")
		got, err := TrustFromEnv()
		if err != nil {
			t.Fatal(err)
		}
		if got.Normalized().MaxRefusalRest > 0 {
			t.Fatalf("MaxRefusalRest = %s, want the escalation disabled", got.MaxRefusalRest)
		}
	})
	t.Run("malformed refuses to start", func(t *testing.T) {
		t.Setenv(EnvRefusalRestMax, "soon")
		if _, err := TrustFromEnv(); err == nil {
			t.Fatal("a malformed duration must refuse to start, never silently default")
		}
	})
}
